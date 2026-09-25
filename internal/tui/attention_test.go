package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/charmbracelet/x/ansi"
)

func TestAttentionWaveAndTitle(t *testing.T) {
	m, _ := testModel(t)
	m.running = true
	m.pending = newApproval(agent.ToolCall{Name: "bash"}, make(chan agent.Answer, 1))
	seen := map[string]bool{}
	for frame := 0; frame < 12; frame++ {
		m.frame = frame
		dots := attention(frame)
		if ansi.StringWidth(dots) != 5 || ansi.Strip(dots) != dots {
			t.Fatalf("bad frame %q", dots)
		}
		if !strings.HasPrefix(m.windowTitle(), dots+" ") || !strings.Contains(ansi.Strip(m.workingStrip(100)), dots+"  waiting for your answer") {
			t.Fatal("title and CLI out of sync")
		}
		seen[dots] = true
	}
	if len(seen) != 6 || attention(0) != attention(12) {
		t.Fatal("wave not periodic")
	}
	m.pending = nil
	if m.activityIndicator() != loader(m.frame) {
		t.Fatal("did not return to working indicator")
	}
}

func TestNotificationBellEvents(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, event := range []tea.Msg{
			approvalMsg{call: agent.ToolCall{Name: "bash"}, reply: make(chan agent.Answer, 1)},
			approvalMsg{retry: true, reply: make(chan agent.Answer, 1)},
			runDoneMsg{}, runDoneMsg{err: errors.New("failed")},
			runDoneMsg{err: &agent.PausedError{Reason: "repeated tool"}},
			runDoneMsg{err: context.Canceled},
		} {
			m, _ := testModel(t)
			if enabled {
				m.o.UI.Bell = new(true)
			}
			m.running = true
			_, cmd := m.Update(event)
			want := enabled
			if done, ok := event.(runDoneMsg); ok && errors.Is(done.err, context.Canceled) {
				want = false
			}
			if (cmd != nil) != want {
				t.Fatalf("event %T enabled=%v cmd=%v", event, enabled, cmd != nil)
			}
			if cmd != nil {
				raw, ok := cmd().(tea.RawMsg)
				if !ok || raw.Msg != "\a" {
					t.Fatalf("not a single terminal bell: %#v", raw)
				}
			}
		}
	}
}
