package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/dantearo/arkex/internal/agent"
	"github.com/dantearo/arkex/internal/session"
)

func TestTimingPresentation(t *testing.T) {
	m, _ := testModel(t)
	m.applyEvent(agent.ReasoningDelta{ID: "r1", Text: "consider this"})
	b := m.blocks[len(m.blocks)-1]
	m.blockLines(b, 90, false) // prime cache before duration arrives
	m.applyEvent(agent.TextDelta{Text: "answer"})
	m.applyEvent(agent.ReasoningTime{ID: "r1", Duration: 179 * time.Millisecond})
	if got := ansi.Strip(strings.Join(m.blockLines(b, 90, false), "\n")); !strings.Contains(got, "Thinking · 179ms") {
		t.Fatal(got)
	}
	m.applyEvent(agent.ReasoningTime{ID: "other", Duration: time.Hour})
	if b.dur != 179*time.Millisecond {
		t.Fatal("unrelated timing attached to reasoning")
	}
	m.applyEvent(agent.RequestTiming{FirstToken: 240 * time.Millisecond, Total: 900 * time.Millisecond, Dispatch: 2 * time.Millisecond})
	m.runDuration = 1300 * time.Millisecond
	items := timingItems(m)
	if items[0].title != "Total run · 1.3s" || items[1].title != "Request 1 · First token 240ms · Total 900ms" || !strings.Contains(items[1].detail, "Connection unavailable") {
		t.Fatalf("wrong timing labels: %+v", items)
	}
	m.command("/timing")
	if m.pal == nil || !strings.Contains(panelText(m), "First token 240ms") {
		t.Fatal("timing command did not open details")
	}
	m.newConv()
	if len(m.requestTimings) != 0 || m.runDuration != 0 {
		t.Fatal("new session retained timing")
	}
}

func TestTitleDirectoryBudgetAndSanitization(t *testing.T) {
	m, _ := testModel(t)
	m.conv = session.New(m.o.Cwd)
	m.conv.Title = strings.Repeat("界", 40)
	m.o.Cwd = "/parent/" + strings.Repeat("界", 20)
	for _, running := range []bool{false, true} {
		m.running = running
		got := m.windowTitle()
		if ansi.StringWidth(got) > 64 || !strings.HasSuffix(got, " - "+strings.Repeat("界", 9)+"… - arkex") || strings.Contains(got, "parent") {
			t.Fatal(got)
		}
	}
	if m.conv.Title != strings.Repeat("界", 40) {
		t.Fatal("saved title was shortened")
	}
	m.running = false
	m.o.Cwd = "/parent/\x1b[31mhello\nworld"
	if got := m.windowTitle(); strings.ContainsAny(got, "\x1b\n") || !strings.HasSuffix(got, " - hello world - arkex") {
		t.Fatal(got)
	}
}
