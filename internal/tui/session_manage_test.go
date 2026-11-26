package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"

	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/chatgpt"
	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/provider"
	"github.com/attrition-tech/arkex/internal/session"
)

func savedManagedSession(t *testing.T, m *model) *session.Session {
	t.Helper()
	s := session.New(m.o.Cwd)
	s.Update(sampleMessages(), session.Usage{Model: m.connection.Name, Input: 4200, Output: 700})
	s.LastInput = 730
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSessionManagementLifecycle(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	m := footerModel(t)
	s := savedManagedSession(t, m)
	m.resume(s.ID)
	m.command("/rename Deployment checks")
	got, err := session.Load(m.o.Cwd, s.ID)
	if err != nil || got.Title != "Deployment checks" || m.session.Title != got.Title || got.UsageIn != 4200 || got.LastInput != 730 {
		t.Fatalf("rename lost state: %+v, %v", got, err)
	}
	// Subsequent autosaves must preserve the manually chosen title.
	m.saveSession()
	got, _ = session.Load(m.o.Cwd, s.ID)
	if got.Title != "Deployment checks" {
		t.Fatal("autosave replaced manual title")
	}
	m.running = true
	m.setSessionState(s.ID, session.Archived)
	got, _ = session.Load(m.o.Cwd, s.ID)
	if got.State != "" || m.session == nil {
		t.Fatal("management must not change a running session")
	}
	m.running = false
	m.setSessionState(s.ID, session.Archived)
	if m.session != nil || len(m.connection.Agent.Messages()) != 0 {
		t.Fatal("archived current session still attached")
	}
	if list, _ := session.List(m.o.Cwd); len(list) != 0 {
		t.Fatal("archive still appears in active sessions")
	}
	if latest, err := session.Latest(m.o.Cwd); err != nil || latest != nil {
		t.Fatalf("latest resumed hidden session: %+v %v", latest, err)
	}
	if list, _ := session.ListState(m.o.Cwd, session.Archived); len(list) != 1 || list[0].ID != s.ID {
		t.Fatalf("archive missing: %+v", list)
	}
	m.resume(s.ID)
	if m.session != nil {
		t.Fatal("direct resume must require restore")
	}
	m.setSessionState(s.ID, "")
	m.resume(s.ID)
	m.confirmSessionDelete(s.ID, false)
	m.paletteKey(key("enter")) // default Cancel
	got, _ = session.Load(m.o.Cwd, s.ID)
	if got.State != "" {
		t.Fatal("default confirmation deleted session")
	}
	m.confirmSessionDelete(s.ID, false)
	typeKeys(m, "down", "enter")
	if m.session != nil {
		t.Fatal("trashed session still attached")
	}
	got, _ = session.Load(m.o.Cwd, s.ID)
	if got.State != session.Trash || !reflect.DeepEqual(got.Messages, s.Messages) {
		t.Fatal("trash lost messages")
	}
	m.setSessionState(s.ID, "")
	got, _ = session.Load(m.o.Cwd, s.ID)
	if got.State != "" || got.Title != "Deployment checks" {
		t.Fatal("restore lost title")
	}
	m.setSessionState(s.ID, session.Trash)
	m.confirmSessionDelete(s.ID, true)
	typeKeys(m, "enter") // Cancel again, including permanent deletion
	if _, err := session.Load(m.o.Cwd, s.ID); err != nil {
		t.Fatal("default permanently deleted session")
	}
	m.confirmSessionDelete(s.ID, true)
	typeKeys(m, "down", "enter")
	if _, err := session.Load(m.o.Cwd, s.ID); err == nil {
		t.Fatal("confirmed permanent delete left file")
	}
}

func TestSessionPickerActionsAndRename(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	m := footerModel(t)
	a := savedManagedSession(t, m)
	b := savedManagedSession(t, m) // same title; search must not deduplicate it
	m.command("/sessions")
	m.pal.input.SetValue("fix the build")
	m.pal.filter()
	if len(m.pal.view) != 2 {
		t.Fatal("same-title sessions disappeared from search")
	}
	id := m.pal.view[0].sessionID
	m.paletteBox(m.width, m.vp.Height())
	p := m.pal
	row := 0
	for i, item := range p.box.rows {
		if item == 0 {
			row = i
			break
		}
	}
	x, y := p.box.x+p.box.w-5, p.box.y+1+paletteHeaderRows+row
	m.paletteHover(x, y)
	m.paletteClick(x, y)
	if !strings.HasPrefix(m.pal.level().title, "Actions:") {
		t.Fatal("Actions click did not open menu")
	}
	typeKeys(m, "enter") // Rename
	if !m.pal.editing || m.pal.input.Value() != a.Title {
		t.Fatal("rename editor missing current title")
	}
	m.Update(tea.PasteMsg{Content: " appended"})
	if len(m.pal.view) != 2 {
		t.Fatal("title typing filtered Save/Cancel")
	}
	m.pal.input.SetValue("Renamed from picker")
	typeKeys(m, "enter")
	got, _ := session.Load(m.o.Cwd, id)
	if got.Title != "Renamed from picker" {
		t.Fatalf("wrong title: %q", got.Title)
	}
	otherID := a.ID
	if id == a.ID {
		otherID = b.ID
	}
	other, _ := session.Load(m.o.Cwd, otherID)
	if other.Title != a.Title {
		t.Fatal("rename touched other session")
	}
	m.command("/sessions")
	typeKeys(m, "ctrl+o")
	if !strings.HasPrefix(m.pal.level().title, "Actions:") {
		t.Fatal("keyboard Actions failed")
	}
	for _, width := range []int{120, 60, 40} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		frameLines(t, m, "session actions")
	}
}

