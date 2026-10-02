package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func registered(issuer string) Tokens {
	return Tokens{AccessToken: "old-access", RefreshToken: "old-refresh", ClientID: "oaiapp_test", Subject: "subject-1", Issuer: issuer, HostID: "urn:uuid:host", Scopes: "openid resource.invoke chatgpt.tokens.use.direct", ExpiresAt: time.Now().Add(time.Hour).UTC()}
}

func TestStore(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "private", "auth.json")}
	if _, ok, err := s.Get("a"); err != nil || ok {
		t.Fatalf("empty store: %v %v", ok, err)
	}
	tok := registered(Issuer)
	if err := s.Put("a", tok); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{s.Path: 0o600, filepath.Dir(s.Path): 0o700} {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != mode {
			t.Fatalf("permissions: %v %v", st, err)
		}
	}
	if err := s.Rename("a", "b"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("a"); ok {
		t.Fatal("old registration remains")
	}
	got, ok, err := s.Get("b")
	if err != nil || !ok || got.ClientID != tok.ClientID || got.RefreshToken != tok.RefreshToken {
		t.Fatal("rename lost credentials")
	}
	if err := s.Remove("b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("b"); err != nil {
		t.Fatal(err)
	}
	if all, err := s.All(); err != nil || len(all) != 0 {
		t.Fatalf("after removal: %v", err)
	}
	if err := os.WriteFile(s.Path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("c", tok); err == nil {
		t.Fatal("corrupt store overwritten")
	}
}

func TestTransportRefreshLifecycle(t *testing.T) {
	var refreshes, requests atomic.Int32
	var reject, expired atomic.Bool
	var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/oauth/token":
			refreshes.Add(1)
			_ = r.ParseForm()
			if r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Form.Get("client_id") != "oaiapp_test" || r.Form.Get("resource") != BaseURL || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("scope") != "" {
				t.Error("wrong refresh request")
			}
			if expired.Load() {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"refresh_token_reused"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-access", "refresh_token": "fresh-refresh", "expires_in": 3600, "token_type": "Bearer"})
		case "/responses":
			requests.Add(1)
			if r.Header.Get("originator") != "" || r.Header.Get("ChatGPT-Account-ID") != "" || r.Header.Get("User-Agent") != "arkex/test" {
				t.Error("legacy client headers")
			}
			if reject.Swap(false) {
				w.WriteHeader(401)
				return
			}
			if requests.Load() > 1 && r.Header.Get("Authorization") != "Bearer fresh-access" {
				t.Error("old bearer reused")
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Error("unexpected route")
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	tok := registered(srv.URL)
	s := &Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	if err := s.Put("cg", tok); err != nil {
		t.Fatal(err)
	}
	tr := NewTransport(nil, tok, s, "cg", "arkex/test")
	tr.Endpoints = Endpoints{Issuer: srv.URL, BaseURL: srv.URL}
	client := &http.Client{Transport: tr}
	do := func() error {
		r, err := client.Post(srv.URL+"/responses", "application/json", strings.NewReader(`{"input":[]}`))
		if r != nil {
			_ = r.Body.Close()
		}
		return err
	}
	if err := do(); err != nil || refreshes.Load() != 0 {
		t.Fatalf("fresh request: %v", err)
	}
	reject.Store(true)
	if err := do(); err != nil || refreshes.Load() != 1 || requests.Load() != 3 {
		t.Fatalf("401 refresh: %v", err)
	}
	saved, _, _ := s.Get("cg")
	if saved.RefreshToken != "fresh-refresh" {
		t.Fatal("rotation not saved")
	}
	saved.ExpiresAt = time.Now()
	if err := s.Put("cg", saved); err != nil {
		t.Fatal(err)
	}
	if err := do(); err != nil || refreshes.Load() != 2 {
		t.Fatalf("proactive refresh: %v", err)
	}
	expired.Store(true)
	saved.ExpiresAt = time.Now()
	if err := s.Put("cg", saved); err != nil {
		t.Fatal(err)
	}
	var signedOut *SignedOutError
	if err := do(); !errors.As(err, &signedOut) {
		t.Fatalf("terminal refresh: %v", err)
	}
	saved, _, _ = s.Get("cg")
	if saved.AccessToken != "" || saved.RefreshToken != "" || saved.ClientID != tok.ClientID {
		t.Fatal("terminal refresh did not retain only registration")
	}
}

func TestTransportConcurrentRefreshUsesStoreUpdate(t *testing.T) {
	var refreshes atomic.Int32
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-access", "refresh_token": "fresh-refresh", "expires_in": 7200, "token_type": "Bearer"})
	}))
	defer auth.Close()
	path := filepath.Join(t.TempDir(), "auth.json")
	old := registered(auth.URL)
	old.ExpiresAt = time.Now().Add(-time.Hour)
	if err := (&Store{Path: path}).Put("cg", old); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tr := NewTransport(nil, old, &Store{Path: path}, "cg", "arkex/test")
			tr.Endpoints = Endpoints{Issuer: auth.URL}
			got, err := tr.token(t.Context(), false)
			if err != nil || got.RefreshToken != "fresh-refresh" {
				t.Errorf("refresh: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes.Load())
	}
}

func TestTransportSavedCredentialChanges(t *testing.T) {
	for _, kind := range []string{"rotated", "removed", "signedout", "different-client", "different-subject", "different-issuer", "permission"} {
		t.Run(kind, func(t *testing.T) {
			old := registered(Issuer)
			saved := old
			s := &Store{Path: filepath.Join(t.TempDir(), "auth.json")}
			switch kind {
			case "rotated":
				saved.AccessToken = "new"
				saved.RefreshToken = "new-refresh"
			case "signedout":
				saved.AccessToken = ""
			case "different-client":
				saved.ClientID = "another"
			case "different-subject":
				saved.Subject = "another"
			case "different-issuer":
				saved.Issuer = "https://another.example"
			case "permission":
				saved.Scopes = "openid"
			}
			if kind != "removed" {
				if err := s.Put("cg", saved); err != nil {
					t.Fatal(err)
				}
			}
			tr := NewTransport(nil, old, s, "cg", "")
			got, err := tr.token(t.Context(), true)
			if kind == "rotated" {
				if err != nil || got.AccessToken != "new" {
					t.Fatalf("new credentials not adopted: %v", err)
				}
			} else if err == nil {
				t.Fatal("stale credentials usable")
			}
		})
	}
}

func TestTransportRefreshPersistenceFailureKeepsOldTokens(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.Remove(s.Path); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(s.Path, 0o700); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new", "refresh_token": "new-refresh", "expires_in": 3600, "token_type": "Bearer"})
	}))
	defer auth.Close()
	old := registered(auth.URL)
	old.ExpiresAt = time.Now().UTC()
	if err := s.Put("cg", old); err != nil {
		t.Fatal(err)
	}
	tr := NewTransport(nil, old, s, "cg", "")
	tr.Endpoints = Endpoints{Issuer: auth.URL}
	if _, err := tr.token(t.Context(), false); err == nil {
		t.Fatal("persistence failure ignored")
	}
	if tr.Tokens() != old {
		t.Fatal("unpersisted token exposed")
	}
}

