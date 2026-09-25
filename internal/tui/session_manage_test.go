package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"

	"github.com/attrition-tech/arkex/internal/session"
)

func savedManagedSession(t *testing.T, m *model) *session.Session {
	t.Helper()
	s := session.New(m.o.Cwd)
	s.Update(sampleMessages(), m.sess.Name, "build", 4200, 700)
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
	if err != nil || got.Title != "Deployment checks" || m.conv.Title != got.Title || got.UsageIn != 4200 || got.LastInput != 730 {
		t.Fatalf("rename lost state: %+v, %v", got, err)
	}
	// Subsequent autosaves must preserve the manually chosen title.
	m.saveConv()
	got, _ = session.Load(m.o.Cwd, s.ID)
	if got.Title != "Deployment checks" {
		t.Fatal("autosave replaced manual title")
	}
	m.running = true
	m.setSessionState(s.ID, session.Archived)
	got, _ = session.Load(m.o.Cwd, s.ID)
	if got.State != "" || m.conv == nil {
		t.Fatal("management must not change a running session")
	}
	m.running = false
	m.setSessionState(s.ID, session.Archived)
	if m.conv != nil || len(m.sess.Agent.Messages()) != 0 {
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
	if m.conv != nil {
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
	if m.conv != nil {
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
	lm := &fakeLM{script: []string{jsonReply("Fix deployment checks")}}
	h := fakeAgentModel(t, lm, 10000)
	s := savedManagedSession(t, h.model)
	h.resume(s.ID)
	before := h.sess.Agent.Messages()
	h.confirmTitleGeneration(s.ID)
	if len(lm.requests) != 0 {
		t.Fatal("opening confirmation called model")
	}
	h.drive(h.generateSessionTitle(s.ID))
	if h.pal == nil || !h.pal.editing || h.pal.input.Value() != "Fix deployment checks" {
		t.Fatal("missing title preview")
	}
	got, _ := session.Load(h.o.Cwd, s.ID)
	if got.Title != s.Title || !reflect.DeepEqual(before, h.sess.Agent.Messages()) || h.lastInput != 730 {
		t.Fatal("suggestion changed conversation or title before acceptance")
	}
	if len(lm.requests) != 1 || len(lm.requests[0]["messages"].([]any)) != 2 {
		t.Fatalf("expected one isolated request: %+v", lm.requests)
	}
	if tools, ok := lm.requests[0]["tools"].([]any); ok && len(tools) > 0 {
		t.Fatal("title request has tools")
	}
	if cap := lm.requests[0]["max_tokens"]; cap != float64(1024) {
		t.Fatalf("output cap = %v", cap)
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
	if h.pal != nil || len(lm.requests) != 1 {
		t.Fatal("dismissed generation still called the provider or opened a preview")
	}
}

func TestTitleExcerptAndValidation(t *testing.T) {
	msgs := []fantasy.Message{fantasy.NewUserMessage("Original request\n\n<file path=\"private\">SECRET FILE</file>")}
	for range 20 {
		msgs = append(msgs, fantasy.NewUserMessage(strings.Repeat("界", 1000)))
	}
	msgs = append(msgs, fantasy.NewUserMessage("Latest request"), fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ReasoningPart{Text: "SECRET REASONING"}}})
	excerpt := titleExcerpt(msgs)
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
	h := fakeAgentModel(t, &fakeLM{script: []string{jsonReply("")}}, 10000)
	s := savedManagedSession(t, h.model)
	h.drive(h.generateSessionTitle(s.ID))
	got, _ := session.Load(h.o.Cwd, s.ID)
	if got.Title != s.Title || h.pal != nil {
		t.Fatal("empty AI result changed title")
	}
}
