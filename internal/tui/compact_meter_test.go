package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/dantearo/arkex/internal/agent"
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
	if a, b := ansi.Strip(m.gauge(footerKey{compacting: true, compactFrame: 0})), ansi.Strip(m.gauge(footerKey{compacting: true, compactFrame: 1})); a != "ctx ▁▂▄▆█▆▄▂ compacting" || b != "ctx ▂▁▂▄▆█▆▄ compacting" {
		t.Fatalf("wave must travel right: %q / %q", a, b)
	}
	m.applyEvent(agent.Compacted{Summary: "summary", Dropped: 10, Trimmed: 3, Reason: "window full"})
	m.applyEvent(agent.Compacting{Active: false})
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
