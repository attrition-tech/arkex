package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/session"
	"github.com/charmbracelet/x/ansi"
)

func TestResponseDurationUnits(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0ms"}, {999999 * time.Microsecond, "999ms"},
		{time.Second, "1s"}, {2356 * time.Millisecond, "2.35s"},
		{59999 * time.Millisecond, "59.99s"}, {time.Minute, "1m 00s"},
		{72345 * time.Millisecond, "1m 12s"}, {time.Hour, "60m 00s"},
	} {
		if got := responseDuration(tc.d); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestResponseTimingBoundariesAndRetries(t *testing.T) {
	start := time.Now()
	at := func(d time.Duration) time.Time { return start.Add(d) }
	r := responseTiming{start: start}
	check := func(now time.Duration, running bool, want string) {
		t.Helper()
		first, llm, total := r.values(at(now), running)
		if got := strings.Join([]string{first, llm, total}, " | "); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	check(120*time.Millisecond, true, "120ms… | pending | 120ms…")
	r.observe(agent.RequestSent{At: at(200 * time.Millisecond)})
	r.observe(agent.ReasoningDelta{Text: "thinking"})
	r.observe(agent.TextDelta{At: at(300 * time.Millisecond)}) // empty chunk
	check(800*time.Millisecond, true, "800ms… | 600ms… | 800ms…")
	// A tool-only or failed attempt must not supply the answer request's timer.
	r.observe(agent.RequestTiming{Total: time.Second})
	r.observe(agent.RequestRestart{})
	check(2*time.Second, true, "2s… | pending | 2s…")
	r.observe(agent.RequestSent{At: at(2100 * time.Millisecond)})
	r.observe(agent.TextDelta{Text: "first", At: at(2900 * time.Millisecond)})
	// A retry after partial text preserves the user's actual first-text wait.
	r.observe(agent.RequestRestart{})
	r.observe(agent.RequestSent{At: at(4 * time.Second)})
	r.observe(agent.TextDelta{Text: "last", At: at(5 * time.Second)})
	check(6*time.Second, true, "2.9s | 800ms | 6s…")
	// Trailing usage packets and UI delays must not inflate last-text receipt.
	check(9*time.Second, false, "2.9s | 800ms | 5s")
	r = responseTiming{start: start}
	r.observe(agent.TextDelta{Text: "no HTTP trace", At: at(time.Second)})
	check(2*time.Second, false, "1s | — | 1s")
}

func TestResponseTimingLiveAndCompletedRendering(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := testModel(t)
		m.setSession(Connection{Agent: &agent.Agent{}})
		m.o.Prepare = func(*agent.Agent, string) error { time.Sleep(350 * time.Millisecond); return nil }
		sentAt := time.Now()
		m.startRunAt(sentAt, func(context.Context, *agent.Agent, func(agent.Event)) error { return nil })
		if got := time.Since(m.responseTiming.start); got != 350*time.Millisecond {
			t.Fatalf("preparation excluded: %s", got)
		}
		m.blocks = []*block{newBlock(blockUser, "Hello")}
		m.applyEvent(agent.TurnStart{Step: 1})
		m.applyEvent(agent.RequestSent{At: time.Now()})
		time.Sleep(650 * time.Millisecond)
		m.command("/stats")
		plain := func() string { return strings.Join(strings.Fields(ansi.Strip(m.View().Content)), " ") }
		if got := plain(); !strings.Contains(got, "First answer 1s…") || !strings.Contains(got, "LLM first answer 650ms…") {
			t.Fatal(got)
		}
		m.applyEvent(agent.TextDelta{Text: "Hello back!", At: time.Now()})
		time.Sleep(72 * time.Second)
		m.workingFrame(workingMsg{gen: m.runGen})
		if got := plain(); !strings.Contains(got, "Answer elapsed 1m 13s…") || !strings.Contains(got, "First answer 1s") {
			t.Fatal(got)
		}
		m.applyEvent(agent.TextDelta{Text: " Done.", At: time.Now()})
		time.Sleep(2 * time.Second)
		m.Update(runDoneMsg{})
		if got := plain(); !strings.Contains(got, "Answer elapsed 1m 13s") || !strings.Contains(got, "Run elapsed 1m 15s") || strings.Contains(got, "1m 15s…") {
			t.Fatalf("completed stats must distinguish answer from run duration: %s", got)
		}
		for _, width := range []int{100, 40} {
			lines := m.renderBlocks(width)
			got := strings.Join(strings.Fields(ansi.Strip(strings.Join(lines, "\n"))), " ")
			if strings.Contains(got, "First text") || strings.Contains(got, "First answer") {
				t.Fatalf("width %d: %s", width, got)
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > width {
					t.Fatalf("overflow: %q", line)
				}
			}
		}
		m.startRun(func(context.Context, *agent.Agent, func(agent.Event)) error { return nil })
		if !m.responseTiming.firstText.IsZero() || !m.responseTiming.requestSent.IsZero() {
			t.Fatal("new run retained previous boundaries")
		}
	})
}

func TestResponseTimingDoesNotLabelIncompleteRunsAsComplete(t *testing.T) {
	for _, err := range []error{context.Canceled, errors.New("disconnected"), &agent.PausedError{Reason: "step limit"}} {
		m, _ := testModel(t)
		start := time.Now().Add(-time.Second)
		m.startedAt, m.running = start, true
		m.responseTiming = responseTiming{start: start}
		m.applyEvent(agent.TextDelta{Text: "partial", At: time.Now()})
		m.command("/stats")
		m.Update(runDoneMsg{err: err})
		if strings.Contains(m.View().Content, "Session stats") {
			t.Fatalf("stats obscured the %v notice", err)
		}
		if got := ansi.Strip(strings.Join(m.renderBlocks(100), "\n")); strings.Contains(got, "First text") {
			t.Fatalf("completion footer on %v: %s", err, got)
		}
	}
}

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
