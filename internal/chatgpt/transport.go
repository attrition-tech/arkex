package chatgpt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Transport signs requests to the Codex backend. It adds the bearer and
// account headers, refreshes the access token before it expires (and once
// more on a 401), saves refreshed tokens back to the Store, and reshapes
// Responses API bodies the way the backend expects them.
type Transport struct {
	Base      http.RoundTripper // nil: http.DefaultTransport
	Endpoints Endpoints
	Store     *Store // nil: refreshed tokens stay in memory
	ID        string // connection id in Store
	UserAgent string // arkex's own UA, appended to the client UA

	mu     sync.Mutex
	tokens Tokens
	now    func() time.Time
}

// NewTransport wraps base with the given tokens.
func NewTransport(base http.RoundTripper, tokens Tokens, store *Store, id, userAgent string) *Transport {
	return &Transport{Base: base, Store: store, ID: id, UserAgent: userAgent, tokens: tokens, now: time.Now}
}

// Tokens returns the current token set.
func (t *Transport) Tokens() Tokens {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tokens
}

func (t *Transport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

// token returns a usable access token, refreshing when it is (nearly)
// expired or when force is set.
func (t *Transport) token(ctx context.Context, force bool) (Tokens, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !force && !t.tokens.Expired(t.now()) {
		return t.tokens, nil
	}
	client := &http.Client{Transport: t.base(), Timeout: 30 * time.Second}
	if t.Store == nil || t.ID == "" {
		nt, err := Refresh(ctx, client, t.Endpoints, t.tokens)
		if err != nil {
			return Tokens{}, err
		}
		t.tokens = nt
		return nt, nil
	}

	current := t.tokens
	nt, err := t.Store.Update(ctx, t.ID, func(saved Tokens) (Tokens, error) {
		// Another process may already have refreshed while this transport was
		// waiting for the store lock. On a forced refresh, differing saved
		// tokens mean the rejected request used an older token pair.
		rotated := saved.AccessToken != current.AccessToken || saved.RefreshToken != current.RefreshToken
		if !saved.Expired(t.now()) && (!force || rotated) {
			return saved, nil
		}
		return Refresh(ctx, client, t.Endpoints, saved)
	})
	if err != nil {
		return Tokens{}, fmt.Errorf("refreshing saved sign-in: %w", err)
	}
	// Do not expose a rotated token in memory unless it was persisted.
	t.tokens = nt
	return nt, nil
}

// UserAgentString is what the backend sees: the Codex client family and
// version, the platform, then arkex's own agent string.
func (t *Transport) UserAgentString() string {
	ua := fmt.Sprintf("%s/%s (%s; %s)", Originator, codexVersion, runtime.GOOS, runtime.GOARCH)
	if t.UserAgent != "" {
		ua += " " + t.UserAgent
	}
	return ua
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		body, err = io.ReadAll(req.Body)
		closeErr := req.Body.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/responses") {
		body = rewriteResponses(body)
	}

	send := func(force bool) (*http.Response, error) {
		tok, err := t.token(req.Context(), force)
		if err != nil {
			return nil, err
		}
		r := req.Clone(req.Context())
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if tok.AccountID != "" {
			r.Header.Set("ChatGPT-Account-ID", tok.AccountID)
		}
		r.Header.Set("originator", Originator)
		r.Header.Set("User-Agent", t.UserAgentString())
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.Header.Del("Content-Encoding")
		}
		return t.base().RoundTrip(r)
	}

	resp, err := send(false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) //nolint:errcheck
		_ = resp.Body.Close()
		return send(true)
	}
	return resp, nil
}

// rewriteResponses shapes a Responses API body for the Codex backend: the
// leading system/developer message becomes `instructions` (the backend
// wants the base prompt there), responses are never stored, streaming is
// on, reasoning is asked to come back encrypted so multi-turn context
// survives store=false, and the output cap the backend does not accept is
// dropped. Bodies that are not JSON objects pass through untouched.
func rewriteResponses(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body
	}
	if s, _ := m["instructions"].(string); s == "" {
		if items, ok := m["input"].([]any); ok && len(items) > 0 {
			msg, _ := items[0].(map[string]any)
			role, _ := msg["role"].(string)
			if role == "system" || role == "developer" {
				if text := contentText(msg["content"]); text != "" {
					m["instructions"] = text
					m["input"] = items[1:]
				}
			}
		}
	}
	m["store"] = false
	m["stream"] = true
	delete(m, "max_output_tokens")
	inc, _ := m["include"].([]any)
	has := false
	for _, v := range inc {
		if v == "reasoning.encrypted_content" {
			has = true
		}
	}
	if !has {
		m["include"] = append(inc, "reasoning.encrypted_content")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// contentText flattens a Responses message content (a string or a list of
// input_text parts) into one string.
func contentText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, p := range v {
			part, _ := p.(map[string]any)
			if s, _ := part["text"].(string); s != "" {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}
