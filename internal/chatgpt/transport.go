package chatgpt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3/packages/ssestream"
)

// Transport signs requests to the public Responses API, refreshes the access
// token before it expires (and once more on a 401), and saves refreshed tokens
// back to the Store.
type Transport struct {
	Base      http.RoundTripper // nil: http.DefaultTransport
	Endpoints Endpoints
	Store     *Store // nil: refreshed tokens stay in memory
	ID        string // connection id in Store
	UserAgent string // arkex's application user agent

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
	// Refresh/discovery requests are preparation, not inference dispatch.
	// Retain cancellation, deadlines and other values, but hide its HTTP trace.
	ctx = authContext{ctx}
	t.mu.Lock()
	defer t.mu.Unlock()
	adopted := false
	// A connection can be signed out or re-registered while a model remains
	// open. Never continue with the transport's cached bearer in that case.
	if t.Store != nil && t.ID != "" {
		saved, ok, err := t.Store.Get(t.ID)
		if err != nil {
			return Tokens{}, err
		}
		if !ok {
			return Tokens{}, fmt.Errorf("connection %q is not signed in; sign in with ChatGPT again", t.ID)
		}
		if saved.ClientID != t.tokens.ClientID || saved.Subject != t.tokens.Subject || saved.Issuer != t.tokens.Issuer {
			return Tokens{}, fmt.Errorf("ChatGPT registration changed; sign in again before continuing")
		}
		if err := saved.ValidateAccess(); err != nil {
			return Tokens{}, err
		}
		if saved.AccessToken != t.tokens.AccessToken || saved.RefreshToken != t.tokens.RefreshToken {
			adopted = true
		}
		t.tokens = saved
	}
	if err := t.tokens.ValidateAccess(); err != nil {
		return Tokens{}, err
	}
	if force && adopted && !t.tokens.Expired(t.now()) {
		return t.tokens, nil
	}
	if !force && !t.tokens.Expired(t.now()) {
		return t.tokens, nil
	}
	client := &http.Client{Transport: t.base(), Timeout: 30 * time.Second}
	if t.Store == nil || t.ID == "" {
		nt, err := Refresh(ctx, client, t.Endpoints, t.tokens)
		if err != nil {
			var signedOut *SignedOutError
			if errors.As(err, &signedOut) {
				t.tokens.AccessToken, t.tokens.RefreshToken, t.tokens.IDToken = "", "", ""
			}
			return Tokens{}, err
		}
		t.tokens = nt
		return nt, nt.ValidateAccess()
	}

	current := t.tokens
	nt, err := t.Store.Update(ctx, t.ID, func(saved Tokens) (Tokens, error) {
		if saved.ClientID != current.ClientID || saved.Subject != current.Subject || saved.Issuer != current.Issuer {
			return Tokens{}, fmt.Errorf("ChatGPT registration changed; reconnect before continuing")
		}
		if err := saved.ValidateAccess(); err != nil {
			return Tokens{}, err
		}
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
	return nt, nt.ValidateAccess()
}

type authContext struct{ context.Context }

func (c authContext) Value(key any) any {
	v := c.Context.Value(key)
	if _, ok := v.(*httptrace.ClientTrace); ok {
		return nil
	}
	return v
}

// UserAgentString is arkex's ordinary application user agent.
func (t *Transport) UserAgentString() string {
	if t.UserAgent == "" {
		return "arkex"
	}
	return t.UserAgent
}

func (t *Transport) requestAllowed(req *http.Request) bool {
	base, err := url.Parse(t.Endpoints.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		base, err = url.Parse(BaseURL)
	}
	if err != nil || req.URL.Scheme != base.Scheme || req.URL.Host != base.Host || req.URL.User != nil {
		return false
	}
	path := strings.TrimRight(base.Path, "/")
	return (req.Method == http.MethodGet && req.URL.Path == path+"/models") ||
		(req.Method == http.MethodPost && req.URL.Path == path+"/responses")
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.requestAllowed(req) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("refusing to send ChatGPT credentials to unexpected origin %q", req.URL.Host)
	}
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
		resp, err = send(true)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode == http.StatusOK && req.Method == http.MethodPost {
		resp.Body = &planStream{Decoder: ssestream.NewDecoder(resp)}
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
	}
	return resp, nil
}

// rewriteResponses enforces the restrictions of ChatGPT plan inference.
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
	if text, ok := m["input"].(string); ok {
		m["input"] = []any{map[string]any{"role": "user", "content": text}}
	} else if _, ok := m["input"].([]any); !ok {
		m["input"] = []any{}
	}
	m["store"] = false
	m["stream"] = true
	for _, field := range []string{"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata", "moderation", "multi_agent", "prompt", "prompt_cache_retention", "safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user", "previous_response_id"} {
		delete(m, field)
	}
	// System input items are rejected. Preserve their semantics by converting
	// them to developer messages when they cannot be lifted into instructions.
	if items, ok := m["input"].([]any); ok {
		for _, item := range items {
			obj, _ := item.(map[string]any)
			if obj["role"] == "system" {
				obj["role"] = "developer"
			}
			if obj["type"] == "function_call" && obj["namespace"] == nil {
				obj["namespace"] = "arkex"
			}
		}
	}
	// Fantasy v0.43 emits flat local tools. The direct route requires local
	// function/custom tools to be grouped in a namespace.
	if tools, ok := m["tools"].([]any); ok {
		var local, other []any
		for _, tool := range tools {
			obj, _ := tool.(map[string]any)
			typ, _ := obj["type"].(string)
			if typ == "function" || typ == "custom" {
				local = append(local, tool)
			} else {
				other = append(other, tool)
			}
		}
		if len(local) > 0 {
			m["tools"] = append(other, map[string]any{
				"type": "namespace", "name": "arkex",
				"description": "Tools executed by arkex to inspect files, make changes, and run commands in the user's workspace.",
				"tools":       local,
			})
		}
	}
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
