// Package chatgpt signs a ChatGPT Plus/Pro/Team account into arkex and
// talks to the Codex backend with it. It is the OAuth flow the Codex CLI
// uses (PKCE against auth.openai.com, callback on localhost:1455), the
// token file that remembers the sign-in, an http.RoundTripper that keeps
// the access token fresh, and the model catalog the backend serves.
//
// Nothing here logs tokens. Errors carry HTTP status text, never bodies
// that could include a token.
package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	// Issuer is the OpenAI account service.
	Issuer = "https://auth.openai.com"
	// ClientID is the public OAuth client of the Codex CLI. Sign-ins made
	// with it may use a ChatGPT subscription.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// BaseURL is where the Codex backend serves the Responses API.
	BaseURL = "https://chatgpt.com/backend-api/codex"
	// Originator identifies the client family to the backend.
	Originator = "codex_cli_rs"
	// codexVersion is the Codex CLI release the backend knows us as; the
	// model catalog hides models newer than the client.
	codexVersion = "0.155.1"

	scope        = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	callbackPath = "/auth/callback"
	// refreshSlack refreshes the access token this long before it expires.
	refreshSlack = 2 * time.Minute
)

// callbackPorts are the loopback ports registered for the client's
// redirect URI, tried in order.
var callbackPorts = []int{1455, 1457}

