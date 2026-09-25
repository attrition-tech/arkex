package tui

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/session"
	"github.com/attrition-tech/arkex/internal/tools"
)

func TestHistoryNavigation(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	cwd := t.TempDir()
	h := loadHistory(cwd)
	h.add("first")
	h.add("second\nline two")
	h.add("second\nline two") // consecutive repeat is dropped
	h.add("/mode plan")
	if len(h.entries) != 3 {
		t.Fatalf("entries = %q", h.entries)
	}

	if _, ok := h.next(); ok {
		t.Fatal("next without navigating must report false")
	}
	got, ok := h.prev("draft in progress")
	if !ok || got != "/mode plan" {
		t.Fatalf("prev = %q %v", got, ok)
	}
	got, _ = h.prev(got)
	if got != "second\nline two" {
		t.Fatalf("prev = %q", got)
	}
	got, _ = h.prev(got)
	got2, ok := h.prev(got)
	if got != "first" || ok || got2 != "" {
		t.Fatalf("oldest: %q then %q %v", got, got2, ok)
	}
	// Forward again ends on the saved draft, not on an empty line.
	h.next()
	h.next()
	got, ok = h.next()
	if !ok || got != "draft in progress" {
		t.Fatalf("draft = %q %v", got, ok)
	}
	if _, ok := h.next(); ok {
		t.Fatal("past the draft there is nothing")
	}

	// Persisted, newest last, multi-line intact.
	again := loadHistory(cwd)
	if strings.Join(again.entries, "|") != "first|second\nline two|/mode plan" {
		t.Fatalf("reloaded = %q", again.entries)
	}
	fi, err := os.Stat(historyPath(cwd))
	if err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("history file: %v %v", fi, err)
	}
}

func TestHistoryCap(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	cwd := t.TempDir()
	h := loadHistory(cwd)
	for i := 0; i < maxHistory+20; i++ {
		h.add(strings.Repeat("x", 1+i%7) + string(rune('a'+i%26)))
	}
	if len(h.entries) != maxHistory {
		t.Fatalf("len = %d", len(h.entries))
	}
	again := loadHistory(cwd)
	if len(again.entries) != maxHistory || again.entries[maxHistory-1] != h.entries[maxHistory-1] {
		t.Fatalf("reloaded %d entries, last %q vs %q", len(again.entries), again.entries[len(again.entries)-1], h.entries[maxHistory-1])
	}
}

func TestUpDownRecallPrompts(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	m, _ := testModel(t)
	m.hist.add("older prompt")
	m.hist.add("/help")
	typeKeys(m, "n", "e", "w")
	m.handleKey(key("up"))
	if m.input.Value() != "/help" {
		t.Fatalf("up = %q", m.input.Value())
	}
	m.handleKey(key("up"))
	if m.input.Value() != "older prompt" {
		t.Fatalf("up up = %q", m.input.Value())
	}
	m.handleKey(key("down"))
	m.handleKey(key("down"))
	if m.input.Value() != "new" {
		t.Fatalf("draft restored = %q", m.input.Value())
	}
	// Sending records the prompt and resets navigation.
	m.handleKey(key("enter"))
	if got := m.hist.entries[len(m.hist.entries)-1]; got != "new" || m.hist.idx != len(m.hist.entries) {
		t.Fatalf("after enter: last=%q idx=%d", got, m.hist.idx)
	}
}

func TestUpInsideMultilineMovesCursor(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	m, _ := testModel(t)
	m.hist.add("recalled")
	m.setInput("line one\nline two")
	// Cursor is on the last line: up moves within the text, not history.
	m.handleKey(key("up"))
	if m.input.Value() != "line one\nline two" || m.input.Line() != 0 {
		t.Fatalf("value=%q line=%d", m.input.Value(), m.input.Line())
	}
	// Now on the first line: up recalls.
	m.handleKey(key("up"))
	if m.input.Value() != "recalled" {
		t.Fatalf("value=%q", m.input.Value())
	}
}

func TestBangRunsShellAndAttachesOutput(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	m, _ := testModel(t)
	m.setSession(Connection{Agent: &agent.Agent{}, Name: "local/a"})
	typeKeys(m, "!", "e", "c", "h", "o", " ", "h", "i")
	_, cmd := m.handleKey(key("enter"))
	if cmd == nil {
		t.Fatal("!cmd must return a command")
	}
	last := m.blocks[len(m.blocks)-1]
	if last.kind != blockTool || last.name != "bash" || last.status != "running" || last.args["command"] != "echo hi" {
		t.Fatalf("card = %+v", last)
	}
	msg, ok := cmd().(shellDoneMsg)
	if !ok {
		t.Fatalf("cmd returned %T", cmd())
	}
	m.Update(msg)
	if last.status != "ok" || !strings.HasPrefix(last.output, "hi\n") || last.dur <= 0 {
		t.Fatalf("finished card = %+v", last)
	}
	if len(m.shellNotes) != 1 {
		t.Fatalf("notes = %+v", m.shellNotes)
	}

	// A failing command is an error card with the exit status and is queued
	// too; the next prompt carries both, newest last, then the queue clears.
	m.blocks = append(m.blocks, &block{kind: blockTool, id: "shell-9", name: "bash", status: "running", args: map[string]any{"command": "false"}})
	m.Update(shellDoneMsg{id: "shell-9", res: tools.Result{Output: "[exit status 1]"}, err: errors.New("exit status 1"), dur: time.Millisecond})
	if m.blocks[len(m.blocks)-1].status != "error" {
		t.Fatalf("failed card = %+v", m.blocks[len(m.blocks)-1])
	}
	notes := m.takeShellNotes()
	if !strings.Contains(notes, "<shell command=\"echo hi\">\nhi\n</shell>") || !strings.Contains(notes, "<shell command=\"false\">\n[exit status 1]\n</shell>") {
		t.Fatalf("notes = %q", notes)
	}
	if strings.Index(notes, "echo hi") > strings.Index(notes, "false") || m.takeShellNotes() != "" {
		t.Fatalf("order or clearing wrong: %q", notes)
	}

	// Session titles ignore the attachment.
	msgText := "now fix it" + notes
	if got := session.UserText(fantasy.NewUserMessage(msgText)); got != "now fix it" {
		t.Fatalf("UserText = %q", got)
	}
}

func TestCtrlLClearsScreenNotConversation(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	m, _ := testModel(t)
	m.o.Connect = nil
	m.setSession(Connection{Agent: &agent.Agent{}, Name: "local/a"})
	m.submit("hello", nil)
	m.sess.Agent.SetMessages(sampleMessages())
	m.Update(runDoneMsg{})
	id := m.conv.ID
	m.handleKey(key("ctrl+l"))
	if len(m.blocks) != 1 || !strings.Contains(m.blocks[0].text.String(), "conversation continues") {
		t.Fatalf("blocks = %d", len(m.blocks))
	}
	if m.conv == nil || m.conv.ID != id || len(m.sess.Agent.Messages()) != 4 {
		t.Fatal("ctrl+l must keep the conversation")
	}
	// /new after ctrl+l brings the banner back.
	m.command("/new")
	if len(m.blocks) != 1 || m.blocks[0] != m.welcome {
		t.Fatalf("after /new: %d blocks, welcome=%v", len(m.blocks), m.blocks[0] == m.welcome)
	}
	// resume after ctrl+l also starts from the banner.
	m.command("/resume " + id)
	if m.blocks[0] != m.welcome || len(m.blocks) < 2 {
		t.Fatalf("after resume: %d blocks", len(m.blocks))
	}
}
