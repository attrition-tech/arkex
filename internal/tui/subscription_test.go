package tui

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dantearo/arkex/internal/chatgpt"
	"github.com/dantearo/arkex/internal/config"
)

func TestLegacySubscriptionEditKeepsProvider(t *testing.T) {
	m, _, fake, _ := subscriptionModel(t)
	m.panel.cfg.Connections["legacy"] = config.Connection{Kind: config.KindSubscription, API: config.APIOpenAI, BaseURL: fake.srv.URL}
	m.openEdit("legacy")
	if !m.buildDraft() {
		t.Fatal("legacy draft rejected")
	}
	c := m.panel.draft
	if c.Subscription != "chatgpt" || c.BaseURL != fake.srv.URL || c.Kind != config.KindSubscription {
		t.Fatalf("legacy connection changed provider: %+v", c)
	}
}

func TestUnsupportedSubscriptionCannotBeEditedIntoAnotherProvider(t *testing.T) {
	m, _, _, _ := subscriptionModel(t)
	m.panel.cfg.Connections["old"] = config.Connection{Kind: config.KindSubscription, Subscription: "removed-provider"}
	m.openEdit("old")
	if m.panel.mode != pmList || !strings.Contains(m.panel.err, "unsupported subscription") {
		t.Fatal("unsupported subscription must not open a different provider's form")
	}
}

// fakeOpenAI is one server standing in for auth.openai.com (token
// exchange) and the Codex backend (model catalog).
type fakeOpenAI struct {
	srv       *httptest.Server
	exchanges atomic.Int32
	catalogs  atomic.Int32
	failList  atomic.Bool
}

func unsignedJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(b) + ".x"
}

func newFakeOpenAI(t *testing.T) *fakeOpenAI {
	f := &fakeOpenAI{}
	auth := map[string]any{"chatgpt_account_id": "acct_7", "chatgpt_plan_type": "pro"}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if form.Get("grant_type") != "authorization_code" || form.Get("code") != "good" || form.Get("code_verifier") == "" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant"}`)) //nolint:errcheck
			return
		}
		f.exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"access_token":  unsignedJWT(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": auth}),
			"refresh_token": "rt",
			"id_token":      unsignedJWT(t, map[string]any{"email": "dev@example.com", "https://api.openai.com/auth": auth}),
		})
	})
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		f.catalogs.Add(1)
		if r.Header.Get("Authorization") == "" || r.Header.Get("ChatGPT-Account-ID") != "acct_7" {
			w.WriteHeader(401)
			return
		}
		if f.failList.Load() {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[
		  {"slug":"gpt-5.3-codex","display_name":"GPT-5.3 Codex","visibility":"list","context_window":400000,"default_reasoning_level":"medium"},
		  {"slug":"gpt-5.3","display_name":"GPT-5.3","visibility":"list","context_window":272000},
		  {"slug":"secret","visibility":"hide"}]}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// subscriptionModel is panelModel plus a token store in the temp home and
// the sign-in flow pointed at fake servers with no browser.
func subscriptionModel(t *testing.T) (*model, *string, *fakeOpenAI, *string) {
	m, sel := panelModel(t)
	fake := newFakeOpenAI(t)
	opened := new(string)
	m.o.Auth = &chatgpt.Store{Path: m.o.ConfigPath[:len(m.o.ConfigPath)-len("config.json")] + "auth.json"}
	m.o.Login = chatgpt.Endpoints{
		Issuer:      fake.srv.URL,
		BaseURL:     fake.srv.URL,
		OpenBrowser: func(u string) error { *opened = u; return nil },
		Ports:       []int{0},
	}
	m.openModels("")
	return m, sel, fake, opened
}