// Tokens is one signed-in account.
type Tokens struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	IDToken      string    `json:"idToken,omitempty"`
	AccountID    string    `json:"accountId"`
	Email        string    `json:"email,omitempty"`
	Plan         string    `json:"plan,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Summary is the one-line "who" for a token set.
func (t Tokens) Summary() string {
	s := t.Email
	if s == "" {
		s = "signed in"
	}
	if t.Plan != "" {
		s += " · " + t.Plan
	}
	return s
}

// Expired reports whether the access token needs a refresh before use.
func (t Tokens) Expired(now time.Time) bool {
	return t.ExpiresAt.IsZero() || !now.Add(refreshSlack).Before(t.ExpiresAt)
}

// Endpoints lets tests point the flow at fake servers.
type Endpoints struct {
	Issuer  string // token/authorize service; Issuer when empty
	BaseURL string // Codex backend; BaseURL when empty
	// OpenBrowser is called with the authorize URL; nil opens the system
	// browser, a no-op function skips that (tests, headless hosts).
	OpenBrowser func(url string) error
	// Ports overrides callbackPorts.
	Ports []int
}

func (e Endpoints) issuer() string {
	if e.Issuer != "" {
		return strings.TrimRight(e.Issuer, "/")
	}
	return Issuer
}

func (e Endpoints) base() string {
	if e.BaseURL != "" {
		return strings.TrimRight(e.BaseURL, "/")
	}
	return BaseURL
}

// Login is one in-progress browser sign-in.
type Login struct {
	// URL is the page the user must open; shown so it can be copied when
	// the browser did not open (ssh, containers).
	URL  string
	done chan result
	srv  *http.Server
	ln   net.Listener
}

type result struct {
	tokens Tokens
	err    error
}

// StartLogin binds the callback port, opens the browser and returns
// without waiting. Call Wait to get the tokens; Close to give up.
func StartLogin(ctx context.Context, client *http.Client, ep Endpoints) (*Login, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	ports := ep.Ports
	if len(ports) == 0 {
		ports = callbackPorts
	}
	var ln net.Listener
	var err error
	for _, port := range ports {
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("sign-in needs a free local port (%v): %w — is another sign-in running?", ports, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirect := fmt.Sprintf("http://localhost:%d%s", port, callbackPath)

	verifier, challenge, err := pkce()
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	state, err := randomToken(24)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}

	q := url.Values{
		"response_type":              {"code"},
		"client_id":                  {ClientID},
		"redirect_uri":               {redirect},
		"scope":                      {scope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {Originator},
	}
	l := &Login{
		URL:  ep.issuer() + "/oauth/authorize?" + q.Encode(),
		done: make(chan result, 1),
		ln:   ln,
	}

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		qs := r.URL.Query()
		if e := qs.Get("error"); e != "" {
			desc := qs.Get("error_description")
			if desc == "" {
				desc = e
			}
			l.finish(w, Tokens{}, fmt.Errorf("sign-in refused: %s", desc))
			return
		}
		if qs.Get("state") != state {
			l.finish(w, Tokens{}, errors.New("sign-in callback did not match this attempt (state mismatch)"))
			return
		}
		code := qs.Get("code")
		if code == "" {
			l.finish(w, Tokens{}, errors.New("sign-in callback had no code"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		t, err := exchange(ctx, client, ep.issuer(), url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {ClientID},
			"code":          {code},
			"redirect_uri":  {redirect},
			"code_verifier": {verifier},
		}, true)
		l.finish(w, t, err)
	})
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go l.srv.Serve(ln) //nolint:errcheck // Serve returns when Close is called

	open := ep.OpenBrowser
	if open == nil {
		open = OpenBrowser
	}
	_ = open(l.URL) // the URL is shown to the user either way
	return l, nil
}

func (l *Login) finish(w http.ResponseWriter, t Tokens, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, page, "Sign-in did not finish", "arkex could not complete the sign-in: "+htmlEscape(err.Error())+". Go back to the terminal to try again.")
	} else {
		_, _ = fmt.Fprintf(w, page, "Signed in to arkex", "You can close this tab and return to the terminal.")
	}
	select {
	case l.done <- result{t, err}:
	default:
	}
}

// Wait blocks until the browser round-trip finishes, ctx ends, or Close
// is called.
func (l *Login) Wait(ctx context.Context) (Tokens, error) {
	select {
	case r := <-l.done:
		l.Close()
		return r.tokens, r.err
	case <-ctx.Done():
		l.Close()
		return Tokens{}, ctx.Err()
	}
}

// Close stops the callback server.
func (l *Login) Close() {
	if l.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = l.srv.Shutdown(ctx)
	}
}

const page = `<!doctype html><html><head><meta charset="utf-8"><title>%[1]s</title>
<style>body{font:16px/1.5 system-ui,sans-serif;background:#111;color:#eee;display:grid;place-items:center;height:100vh;margin:0}
main{max-width:32rem;padding:2rem;text-align:center}h1{font-size:1.4rem;color:#4ea1ff}</style></head>
<body><main><h1>%[1]s</h1><p>%[2]s</p></main></body></html>`

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// Refresh trades the refresh token for a new access token.
func Refresh(ctx context.Context, client *http.Client, ep Endpoints, t Tokens) (Tokens, error) {
	if t.RefreshToken == "" {
		return Tokens{}, errors.New("no refresh token; sign in again")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	nt, err := exchange(ctx, client, ep.issuer(), url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {t.RefreshToken},
	}, false)
	if err != nil {
		return Tokens{}, err
	}
	// The service may omit fields it did not rotate.
	if nt.RefreshToken == "" {
		nt.RefreshToken = t.RefreshToken
	}
	if nt.IDToken == "" {
		nt.IDToken, nt.Email, nt.Plan = t.IDToken, t.Email, t.Plan
	}
	if nt.AccountID == "" {
		nt.AccountID = t.AccountID
	}
	return nt, nil
}

// exchange posts to /oauth/token and parses the tokens out. The code grant
// is form-encoded, the refresh grant JSON, matching what the service
// expects from this client.
func exchange(ctx context.Context, client *http.Client, issuer string, form url.Values, asForm bool) (Tokens, error) {
	var body io.Reader
	ctype := "application/json"
	if asForm {
		body, ctype = strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
	} else {
		m := map[string]string{}
		for k := range form {
			m[k] = form.Get(k)
		}
		b, _ := json.Marshal(m)
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/oauth/token", body)
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Tokens{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
			Desc  string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &e)
		switch {
		case e.Error == "invalid_grant" || resp.StatusCode == http.StatusUnauthorized:
			return Tokens{}, &SignedOutError{Status: resp.Status, Code: e.Error}
		case e.Desc != "":
			return Tokens{}, fmt.Errorf("token request: %s: %s", resp.Status, e.Desc)
		}
		return Tokens{}, fmt.Errorf("token request: %s", resp.Status)
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil || tr.AccessToken == "" {
		return Tokens{}, errors.New("token request: response had no access token")
	}
	now := time.Now()
	t := Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		UpdatedAt:    now,
	}
	if exp, ok := claimTime(tr.AccessToken, "exp"); ok {
		t.ExpiresAt = exp
	} else if tr.ExpiresIn > 0 {
		t.ExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	} else {
		t.ExpiresAt = now.Add(time.Hour)
	}
	for _, jwt := range []string{tr.IDToken, tr.AccessToken} {
		c := claims(jwt)
		if t.AccountID == "" {
			t.AccountID = c.AccountID
		}
		if t.Email == "" {
			t.Email = c.Email
		}
		if t.Plan == "" {
			t.Plan = c.Plan
		}
	}
	if t.AccountID == "" {
		return Tokens{}, errors.New("sign-in did not include a ChatGPT account id; is this a ChatGPT account?")
	}
	return t, nil
}

// SignedOutError means the refresh token was rejected and the user has to
// sign in again.
type SignedOutError struct {
	Status string
	Code   string
}

func (e *SignedOutError) Error() string {
	return "ChatGPT sign-in expired (" + e.Status + "); sign in again from the Connections panel"
}

// jwtClaims is the subset of the OpenAI id/access token we read.
type jwtClaims struct {
	Email     string
	AccountID string
	Plan      string
}

func claims(jwt string) jwtClaims {
	var out jwtClaims
	var c struct {
		Email   string `json:"email"`
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			Plan      string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if !decodeJWT(jwt, &c) {
		return out
	}
	out.Email = c.Email
	if out.Email == "" {
		out.Email = c.Profile.Email
	}
	out.AccountID, out.Plan = c.Auth.AccountID, c.Auth.Plan
	return out
}

func claimTime(jwt, name string) (time.Time, bool) {
	var m map[string]any
	if !decodeJWT(jwt, &m) {
		return time.Time{}, false
	}
	f, ok := m[name].(float64)
	if !ok || f <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// decodeJWT unmarshals the payload of a JWT without verifying it: the
// tokens come straight from the issuer over TLS, and we only read
// display data and expiry from them.
func decodeJWT(jwt string, v any) bool {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}

func pkce() (verifier, challenge string, err error) {
	verifier, err = randomToken(64)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// OpenBrowser opens u in the system browser and returns once the opener
// was started; it does not wait for the page. $BROWSER, when set, names
// the program to use instead of the platform default.
func OpenBrowser(u string) error {
	var cmd *exec.Cmd
	if b := os.Getenv("BROWSER"); b != "" {
		cmd = exec.Command(b, u)
		return cmd.Start()
	}
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}
