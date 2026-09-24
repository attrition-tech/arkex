package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestSpanAtBoundaries(t *testing.T) {
	m := &model{spans: []span{
		{top: 1, bottom: 3, b: &block{kind: blockUser}},
		{top: 4, bottom: 4}, // invisible content must not capture a click
		{top: 4, bottom: 5, b: &block{kind: blockTool}, header: true},
		{top: 5, bottom: 9, b: &block{kind: blockTool}},
		{top: 11, bottom: 12, b: &block{kind: blockReasoning}, workHeader: true},
	}}
	for line := -1; line <= 13; line++ {
		var want span
		found := false
		for _, s := range m.spans {
			if s.top <= line && line < s.bottom {
				want, found = s, true
				break
			}
		}
		if got, ok := m.spanAt(line); ok != found || got != want {
			t.Fatalf("line %d: got %+v %v, want %+v %v", line, got, ok, want, found)
		}
	}
	m.spans = nil
	if _, ok := m.spanAt(0); ok {
		t.Fatal("empty transcript captured a click")
	}
}

func BenchmarkSpanAt(b *testing.B) {
	m := &model{spans: make([]span, 10000)}
	for i := range m.spans {
		m.spans[i] = span{top: i * 3, bottom: i*3 + 2}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.spanAt(i % 30000)
	}
}

func wheel(m *model, dir int) {
	b := tea.MouseWheelUp
	if dir > 0 {
		b = tea.MouseWheelDown
	}
	m.handleMouse(tea.MouseWheelMsg{X: 1, Y: 1, Button: b})
}