// finishLogin plays the browser: it hits the callback the sign-in is
// waiting on with the right state, then runs the blocked Wait command and
// feeds its result back into the model, returning the follow-up command.
func finishLogin(t *testing.T, m *model, waitCmd tea.Cmd, code string) tea.Cmd {
	t.Helper()
	p := m.panel
	if p.mode != pmLogin || p.login == nil {
		t.Fatalf("not on the sign-in screen: mode=%v", p.mode)
	}
	u, err := url.Parse(p.login.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	// The listener is on 127.0.0.1; the URL says localhost for the browser.
	cb := "http://127.0.0.1:" + redirect.Port() + redirect.Path + "?code=" + code + "&state=" + url.QueryEscape(q.Get("state"))
	done := make(chan []tea.Msg, 1)
	go func() { done <- runCmd(waitCmd) }()
	resp, err := http.Get(cb)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	var next tea.Cmd
	select {
	case msgs := <-done:
		found := false
		for _, msg := range msgs {
			if ld, ok := msg.(loginDoneMsg); ok {
				found = true
				_, next = m.Update(ld)
			}
		}
		if !found {
			t.Fatalf("no loginDoneMsg among %v", msgs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sign-in did not finish")
	}
	return next
}

func TestSubscriptionAddSignInPickSave(t *testing.T) {
	m, sel, fake, opened := subscriptionModel(t)
	p := m.panel

	typeKeys(m, "a", "right", "right") // kind → Subscription
	f := p.form
	if p.formKind() != config.KindSubscription || f.value("name") != "chatgpt" || p.preset.ID != "chatgpt" {
		t.Fatalf("subscription prefill: kind=%v name=%q preset=%s", p.formKind(), f.value("name"), p.preset.ID)
	}
	if !strings.Contains(panelText(m), "Sign in with ChatGPT") || !strings.Contains(panelText(m), "auth.json") {
		t.Fatalf("form:\n%s", panelText(m))
	}
	// tab: service → name → enabled → sign-in button.
	typeKeys(m, "tab", "tab", "tab", "tab")
	if f.focused().id != "login" {
		t.Fatalf("focus=%s", f.focused().id)
	}
	_, cmd := m.Update(key("enter"))
	if p.mode != pmLogin || p.login == nil || *opened != p.login.URL {
		t.Fatalf("sign-in not started: mode=%v opened=%q", p.mode, *opened)
	}
	if !strings.HasPrefix(p.login.URL, fake.srv.URL+"/oauth/authorize?") {
		t.Fatalf("authorize URL %q", p.login.URL)
	}
	txt := panelText(m)
	if !strings.Contains(txt, "Sign in with ChatGPT") || !strings.Contains(txt, "/oauth/authorize?") || !strings.Contains(txt, "  Copy link  ") || strings.Contains(txt, "[ Copy link ]") {
		t.Fatalf("login screen:\n%s", txt)
	}
	// c copies the link (a clipboard command comes back) and says so.
	_, copyCmd := m.Update(key("c"))
	if copyCmd == nil || p.note != "link copied" {
		t.Fatalf("copy: cmd=%v note=%q", copyCmd != nil, p.note)
	}

	next := finishLogin(t, m, cmd, "good")
	if fake.exchanges.Load() != 1 {
		t.Fatalf("code exchanges = %d", fake.exchanges.Load())
	}
	if p.mode != pmBusy || p.draftTok == nil || p.draftTok.Email != "dev@example.com" {
		t.Fatalf("after sign-in: mode=%v tok=%+v", p.mode, p.draftTok)
	}
	// Nothing is saved until the models are picked.
	if all, _ := m.o.Auth.All(); len(all) != 0 {
		t.Fatalf("tokens saved early: %v", all)
	}
	for _, msg := range runCmd(next) {
		if fm, ok := msg.(modelsFetchedMsg); ok {
			m.Update(fm)
		}
	}
	if p.mode != pmPick || strings.Join(p.found, ",") != "gpt-5.3-codex,gpt-5.3" {
		t.Fatalf("picker: mode=%v found=%v", p.mode, p.found)
	}
	// Tick the first model only, then Confirm.
	typeKeys(m, "space", "down", "down")
	_, connectCmd := m.Update(key("enter"))
	if p.mode != pmBusy || !strings.Contains(p.busy, "connecting to chatgpt/gpt-5.3-codex") {
		t.Fatalf("after confirm: mode=%v busy=%q", p.mode, p.busy)
	}
	var connected *connectedMsg
	for _, msg := range runCmd(connectCmd) {
		if c, ok := msg.(connectedMsg); ok {
			connected = &c
		}
	}
	if connected == nil || *sel != "chatgpt/gpt-5.3-codex" {
		t.Fatalf("connect cmd did not dial: sel=%q", *sel)
	}
	cfg := mustLoad(t, m)
	c := cfg.Connections["chatgpt"]
	if c.Kind != config.KindSubscription || c.API != config.APIOpenAI || c.BaseURL != chatgpt.BaseURL || c.APIKey != "" {
		t.Fatalf("saved connection %+v", c)
	}
	if len(c.Models) != 1 || c.Models[0].ID != "gpt-5.3-codex" || c.Models[0].ContextWindow != 400000 || c.Models[0].Name != "GPT-5.3 Codex" || !c.Models[0].Reasoning {
		t.Fatalf("saved models %+v", c.Models)
	}
	if cfg.Default != "chatgpt/gpt-5.3-codex" {
		t.Fatalf("default %q", cfg.Default)
	}
	tok, ok, err := m.o.Auth.Get("chatgpt")
	if err != nil || !ok || tok.RefreshToken != "rt" || tok.AccountID != "acct_7" || tok.Plan != "pro" {
		t.Fatalf("stored tokens: %+v %v %v", tok, ok, err)
	}
	// Config never carries the tokens.
	rawB, _ := os.ReadFile(m.o.ConfigPath)
	if raw := string(rawB); strings.Contains(raw, "refresh") || strings.Contains(raw, "acct_7") {
		t.Fatalf("config.json leaks sign-in data:\n%s", raw)
	}
	// The list names the account.
	m.Update(*connected)
	if !strings.Contains(panelText(m), "dev@example.com · pro") {
		t.Fatalf("list row:\n%s", panelText(m))
	}
}

func TestSubscriptionLoginCancelAndCatalogFallback(t *testing.T) {
	m, _, fake, _ := subscriptionModel(t)
	p := m.panel
	typeKeys(m, "a", "right", "right", "tab", "tab", "tab", "tab")
	_, cmd := m.Update(key("enter"))
	if p.mode != pmLogin {
		t.Fatalf("mode=%v", p.mode)
	}
	// esc cancels: the waiting command returns cancelled and the form is back.
	typeKeys(m, "esc")
	for _, msg := range runCmd(cmd) {
		if ld, ok := msg.(loginDoneMsg); ok {
			m.Update(ld)
		}
	}
	if p.mode != pmAdd || p.form == nil || p.note != "sign-in cancelled" || p.login != nil {
		t.Fatalf("after cancel: mode=%v note=%q login=%v", p.mode, p.note, p.login)
	}
	// A wrong code is reported, not saved.
	_, cmd = m.Update(key("enter"))
	finishLogin(t, m, cmd, "bad")
	if p.mode != pmAdd || p.err == "" || p.draftTok != nil {
		t.Fatalf("bad code: mode=%v err=%q", p.mode, p.err)
	}
	// Catalog failure falls back to typing model ids; tokens still save.
	fake.failList.Store(true)
	_, cmd = m.Update(key("enter"))
	next := finishLogin(t, m, cmd, "good")
	for _, msg := range runCmd(next) {
		if fm, ok := msg.(modelsFetchedMsg); ok {
			m.Update(fm)
		}
	}
	if p.mode != pmAdd || p.form.get("models").hidden || p.form.focused().id != "models" {
		t.Fatalf("fallback: mode=%v focus=%v", p.mode, p.form.focused())
	}
	typeText(m, "gpt-5.3")
	_ = p.form.focusID("save")
	typeKeys(m, "enter")
	cfg := mustLoad(t, m)
	if c := cfg.Connections["chatgpt"]; len(c.Models) != 1 || c.Models[0].ID != "gpt-5.3" || !c.Models[0].Reasoning {
		t.Fatalf("manual models %+v", c.Models)
	}
	if _, ok, _ := m.o.Auth.Get("chatgpt"); !ok {
		t.Fatal("tokens not saved on manual save")
	}
}

func TestSubscriptionEditSignOutRenameDelete(t *testing.T) {
	m, _, fake, _ := subscriptionModel(t)
	p := m.panel
	conn := config.Connection{Kind: config.KindSubscription, API: config.APIOpenAI, BaseURL: chatgpt.BaseURL,
		Models: []config.Model{{ID: "gpt-5.3-codex", Reasoning: true}}}
	if err := config.SaveConnection(m.o.ConfigPath, "cg", conn, "cg/gpt-5.3-codex"); err != nil {
		t.Fatal(err)
	}
	if err := m.o.Auth.Put("cg", chatgpt.Tokens{AccessToken: "a", RefreshToken: "r", AccountID: "acct_7", Email: "old@example.com", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	typeKeys(m, "r")
	if !strings.Contains(panelText(m), "old@example.com") {
		t.Fatalf("list:\n%s", panelText(m))
	}
	typeKeys(m, "enter")
	f := p.form
	if p.mode != pmEdit || f.get("url") != nil || f.get("key") != nil || f.get("login") == nil || f.get("signout") == nil {
		t.Fatalf("edit form fields: %v", p.mode)
	}
	if !strings.Contains(panelText(m), "signed in as old@example.com") {
		t.Fatalf("edit form:\n%s", panelText(m))
	}
	// Sign out forgets the tokens but keeps the connection.
	_ = f.focusID("signout")
	typeKeys(m, "enter")
	if _, ok, _ := m.o.Auth.Get("cg"); ok {
		t.Fatal("sign out kept the tokens")
	}
	if !strings.Contains(panelText(m), "not signed in") {
		t.Fatalf("after sign out:\n%s", panelText(m))
	}
	// Refetch without a sign-in is refused.
	_ = f.focusID("refetch")
	typeKeys(m, "enter")
	if p.mode != pmEdit || !strings.Contains(p.err, "not signed in") {
		t.Fatalf("refetch signed out: mode=%v err=%q", p.mode, p.err)
	}
	// Sign in again saves under the edited id straight away.
	_ = f.focusID("login")
	_, cmd := m.Update(key("enter"))
	finishLogin(t, m, cmd, "good")
	tok, ok, _ := m.o.Auth.Get("cg")
	if !ok || tok.Email != "dev@example.com" || p.mode != pmEdit || !strings.Contains(panelText(m), "signed in as dev@example.com") {
		t.Fatalf("sign in again: ok=%v tok=%+v mode=%v", ok, tok, p.mode)
	}
	// Refetch now uses the saved sign-in.
	_ = f.focusID("refetch")
	typeKeys(m, "enter")
	for _, msg := range runCmd(m.fetchCatalog(tok)) {
		if fm, ok := msg.(modelsFetchedMsg); ok {
			m.Update(fm)
		}
	}
	if p.mode != pmPick || fake.catalogs.Load() == 0 || !p.picked["gpt-5.3-codex"] {
		t.Fatalf("refetch: mode=%v catalogs=%d picked=%v", p.mode, fake.catalogs.Load(), p.picked)
	}
	typeKeys(m, "esc")
	// Rename moves the sign-in with the connection.
	f.get("name").input.SetValue("work")
	_ = f.focusID("save")
	typeKeys(m, "enter")
	if _, ok, _ := m.o.Auth.Get("cg"); ok {
		t.Fatal("old id still has tokens after rename")
	}
	if _, ok, _ := m.o.Auth.Get("work"); !ok {
		t.Fatal("renamed id has no tokens")
	}
	if _, ok := mustLoad(t, m).Connections["work"]; !ok {
		t.Fatal("rename not saved")
	}
	// Deleting the connection deletes the sign-in.
	typeKeys(m, "d", "y")
	if _, ok := mustLoad(t, m).Connections["work"]; ok {
		t.Fatal("connection not deleted")
	}
	if all, _ := m.o.Auth.All(); len(all) != 0 {
		t.Fatalf("tokens survive delete: %v", all)
	}
}
