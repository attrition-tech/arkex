package chatgpt

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/attrition-tech/arkex/internal/testutil"
)

func TestSIWCRegistrationSignedIDToken(t *testing.T) {
	fake := testutil.NewSIWC(t)
	var authQuery atomic.Value
	fake.Mux.HandleFunc("/api/accounts/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("token grant was not form encoded")
		}
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "oaiapp_test" || r.Form.Get("resource") != BaseURL {
			t.Errorf("unexpected token form: %v", r.Form)
		}
		q := authQuery.Load().(url.Values)
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || r.Form.Get("redirect_uri") != q.Get("redirect_uri") {
			t.Error("PKCE or redirect mismatch")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "refresh_token": "refresh", "id_token": fake.IDToken("subject-1", "oaiapp_test", q.Get("nonce")), "scope": scope, "expires_in": 3600, "token_type": "Bearer"})
	})
	store := &Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	login, err := StartLogin(context.Background(), fake.Server.Client(), Endpoints{Issuer: fake.Server.URL, Store: store, Ports: []int{0}, OpenBrowser: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer login.Close()
	auth, _ := url.Parse(login.URL)
	q := auth.Query()
	authQuery.Store(q)
	if auth.Path != "/api/accounts/authorize" || q.Get("client_id") != ClientID || q.Get("agent_name_hint") != "arkex" || !strings.HasPrefix(q.Get("ext_agent_host_id"), "urn:uuid:") {
		t.Fatalf("bad authorize URL: %s", login.URL)
	}
	callback := q.Get("redirect_uri") + "?code=ok&state=" + url.QueryEscape(q.Get("state")) + "&client_id=oaiapp_test"
	resp, err := http.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	tok, err := login.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.Subject != "subject-1" || tok.ClientID != "oaiapp_test" || !tok.PlanEnabled() {
		t.Fatalf("bad tokens: %+v", tok)
	}
	if err := tok.ValidateAccess(); err != nil {
		t.Fatal(err)
	}

	// The host identity is durable across independent Store values.
	host2, err := (&Store{Path: store.Path}).hostID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if host2 != q.Get("ext_agent_host_id") {
		t.Fatalf("host ID changed: %q != %q", host2, q.Get("ext_agent_host_id"))
	}
}

func TestLoginBadStateDoesNotConsumeAndCloseUnblocks(t *testing.T) {
	store := &Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	l, err := StartLogin(context.Background(), http.DefaultClient, Endpoints{Issuer: "http://127.0.0.1:1", Store: store, Ports: []int{0}, OpenBrowser: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(l.URL)
	cb := u.Query().Get("redirect_uri")
	r, err := http.Get(cb + "?error=access_denied&state=wrong")
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state status %d", r.StatusCode)
	}
	done := make(chan error, 1)
	go func() { _, e := l.Wait(context.Background()); done <- e }()
	l.Close()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("Close returned no cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Wait")
	}
}

func TestValidateAccessPlanPermission(t *testing.T) {
	tok := Tokens{AccessToken: "a", ClientID: "issued", Issuer: Issuer, Subject: "s", Scopes: "openid profile"}
	if tok.ValidateAccess() == nil || !strings.Contains(tok.Summary(), "plan disabled") {
		t.Fatal("missing plan grant was accepted")
	}
	tok.Scopes += " chatgpt.tokens.use.direct"
	if err := tok.ValidateAccess(); err != nil {
		t.Fatal(err)
	}
}

func TestSIWCCallbackAndIDValidation(t *testing.T) {
	fake := testutil.NewSIWC(t)
	other := testutil.NewSIWC(t)
	var payload atomic.Value
	var calls atomic.Int32
	fake.Mux.HandleFunc("/api/accounts/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(payload.Load())
	})
	for _, kind := range []string{"valid", "no-plan", "signature", "issuer", "audience", "expiry", "nonce", "missing-subject", "different-subject", "missing-client", "bootstrap-client", "different-client", "denied"} {
		t.Run(kind, func(t *testing.T) {
			reg := Tokens{}
			if kind == "different-client" || kind == "different-subject" {
				reg = registered(fake.Server.URL)
			}
			l, err := StartLogin(t.Context(), fake.Server.Client(), Endpoints{Issuer: fake.Server.URL, Store: &Store{Path: filepath.Join(t.TempDir(), "auth.json")}, Registration: reg, Ports: []int{0}, OpenBrowser: func(string) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			u, _ := url.Parse(l.URL)
			q := u.Query()
			claims := map[string]any{"iss": fake.Server.URL, "sub": "subject-1", "aud": "oaiapp_test", "nonce": q.Get("nonce"), "exp": time.Now().Add(time.Hour).Unix()}
			switch kind {
			case "issuer":
				claims["iss"] = "https://untrusted.example"
			case "audience":
				claims["aud"] = "someone-else"
			case "expiry":
				claims["exp"] = time.Now().Add(-time.Minute).Unix()
			case "nonce":
				claims["nonce"] = "other-attempt"
			case "missing-subject":
				delete(claims, "sub")
			case "different-subject":
				claims["sub"] = "other-account"
			}
			id := fake.Sign(claims)
			if kind == "signature" {
				id = other.Sign(claims)
			}
			scopes := scope
			if kind == "no-plan" {
				scopes = "openid profile"
			}
			payload.Store(map[string]any{"access_token": "secret-access", "refresh_token": "secret-refresh", "token_type": "Bearer", "expires_in": 3600, "id_token": id, "scope": scopes})
			// A stale/error callback must not consume the good attempt.
			resp, err := http.Get(q.Get("redirect_uri") + "?state=wrong&error=access_denied")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			before := calls.Load()
			cb := url.Values{"state": {q.Get("state")}, "code": {"code"}, "client_id": {"oaiapp_test"}}
			switch kind {
			case "missing-client":
				cb.Del("client_id")
			case "bootstrap-client":
				cb.Set("client_id", "dynamic_agent_client")
			case "different-client":
				cb.Set("client_id", "oaiapp_other")
			case "denied":
				cb.Set("error", "access_denied")
			}
			resp, err = http.Get(q.Get("redirect_uri") + "?" + cb.Encode())
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			// A duplicate must never redeem the code twice.
			resp, err = http.Get(q.Get("redirect_uri") + "?" + cb.Encode())
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("duplicate status %d", resp.StatusCode)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			tok, err := l.Wait(ctx)
			valid := kind == "valid" || kind == "no-plan"
			if valid != (err == nil) {
				t.Fatalf("outcome %v", err)
			}
			if kind == "no-plan" && (tok.Subject == "" || tok.ValidateAccess() == nil) {
				t.Fatal("identity/permission distinction lost")
			}
			if err != nil && (strings.Contains(err.Error(), "secret-access") || strings.Contains(err.Error(), id)) {
				t.Fatal("credential in error")
			}
			want := int32(1)
			if kind == "missing-client" || kind == "bootstrap-client" || kind == "different-client" || kind == "denied" {
				want = 0
			}
			if calls.Load()-before != want {
				t.Fatal("unexpected code exchanges")
			}
		})
	}
}

func TestSIWCReauthorizationAndFallback(t *testing.T) {
	fake := testutil.NewSIWC(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	store := &Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	reg := registered(fake.Server.URL)
	reg.Email = "same@example.com"
	reg.IDToken = "must-not-appear-in-url"
	ep := Endpoints{Issuer: fake.Server.URL, Store: store, Registration: reg, Ports: []int{busy.Addr().(*net.TCPAddr).Port, 0}, OpenBrowser: func(string) error { return nil }}
	l, err := StartLogin(t.Context(), fake.Server.Client(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	u, _ := url.Parse(l.URL)
	q := u.Query()
	cb, _ := url.Parse(q.Get("redirect_uri"))
	if q.Get("client_id") != "oaiapp_test" || q.Get("agent_name_hint") != "" || q.Get("prompt") != "" || q.Get("login_hint") != reg.Email || strings.Contains(l.URL, reg.IDToken) {
		t.Fatal("incorrect reauthorization request")
	}
	if cb.Hostname() != "127.0.0.1" || cb.Host == busy.Addr().String() {
		t.Fatal("bad fallback callback")
	}
	fake.Mux.HandleFunc("/api/accounts/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "refresh_token": "refresh", "token_type": "Bearer", "expires_in": 3600, "scope": scope, "id_token": fake.IDToken(reg.Subject, reg.ClientID, q.Get("nonce"))})
	})
	resp, err := http.Get(cb.String() + "?code=ok&state=" + url.QueryEscape(q.Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if tok, err := l.Wait(t.Context()); err != nil || tok.ClientID != reg.ClientID {
		t.Fatalf("reauth: %v", err)
	}
	ep.Consent = true
	l2, err := StartLogin(t.Context(), fake.Server.Client(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	u2, _ := url.Parse(l2.URL)
	if u2.Query().Get("prompt") != "consent" || u2.Query().Get("ext_agent_host_id") != q.Get("ext_agent_host_id") {
		t.Fatal("explicit consent or stable host lost")
	}
}

func TestSIWCRefreshScopesAndFailures(t *testing.T) {
	for _, kind := range []string{"omitted", "empty", "reduced", "invalid_grant", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused", "invalid_refresh_token", "token_expired", "invalid_client", "transient"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "transient" {
					w.WriteHeader(503)
					return
				}
				if kind != "omitted" && kind != "empty" && kind != "reduced" {
					w.WriteHeader(400)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": kind})
					return
				}
				out := map[string]any{"access_token": "new", "refresh_token": "rotated", "token_type": "Bearer", "expires_in": 3600}
				if kind == "empty" {
					out["scope"] = ""
				}
				if kind == "reduced" {
					out["scope"] = "openid"
				}
				_ = json.NewEncoder(w).Encode(out)
			}))
			defer srv.Close()
			old := registered(srv.URL)
			s := &Store{Path: filepath.Join(t.TempDir(), "auth.json")}
			if err := s.Put("cg", old); err != nil {
				t.Fatal(err)
			}
			_, err := s.Update(t.Context(), "cg", func(tok Tokens) (Tokens, error) {
				return Refresh(t.Context(), srv.Client(), Endpoints{Issuer: srv.URL}, tok)
			})
			saved, _, _ := s.Get("cg")
			switch kind {
			case "omitted", "empty", "reduced":
				if err != nil || saved.RefreshToken != "rotated" || saved.Subject != old.Subject {
					t.Fatalf("refresh: %v", err)
				}
				if (kind == "omitted") != saved.PlanEnabled() {
					t.Fatal("omitted vs revoked scope conflated")
				}
			case "transient", "invalid_client":
				if err == nil || saved.RefreshToken != old.RefreshToken {
					t.Fatal("retryable/config error destroyed credentials")
				}
			default:
				var terminal *SignedOutError
				if !errors.As(err, &terminal) || saved.RefreshToken != "" || saved.ClientID != old.ClientID {
					t.Fatal("terminal error did not retain only registration")
				}
			}
		})
	}
}

func TestSIWCSignOut(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "revoked", true: "unavailable"}[fail], func(t *testing.T) {
			fake := testutil.NewSIWC(t)
			var calls atomic.Int32
			fake.Mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_ = r.ParseForm()
				if r.Form.Get("token") != "old-refresh" || r.Form.Get("client_id") != "oaiapp_test" || r.Form.Get("token_type_hint") != "refresh_token" {
					t.Error("bad revocation request")
				}
				if fail {
					w.WriteHeader(503)
				}
			})
			s := &Store{Path: filepath.Join(t.TempDir(), "auth.json")}
			old := registered(fake.Server.URL)
			if err := s.Put("cg", old); err != nil {
				t.Fatal(err)
			}
			err := s.SignOut(t.Context(), fake.Server.Client(), Endpoints{Issuer: fake.Server.URL}, "cg", false)
			if fail != (err != nil) {
				t.Fatalf("signout: %v", err)
			}
			got, ok, _ := s.Get("cg")
			if !ok || got.AccessToken != "" || got.RefreshToken != "" || got.ClientID != old.ClientID || got.Subject != old.Subject {
				t.Fatal("registration not preserved after signout")
			}
			want := int32(1)
			if fail {
				want = 2
			}
			if calls.Load() != want {
				t.Fatal("bounded revocation retry count")
			}
		})
	}
}

func TestSIWCForgetKeepsStoreLockedAndHostIdentity(t *testing.T) {
	fake := testutil.NewSIWC(t)
	s := &Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	host, err := s.hostID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("cg", registered(fake.Server.URL)); err != nil {
		t.Fatal(err)
	}
	fake.Mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
		defer cancel()
		unlock, err := (&Store{Path: s.Path}).lock(ctx)
		if err == nil {
			unlock()
			t.Error("concurrent store write allowed during revocation")
		}
	})
	if err := s.SignOut(t.Context(), fake.Server.Client(), Endpoints{Issuer: fake.Server.URL}, "cg", true); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Get("cg"); err != nil || ok {
		t.Fatal("forgotten registration still present")
	}
	if got, err := s.hostID(t.Context()); err != nil || got != host {
		t.Fatal("forget reset host identity")
	}
}
