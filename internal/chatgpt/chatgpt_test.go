package chatgpt

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// jwt builds an unsigned JWT with the given payload; the code never
// verifies signatures so a fake header/signature is enough.
func jwt(t *testing.T, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(b) + ".sig"
}

func accessJWT(t *testing.T, exp time.Time) string {
	return jwt(t, map[string]any{
		"exp": exp.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_1",
			"chatgpt_plan_type":  "plus",
		},
	})
}

func idJWT(t *testing.T) string {
	return jwt(t, map[string]any{
		"email": "dev@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_1",
			"chatgpt_plan_type":  "plus",
		},
	})
}

// issuer is a fake auth.openai.com. It records the grants it saw.
type issuer struct {
	t        *testing.T
	srv      *httptest.Server
	code     string // the code it hands out
	refresh  string
	exchange atomic.Int32
	refreshN atomic.Int32
	// rejectRefresh makes refresh return invalid_grant.
	rejectRefresh atomic.Bool
	lastCodeForm  url.Values
	lastRefresh   map[string]string
}

func newIssuer(t *testing.T) *issuer {
	is := &issuer{t: t, code: "code-123", refresh: "rt-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		ct := r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
			form, err := url.ParseQuery(string(body))
			if err != nil {
				http.Error(w, "bad form", 400)
				return
			}
			is.lastCodeForm = form
			is.exchange.Add(1)
			if form.Get("grant_type") != "authorization_code" || form.Get("code") != is.code {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"invalid_grant"}`)) //nolint:errcheck
				return
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"access_token":  accessJWT(t, time.Now().Add(time.Hour)),
				"refresh_token": is.refresh,
				"id_token":      idJWT(t),
				"expires_in":    3600,
			})
		case strings.HasPrefix(ct, "application/json"):
			var m map[string]string
			if err := json.Unmarshal(body, &m); err != nil {
				http.Error(w, "bad json", 400)
				return
			}
			is.lastRefresh = m
			is.refreshN.Add(1)
			if is.rejectRefresh.Load() || m["grant_type"] != "refresh_token" || m["refresh_token"] != is.refresh {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"invalid_grant","error_description":"expired"}`)) //nolint:errcheck
				return
			}
			is.refresh = "rt-2"
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"access_token":  accessJWT(t, time.Now().Add(2*time.Hour)),
				"refresh_token": is.refresh,
			})
		default:
			http.Error(w, "content type "+ct, http.StatusUnsupportedMediaType)
		}
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

func (is *issuer) endpoints() Endpoints {
	return Endpoints{Issuer: is.srv.URL, OpenBrowser: func(string) error { return nil }, Ports: []int{0}}
}

