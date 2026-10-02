// Package chatgpt implements OpenAI's Sign in with ChatGPT flow.
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
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	Issuer       = "https://auth.openai.com"
	ClientID     = "dynamic_agent_client"
	BaseURL      = "https://api.openai.com/v1"
	scope        = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	callbackPath = "/auth/callback"
	refreshSlack = 2 * time.Minute
)

var callbackPorts = []int{1455, 1457, 0}

type Tokens struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	IDToken      string    `json:"idToken,omitempty"`
	AccountID    string    `json:"accountId"` // retained for old records/backends
	Email        string    `json:"email,omitempty"`
	Plan         string    `json:"plan,omitempty"` // retained for old records
	ExpiresAt    time.Time `json:"expiresAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	ClientID     string    `json:"clientId,omitempty"`
	HostID       string    `json:"hostId,omitempty"`
	Issuer       string    `json:"issuer,omitempty"`
	Subject      string    `json:"subject,omitempty"`
	Scopes       string    `json:"scopes,omitempty"`
}

func (t Tokens) PlanEnabled() bool {
	for _, s := range strings.Fields(t.Scopes) {
		if s == "chatgpt.tokens.use.direct" {
			return true
		}
	}
	return false
}
func (t Tokens) ValidateAccess() error {
	if t.ClientID == "" || t.ClientID == ClientID || t.Subject == "" || t.Issuer == "" {
		return errors.New("ChatGPT connection needs the new sign-in flow; open /connections and choose Continue with ChatGPT")
	}
	if t.AccessToken == "" {
		return errors.New("ChatGPT sign-in needed: no access token")
	}
	if !t.PlanEnabled() {
		return errors.New("ChatGPT plan usage is not enabled for this connection")
	}
	return nil
}
func (t Tokens) Summary() string {
	who := t.Email
	if who == "" {
		who = t.Subject
	}
	if who == "" {
		return "sign-in needed"
	}
	if t.ClientID == "" || t.ClientID == ClientID || t.Subject == "" || t.AccessToken == "" {
		return who + " · sign-in needed"
	}
	if !t.PlanEnabled() {
		return who + " · plan disabled"
	}
	return who
}
func (t Tokens) Expired(now time.Time) bool {
	return t.ExpiresAt.IsZero() || !now.Add(refreshSlack).Before(t.ExpiresAt)
}

type Endpoints struct {
	Issuer       string
	BaseURL      string
	OpenBrowser  func(string) error
	Ports        []int
	Store        *Store
	Registration Tokens
	// Consent is set only when the user explicitly enables plan usage.
	Consent bool
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

type Login struct {
	URL      string
	done     chan result
	srv      *http.Server
	ln       net.Listener
	cancel   context.CancelFunc
	once     sync.Once
	accepted atomic.Bool
}
type result struct {
	tokens Tokens
	err    error
}

func StartLogin(ctx context.Context, client *http.Client, ep Endpoints) (*Login, error) {
	client = oauthClient(client)
	store := ep.Store
	if store == nil {
		var err error
		store, err = DefaultStore()
		if err != nil {
			return nil, err
		}
	}
	hostID, err := store.hostID(ctx)
	if err != nil {
		return nil, err
	}
	ports := ep.Ports
	if len(ports) == 0 {
		ports = callbackPorts
	}
	var ln net.Listener
	for _, p := range ports {
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("sign-in needs a free local port (%v): %w", ports, err)
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d%s", ln.Addr().(*net.TCPAddr).Port, callbackPath)
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
	nonce, err := randomToken(24)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	reg := ep.Registration
	if reg.ClientID != "" && reg.Issuer != ep.issuer() {
		_ = ln.Close()
		return nil, errors.New("saved ChatGPT registration has a different issuer")
	}
	clientID := reg.ClientID
	fresh := clientID == ""
	if fresh {
		clientID = ClientID
	} else if clientID == ClientID {
		_ = ln.Close()
		return nil, errors.New("saved ChatGPT registration has an invalid client ID")
	}
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "scope": {scope}, "resource": {BaseURL}, "state": {state}, "nonce": {nonce}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "ext_agent_host_id": {hostID}}
	if fresh {
		q.Set("agent_name_hint", "arkex")
	} else if reg.Email != "" {
		q.Set("login_hint", reg.Email)
	}
	if ep.Consent {
		q.Set("prompt", "consent")
	}
	flowCtx, cancel := context.WithCancel(ctx)
	l := &Login{URL: ep.issuer() + "/api/accounts/authorize?" + q.Encode(), done: make(chan result, 1), ln: ln, cancel: cancel}
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		qs := r.URL.Query()
		// Invalid callbacks neither consume nor alter the real pending login.
		if qs.Get("state") != state {
			http.Error(w, "sign-in callback state mismatch", http.StatusBadRequest)
			return
		}
		if !l.accepted.CompareAndSwap(false, true) {
			http.Error(w, "sign-in callback already handled", http.StatusConflict)
			return
		}
		if e := qs.Get("error"); e != "" {
			l.finish(w, Tokens{}, fmt.Errorf("sign-in refused (%s)", safeCode(e)))
			return
		}
		code := qs.Get("code")
		if code == "" {
			l.finish(w, Tokens{}, errors.New("sign-in callback had no code"))
			return
		}
		issued := qs.Get("client_id")
		if fresh {
			if issued == "" || issued == ClientID {
				l.finish(w, Tokens{}, errors.New("sign-in did not issue a client ID"))
				return
			}
			clientID = issued
		} else if issued != "" && issued != clientID {
			l.finish(w, Tokens{}, errors.New("sign-in returned a different client ID"))
			return
		}
		t, exErr := exchange(flowCtx, client, ep.issuer(), url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {BaseURL}}, clientID, nonce, Tokens{})
		if exErr == nil {
			t.ClientID = clientID
			t.HostID = hostID
			if reg.Subject != "" && t.Subject != reg.Subject {
				exErr = errors.New("sign-in returned a different account")
				t = Tokens{}
			}
		}
		if _, ok := exErr.(*SignedOutError); ok && fresh { // Keep registration locally when the one-time code was lost.
			t = Tokens{ClientID: clientID, HostID: hostID, Issuer: ep.issuer()}
		}
		l.finish(w, t, exErr)
	})
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = l.srv.Serve(ln) }()
	go func() {
		<-flowCtx.Done()
		l.complete(Tokens{}, flowCtx.Err())
		shutdown, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if l.srv.Shutdown(shutdown) != nil {
			_ = l.srv.Close()
		}
	}()
	open := ep.OpenBrowser
	if open == nil {
		open = OpenBrowser
	}
	_ = open(l.URL)
	return l, nil
}

func (l *Login) complete(t Tokens, err error) {
	l.once.Do(func() { l.done <- result{t, err} })
}
func (l *Login) finish(w http.ResponseWriter, t Tokens, err error) {
	if err != nil {
		http.Error(w, "Sign-in did not finish. Return to arkex.", http.StatusBadRequest)
	} else {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, page)
	}
	l.complete(t, err)
}
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
func (l *Login) Close() {
	if l.cancel != nil {
		l.cancel()
	}
	if l.srv != nil {
		c, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = l.srv.Shutdown(c)
	}
	l.complete(Tokens{}, context.Canceled)
}

const page = `<!doctype html><html><head><meta charset="utf-8"><title>Signed in to arkex</title>
<style>body{font:16px/1.5 system-ui,sans-serif;background:#111;color:#eee;display:grid;place-items:center;height:100vh;margin:0}
main{max-width:32rem;padding:2rem;text-align:center}h1{font-size:1.4rem;color:#4ea1ff}</style></head>
<body><main><h1>Signed in to arkex</h1><p>You can close this tab and return to the terminal.</p></main></body></html>`

// Token grants must never follow redirects carrying credentials to another URL.
func oauthClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func revoke(ctx context.Context, client *http.Client, ep Endpoints, t Tokens) error {
	if t.ClientID == "" || t.ClientID == ClientID || t.Issuer != ep.issuer() {
		return errors.New("no matching ChatGPT registration")
	}
	client = oauthClient(client)
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, client), ep.issuer())
	if err != nil {
		return errors.New("revocation discovery unavailable")
	}
	var metadata struct {
		URL string `json:"revocation_endpoint"`
	}
	if p.Claims(&metadata) != nil {
		return errors.New("invalid revocation metadata")
	}
	u, err := url.Parse(metadata.URL)
	issuer, _ := url.Parse(ep.issuer())
	if err != nil || u.Host != issuer.Host || u.Scheme != issuer.Scheme || u.User != nil {
		return errors.New("untrusted revocation endpoint")
	}
	form := url.Values{"token": {t.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {t.ClientID}}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			if resp.StatusCode < 500 {
				break
			}
		}
		if attempt == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return errors.New("revocation not confirmed")
}

func Refresh(ctx context.Context, client *http.Client, ep Endpoints, t Tokens) (Tokens, error) {
	if t.RefreshToken == "" {
		return Tokens{}, errors.New("no refresh token; sign in again")
	}
	if t.ClientID == "" || t.ClientID == ClientID {
		return Tokens{}, errors.New("legacy ChatGPT sign-in must be renewed")
	}
	if t.Issuer != ep.issuer() {
		return Tokens{}, errors.New("ChatGPT registration issuer mismatch")
	}
	client = oauthClient(client)
	nt, err := exchange(ctx, client, ep.issuer(), url.Values{"grant_type": {"refresh_token"}, "client_id": {t.ClientID}, "refresh_token": {t.RefreshToken}, "resource": {BaseURL}}, t.ClientID, "", t)
	if err != nil {
		return Tokens{}, err
	}
	if nt.RefreshToken == "" {
		nt.RefreshToken = t.RefreshToken
	}
	if nt.IDToken == "" {
		nt.IDToken = t.IDToken
	}
	if nt.Subject != "" && nt.Subject != t.Subject {
		return Tokens{}, &SignedOutError{Status: "identity_changed", Code: "subject_mismatch"}
	}
	nt.ClientID = t.ClientID
	nt.HostID = t.HostID
	nt.Issuer = t.Issuer
	nt.Subject = t.Subject
	nt.Email = t.Email
	nt.AccountID = t.AccountID
	nt.Plan = t.Plan
	return nt, nil
}

func exchange(ctx context.Context, client *http.Client, issuer string, form url.Values, audience, nonce string, previous Tokens) (Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/api/accounts/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		var oe struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &oe)
		switch oe.Error {
		case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
			return Tokens{}, &SignedOutError{Status: resp.Status, Code: safeCode(oe.Error)}
		}
		return Tokens{}, fmt.Errorf("token request failed (%s, %s)", resp.Status, safeCode(oe.Error))
	}
	var tr struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		IDToken      string  `json:"id_token"`
		Scope        *string `json:"scope"`
		TokenType    string  `json:"token_type"`
		ExpiresIn    int64   `json:"expires_in"`
	}
	if json.Unmarshal(raw, &tr) != nil || tr.AccessToken == "" {
		return Tokens{}, errors.New("token request response had no access token")
	}
	if tr.IDToken == "" && nonce != "" {
		return Tokens{}, errors.New("token response had no ID token")
	}
	if !strings.EqualFold(tr.TokenType, "Bearer") || tr.ExpiresIn <= 0 || tr.ExpiresIn > int64((1<<63-1)/time.Second) {
		return Tokens{}, errors.New("token response had invalid token type or expiry")
	}
	t := Tokens{AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, IDToken: tr.IDToken, Issuer: issuer, Scopes: previous.Scopes, UpdatedAt: time.Now()}
	if tr.Scope != nil {
		t.Scopes = *tr.Scope
	}
	t.ExpiresAt = t.UpdatedAt.Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.IDToken != "" {
		provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
		if err != nil {
			return Tokens{}, errors.New("could not load the trusted OpenAI identity provider")
		}
		tok, err := provider.Verifier(&oidc.Config{ClientID: audience}).Verify(oidc.ClientContext(ctx, client), tr.IDToken)
		if err != nil {
			return Tokens{}, errors.New("ID token signature or claims validation failed")
		}
		var c struct {
			Subject string `json:"sub"`
			Email   string `json:"email"`
			Nonce   string `json:"nonce"`
		}
		if err = tok.Claims(&c); err != nil {
			return Tokens{}, errors.New("invalid ID token claims")
		}
		if c.Subject == "" {
			return Tokens{}, errors.New("ID token had no subject")
		}
		if nonce != "" && c.Nonce != nonce {
			return Tokens{}, errors.New("ID token nonce mismatch")
		}
		t.Subject = c.Subject
		t.Email = c.Email
	}
	return t, nil
}

type SignedOutError struct {
	Status string
	Code   string
}

func (e *SignedOutError) Error() string {
	return "ChatGPT sign-in expired (" + e.Status + ", " + safeCode(e.Code) + "); sign in again"
}
func safeCode(s string) string {
	switch s {
	case "access_denied", "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused", "invalid_client", "invalid_scope", "invalid_request", "server_error", "temporarily_unavailable", "subject_mismatch":
		return s
	}
	return "oauth_error"
}

func pkce() (string, string, error) {
	v, e := randomToken(64)
	if e != nil {
		return "", "", e
	}
	s := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(s[:]), nil
}
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func OpenBrowser(u string) error {
	var c *exec.Cmd
	if b := os.Getenv("BROWSER"); b != "" {
		c = exec.Command(b, u)
	} else if runtime.GOOS == "darwin" {
		c = exec.Command("open", u)
	} else {
		c = exec.Command("xdg-open", u)
	}
	return c.Start()
}
