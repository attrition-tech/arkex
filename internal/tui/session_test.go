package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/session"
)

func TestTerminalTitleFollowsSession(t *testing.T) {
	m, _ := testModel(t)
	m.o.Cwd = "/workspace/nsutm"
	if got := m.View().WindowTitle; got != "New session - nsutm - arkex" {
		t.Fatal(got)
	}
	m.session = session.New(m.o.Cwd)
	m.session.Title = "Fix rendering"
	if got := m.View().WindowTitle; got != "Fix rendering - nsutm - arkex" {
		t.Fatal(got)
	}
	m.session.Title = "Renamed session"
	m.openModels("")
	if got := m.View().WindowTitle; got != "Renamed session - nsutm - arkex" {
		t.Fatal("panel lost session title: " + got)
	}
	m.panel = nil
	m.session.Title = "\x1b]2;injected\a  Hello\n\tworld\x1b[31m"
	if got := m.View().WindowTitle; got != "Hello world - nsutm - arkex" {
		t.Fatalf("unsafe title: %q", got)
	}
	m.session.Title = strings.Repeat("界", 40)
	got := m.View().WindowTitle
	if ansi.StringWidth(got) > 64 || !strings.HasSuffix(got, "… - nsutm - arkex") {
		t.Fatalf("long title: %q", got)
	}
	m.running, m.frame = true, 3
	got = m.View().WindowTitle
	if !strings.HasPrefix(got, loader(3)+" ") || ansi.StringWidth(got) > 64 || !strings.HasSuffix(got, "… - nsutm - arkex") {
		t.Fatalf("long active title: %q", got)
	}
	m.running = false
	m.resetSession()
	if got := m.View().WindowTitle; got != "New session - nsutm - arkex" {
		t.Fatal(got)
	}
}