// hasLine reports whether the stripped view has a line whose trimmed text
// equals want.
func hasLine(view, want string) bool {
	for _, l := range strings.Split(ansi.Strip(view), "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

// click is a press and release at the same cell, like a real click.
func click(m *model, x, y int) {
	m.handleMouse(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.handleMouse(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
}

func TestWheelScrollsTranscript(t *testing.T) {
	m, _ := testModel(t)
	for i := 0; i < 40; i++ {
		m.blocks = append(m.blocks, newBlock(blockSystem, "line"))
	}
	m.refresh()
	if !m.vp.AtBottom() || !m.stickBottom {
		t.Fatal("transcript should start pinned to the bottom")
	}
	off := m.vp.YOffset()
	wheel(m, -1)
	if m.vp.YOffset() != off-1 || m.stickBottom {
		t.Fatalf("wheel up: offset %d→%d stick=%v", off, m.vp.YOffset(), m.stickBottom)
	}
	wheel(m, 1)
	if m.vp.YOffset() != off || !m.stickBottom {
		t.Fatalf("wheel down back to bottom: offset=%d stick=%v", m.vp.YOffset(), m.stickBottom)
	}

	// Mouse events must be ignored entirely once /mouse turns reporting off
	// in the view; the model still receives none, but the mode must change.
	if m.mouseMode() != tea.MouseModeAllMotion {
		t.Fatal("mouse should be on by default")
	}
	m.command("/mouse")
	if m.mouseMode() != tea.MouseModeNone {
		t.Fatal("/mouse should turn reporting off")
	}
}

func TestClickTogglesToolCard(t *testing.T) {
	m, _ := testModel(t)
	tool := newBlock(blockTool, "")
	tool.name, tool.status = "bash", "ok"
	tool.args = map[string]any{"command": "seq 9"}
	tool.output = "1\n2\n3\n4\n5\n6\n7\n8\n9\n"
	m.blocks = append(m.blocks, newBlock(blockAssistant, "hello"), tool)
	m.refresh()

	var sp span
	for _, s := range m.spans {
		if s.b == tool {
			sp = s
		}
	}
	if sp.b == nil || sp.bottom-sp.top != 1 { // collapsed cards are exactly one line
		t.Fatalf("tool span = %+v (want a single header line)", sp)
	}
	if hasLine(m.vp.View(), "1") || hasLine(m.vp.View(), "9") {
		t.Fatal("collapsed card must show no output")
	}

	// Click on the card header (transcript is pinned to the bottom, so
	// subtract the viewport offset).
	click(m, 2, sp.top-m.vp.YOffset())
	if m.openDetail != tool {
		t.Fatal("click must flip the card")
	}
	if !hasLine(m.vp.View(), "1") || !hasLine(m.vp.View(), "9") {
		t.Fatalf("expanded card must show every line:\n%s", m.vp.View())
	}
	// A click on the assistant text does nothing.
	click(m, 2, m.spans[1].top-m.vp.YOffset())
	if m.openDetail != tool {
		t.Fatal("clicking another block must not touch the card")
	}
	// Clicking the open card closes it again.
	m.toggleDetail(tool)
	if hasLine(m.vp.View(), "1") {
		t.Fatal("second click must collapse the card")
	}
}

func TestDetailsAreMouseOnlyAndSingleOpen(t *testing.T) {
	m, _ := testModel(t)
	first := &block{kind: blockTool, name: "bash", status: "ok", output: "first output\nsecond line"}
	second := &block{kind: blockTool, name: "read", status: "ok", output: "other output"}
	thinking := newBlock(blockReasoning, "private reasoning detail")
	summary := compactBlock("summary detail", 4)
	m.blocks = []*block{first, second, thinking, summary}
	m.refresh()
	for _, target := range []*block{first, second, thinking, summary, first, first} {
		previous := m.openDetail
		for _, s := range m.spans {
			if s.b == target {
				click(m, 2, s.top-m.vp.YOffset())
				break
			}
		}
		want := target
		if previous == target {
			want = nil
		}
		for _, b := range m.blocks {
			if b.key.expanded != (b == want) {
				t.Fatalf("wrong expansion after clicking %v: block=%v open=%v", target.kind, b.kind, b.key.expanded)
			}
		}
		for _, key := range []string{"ctrl+o", "ctrl+r"} {
			typeKeys(m, key)
			if m.openDetail != want {
				t.Fatalf("removed shortcut %s changed details", key)
			}
		}
	}
	for _, item := range m.paletteItems() {
		if item.cmd == "/tools" || item.cmd == "/thinking" {
			t.Fatal("global detail action remains in palette")
		}
	}
	for _, removed := range []string{"ctrl+o", "ctrl+r", "/tools", "/thinking"} {
		if strings.Contains(helpText(), removed) {
			t.Fatalf("help still advertises %s", removed)
		}
	}
	m.toggleDetail(thinking)
	typeKeys(m, "ctrl+l")
	if m.openDetail != nil {
		t.Fatal("cleared transcript retained open detail")
	}
}

func TestPaletteClicks(t *testing.T) {
	m, _ := testModel(t)
	typeKeys(m, "/")
	m.paletteBox(m.width, m.vp.Height()) // records geometry like View does
	g := m.pal.box
	if g.w == 0 || len(g.rows) == 0 {
		t.Fatalf("geometry not recorded: %+v", g)
	}
	// First row is the "Suggested" header; the second is an item.
	if g.rows[0] != -1 || g.rows[1] < 0 {
		t.Fatalf("rows = %v", g.rows)
	}
	want := m.pal.view[g.rows[2]] // "Switch to plan mode"
	if !strings.HasPrefix(want.title, "Switch to plan") {
		t.Fatalf("unexpected third row %+v", want)
	}
	click(m, g.x+3, g.y+1+paletteHeaderRows+2)
	if m.pal != nil || string(m.mode()) != "plan" {
		t.Fatalf("click on a row must run it: pal=%v mode=%s", m.pal != nil, m.mode())
	}

	typeKeys(m, "/")
	m.paletteBox(m.width, m.vp.Height())
	click(m, 0, 0)
	if m.pal != nil {
		t.Fatal("click outside the box must close the palette")
	}
	typeKeys(m, "/")
	m.paletteBox(m.width, m.vp.Height())
	g = m.pal.box
	click(m, g.x+3, g.y+1+paletteHeaderRows) // header row: nothing happens
	if m.pal == nil {
		t.Fatal("click on a group header must keep the palette open")
	}
}
