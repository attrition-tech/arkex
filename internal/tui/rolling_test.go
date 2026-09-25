package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/charmbracelet/x/ansi"
)

func rollingModel(t testing.TB, n int) *model {
	m, _ := testModel(t)
	m.blocks = []*block{newBlock(blockUser, "Check the project")}
	for i := 0; i < n; i++ {
		b := groupTool("bash", "")
		b.id = fmt.Sprintf("call-%d", i)
		b.args = toolArgs(fmt.Sprintf(`{"command":"command-%02d"}`, i))
		m.blocks = append(m.blocks, newBlock(blockReasoning, "thinking"), b)
	}
	m.blocks[len(m.blocks)-1].status = "running"
	m.running, m.stickBottom = true, true
	m.startedAt = time.Now()
	m.layout()
	return m
}

func TestRollingBoundaryAndInspection(t *testing.T) {
	m := rollingModel(t, 4)
	if m.liveCut != 0 {
		t.Fatal("fold before threshold")
	}
	m = rollingModel(t, 5)
	m.rollAt = time.Time{}
	got := groupText(m)
	if !strings.Contains(got, "Earlier work · 2 actions") || strings.Contains(got, "command-00") || !strings.Contains(got, "command-02") || !strings.Contains(got, "command-03") || !strings.Contains(got, "command-04") {
		t.Fatal(got)
	}
	cut := m.liveCut
	m.stickBottom = false
	m.blocks[len(m.blocks)-1].status = "ok"
	m.refresh()
	if m.liveCut != cut {
		t.Fatal("fold moved while scrolled")
	}
	m.stickBottom = true
	m.openWork = m.workSections()[0].head
	m.refresh()
	if m.liveCut != cut {
		t.Fatal("inspection moved")
	}
	m.openWork = nil
	m.refresh()
	if m.liveCut <= cut {
		t.Fatal("fold did not resume")
	}
	m.rollAt = time.Time{}
	m.blocks[4].status = "error"
	if got = groupText(m); !strings.Contains(got, "Earlier work · 3 actions · 1 failed") || strings.Count(got, "Earlier work") != 1 {
		t.Fatal("failure not counted in unified history", got)
	}
}

func TestRollingCountsOnlyCurrentPrompt(t *testing.T) {
	m := rollingModel(t, 8)
	m.blocks = append(m.blocks, newBlock(blockUser, "Next prompt"))
	start := len(m.blocks)
	m.liveCut = 0
	for range 4 {
		m.blocks = append(m.blocks, groupTool("bash", ""))
	}
	m.advanceRoll()
	if m.liveCut != 0 {
		t.Fatal("previous prompt's tools triggered folding")
	}
	m.blocks = append(m.blocks, &block{kind: blockTool, status: "running"})
	m.advanceRoll()
	if m.liveCut != start+2 || m.rollFrom != start {
		t.Fatalf("cut=%d from=%d, want %d and %d", m.liveCut, m.rollFrom, start+2, start)
	}
}

func TestRollingTransitionStops(t *testing.T) {
	m := rollingModel(t, 11)
	s := m.workSections()[0]
	first := len(m.rollingTail(s, 90))
	m.rollAt = time.Now().Add(-100 * time.Millisecond)
	if next := len(m.rollingTail(s, 90)); first <= next {
		t.Fatalf("not shrinking %d %d", first, next)
	}
	m.rollAt = time.Now().Add(-rollDuration)
	m.Update(rollMsg{m.runGen})
	if !m.rollAt.IsZero() || m.rollScheduled {
		t.Fatal("animation timer did not stop")
	}
}

func TestRollingTransitionPauses(t *testing.T) {
	m := rollingModel(t, 11)
	m.stickBottom = false
	m.Update(rollMsg{m.runGen})
	if m.rollPaused.IsZero() || m.rollScheduled {
		t.Fatal("scrolling did not pause the timer")
	}
	// Simulate a long inspection without sleeping: the elapsed animation
	// time must be preserved when following the bottom again.
	m.rollAt = time.Now().Add(-time.Second)
	m.rollPaused = m.rollAt.Add(50 * time.Millisecond)
	m.stickBottom = true
	m.refresh()
	if !m.rollPaused.IsZero() || time.Since(m.rollAt) >= rollDuration {
		t.Fatal("paused time consumed the animation")
	}
}

func TestRetryDialogAndPartialReset(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		m, _ := testModel(t)
		m.blocks = []*block{newBlock(blockUser, "hi"), groupTool("edit", "saved.go")}
		m.applyEvent(agent.TurnStart{Step: 2})
		m.applyEvent(agent.TextDelta{Text: "unfinished"})
		reply := make(chan agent.Answer, 1)
		m.Update(approvalMsg{retry: true, partial: true, reply: reply})
		if !strings.Contains(panelText(m), "retry restarts this response") || !strings.Contains(panelText(m), "Try again") {
			t.Fatal(panelText(m))
		}
		if cancel {
			typeKeys(m, "esc", "esc")
		} else {
			typeKeys(m, "enter")
		}
		want := agent.AllowOnce
		if cancel {
			want = agent.Deny
		}
		if got := <-reply; got != want || m.pending != nil {
			t.Fatal("wrong response", got)
		}
		m.applyEvent(agent.RequestRestart{Partial: true})
		if m.blocks[1].kind != blockTool || strings.Contains(groupText(m), "unfinished") {
			t.Fatal("completed work lost or partial retained")
		}
	}
}

