package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/dantearo/arkex/internal/agent"
)

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "space", " ":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "shift+enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	}
	if rest, ok := strings.CutPrefix(s, "ctrl+"); ok && len([]rune(rest)) == 1 {
		return tea.KeyPressMsg{Code: []rune(rest)[0], Mod: tea.ModCtrl}
	}
	r := []rune(s)
	if len(r) != 1 {
		panic("key: unknown key name " + s)
	}
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

// testModel returns a chat-ready model: no agent is configured, so the
// Models panel that opens on startup is closed first. Connect records the
// selection instead of dialing anything.
func testModel(t testing.TB) (*model, *string) {
	sel := new(string)
	m := newModel(Options{
		Cwd:  t.TempDir(),
		Mode: agent.NewModePolicy(agent.ModeBuild, nil),
		Connect: func(_ context.Context, s string) (Connection, error) {
			*sel = s
			return Connection{}, errors.New("test")
		},
	})
	m.width, m.height = 100, 30
	m.closeModels()
	m.ready = true
	return m, sel
}

// typeKeys sends key presses through Update, the same path the program uses,
// so panel/approval routing is exercised.
func typeKeys(m *model, keys ...string) {
	for _, k := range keys {
		m.Update(key(k))
	}
}

func titles(items []paletteItem) string {
	var out []string
	for _, it := range items {
		out = append(out, it.title)
	}
	return strings.Join(out, "|")
}

func TestPaletteOpensOnSlashOnlyWhenEmpty(t *testing.T) {
	m, _ := testModel(t)

	typeKeys(m, "a", "/")
	if m.pal != nil || m.input.Value() != "a/" {
		t.Fatalf("slash mid-text must be inserted, got pal=%v value=%q", m.pal != nil, m.input.Value())
	}
	m.input.Reset()
	typeKeys(m, "/")
	if m.pal == nil || m.input.Value() != "" {
		t.Fatalf("slash on empty input must open the palette without inserting it")
	}
	if m.pal.level().title != "Commands" || len(m.pal.view) < 8 {
		t.Fatalf("top level = %q with %d rows", m.pal.level().title, len(m.pal.view))
	}
	typeKeys(m, "esc")
	if m.pal != nil {
		t.Fatal("esc must close the palette")
	}
	typeKeys(m, "ctrl+p")
	if m.pal == nil {
		t.Fatal("ctrl+p must open the palette")
	}
	typeKeys(m, "ctrl+p")
	if m.pal != nil {
		t.Fatal("ctrl+p again must close it")
	}
}

func TestPaletteFilterAndRun(t *testing.T) {
	m, sel := testModel(t)

	typeKeys(m, "/", "p", "l", "a", "n")
	if len(m.pal.view) == 0 || !strings.HasPrefix(m.pal.view[0].title, "Plan") {
		t.Fatalf("filter 'plan' should rank the Plan mode first, got %q", titles(m.pal.view))
	}
	typeKeys(m, "enter")
	if m.pal != nil || m.mode() != agent.ModePlan {
		t.Fatalf("enter must run the row and close: pal=%v mode=%s", m.pal != nil, m.mode())
	}

	// Unfiltered rows keep registration order and the current mode is marked.
	typeKeys(m, "/")
	var current string
	for _, it := range m.pal.view {
		if it.group == "Mode" && it.hint == "current" {
			current = it.title
		}
	}
	if !strings.HasPrefix(current, "Plan") {
		t.Fatalf("current mode row = %q", current)
	}

	// "Switch model" opens a nested list; esc backs out to the top level.
	typeKeys(m, "s", "w", "i", "t", "c", "h", " ", "m", "o", "d", "e", "l", "enter")
	if m.pal == nil || len(m.pal.levels) != 2 || m.pal.level().title != "Switch model" {
		t.Fatalf("expected the model sub-list, got %+v", m.pal)
	}
	if m.pal.input.Value() != "" {
		t.Fatal("search text must reset when entering a sub-list")
	}
	typeKeys(m, "esc")
	if m.pal == nil || len(m.pal.levels) != 1 {
		t.Fatal("esc in a sub-list must go back, not close")
	}

	// No match + enter runs the text as a typed command.
	typeKeys(m, "esc", "/", "m", "o", "d", "e", "l", " ", "l", "o", "c", "a", "l", "/", "x")
	if len(m.pal.view) != 0 {
		t.Fatalf("expected no rows for a raw command, got %q", titles(m.pal.view))
	}
	typeKeys(m, "enter")
	if m.pal != nil || !strings.HasPrefix(m.status, "connecting to local/x") {
		t.Fatalf("fallback must run /model local/x; status=%q", m.status)
	}
	if _, run := m.command("/model local/x"); run != nil {
		run() // the Connect stub records the selection
	}
	if *sel != "local/x" {
		t.Fatalf("connect selection = %q", *sel)
	}
}

func TestPaletteRendersCentered(t *testing.T) {
	m, _ := testModel(t)
	typeKeys(m, "/")
	box, x, y, cur := m.paletteBox(100, 30)
	if box == "" || x < 10 || y < 1 || cur == nil {
		t.Fatalf("box empty or not centered: x=%d y=%d cursor=%v", x, y, cur)
	}
	if !strings.Contains(box, "Commands") || !strings.Contains(box, "Suggested") || !strings.Contains(box, "/mode") {
		t.Fatalf("box missing title, group or hint:\n%s", box)
	}
	out := overlay(strings.Repeat(strings.Repeat("x", 100)+"\n", 29)+strings.Repeat("x", 100), box, x, y)
	lines := strings.Split(out, "\n")
	if len(lines) != 30 {
		t.Fatalf("overlay must keep the area height, got %d lines", len(lines))
	}
	if !strings.HasPrefix(lines[y], strings.Repeat("x", x)) || !strings.Contains(lines[y+1], "Commands") {
		t.Fatalf("box not placed at %d,%d:\n%s", x, y, out)
	}
}