func TestAITitlePreviewAndCancellation(t *testing.T) {
	lm := &fakeLM{titles: []string{streamText("Fix deployment checks", 50)}}
	h := fakeAgentModel(t, lm, 10000)
	s := savedManagedSession(t, h.model)
	h.resume(s.ID)
	before := h.connection.Agent.Messages()
	h.confirmTitleGeneration(s.ID)
	if len(lm.titleRequests) != 0 {
		t.Fatal("opening confirmation called model")
	}
	h.drive(h.generateSessionTitle(s.ID))
	if h.pal == nil || !h.pal.editing || h.pal.input.Value() != "Fix deployment checks" {
		t.Fatal("missing title preview")
	}
	got, _ := session.Load(h.o.Cwd, s.ID)
	if got.Title != s.Title || !reflect.DeepEqual(before, h.connection.Agent.Messages()) || h.lastInput != 730 {
		t.Fatal("suggestion changed conversation or title before acceptance")
	}
	if len(lm.titleRequests) != 1 || len(lm.titleRequests[0]["messages"].([]any)) != 2 {
		t.Fatalf("expected one isolated request: %+v", lm.titleRequests)
	}
	if tools, ok := lm.titleRequests[0]["tools"].([]any); ok && len(tools) > 0 {
		t.Fatal("title request has tools")
	}
	if cap := lm.titleRequests[0]["max_tokens"]; cap != float64(1024) {
		t.Fatalf("output cap = %v", cap)
	}
	if got.TitleUsageIn != 50 || got.TitleUsageOut != 5 || got.UsageIn != 4200 || got.LastInput != 730 {
		t.Fatalf("naming usage must be recorded without changing conversation accounting: %+v", got)
	}
	typeKeys(h.model, "enter")
	got, _ = session.Load(h.o.Cwd, s.ID)
	if got.Title != "Fix deployment checks" {
		t.Fatal("accepted suggestion not saved")
	}
	// Closing the UI cancels and ignores an already queued result.
	h.openPalette()
	p := h.pal
	ctx, cancel := context.WithCancel(context.Background())
	p.busy, p.cancel = true, cancel
	typeKeys(h.model, "esc")
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("request was not cancelled")
	}
	h.Update(sessionTitleMsg{palette: p, id: s.ID, title: "Late result"})
	if h.pal != nil {
		t.Fatal("late result reopened editor")
	}
	got, _ = session.Load(h.o.Cwd, s.ID)
	if got.Title != "Fix deployment checks" {
		t.Fatal("cancelled result overwrote title")
	}
	// Also exercise the actual generation command with a cancelled context:
	// no provider request or preview may escape after dismissal.
	cmd := h.generateSessionTitle(s.ID)
	typeKeys(h.model, "esc")
	h.drive(cmd)
	if h.pal != nil || len(lm.titleRequests) != 1 {
		t.Fatal("dismissed generation still called the provider or opened a preview")
	}
}