func TestLoginRoundTrip(t *testing.T) {
	is := newIssuer(t)
	var opened string
	ep := is.endpoints()
	ep.OpenBrowser = func(u string) error { opened = u; return nil }

	l, err := StartLogin(context.Background(), is.srv.Client(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if opened != l.URL {
		t.Fatalf("browser got %q, Login.URL %q", opened, l.URL)
	}
	au, err := url.Parse(l.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := au.Query()
	if !strings.HasPrefix(l.URL, is.srv.URL+"/oauth/authorize?") {
		t.Fatalf("authorize URL %q", l.URL)
	}
	for k, want := range map[string]string{
		"response_type": "code", "client_id": ClientID, "code_challenge_method": "S256",
		"scope": scope, "codex_cli_simplified_flow": "true", "originator": Originator,
		"id_token_add_organizations": "true",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if q.Get("state") == "" || q.Get("code_challenge") == "" {
		t.Fatalf("missing state/challenge in %v", q)
	}
	redirect := q.Get("redirect_uri")
	if !strings.HasPrefix(redirect, "http://localhost:") || !strings.HasSuffix(redirect, callbackPath) {
		t.Fatalf("redirect_uri %q", redirect)
	}
	// The callback host is the loopback listener; localhost in the URL is
	// for the browser.
	cb := "http://" + l.ln.Addr().String() + callbackPath

	// Wrong state must be refused and must not consume the attempt.
	resp, err := http.Get(cb + "?code=code-123&state=nope")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state: status %d", resp.StatusCode)
	}
	if is.exchange.Load() != 0 {
		t.Fatal("bad state reached the token endpoint")
	}
	// The first result wins; drain it so the good callback is observed.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := l.Wait(ctx); err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("Wait after bad state: %v", err)
	}

	// Fresh login for the happy path.
	l2, err := StartLogin(context.Background(), is.srv.Client(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	q2 := mustQuery(t, l2.URL)
	cb2 := "http://" + l2.ln.Addr().String() + callbackPath
	resp, err = http.Get(cb2 + "?code=code-123&state=" + url.QueryEscape(q2.Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.Contains(string(page), "Signed in") {
		t.Fatalf("callback: %d %s", resp.StatusCode, page)
	}
	tok, err := l2.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccountID != "acct_1" || tok.Email != "dev@example.com" || tok.Plan != "plus" || tok.RefreshToken != "rt-1" {
		t.Fatalf("tokens %+v", tok)
	}
	if tok.Expired(time.Now()) {
		t.Fatalf("fresh token reported expired: %v", tok.ExpiresAt)
	}
	if got := tok.Summary(); got != "dev@example.com · plus" {
		t.Fatalf("summary %q", got)
	}
	f := is.lastCodeForm
	if f.Get("redirect_uri") != q2.Get("redirect_uri") || f.Get("code_verifier") == "" || f.Get("client_id") != ClientID {
		t.Fatalf("code exchange form %v", f)
	}
	// PKCE: the challenge in the URL must be S256 of the verifier sent.
	sum := sha256.Sum256([]byte(f.Get("code_verifier")))
	if ch := base64.RawURLEncoding.EncodeToString(sum[:]); ch != q2.Get("code_challenge") {
		t.Fatalf("challenge mismatch")
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestLoginCancelAndDenied(t *testing.T) {
	is := newIssuer(t)
	l, err := StartLogin(context.Background(), is.srv.Client(), is.endpoints())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
	// Port is free again after Close.
	l2, err := StartLogin(context.Background(), is.srv.Client(), is.endpoints())
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	cb := "http://" + l2.ln.Addr().String() + callbackPath
	resp, err := http.Get(cb + "?error=access_denied&error_description=user+said+no")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = l2.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "user said no") {
		t.Fatalf("denied: %v", err)
	}
}

func TestRefresh(t *testing.T) {
	is := newIssuer(t)
	old := Tokens{RefreshToken: "rt-1", AccountID: "acct_1", Email: "dev@example.com", Plan: "plus", IDToken: "x.y.z"}
	nt, err := Refresh(context.Background(), is.srv.Client(), is.endpoints(), old)
	if err != nil {
		t.Fatal(err)
	}
	if nt.RefreshToken != "rt-2" || nt.AccessToken == "" || nt.AccountID != "acct_1" || nt.Email != "dev@example.com" {
		t.Fatalf("refreshed %+v", nt)
	}
	if !nt.ExpiresAt.After(time.Now().Add(90 * time.Minute)) {
		t.Fatalf("expiry from access token exp not used: %v", nt.ExpiresAt)
	}
	if is.lastRefresh["grant_type"] != "refresh_token" || is.lastRefresh["client_id"] != ClientID {
		t.Fatalf("refresh body %v", is.lastRefresh)
	}

	is.rejectRefresh.Store(true)
	_, err = Refresh(context.Background(), is.srv.Client(), is.endpoints(), nt)
	var so *SignedOutError
	if !errors.As(err, &so) {
		t.Fatalf("rejected refresh: %v", err)
	}
	if _, err := Refresh(context.Background(), nil, is.endpoints(), Tokens{}); err == nil {
		t.Fatal("refresh without refresh token succeeded")
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Path: filepath.Join(dir, "sub", "auth.json")}
	if _, ok, err := s.Get("a"); err != nil || ok {
		t.Fatalf("empty store: %v %v", ok, err)
	}
	tok := Tokens{AccessToken: "at", RefreshToken: "rt", AccountID: "acct", Email: "e", ExpiresAt: time.Now().Round(time.Second)}
	if err := s.Put("a", tok); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o600 {
		t.Fatalf("auth.json mode %o", perm)
	}
	if dst, _ := os.Stat(filepath.Dir(s.Path)); runtime.GOOS != "windows" && dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o", dst.Mode().Perm())
	}
	got, ok, err := s.Get("a")
	if err != nil || !ok || got.RefreshToken != "rt" || !got.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Fatalf("get: %+v %v %v", got, ok, err)
	}
	if err := s.Rename("a", "b"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("a"); ok {
		t.Fatal("old id still present after rename")
	}
	if _, ok, _ := s.Get("b"); !ok {
		t.Fatal("new id missing after rename")
	}
	if err := s.Rename("missing", "c"); err != nil {
		t.Fatalf("rename of missing id: %v", err)
	}
	all, err := s.All()
	if err != nil || len(all) != 1 {
		t.Fatalf("all: %v %v", all, err)
	}
	if err := s.Remove("b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("b"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
	if all, _ := s.All(); len(all) != 0 {
		t.Fatalf("after remove: %v", all)
	}
	// Corrupt file is an error, not silently empty (would lose sign-ins on
	// the next Put).
	os.WriteFile(s.Path, []byte("{nope"), 0o600) //nolint:errcheck
	if _, _, err := s.Get("a"); err == nil {
		t.Fatal("corrupt auth.json read as empty")
	}
}

// backend is a fake chatgpt.com/backend-api/codex that records requests
// and can reject the first with 401.
type backend struct {
	srv      *httptest.Server
	reqs     []recorded
	reject   atomic.Int32 // number of 401s still to serve
	catalog  string
	catalogN atomic.Int32
}

type recorded struct {
	path, auth, account, originator, ua string
	body                                map[string]any
}

func newBackend(t *testing.T) *backend {
	b := &backend{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := recorded{path: r.URL.RequestURI(), auth: r.Header.Get("Authorization"), account: r.Header.Get("ChatGPT-Account-ID"),
			originator: r.Header.Get("originator"), ua: r.Header.Get("User-Agent")}
		if len(raw) > 0 {
			json.Unmarshal(raw, &rec.body) //nolint:errcheck
		}
		b.reqs = append(b.reqs, rec)
		if b.reject.Load() > 0 {
			b.reject.Add(-1)
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"message":"expired"}}`)) //nolint:errcheck
			return
		}
		if strings.HasPrefix(r.URL.Path, "/models") {
			b.catalogN.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(b.catalog)) //nolint:errcheck
			return
		}
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

func TestTransportHeadersAndRefresh(t *testing.T) {
	is := newIssuer(t)
	be := newBackend(t)
	store := &Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	tok := Tokens{AccessToken: "old", RefreshToken: "rt-1", AccountID: "acct_1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.Put("cg", tok); err != nil {
		t.Fatal(err)
	}
	tr := NewTransport(be.srv.Client().Transport, tok, store, "cg", "arkex/test")
	tr.Endpoints = is.endpoints()
	client := &http.Client{Transport: tr}

	do := func(body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, be.srv.URL+"/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := do(`{"model":"gpt-5","input":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"max_output_tokens":10}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(be.reqs) != 1 {
		t.Fatalf("%d requests", len(be.reqs))
	}
	r := be.reqs[0]
	if r.auth != "Bearer old" || r.account != "acct_1" || r.originator != Originator {
		t.Fatalf("headers %+v", r)
	}
	if !strings.HasPrefix(r.ua, "codex_cli_rs/"+codexVersion+" (") || !strings.HasSuffix(r.ua, " arkex/test") {
		t.Fatalf("user agent %q", r.ua)
	}
	if r.body["instructions"] != "be brief" || r.body["store"] != false || r.body["stream"] != true {
		t.Fatalf("rewritten body %v", r.body)
	}
	if _, ok := r.body["max_output_tokens"]; ok {
		t.Fatal("max_output_tokens kept")
	}
	if in, _ := r.body["input"].([]any); len(in) != 1 {
		t.Fatalf("system message not lifted out of input: %v", r.body["input"])
	}
	if is.refreshN.Load() != 0 {
		t.Fatal("refreshed a token that was still valid")
	}

	// A 401 refreshes once, retries with the new token, and persists it.
	be.reject.Store(1)
	resp = do(`{"model":"gpt-5","input":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("after 401: status %d", resp.StatusCode)
	}
	if len(be.reqs) != 3 || is.refreshN.Load() != 1 {
		t.Fatalf("requests %d refreshes %d", len(be.reqs), is.refreshN.Load())
	}
	if be.reqs[2].auth == "Bearer old" {
		t.Fatal("retry reused the rejected token")
	}
	saved, ok, _ := store.Get("cg")
	if !ok || saved.RefreshToken != "rt-2" || saved.AccessToken == "old" {
		t.Fatalf("store not updated: %+v", saved)
	}

	// Near expiry refreshes proactively, before sending.
	tr.tokens.ExpiresAt = time.Now().Add(30 * time.Second)
	if err := store.Put("cg", tr.tokens); err != nil {
		t.Fatal(err)
	}
	is.rejectRefresh.Store(false)
	do(`{"model":"gpt-5","input":[]}`)
	if is.refreshN.Load() != 2 {
		t.Fatalf("no proactive refresh: %d", is.refreshN.Load())
	}

	// A refresh the issuer rejects surfaces as SignedOutError to the caller.
	is.rejectRefresh.Store(true)
	tr.tokens.ExpiresAt = time.Now().Add(30 * time.Second)
	if err := store.Put("cg", tr.tokens); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, be.srv.URL+"/models", nil)
	_, err := client.Do(req)
	var so *SignedOutError
	if !errors.As(err, &so) {
		t.Fatalf("rejected refresh error: %v", err)
	}
}

func TestTransportConcurrentRefreshUsesStoreUpdate(t *testing.T) {
	var refreshes atomic.Int32
	freshAccess := accessJWT(t, time.Now().Add(2*time.Hour))
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
			"access_token": freshAccess, "refresh_token": "rt-new",
		})
	}))
	t.Cleanup(auth.Close)

	path := filepath.Join(t.TempDir(), "auth.json")
	old := Tokens{AccessToken: "old", RefreshToken: "rt-old", ExpiresAt: time.Now().Add(-time.Hour)}
	if err := (&Store{Path: path}).Put("cg", old); err != nil {
		t.Fatal(err)
	}
	transports := []*Transport{
		NewTransport(nil, old, &Store{Path: path}, "cg", ""),
		NewTransport(nil, old, &Store{Path: path}, "cg", ""),
	}
	for _, tr := range transports {
		tr.Endpoints = Endpoints{Issuer: auth.URL}
	}

	start := make(chan struct{})
	errs := make(chan error, len(transports))
	var wg sync.WaitGroup
	for _, tr := range transports {
		wg.Add(1)
		go func(tr *Transport) {
			defer wg.Done()
			<-start
			_, err := tr.token(context.Background(), false)
			errs <- err
		}(tr)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refreshes = %d, want 1", got)
	}
	for _, tr := range transports {
		if got := tr.Tokens(); got.RefreshToken != "rt-new" || got.AccessToken != freshAccess {
			t.Fatalf("transport did not adopt saved refresh: %+v", got)
		}
	}
}

func TestTransportForcedRefreshAdoptsNewerSavedTokens(t *testing.T) {
	var refreshes atomic.Int32
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		http.Error(w, "must not refresh", http.StatusInternalServerError)
	}))
	t.Cleanup(auth.Close)

	store := &Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	old := Tokens{AccessToken: "rejected", RefreshToken: "rt-old", ExpiresAt: time.Now().Add(time.Hour)}
	newer := Tokens{AccessToken: "rotated", RefreshToken: "rt-new", ExpiresAt: time.Now().Add(2 * time.Hour)}
	if err := store.Put("cg", newer); err != nil {
		t.Fatal(err)
	}
	tr := NewTransport(nil, old, store, "cg", "")
	tr.Endpoints = Endpoints{Issuer: auth.URL}
	got, err := tr.token(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != newer.AccessToken || got.RefreshToken != newer.RefreshToken ||
		!got.ExpiresAt.Equal(newer.ExpiresAt) || tr.Tokens().AccessToken != newer.AccessToken {
		t.Fatalf("tokens = %+v, want newer saved tokens %+v", got, newer)
	}
	if got := refreshes.Load(); got != 0 {
		t.Fatalf("unnecessary forced refreshes = %d", got)
	}
}

func TestTransportRefreshDoesNotRecreateRemovedAccount(t *testing.T) {
	store := &Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	old := Tokens{AccessToken: "old", RefreshToken: "rt-old", ExpiresAt: time.Now().Add(-time.Hour)}
	if err := store.Put("cg", old); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("cg"); err != nil {
		t.Fatal(err)
	}
	tr := NewTransport(nil, old, store, "cg", "")
	_, err := tr.token(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("removed account refresh error = %v", err)
	}
	if tr.Tokens() != old {
		t.Fatalf("in-memory tokens changed after failure: %+v", tr.Tokens())
	}
	if _, ok, getErr := store.Get("cg"); getErr != nil || ok {
		t.Fatalf("removed account recreated: ok=%v err=%v", ok, getErr)
	}
}

func TestTransportRefreshPersistenceFailureKeepsOldTokens(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Path: filepath.Join(dir, "auth.json")}
	old := Tokens{AccessToken: "old", RefreshToken: "rt-old", ExpiresAt: time.Now().Add(-time.Hour)}
	if err := store.Put("cg", old); err != nil {
		t.Fatal(err)
	}
	freshAccess := accessJWT(t, time.Now().Add(2*time.Hour))
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Store.Update has read the account before it calls Refresh. Replace
		// the target with a directory so atomic replacement fails without
		// deleting the open lock file (which Windows forbids).
		if err := os.Remove(store.Path); err != nil {
			t.Errorf("remove store target: %v", err)
		}
		if err := os.Mkdir(store.Path, 0o700); err != nil {
			t.Errorf("obstruct store target: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
			"access_token": freshAccess, "refresh_token": "rt-new",
		})
	}))
	t.Cleanup(auth.Close)

	tr := NewTransport(nil, old, store, "cg", "")
	tr.Endpoints = Endpoints{Issuer: auth.URL}
	if _, err := tr.token(context.Background(), false); err == nil {
		t.Fatal("refresh succeeded despite persistence failure")
	}
	if tr.Tokens() != old {
		t.Fatalf("in-memory tokens changed after persistence failure: %+v", tr.Tokens())
	}
	if _, ok, err := store.Get("cg"); err == nil || ok {
		t.Fatalf("persistence obstruction unexpectedly retained new tokens: ok=%v err=%v", ok, err)
	}
}

func TestRewriteResponses(t *testing.T) {
	// Content parts, existing include, existing instructions untouched.
	in := `{"instructions":"keep","include":["x"],"input":[{"role":"developer","content":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]},{"role":"user","content":"hi"}]}`
	var m map[string]any
	json.Unmarshal(rewriteResponses([]byte(in)), &m) //nolint:errcheck
	if m["instructions"] != "keep" {
		t.Fatalf("instructions overwritten: %v", m["instructions"])
	}
	if in, _ := m["input"].([]any); len(in) != 2 {
		t.Fatalf("input changed although instructions were set: %v", m["input"])
	}
	inc, _ := m["include"].([]any)
	if len(inc) != 2 || inc[0] != "x" || inc[1] != "reasoning.encrypted_content" {
		t.Fatalf("include %v", inc)
	}
	// Developer message with content parts is flattened.
	in = `{"input":[{"role":"developer","content":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]},{"role":"user","content":"hi"}]}`
	m = nil
	json.Unmarshal(rewriteResponses([]byte(in)), &m) //nolint:errcheck
	if m["instructions"] != "ab" {
		t.Fatalf("instructions %v", m["instructions"])
	}
	// Non-JSON passes through.
	if got := rewriteResponses([]byte("nope")); string(got) != "nope" {
		t.Fatalf("passthrough %q", got)
	}
	// Only the leading system message is lifted; a later one stays.
	in = `{"input":[{"role":"user","content":"hi"},{"role":"system","content":"late"}]}`
	m = nil
	json.Unmarshal(rewriteResponses([]byte(in)), &m) //nolint:errcheck
	if _, ok := m["instructions"]; ok {
		t.Fatalf("non-leading system message lifted: %v", m)
	}
}

func TestListModels(t *testing.T) {
	be := newBackend(t)
	be.catalog = `{"models":[
	  {"slug":"gpt-5.3-codex","display_name":"GPT-5.3 Codex","visibility":"list","context_window":400000,"default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]},
	  {"slug":"hidden","visibility":"hide"},
	  {"slug":"gpt-5.3","display_name":"GPT-5.3","visibility":"list"}]}`
	tok := Tokens{AccessToken: "at", AccountID: "acct_1"}
	ms, err := ListModels(context.Background(), be.srv.Client(), Endpoints{BaseURL: be.srv.URL}, tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].ID != "gpt-5.3-codex" || ms[1].ID != "gpt-5.3" {
		t.Fatalf("models %+v", ms)
	}
	if ms[0].ContextWindow != 400000 || ms[0].Reasoning != "medium" || strings.Join(ms[0].ReasoningLevels, ",") != "low,medium,high" {
		t.Fatalf("model detail %+v", ms[0])
	}
	r := be.reqs[0]
	if !strings.HasPrefix(r.path, "/models?client_version="+codexVersion) || r.auth != "Bearer at" || r.account != "acct_1" || r.originator != Originator {
		t.Fatalf("catalog request %+v", r)
	}

	be.reject.Store(1)
	_, err = ListModels(context.Background(), be.srv.Client(), Endpoints{BaseURL: be.srv.URL}, tok)
	var so *SignedOutError
	if !errors.As(err, &so) {
		t.Fatalf("401 catalog: %v", err)
	}
	be.catalog = `{"models":[]}`
	if _, err := ListModels(context.Background(), be.srv.Client(), Endpoints{BaseURL: be.srv.URL}, tok); err == nil {
		t.Fatal("empty catalog accepted")
	}
}

func TestClaimsFallbacks(t *testing.T) {
	// Email under the profile claim, plan missing.
	c := claims(jwt(t, map[string]any{
		"https://api.openai.com/profile": map[string]any{"email": "p@example.com"},
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "a"},
	}))
	if c.Email != "p@example.com" || c.AccountID != "a" || c.Plan != "" {
		t.Fatalf("%+v", c)
	}
	if c := claims("not-a-jwt"); c != (jwtClaims{}) {
		t.Fatalf("garbage jwt: %+v", c)
	}
	if (Tokens{}).Expired(time.Now()) != true {
		t.Fatal("zero expiry must count as expired")
	}
	if (Tokens{}).Summary() != "signed in" {
		t.Fatal("empty summary")
	}
}