func TestTransportBoundaries(t *testing.T) {
	var calls atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/responses", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	tr := NewTransport(nil, registered(Issuer), nil, "", "arkex/test")
	tr.Endpoints.BaseURL = srv.URL
	client := &http.Client{Transport: tr}
	for _, target := range []string{other.URL + "/responses", srv.URL + "/responses", srv.URL + "/other"} {
		resp, err := client.Post(target, "application/json", strings.NewReader(`{}`))
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatal("unexpected destination accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("credentials crossed origins")
	}
	tr.tokens = Tokens{AccessToken: "legacy", RefreshToken: "legacy"}
	if _, err := tr.token(context.Background(), false); err == nil {
		t.Fatal("legacy credentials accepted")
	}
}

func TestRewriteResponses(t *testing.T) {
	in := `{"instructions":"keep","include":["x"],"temperature":1,"previous_response_id":"old","max_output_tokens":10,"input":[{"role":"user","content":"hi"},{"role":"system","content":"later"},{"type":"function_call","call_id":"c","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"contents"}],"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}`
	var m map[string]any
	if err := json.Unmarshal(rewriteResponses([]byte(in)), &m); err != nil {
		t.Fatal(err)
	}
	if m["instructions"] != "keep" || m["store"] != false || m["stream"] != true {
		t.Fatal("request options changed")
	}
	for _, field := range []string{"temperature", "previous_response_id", "max_output_tokens"} {
		if _, ok := m[field]; ok {
			t.Errorf("retained %s", field)
		}
	}
	items := m["input"].([]any)
	if items[1].(map[string]any)["role"] != "developer" || items[2].(map[string]any)["namespace"] != "arkex" || items[3].(map[string]any)["namespace"] != nil {
		t.Fatal("history rewrite corrupted roles or calls")
	}
	ns := m["tools"].([]any)[0].(map[string]any)
	if ns["type"] != "namespace" || ns["name"] != "arkex" || ns["tools"].([]any)[0].(map[string]any)["name"] != "read" {
		t.Fatal("tool namespace wrong")
	}
	inc := m["include"].([]any)
	if len(inc) != 2 || inc[0] != "x" || inc[1] != "reasoning.encrypted_content" {
		t.Fatal("include lost")
	}
	m = nil
	_ = json.Unmarshal(rewriteResponses([]byte(`{"input":[{"role":"developer","content":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]}]}`)), &m)
	if m["instructions"] != "ab" {
		t.Fatal("instructions not lifted")
	}
	if string(rewriteResponses([]byte("bad"))) != "bad" {
		t.Fatal("non-JSON changed")
	}
}

func TestListModels(t *testing.T) {
	var status atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/models" || r.Header.Get("Authorization") != "Bearer old-access" || r.Header.Get("originator") != "" || r.Header.Get("ChatGPT-Account-ID") != "" {
			t.Error("wrong model request")
		}
		if status.Load() != 0 {
			w.WriteHeader(int(status.Load()))
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"second","display_name":"Second","visibility":"list","context_window":123456,"default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]},{"slug":"hidden","visibility":"hide"},{"slug":"absent"},{"slug":"first","visibility":"list"}]}`))
	}))
	defer srv.Close()
	ms, err := ListModels(t.Context(), srv.Client(), Endpoints{BaseURL: srv.URL}, registered(Issuer))
	if err != nil || len(ms) != 2 || ms[0].ID != "second" || ms[1].ID != "first" || ms[0].ContextWindow != 123456 || strings.Join(ms[0].ReasoningLevels, ",") != "low,high" {
		t.Fatalf("catalog: %v %v", ms, err)
	}
	for _, code := range []int{401, 403, 503} {
		status.Store(int32(code))
		_, err := ListModels(t.Context(), srv.Client(), Endpoints{BaseURL: srv.URL}, registered(Issuer))
		var signedOut *SignedOutError
		if err == nil || errors.As(err, &signedOut) {
			t.Fatalf("HTTP %d is not confirmed revocation: %v", code, err)
		}
	}
}
