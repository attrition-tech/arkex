package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/agent"
)

func TestLoaderUsesTwoPlainDotCells(t *testing.T) {
	seen := map[string]bool{}
	for f := 0; f < loaderFrames; f++ {
		fr := loader(f)
		if ansi.Strip(fr) != fr || ansi.StringWidth(fr) != 2 {
			t.Fatalf("frame %d text = %q", f, ansi.Strip(fr))
		}
		seen[fr] = true
	}
	if len(seen) != loaderFrames {
		t.Fatalf("distinct frames = %d", len(seen))
	}
	if loader(3) != loader(3+loaderFrames) {
		t.Fatal("animation must be periodic")
	}
}

func TestWorkingStripFollowsTheRun(t *testing.T) {
	m, _ := testModel(t)
	m.setSession(Connection{Agent: &agent.Agent{}})
	m.layout()

	// Idle: no strip.
	if strings.Contains(ansi.Strip(strings.Join(m.renderBlocks(100), "\n")), loader(m.frame)) {
		t.Fatal("strip shown while idle")
	}

	cmd := m.startRun(func(ctx context.Context, ag *agent.Agent, emit func(agent.Event)) error { return nil })
	if cmd == nil || !m.running || m.runGen != 1 {
		t.Fatalf("startRun: cmd=%v running=%v gen=%d", cmd != nil, m.running, m.runGen)
	}
	plain := ansi.Strip(strings.Join(m.renderBlocks(100), "\n"))
	if !strings.HasSuffix(plain, loader(m.frame)+"  thinking…") {
		t.Fatalf("strip while waiting for the model:\n%s", plain)
	}

	events(m,
		agent.TurnStart{Step: 2},
		agent.ToolCall{ID: "c1", Name: "edit", Input: `{"path":"src/app.go"}`},
	)
	m.startedAt = time.Now().Add(-12 * time.Second)
	plain = ansi.Strip(strings.Join(m.renderBlocks(100), "\n"))
	last := plain[strings.LastIndex(plain, "\n")+1:]
	if !strings.HasPrefix(last, "  "+loader(m.frame)+" edit src/app.go") || !strings.Contains(last, "12s") || strings.Count(plain, "src/app.go") != 1 || strings.Count(plain, loader(m.frame)) != 1 {
		t.Fatalf("strip for a running edit = %q", last)
	}
	events(m, agent.ToolResult{ID: "c1", Name: "edit"}, agent.TextDelta{Text: "Done"})
	if m.activity() != "writing…" {
		t.Fatalf("activity while streaming = %q", m.activity())
	}

	// Ticks advance the frame and re-arm only for the live run.
	f := m.frame
	if next := m.workingFrame(workingMsg{gen: m.runGen}); next == nil || m.frame != f+1 {
		t.Fatalf("tick: rearmed=%v frame=%d", next != nil, m.frame)
	}
	if next := m.workingFrame(workingMsg{gen: m.runGen - 1}); next != nil || m.frame != f+1 {
		t.Fatal("stale tick must be dropped")
	}
	m.Update(runDoneMsg{})
	if next := m.workingFrame(workingMsg{gen: m.runGen}); next != nil {
		t.Fatal("tick after the run ended must not re-arm")
	}
	plain = ansi.Strip(strings.Join(m.renderBlocks(100), "\n"))
	if strings.Contains(plain, loader(m.frame)) || m.step != 0 {
		t.Fatalf("strip must vanish when the run ends:\n%s", plain)
	}
}

func TestLatestThinkingAndArchive(t *testing.T) {
	m := rollingModel(t, 5)
	m.rollAt = time.Time{}
	for i, b := range m.blocks {
		if b.kind == blockReasoning {
			b.name = fmt.Sprintf("Thought %d", i)
			b.lines = nil
		}
	}
	m.refresh()
	got := transcript(m)
	if !strings.Contains(got, "Earlier thinking · 4 sections") || strings.Count(got, "Thought ") != 1 || !strings.Contains(got, "Thought 9") {
		t.Fatal(got)
	}
	latest := m.blocks[9]
	m.toggleDetail(latest)
	m.blocks = append(m.blocks, newBlock(blockReasoning, "new reasoning"))
	m.refresh()
	if m.openDetail != latest || !strings.Contains(transcript(m), "thinking") || strings.Contains(transcript(m), "new reasoning") {
		t.Fatal("inspected reasoning was replaced")
	}
	m.toggleDetail(latest)
	// Open the archive, then an older section; it must stay in the archive.
	for _, s := range m.spans {
		if s.workHeader && s.b == m.blocks[0] {
			m.transcriptClick(s.top)
			break
		}
	}
	if m.openWork != m.blocks[0] || !strings.Contains(transcript(m), "Thought 1") {
		t.Fatal("archive is not accessible", transcript(m))
	}
	for _, s := range m.spans {
		if s.b == m.blocks[1] && !s.workHeader {
			m.transcriptClick(s.top)
			break
		}
	}
	if m.openDetail != m.blocks[1] || m.openWork != m.blocks[0] {
		t.Fatal("archive inspection moved")
	}
}

func TestDotsAnimateWithoutRedrawingBannerOrInput(t *testing.T) {
	m, _ := testModel(t)
	m.o.Cwd = "/workspace/nsutm"
	m.setInput("draft\nsecond line")
	idleInput := m.inputView()
	idleBanner := strings.Join(m.blockLines(m.welcome, 98, false), "\n")
	m.running, m.runGen = true, 1
	for f := 0; f < loaderFrames; f++ {
		m.frame = f
		m.refresh()
		view := ansi.Strip(m.View().Content)
		if strings.Contains(view, "›arkex") || strings.Contains(view, "▄▀█") {
			t.Fatal("legacy wordmark or ASCII banner remains in the session")
		}
		input := m.inputView()
		mark := strings.Join(m.blockLines(m.welcome, 98, false), "\n")
		if input != idleInput || mark != idleBanner {
			t.Fatalf("animation changed text or geometry at frame %d", f)
		}
		if got := m.View().WindowTitle; got != loader(f)+" New session - nsutm - arkex" {
			t.Fatalf("title not synchronized with dot frame: %q", got)
		}
		frameLines(t, m, "animated draft")
	}
	m.running = false
	m.refresh()
	if m.View().WindowTitle != "New session - nsutm - arkex" {
		t.Fatal("title loader must disappear when idle")
	}
	if m.inputView() != idleInput || strings.Join(m.blockLines(m.welcome, 98, false), "\n") != idleBanner {
		t.Fatal("idle styles were not restored")
	}
	if cmd := m.workingFrame(workingMsg{gen: 1}); cmd != nil {
		t.Fatal("animation must not re-arm while idle")
	}
}
