package tui

import (
	"math/rand"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

// TestInputRowsMatchTextarea guards the vendored wrap: for many random
// single lines the row count must equal what the textarea itself reports
// for the cursor's line.
func TestInputRowsMatchTextarea(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	alphabet := []rune("ab cde  fghij k lmnopqrstuvwxyz 日本語 ,.-/@")
	for i := 0; i < 400; i++ {
		width := 5 + rng.Intn(60)
		n := rng.Intn(200)
		var sb strings.Builder
		for j := 0; j < n; j++ {
			sb.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		line := sb.String()

		ta := textarea.New()
		ta.Prompt = ""
		ta.ShowLineNumbers = false
		ta.SetWidth(width)
		ta.SetValue(line)
		want := ta.LineInfo().Height
		if got := inputRows(line, ta.Width()); got != want {
			t.Fatalf("width %d line %q: inputRows=%d textarea=%d", ta.Width(), line, got, want)
		}
	}
}

func TestInputRowsCountsEveryLine(t *testing.T) {
	if got := inputRows("a\nb\nc", 40); got != 3 {
		t.Fatalf("three short lines = %d rows", got)
	}
	if got := inputRows("", 40); got != 1 {
		t.Fatalf("empty = %d rows", got)
	}
	// One long line wraps.
	long := strings.Repeat("word ", 30) // 150 cells
	if got := inputRows(long, 40); got < 4 {
		t.Fatalf("150 cells at width 40 = %d rows", got)
	}
}

// A long pasted line must grow the box so the whole text is visible, up to
// the cap; beyond it the status bar reports the real height.
func TestInputGrowsWithWrappedPaste(t *testing.T) {
	m, _ := testModel(t)
	if m.input.Height() != 1 {
		t.Fatalf("initial height %d", m.input.Height())
	}
	inner := m.input.Width()
	m.Update(tea.PasteMsg{Content: strings.Repeat("x ", inner)}) // ~2 rows
	if m.input.Height() != 3 && m.input.Height() != 2 {
		t.Fatalf("after a two-row paste height = %d", m.input.Height())
	}
	if strings.Contains(m.statusBar(), "input") {
		t.Fatal("status must not mention the input while it fits")
	}

	m.Update(tea.PasteMsg{Content: strings.Repeat("\nline", 12)})
	if m.input.Height() != inputMaxRows {
		t.Fatalf("height capped at %d, got %d", inputMaxRows, m.input.Height())
	}
	if !strings.Contains(m.statusBar(), "input 1") { // "input 14 lines" or similar
		t.Fatalf("status must report the hidden input rows: %q", m.statusBar())
	}
	// The transcript window shrank to make room.
	if m.vp.Height() != m.height-(inputMaxRows+2)-footerRows {
		t.Fatalf("viewport height %d with input %d", m.vp.Height(), m.input.Height())
	}

	typeKeys(m, "ctrl+c")
	if m.input.Height() != inputMaxRows || m.input.Value() == "" {
		t.Fatalf("ctrl+c must preserve the draft: h=%d v=%q", m.input.Height(), m.input.Value())
	}
}

func TestScrollKeysAndBelowIndicator(t *testing.T) {
	m, _ := testModel(t)
	for i := 0; i < 60; i++ {
		m.blocks = append(m.blocks, newBlock(blockSystem, "line"))
	}
	m.refresh()
	if strings.Contains(m.statusBar(), "below") {
		t.Fatal("pinned to the bottom: nothing is below")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModCtrl})
	box, _, _ := m.scrollBox()
	if m.stickBottom || !strings.Contains(box, "↓ 1 line below") || strings.Contains(m.statusBar(), "below") {
		t.Fatalf("ctrl+up: stick=%v status=%q", m.stickBottom, m.statusBar())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	if m.vp.YOffset() != 0 {
		t.Fatalf("ctrl+home offset = %d", m.vp.YOffset())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl})
	if m.vp.YOffset() != 1 {
		t.Fatalf("ctrl+down offset = %d", m.vp.YOffset())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	box, _, _ = m.scrollBox()
	if !m.stickBottom || !m.vp.AtBottom() || box != "" {
		t.Fatalf("ctrl+end: stick=%v bottom=%v status=%q", m.stickBottom, m.vp.AtBottom(), m.statusBar())
	}
}

// Typing past the box's current height must not leave a stale scroll offset:
// with the plain textarea the box grew to three rows but kept showing rows
// two and three plus an empty row, hiding the start of the text.
func TestInputGrowthKeepsFirstRowVisible(t *testing.T) {
	m, _ := testModel(t)
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	words := 2*m.input.Width()/5 + 10 // a little over two rows
	line := strings.Repeat("word ", words)
	for _, r := range line {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if m.input.Height() != 3 {
		t.Fatalf("height %d, want 3 (rows=%d)", m.input.Height(), m.inputRows)
	}
	if off := m.input.ScrollYOffset(); off != 0 {
		t.Fatalf("scroll offset %d after growing, want 0", off)
	}
	if v := m.input.View(); strings.Count(v, "word") != words {
		t.Fatalf("view lost text:\n%s", v)
	}
}
