package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/charmbracelet/x/ansi"
)

func TestStopConfirmation(t *testing.T) {
	for _, stroke := range []string{"esc", "ctrl+c"} {
		t.Run(stroke, func(t *testing.T) {
			m, _ := testModel(t)
			m.layout()
			m.running = true
			calls := 0
			m.cancel = func() { calls++ }
			m.setInput("keep this draft")
			typeKeys(m, stroke)
			if calls != 0 || !strings.Contains(m.flashText, "stop this run") {
				t.Fatal("first stroke must only confirm")
			}
			box, _, _ := m.noticeBox()
			if box == "" {
				t.Fatal("confirmation must be visible")
			}
			typeKeys(m, stroke)
			if calls != 1 || m.input.Value() != "keep this draft" || m.confirmKey != "" {
				t.Fatal("second stroke must cancel once and preserve draft")
			}
		})
	}
}

func TestConfirmationDisarms(t *testing.T) {
	for _, event := range []string{"key", "mixed", "expired", "timer", "done", "compact", "notice", "paste"} {
		t.Run(event, func(t *testing.T) {
			m, _ := testModel(t)
			m.running = true
			calls := 0
			m.cancel = func() { calls++ }
			typeKeys(m, "ctrl+c")
			switch event {
			case "key":
				typeKeys(m, "x")
			case "mixed":
				typeKeys(m, "esc")
			case "expired":
				m.confirmUntil = time.Now().Add(-time.Millisecond)
			case "timer":
				m.Update(flashClearMsg{gen: m.flashGen})
			case "done":
				m.Update(runDoneMsg{})
			case "compact":
				m.Update(compactDoneMsg{err: context.Canceled})
			case "notice":
				m.flash("Another notice")
			case "paste":
				m.Update(tea.PasteMsg{Content: "draft"})
			}
			m.Update(key("ctrl+c"))
			if calls != 0 || m.confirmKey != "ctrl+c" {
				t.Fatal("must re-arm, not cancel/exit")
			}
		})
	}
}

func TestIdleQuitPreservesDraft(t *testing.T) {
	m, _ := testModel(t)
	m.setInput("unsent")
	m.attachments = []attachment{{}}
	typeKeys(m, "esc")
	if m.confirmKey != "" {
		t.Fatal("idle Esc must not arm exit")
	}
	typeKeys(m, "ctrl+c")
	if m.input.Value() != "unsent" || len(m.attachments) != 1 || !strings.Contains(m.flashText, "exit arkex") {
		t.Fatal("first Ctrl+C must preserve draft and attachments")
	}
	_, cmd := m.Update(key("ctrl+c"))
	if cmd == nil {
		t.Fatal("missing quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("second Ctrl+C must quit")
	}
}

func TestRetryAndToolConfirmation(t *testing.T) {
	for _, retry := range []bool{false, true} {
		m, _ := testModel(t)
		m.running = true
		reply := make(chan agent.Answer, 1)
		m.Update(approvalMsg{retry: retry, reply: reply})
		typeKeys(m, "esc")
		if retry {
			if len(reply) != 0 {
				t.Fatal("single Esc canceled retry")
			}
			box, _, y := m.noticeBox()
			if box == "" || y >= m.vp.Height() {
				t.Fatal("notice missing above retry dialog")
			}
			typeKeys(m, "esc")
		}
		select {
		case ans := <-reply:
			if ans != agent.Deny {
				t.Fatal(ans)
			}
		default:
			t.Fatal("dialog not answered")
		}
		if m.confirmKey != "" {
			t.Fatal("dialog confirmation leaked")
		}
	}
}

func TestQuitCommandsConfirmStay(t *testing.T) {
	for _, command := range []string{"/quit", "/exit", "/q"} {
		m, _ := testModel(t)
		m.command(command)
		if m.pal == nil || m.pal.view[m.pal.sel].title != "Stay" {
			t.Fatal("Stay must be default")
		}
		typeKeys(m, "enter")
		if m.pal != nil {
			t.Fatal("Stay must close confirmation")
		}
		m.command(command)
		typeKeys(m, "down")
		_, cmd := m.Update(key("enter"))
		if cmd == nil {
			t.Fatal("missing exit action")
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatal("Exit must quit")
		}
	}
}

func TestTabPromptFocus(t *testing.T) {
	m, _ := testModel(t)
	first, last := newBlock(blockUser, "first\nsecond line"), newBlock(blockUser, "newest")
	m.blocks = []*block{first, newBlock(blockAssistant, strings.Repeat("reply\n", 60)), last}
	m.hist = &history{entries: []string{"unrelated persisted history"}, idx: 1}
	m.layout()
	mode := m.mode()
	for _, step := range []struct {
		key  string
		want *block
	}{
		{"tab", last}, {"tab", first}, {"tab", first},
		{"shift+tab", last}, {"shift+tab", nil}, {"shift+tab", nil},
	} {
		typeKeys(m, step.key)
		if m.input.Value() != "" || m.promptFocus != step.want || m.mode() != mode {
			t.Fatalf("%s: input=%q focus=%p want=%p", step.key, m.input.Value(), m.promptFocus, step.want)
		}
		if first.key.hover != (step.want == first) || last.key.hover != (step.want == last) {
			t.Fatal("must highlight exactly one prompt")
		}
		if step.want != nil {
			for _, row := range step.want.lines {
				if ansi.StringWidth(row) != m.width-1 || !strings.Contains(ansi.Cut(row, m.width-5, m.width-2), "\x1b[48;") {
					t.Fatal("highlight must include padding near the right margin")
				}
			}
		}
		if step.want == first && m.vp.YOffset() != 0 {
			t.Fatal("older prompt not scrolled into view")
		}
	}
	typeKeys(m, "tab", "x")
	if m.promptFocus != nil || m.input.Value() != "x" {
		t.Fatal("typing must return to composer without recalling text")
	}
	want := m.input.Value()
	typeKeys(m, "shift+tab")
	if m.input.Value() != want {
		t.Fatal("must not overwrite edited prompt")
	}
	m.setInput("")
	m.replaceTranscript(nil)
	typeKeys(m, "tab", "shift+tab")
	if m.input.Value() != "" {
		t.Fatal("empty history changed input")
	}
}

func TestConfirmationPopupAndTimerIsolation(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	typeKeys(m, "ctrl+c")
	old := m.flashGen
	typeKeys(m, "x", "ctrl+c")
	m.Update(flashClearMsg{gen: old})
	if m.confirmKey != "ctrl+c" || m.flashText == "" {
		t.Fatal("old notice timer cleared a new confirmation")
	}
	typeKeys(m, "ctrl+p", "ctrl+c")
	if m.pal != nil || m.confirmKey != "" {
		t.Fatal("Ctrl+C should only dismiss the palette")
	}
	m.comp = &completion{word: "@file"}
	typeKeys(m, "ctrl+c")
	if m.comp != nil || m.confirmKey != "" {
		t.Fatal("Ctrl+C should dismiss completion, not arm an invisible confirmation")
	}
	m.running = true
	typeKeys(m, "ctrl+c")
	reply := make(chan agent.Answer, 1)
	m.Update(approvalMsg{reply: reply})
	typeKeys(m, "ctrl+c")
	if len(reply) != 0 || m.confirmKey != "ctrl+c" {
		t.Fatal("approval must require its own confirmation")
	}
}
