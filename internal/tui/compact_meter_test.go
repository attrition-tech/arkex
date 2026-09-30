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

func TestCompactionMeterWaveAndLifecycle(t *testing.T) {
	m := footerModel(t)
	m.running = true
	m.applyEvent(agent.Compacting{Active: true})
	for _, width := range []int{120, 80, 60, 40} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		first := m.footer()
		m.workingFrame(workingMsg{gen: m.runGen})
		second := m.footer()
		if first == second || !strings.Contains(ansi.Strip(second), "compacting") {
			t.Fatalf("meter not visible/animated at %d: %s", width, ansi.Strip(second))
		}
		frameLines(t, m, "compaction meter")
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
	if a, b := ansi.Strip(m.gauge(footerKey{compacting: true, compactFrame: 0})), ansi.Strip(m.gauge(footerKey{compacting: true, compactFrame: 1})); a != "ctx ▁▂▄▆█▆▄▂ compacting" || b != "ctx ▂▁▂▄▆█▆▄ compacting" {
		t.Fatalf("wave must travel right: %q / %q", a, b)
	}
	m.applyEvent(agent.Compacted{Summary: "summary", Dropped: 10, Trimmed: 3, Reason: "window full"})
	m.applyEvent(agent.Compacting{Active: false, Usage: fantasy.Usage{InputTokens: 41, OutputTokens: 13}})
	if m.usageIn != 83 || m.usageOut != 20 { // fixture starts at 42 in / 7 out
		t.Fatal("compaction usage lost")
	}
	m.refresh()
	if strings.Contains(ansi.Strip(m.footer()), "compacting") {
		t.Fatal("completed animation stuck")
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