func TestInvalidResponseDialog(t *testing.T) {
	for _, tc := range []struct {
		err   error
		title string
	}{
		{agent.ErrIncompleteResponse, "Incomplete response · 2 retries exhausted"},
		{agent.ErrMalformedToolResponse, "Malformed tool response · 2 retries exhausted"},
	} {
		m, _ := testModel(t)
		reply := make(chan agent.Answer, 1)
		m.Update(approvalMsg{retry: true, err: tc.err, reply: reply})
		if got := panelText(m); !strings.Contains(got, tc.title) || strings.Contains(got, "3 retries exhausted") {
			t.Fatal(got)
		}
		typeKeys(m, "esc", "esc")
		if got := <-reply; got != agent.Deny || m.pending != nil {
			t.Fatal("cancel failed")
		}
	}
}

func BenchmarkRollingFrames(b *testing.B) {
	m := rollingModel(b, 40)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.rollAt = time.Now().Add(-75 * time.Millisecond)
		m.refresh()
		m.View()
	}
}

func TestLiveHistoryUnifiesFailuresAndKeepsActiveRows(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = []*block{newBlock(blockUser, "Check terrain")}
	var tools []*block
	// Active/approval rows in the middle must survive folding, not merely
	// the usual case where the running call happens to be the final block.
	for i, status := range []string{"ok", "error", "running", "denied", "ok", "ok", "ok", "ok", "running"} {
		b := groupTool("bash", "")
		b.id, b.status = fmt.Sprint(i), status
		b.args = map[string]any{"command": fmt.Sprintf("command-%02d", i)}
		b.output = fmt.Sprintf("output-%02d", i)
		tools = append(tools, b)
		m.blocks = append(m.blocks, newBlock(blockReasoning, "thinking"), b)
		if i == 3 {
			m.blocks = append(m.blocks, newBlock(blockSystem, "Persistent warning"))
		}
	}
	m.pending = &approval{call: agent.ToolCall{ID: "4"}}
	m.running, m.stickBottom = true, true
	m.layout()
	m.rollAt = time.Time{}
	before := append([]*block(nil), m.blocks...)
	sections := m.workSections()
	if len(sections) != 1 || sections[0].label != "Earlier work · 4 actions · 1 failed · 1 denied" {
		t.Fatalf("wrong archive: %+v", sections)
	}
	got := groupText(m)
	for _, i := range []int{2, 4, 6, 7, 8} {
		if strings.Count(got, fmt.Sprintf("command-%02d", i)) != 1 {
			t.Fatalf("visible tool %d missing or duplicated:\n%s", i, got)
		}
	}
	if !strings.Contains(got, "Persistent warning") || strings.Count(got, "Earlier work") != 1 || strings.Contains(got, "command-01") {
		t.Fatal(got)
	}
	// Click through the same transcript handler as mouse input. A pending
	// modal would otherwise intercept clicks, so dismiss only the modal here.
	m.pending = nil
	clickWork(t, m, sections[0].head)
	groupClick(t, m, tools[1], false)
	if !strings.Contains(groupText(m), "output-01") || m.openDetail != tools[1] {
		t.Fatal("failed command details unavailable")
	}
	groupClick(t, m, tools[3], false)
	if m.openDetail != tools[3] || strings.Contains(groupText(m), "output-01") {
		t.Fatal("more than one detail open")
	}
	for _, width := range []int{24, 40, 80, 120} {
		for _, line := range m.renderBlocks(width) {
			if ansi.StringWidth(line) > width {
				t.Fatalf("overflow at width %d: %q", width, line)
			}
		}
	}
	for i, b := range before {
		if m.blocks[i] != b {
			t.Fatal("view mutated conversation order")
		}
	}
	for i, b := range tools {
		if b.output != fmt.Sprintf("output-%02d", i) {
			t.Fatal("view lost output")
		}
	}
}

func TestLiveHistoryFailurePositions(t *testing.T) {
	// Every old position, including both edges, must remain in one archive.
	for failure := 0; failure < 6; failure++ {
		m := rollingModel(t, 9)
		m.blocks[2+failure*2].status = "error"
		m.rollAt = time.Time{}
		if got := groupText(m); strings.Count(got, "Earlier work") != 1 || !strings.Contains(got, "6 actions · 1 failed") {
			t.Fatalf("failure at %d split archive: %s", failure, got)
		}
	}
}
