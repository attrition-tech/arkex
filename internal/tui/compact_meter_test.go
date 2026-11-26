package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/charmbracelet/x/ansi"
)

func TestCompactionStatusAndLifecycle(t *testing.T) {
	m := footerModel(t)
	m.running = true
	m.applyEvent(agent.Compacting{Active: true})
	for _, width := range []int{120, 80, 60, 40} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		m.workingFrame(workingMsg{gen: m.runGen})
		if status := ansi.Strip(m.footer()); !strings.Contains(status, "Compacting") {
			t.Fatalf("compaction status not visible at %d: %s", width, status)
		}
		frameLines(t, m, "compaction status")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	m.compactStarted = time.Now().Add(-12 * time.Second)
	m.applyEvent(agent.CompactProgress{Stage: "waiting for model", Part: 2})
	status := ansi.Strip(m.footer())
	for _, want := range []string{"waiting for model", "request 2", "12s", "esc twice cancels"} {
		if !strings.Contains(status, want) {
			t.Fatalf("missing %q: %s", want, status)
		}
	}
	m.applyEvent(agent.Compacted{Summary: "summary", Dropped: 10, Trimmed: 3, Reason: "window full"})
	m.applyEvent(agent.Compacting{Active: false, Usage: fantasy.Usage{InputTokens: 41, OutputTokens: 13}})
	if m.usageIn != 83 || m.usageOut != 20 { // fixture starts at 42 in / 7 out
		t.Fatal("compaction usage lost")
	}
	m.refresh()
	if strings.Contains(ansi.Strip(m.footer()), "Compacting") {
		t.Fatal("completed compaction status stuck")
	}
	if tr := transcript(m); !strings.Contains(tr, "3 oldest messages") || strings.Contains(tr, "context compacted because") {
		t.Fatalf("loss warning missing or routine line retained: %s", tr)
	}
	m.applyEvent(agent.Compacting{Active: true})
	m.Update(runDoneMsg{err: context.Canceled})
	if m.compacting {
		t.Fatal("cancel left animation running")
	}
}