func sampleMessages() []fantasy.Message {
	return []fantasy.Message{
		fantasy.NewUserMessage("fix the build\n\n<file path=\"go.mod\">\nmodule x\n</file>"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ReasoningPart{Text: "let me look"},
			fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: `{"command":"go build"}`},
			fantasy.ToolCallPart{ToolCallID: "c2", ToolName: "bash", Input: `{"command":"false"}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: "ok\n"}},
			fantasy.ToolResultPart{ToolCallID: "c2", Output: fantasy.ToolResultOutputContentError{Error: errors.New("exit 1")}},
		}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "It builds now."}}},
	}
}

func TestBlocksFromMessages(t *testing.T) {
	blocks := blocksFromMessages(sampleMessages())
	kinds := []string{}
	for _, b := range blocks {
		kinds = append(kinds, map[blockKind]string{blockUser: "user", blockAssistant: "assistant", blockReasoning: "reasoning", blockTool: "tool", blockSystem: "system"}[b.kind])
	}
	if strings.Join(kinds, ",") != "user,reasoning,tool,tool,assistant" {
		t.Fatalf("kinds = %v", kinds)
	}
	if blocks[0].text.String() != "fix the build" {
		t.Fatalf("user block must drop attached files, got %q", blocks[0].text.String())
	}
	if blocks[2].status != "ok" || blocks[2].output != "ok\n" || blocks[2].args["command"] != "go build" {
		t.Fatalf("first tool card = %+v", blocks[2])
	}
	if blocks[3].status != "error" || blocks[3].output != "exit 1" {
		t.Fatalf("failed tool card = %+v", blocks[3])
	}
}

func TestResumeStatsBeforeFirstResponse(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	m := footerModel(t)
	for _, lastInput := range []int64{730, 0} {
		s := session.New(m.o.Cwd)
		s.Update(sampleMessages(), session.Usage{Model: m.connection.Name, Input: 50000, Output: 2000})
		s.LastInput = lastInput // zero omits the field, like legacy sessions
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		m.command("/resume " + s.ID)
		m.command("/stats")
		want := "73%"
		if lastInput == 0 {
			want = "— / 1000 tokens"
		}
		if got := ansi.Strip(m.View().Content); !strings.Contains(got, want) || strings.Contains(got, "0%") {
			t.Fatalf("last=%d stats=%q", lastInput, got)
		}
		m.applyEvent(agent.TurnEnd{Usage: fantasy.Usage{InputTokens: 810}})
		if got := ansi.Strip(m.View().Content); !strings.Contains(got, "81%") {
			t.Fatalf("fresh measurement not shown: %q", got)
		}
	}
}

func TestSaveAndResume(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	m, _ := testModel(t)
	m.o.Connect = func(_ context.Context, selector string) (Connection, error) {
		if selector == "" {
			selector = "local/b"
		}
		return Connection{Agent: &agent.Agent{}, Name: selector}, nil
	}
	h := &harness{model: m}
	m.send = h.enqueue
	ag := &agent.Agent{}
	m.setConnection(Connection{Agent: ag, Name: "local/a"})

	// A turn: submit creates the session; the worker saves before runDone.
	m.submit("fix the build", nil)
	if m.session == nil {
		t.Fatal("submit must open a session")
	}
	ag.SetMessages(sampleMessages())
	m.usageIn, m.usageOut, m.lastInput = 42, 7, 17
	if err := m.persistence.finish(t.Context(), ag.Messages(), fantasy.Usage{InputTokens: 42, OutputTokens: 7}, 17, nil); err != nil {
		t.Fatal(err)
	}
	h.drive(func() tea.Msg { return runDoneMsg{} })
	list, err := session.List(m.o.Cwd)
	if err != nil || len(list) != 1 || list[0].Title != "fix the build" || list[0].Messages != 4 {
		t.Fatalf("saved list = %+v err = %v", list, err)
	}
	id := list[0].ID

	dir, err := session.Dir(m.o.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Both spellings start a fresh session without changing the saved one.
	for _, command := range []string{"/new", "/clear"} {
		_, cmd := m.command(command)
		h.drive(cmd)
		if m.session != nil || len(m.connection.Agent.Messages()) != 0 || len(m.blocks) != 1 {
			t.Fatalf("%s must reset the active session", command)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("%s changed the saved session: %v", command, err)
		}

		// Resume with a different model rebuilds everything.
		m.setConnection(Connection{Agent: ag, Name: "local/b"})
		_, cmd = m.command("/resume " + id)
		h.drive(cmd)
		if m.session == nil || m.session.ID != id || len(m.connection.Agent.Messages()) != 4 || m.connection.Name != "local/a" {
			t.Fatalf("resume did not restore the agent: session=%v model=%s", m.session, m.connection.Name)
		}
		if m.usageIn != 42 || m.lastInput != 17 {
			t.Fatalf("usage not restored: in=%d last=%d", m.usageIn, m.lastInput)
		}
		if got := len(m.blocks); got != 1+5 { // welcome + 5 rebuilt; notices are not transcript blocks
			t.Fatalf("blocks = %d", got)
		}
	}

	// The next turn appends to the same session file rather than a new one.
	m.submit("thanks", nil)
	if err := m.persistence.finish(t.Context(), m.connection.Agent.Messages(), fantasy.Usage{}, 17, nil); err != nil {
		t.Fatal(err)
	}
	h.drive(func() tea.Msg { return runDoneMsg{} })
	if list, _ := session.List(m.o.Cwd); len(list) != 1 || list[0].ID != id {
		t.Fatalf("resumed session must be updated in place, got %+v", list)
	}

	// Errors are reported, not fatal.
	m.command("/resume deadbeef")
	if last := m.blocks[len(m.blocks)-1].text.String(); !strings.Contains(last, `no session "deadbeef"`) {
		t.Fatalf("bad id note = %q", last)
	}
	// /resume without an id opens the palette on the sessions list.
	m.command("/resume")
	if m.pal == nil || m.pal.level().title != "Resume session" || len(m.pal.view) != 3 || !strings.HasPrefix(m.pal.view[0].hint, "current") {
		t.Fatalf("palette = %+v", m.pal)
	}
	// A fresh model in this directory lists the session on the welcome screen.
	fresh := newModel(Options{Cwd: m.o.Cwd, Connect: m.o.Connect, Resume: "latest"})
	(&harness{model: fresh}).drive(fresh.Init())
	if len(fresh.start.recent) != 1 || fresh.start.recent[0].ID != id || fresh.start.recent[0].Title != "fix the build" {
		t.Fatalf("recent sessions = %+v", fresh.start.recent)
	}
	if fresh.session == nil || fresh.session.ID != id {
		t.Fatal("Resume: latest must load the session at startup")
	}
}

func TestModelSwitchPreservesSession(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	m, _ := testModel(t)
	a := &agent.Agent{}
	m.setConnection(Connection{Agent: a, Name: "local/a"})
	m.submit("fix the build", nil)
	a.SetMessages(sampleMessages())
	m.Update(runDoneMsg{})
	id := m.session.ID

	// Same provider: everything, including reasoning, moves to the new
	// agent and the session file stays the same.
	b := &agent.Agent{}
	m.Update(connectedMsg{sel: "local/b", name: "local/b", conn: Connection{Agent: b, Name: "local/b"}})
	if m.connection.Agent != b || m.session == nil || m.session.ID != id {
		t.Fatalf("switch must keep the session: session=%v", m.session)
	}
	if got := b.Messages(); len(got) != 4 || len(got[1].Content) != 3 {
		t.Fatalf("same-provider switch must carry all parts, got %+v", got)
	}
	if !strings.Contains(m.flashText, "carried over") {
		t.Fatalf("notice = %q", m.flashText)
	}

	// Different provider: reasoning parts are dropped, tool calls and text
	// survive, and the next save records the new model.
	c := &agent.Agent{}
	m.Update(connectedMsg{sel: "other/c", name: "other/c", conn: Connection{Agent: c, Name: "other/c"}})
	got := c.Messages()
	if len(got) != 4 || len(got[1].Content) != 2 {
		t.Fatalf("cross-provider switch must drop reasoning only, got %+v", got)
	}
	for _, msg := range got {
		for _, p := range msg.Content {
			if _, ok := p.(fantasy.ReasoningPart); ok {
				t.Fatal("reasoning part survived a provider change")
			}
		}
	}
	if !strings.Contains(m.flashText, "reasoning dropped") {
		t.Fatalf("notice = %q", m.flashText)
	}
	s, err := session.Load(m.o.Cwd, id)
	if err != nil || s.Model != "other/c" || len(s.Messages) != 4 {
		t.Fatalf("saved session = %+v err = %v", s, err)
	}
}

func TestStripReasoningDropsEmptyAssistant(t *testing.T) {
	msgs := []fantasy.Message{
		fantasy.NewUserMessage("hi"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ReasoningPart{Text: "only thinking"}}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "answer", ProviderOptions: fantasy.ProviderOptions{"x": nil}},
		}},
	}
	out := stripReasoning(msgs)
	if len(out) != 2 || out[1].Content[0].(fantasy.TextPart).ProviderOptions != nil {
		t.Fatalf("got %+v", out)
	}
	if len(msgs[2].Content[0].(fantasy.TextPart).ProviderOptions) != 1 {
		t.Fatal("stripReasoning must not mutate its input")
	}
}
