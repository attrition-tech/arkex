package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/agent"
)

func transcript(m *model) string {
	return ansi.Strip(m.vp.View())
}

func events(m *model, es ...agent.Event) {
	m.Update(eventsMsg{events: es})
}

func TestToolCardLifecycle(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	m.running = true

	events(m,
		agent.TurnStart{Step: 1},
		agent.TextDelta{Text: "Looking."},
		agent.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"go test ./..."}`},
	)
	if m.step != 1 || m.activity() != "" {
		t.Fatalf("step=%d activity=%q", m.step, m.activity())
	}
	tr := transcript(m)
	if !strings.Contains(tr, "go test ./...") || !strings.Contains(tr, "bash") {
		t.Fatalf("running card missing command:\n%s", tr)
	}
	card := m.tool("c1")
	if card == nil || card.status != "running" {
		t.Fatalf("card = %+v", card)
	}

	// Text after a tool call must start a new assistant block, not be
	// appended to the one before the card.
	events(m, agent.TextDelta{Text: "Then more."})
	if n := len(m.blocks); m.blocks[n-1].kind != blockAssistant || m.blocks[n-1].text.String() != "Then more." || m.blocks[1].text.String() != "Looking." {
		t.Fatalf("text ordering broken: %q / %q", m.blocks[1].text.String(), m.blocks[n-1].text.String())
	}

	events(m, agent.ToolResult{ID: "c1", Name: "bash", Output: "ok\tpkg\n", Summary: "go test ./...", Duration: 1500 * time.Millisecond})
	if card.status != "ok" || card.dur != 1500*time.Millisecond || card.summary != "go test ./..." {
		t.Fatalf("card after result = %+v", card)
	}
	tr = transcript(m)
	if !strings.Contains(tr, "✓") || !strings.Contains(tr, "1.5s") {
		t.Fatalf("ok card not drawn with check and duration:\n%s", tr)
	}

	// Denied: decision first, then the result must not flip it to ok/error.
	events(m,
		agent.ToolCall{ID: "c2", Name: "edit", Input: `{"path":"a.go"}`},
		agent.ToolDecision{ID: "c2", Name: "edit", Allowed: false, Reason: "denied by config"},
		agent.ToolResult{ID: "c2", Name: "edit", Output: "denied", IsError: true},
	)
	if c := m.tool("c2"); c.status != "denied" || c.summary != "denied by config" {
		t.Fatalf("denied card = %+v", c)
	}
	// Error result.
	events(m,
		agent.ToolCall{ID: "c3", Name: "read", Input: `{"path":"missing"}`},
		agent.ToolResult{ID: "c3", Name: "read", Output: "open missing: no such file", IsError: true},
	)
	if c := m.tool("c3"); c.status != "error" {
		t.Fatalf("error card = %+v", c)
	}
	if !strings.Contains(transcript(m), "✗") {
		t.Fatalf("error card has no cross:\n%s", transcript(m))
	}

	// Usage accumulates across turns; lastInput is the latest request only.
	events(m,
		agent.TurnEnd{Usage: fantasy.Usage{InputTokens: 100, OutputTokens: 10}},
		agent.TurnEnd{Usage: fantasy.Usage{InputTokens: 150, OutputTokens: 5}},
	)
	if m.usageIn != 250 || m.usageOut != 15 || m.lastInput != 150 {
		t.Fatalf("usage = %d/%d last=%d", m.usageIn, m.usageOut, m.lastInput)
	}

	// Run end: error is surfaced as a system line, status resets.
	m.Update(runDoneMsg{err: errors.New("model exploded")})
	if m.running || m.status != "" {
		t.Fatalf("running=%v status=%q after done", m.running, m.status)
	}
	if !strings.Contains(transcript(m), "error: model exploded") {
		t.Fatalf("run error not shown:\n%s", transcript(m))
	}
	// Cancellation is reported as such, not as an error.
	m.running = true
	m.Update(runDoneMsg{err: context.Canceled})
	tr = transcript(m)
	if !strings.Contains(tr, "cancelled") || strings.Contains(tr, "error: context canceled") {
		t.Fatalf("cancel not reported:\n%s", tr)
	}
}

func TestRetryPreviewScrolling(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	reply := make(chan bool, 1)
	m.Update(approvalMsg{retry: true, reply: reply})
	a := m.pending
	a.preview = strings.Repeat("line\n", 30) + "last"
	a.caption = "Response detail"
	m.layout()
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if a.offset != a.page || len(reply) != 0 {
		t.Fatalf("page down offset=%d page=%d replies=%d", a.offset, a.page, len(reply))
	}
	m.wheel(-1)
	if a.offset != max(0, a.page-wheelLines) {
		t.Fatal("wheel did not scroll preview")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	if !strings.Contains(ansi.Strip(m.View().Content), "last") {
		t.Fatal("last preview line inaccessible")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	if a.offset != 0 {
		t.Fatal("home did not restore start")
	}
	frameLines(t, m, "retry preview")
}

// frameLines splits a View into lines and checks the frame is exactly the
// terminal size: every line fits and there are exactly height of them.
func frameLines(t *testing.T, m *model, what string) []string {
	t.Helper()
	lines := strings.Split(m.View().Content, "\n")
	if len(lines) != m.height {
		t.Fatalf("%s: frame has %d lines, want %d:\n%s", what, len(lines), m.height, m.View().Content)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > m.width {
			t.Fatalf("%s: line %d is %d wide (max %d): %q", what, i, w, m.width, ansi.Strip(l))
		}
	}
	return lines
}

func TestViewFrameGeometryAndCursor(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	for i := 0; i < 40; i++ {
		m.blocks = append(m.blocks, newBlock(blockAssistant, strings.Repeat("word ", 40)))
	}
	m.refresh()

	lines := frameLines(t, m, "default")
	v := m.View()
	// 30 rows: 25 transcript, top border, input text, bottom border,
	// toolbar, hints. The cursor sits after the border and padding.
	if v.Cursor == nil || v.Cursor.Y != 26 || v.Cursor.X != 2 {
		t.Fatalf("cursor = %+v", v.Cursor)
	}
	if !strings.Contains(ansi.Strip(lines[28]), "no model ▾") || strings.Contains(ansi.Strip(lines[28]), "BUILD") {
		t.Fatalf("toolbar not on row 28: %q", ansi.Strip(lines[28]))
	}
	if !strings.HasSuffix(ansi.Strip(lines[29]), "/ or cmd+p for command palette") {
		t.Fatalf("hints not on row 29: %q", ansi.Strip(lines[29]))
	}
	if !v.AltScreen {
		t.Fatal("alt screen off")
	}

	// Two input rows: the transcript shrinks by one and the cursor follows.
	typeKeys(m, "a", "shift+enter", "b")
	frameLines(t, m, "two-line input")
	if v := m.View(); v.Cursor == nil || v.Cursor.Y != 26 || v.Cursor.X != 3 {
		t.Fatalf("two-line cursor = %+v", v.Cursor)
	}
	if m.vp.Height() != 24 {
		t.Fatalf("viewport height = %d", m.vp.Height())
	}
	typeKeys(m, "ctrl+u", "backspace", "ctrl+u")

	// Palette overlay stays inside the frame and owns the cursor.
	typeKeys(m, "ctrl+p")
	lines = frameLines(t, m, "palette")
	if m.pal == nil || !strings.Contains(ansi.Strip(strings.Join(lines, "\n")), "/connections") {
		t.Fatal("palette not drawn")
	}
	if v := m.View(); v.Cursor == nil || v.Cursor.Y >= 26 {
		t.Fatalf("palette cursor = %+v", v.Cursor)
	}
	typeKeys(m, "esc")

	// Slash popup at the bottom of the transcript.
	typeKeys(m, "/", "t", "h")
	lines = frameLines(t, m, "popup")
	if !strings.Contains(ansi.Strip(strings.Join(lines[:26], "\n")), "/theme") {
		t.Fatal("slash popup not drawn above the bar")
	}
	typeKeys(m, "esc", "ctrl+u")

	// Connections panel overlays the transcript; input box is one line.
	m.openModels("")
	lines = frameLines(t, m, "panel")
	if !strings.Contains(ansi.Strip(strings.Join(lines, "\n")), "Connections") {
		t.Fatalf("panel not drawn:\n%s", ansi.Strip(strings.Join(lines, "\n")))
	}
	m.closeModels()

	// Narrow terminal: still exactly sized.
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	frameLines(t, m, "narrow")
}

func TestSlashCommandsHelpNewAndCompact(t *testing.T) {
	m, _ := testModel(t)
	m.o.Connect = nil
	m.layout()
	m.blocks = append(m.blocks, newBlock(blockUser, "old question"))
	m.refresh()

	m.command("/help")
	if !strings.Contains(transcript(m), "/models") {
		t.Fatalf("help missing:\n%s", transcript(m))
	}
	m.command("/new")
	if strings.Contains(transcript(m), "old question") || len(m.blocks) != 1 {
		t.Fatalf("/new kept the conversation: %d blocks", len(m.blocks))
	}
	m.command("/nope")
	if !strings.Contains(transcript(m), "unknown command") {
		t.Fatalf("unknown command not reported:\n%s", transcript(m))
	}
}

// TestUntrustedTextCannotDriveTheTerminal feeds escape sequences through
// every ingestion point (model text, reasoning, tool output, tool title,
// denial reason, system notes) and checks the rendered frame contains no
// escape other than the styling we emit ourselves.
func TestUntrustedTextCannotDriveTheTerminal(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	m.running = true
	osc52 := "\x1b]52;c;SGVsbG8=\x07"
	events(m,
		agent.TextDelta{Text: "hello " + osc52 + "world\x1b[2J!"},
		agent.ReasoningDelta{Text: "thinking" + osc52},
		agent.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"echo \u001b]52;c;SGVsbG8=\u0007hi"}`},
		agent.ToolDecision{ID: "c1", Name: "bash", Allowed: false, Reason: "nope" + osc52},
		agent.ToolResult{ID: "c1", Name: "bash", Output: "out" + osc52 + "\x1b[Hput", IsError: true},
	)
	m.appendSystem("error: " + osc52 + "boom")
	var tr string
	for _, b := range m.blocks {
		m.openDetail = b
		m.refresh()
		tr += transcript(m)
		frame := m.View().Content
		for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\x1b[H"} {
			if strings.Contains(frame, bad) {
				t.Fatalf("frame contains %q:\n%q", bad, frame)
			}
		}
	}
	for _, want := range []string{"hello world!", "$ echo hi", "nope", "boom"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("visible text %q missing from:\n%s", want, tr)
		}
	}
}