func TestTitleExcerptAndValidation(t *testing.T) {
	msgs := []fantasy.Message{fantasy.NewUserMessage("Original request\n\n<file path=\"private\">SECRET FILE</file>")}
	for range 20 {
		msgs = append(msgs, fantasy.NewUserMessage(strings.Repeat("界", 1000)))
	}
	msgs = append(msgs, fantasy.NewUserMessage("Latest request"), fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ReasoningPart{Text: "SECRET REASONING"}}})
	excerpt := titleExcerpt(&session.Session{Messages: msgs})
	if len([]rune(excerpt)) > 6000 || !strings.Contains(excerpt, "Original request") || !strings.Contains(excerpt, "Latest request") || strings.Contains(excerpt, "SECRET") {
		t.Fatal("excerpt exceeded budget, lost endpoints, or included private parts")
	}
	for _, input := range []string{" \n", strings.Repeat("界", 61)} {
		if _, err := cleanSessionTitle(input); err == nil {
			t.Fatalf("accepted invalid title %q", input)
		}
	}
	if got, err := cleanSessionTitle("  Test\n title  "); err != nil || got != "Test title" {
		t.Fatalf("normalized title %q, %v", got, err)
	}
	// Failure must leave the persisted title and history intact.
	h := fakeAgentModel(t, &fakeLM{titles: []string{streamText("", 50)}}, 10000)
	s := savedManagedSession(t, h.model)
	h.drive(h.generateSessionTitle(s.ID))
	got, _ := session.Load(h.o.Cwd, s.ID)
	if got.Title != s.Title || h.pal != nil {
		t.Fatal("empty AI result changed title")
	}
	// Compaction must not replace the original request with its summary or
	// consume the recent-text budget with synthetic acknowledgments.
	s = session.New(h.o.Cwd)
	s.Update(msgs, session.Usage{})
	if _, err := s.SaveContext(t.Context(), agent.CompactedMessages(strings.Repeat("Summary detail. ", 100))); err != nil {
		t.Fatal(err)
	}
	excerpt = titleExcerpt(s)
	if !strings.Contains(excerpt, "Original request") || !strings.Contains(excerpt, "Latest request") || strings.Contains(excerpt, "Summary detail") || strings.Contains(excerpt, agent.CompactAck) || strings.Contains(excerpt, "SECRET") {
		t.Fatalf("compacted excerpt lost original text or included synthetic/private text: %s", excerpt)
	}
}

func TestAutomaticSessionTitle(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint("failed=", failed), func(t *testing.T) {
			titleStream := streamText("Deployment approval checks", 17)
			if failed {
				titleStream = chunk(`{"content":"Unfinished title"}`, "")
			}
			lm := &fakeLM{script: []string{streamText("First answer", 70), streamText("Second answer", 90)}, titles: []string{titleStream}}
			h := fakeAgentModel(t, lm, 10000)
			cmd := h.submit("Could you investigate deployment approval checks?", nil)
			fallback := h.session.Title
			if fallback != "Could you investigate deployment approval checks?" || len(lm.titleRequests) != 0 {
				t.Fatal("missing immediate fallback or naming started before the answer")
			}
			h.drive(cmd)
			want, state := "Deployment approval checks", session.TitleGenerated
			if failed {
				want, state = fallback, session.TitleFallback
			}
			got, err := session.Load(h.o.Cwd, h.session.ID)
			if err != nil || got.Title != want || got.TitleState != state || h.session.Title != want || h.running || h.pal != nil {
				t.Fatalf("automatic title did not settle: session=%+v err=%v", got, err)
			}
			if len(got.Messages) != 2 || got.LastInput != 70 || got.UsageIn != 70 || got.UsageOut != 5 {
				t.Fatalf("naming changed conversation state: %+v", got)
			}
			if !failed && (got.TitleUsageIn != 17 || got.TitleUsageOut != 5 || !strings.Contains(h.View().WindowTitle, want)) {
				t.Fatal("generated title or usage did not reach the persisted session and UI")
			}
			// Resume and another turn neither rename nor retry a failed title.
			h.resume(got.ID)
			h.drive(h.submit("Continue with the second check", nil))
			if h.session.Title != want || len(lm.titleRequests) != 1 || len(h.connection.Agent.Messages()) != 4 {
				t.Fatal("subsequent turn repeated naming or corrupted history")
			}
		})
	}
}

func TestAutomaticTitleLateResult(t *testing.T) {
	for _, scenario := range []string{"manual", "external rename", "delete", "switch", "next run", "legacy"} {
		t.Run(scenario, func(t *testing.T) {
			lm := &fakeLM{script: []string{streamText("Next answer", 81)}, titles: []string{streamText("Generated deployment title", 17)}}
			h := fakeAgentModel(t, lm, 10000)
			s := savedManagedSession(t, h.model)
			if scenario == "legacy" {
				s.TitleState = ""
				if err := s.Save(); err != nil {
					t.Fatal(err)
				}
			}
			h.resume(s.ID)
			_, cmd := h.Update(runDoneMsg{})
			messages := runCmd(cmd) // response completed, but UI has not received it
			var next tea.Cmd
			switch scenario {
			case "manual":
				h.renameSession(s.ID, s.Title) // even an identical manual name wins
			case "external rename", "delete":
				external, err := session.Load(h.o.Cwd, s.ID)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "delete" {
					err = session.Delete(h.o.Cwd, s.ID)
				} else {
					external.Title = "Name from another process"
					err = external.Save()
				}
				if err != nil {
					t.Fatal(err)
				}
			case "switch":
				h.resetSession()
			case "next run":
				next = h.submit("A follow-up while naming finishes", nil)
			}
			for _, msg := range messages {
				h.Update(msg)
			}
			if scenario == "next run" {
				if h.session.Title != s.Title {
					t.Fatal("title was saved while the run owned persistence")
				}
				h.drive(next)
			}
			got, err := session.Load(h.o.Cwd, s.ID)
			if scenario == "delete" {
				if err == nil {
					t.Fatal("late title recreated a deleted session")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := "Generated deployment title"
			switch scenario {
			case "manual", "legacy":
				want = s.Title
			case "external rename":
				want = "Name from another process"
			}
			if got.Title != want || (scenario == "switch" && h.session != nil) || (scenario == "legacy" && len(lm.titleRequests) != 0) {
				t.Fatalf("late result violated title ownership: %+v", got)
			}
			if scenario == "next run" && (len(got.Messages) != len(s.Messages)+2 || got.LastInput != 81 || strings.Contains(transcript(h.model), "could not save")) {
				t.Fatal("title result raced the next run's history or usage save")
			}
		})
	}
}

func TestGeneratedTitleCompletion(t *testing.T) {
	for _, tc := range []struct{ name, response, want string }{
		{"complete", streamText("“Deployment checks”", 50), "Deployment checks"},
		{"unicode limit", streamText(strings.Repeat("界", 70), 50), strings.Repeat("界", 59) + "…"},
		{"length", chunk(`{"content":"Partial title"}`, "length") + "data: [DONE]\n\n", ""},
		{"disconnect", chunk(`{"content":"Partial title"}`, ""), ""},
		{"tools", streamToolCall("c1", "go.mod"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := fakeAgentModel(t, &fakeLM{titles: []string{tc.response}}, 10000)
			got, _, err := generateTitle(t.Context(), h.connection.Agent.Model, "user: Check deployment")
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("title=%q err=%v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestSubscriptionTitleStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["stream"] != true || body["max_output_tokens"] != nil {
			t.Error("subscription title must use its streaming protocol without unsupported caps")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"Deployment checks\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":4,\"total_tokens\":13}}}\n\n")
	}))
	defer srv.Close()
	store := &chatgpt.Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	if err := store.Put("test", chatgpt.Tokens{AccessToken: "fake-access", ClientID: "oaiapp_test", Subject: "test-user", Issuer: "https://auth.example", Scopes: "resource.invoke chatgpt.tokens.use.direct", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	old := provider.AuthStore
	provider.AuthStore = func() (*chatgpt.Store, error) { return store, nil }
	defer func() { provider.AuthStore = old }()
	mdl, err := provider.Open(t.Context(), config.ModelRef{ConnID: "test", Conn: config.Connection{Kind: config.KindSubscription, BaseURL: srv.URL}, Model: config.Model{ID: "gpt-5.3-codex"}}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	title, usage, err := generateTitle(t.Context(), mdl, "user: Check deployment")
	if err != nil || title != "Deployment checks" || usage.InputTokens != 9 || usage.OutputTokens != 4 {
		t.Fatalf("subscription title=%q usage=%+v err=%v", title, usage, err)
	}
}
