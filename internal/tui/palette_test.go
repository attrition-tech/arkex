package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
		Cwd: t.TempDir(),
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
	if m.pal.level().title != "Commands" || len(m.pal.view) != 7 {
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
	typeKeys(m, "/")
	for _, it := range m.pal.view {
		if it.group == "Mode" || strings.HasPrefix(strings.ToLower(it.title), "switch to plan") || strings.HasPrefix(strings.ToLower(it.title), "switch to build") {
			t.Fatalf("deleted mode command still present: %+v", it)
		}
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
	if m.pal == nil || len(m.pal.levels) != 1 || m.pal.input.Value() != "switch model" {
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
	if !strings.Contains(box, "Commands") || !strings.Contains(box, "Session") || strings.Contains(box, "Suggested") || !strings.Contains(box, "esc close") {
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
	// Empty search results must not wrap past a short terminal's bottom edge.
	m.pal.input.SetValue("no-such-command")
	m.pal.filter()
	for _, width := range []int{24, 40} {
		box, x, y, _ := m.paletteBox(width, 8)
		lines := strings.Split(box, "\n")
		if box == "" || y+len(lines) > 8 {
			t.Fatalf("empty palette exceeds %dx8:\n%s", width, box)
		}
		for _, line := range lines {
			if x+ansi.StringWidth(line) > width {
				t.Fatalf("empty palette exceeds width %d: %q", width, line)
			}
		}
	}
}

func TestPaletteCatalogAvailabilityAndBackNavigation(t *testing.T) {
	m, _ := testModel(t)
	m.setInput("keep this unsent draft")
	typeKeys(m, "ctrl+p")
	seen := map[string]bool{}
	for _, item := range m.pal.level().items {
		if seen[item.title] {
			t.Fatalf("duplicate action %q", item.title)
		}
		seen[item.title] = true
	}
	if seen["Copy selection"] || seen["Copy entire prompt"] {
		t.Fatal("irrelevant copy actions remain")
	}
	for _, command := range commands {
		m.pal.input.SetValue(command.name)
		m.pal.filter()
		if len(m.pal.view) == 0 || !strings.Contains(m.pal.view[0].hint+" "+m.pal.view[0].aliases, command.name) {
			t.Fatalf("command %s is missing or loses to a fuzzy match: %s", command.name, titles(m.pal.view))
		}
	}
	// Queue is distinct from queuing the draft; exact aliases win.
	m.pal.input.SetValue("/queue")
	m.pal.filter()
	if m.pal.view[0].title != "Pending messages" {
		t.Fatal("/queue selects the draft action")
	}
	// An idle-opened menu must recheck availability after a run starts.
	m.pal.input.SetValue("/model")
	m.pal.filter()
	m.running = true
	typeKeys(m, "enter")
	box, _, _, _ := m.paletteBox(100, 30)
	if len(m.pal.levels) != 1 || !strings.Contains(ansi.Strip(box), "Finish or stop the current run first") {
		t.Fatal("unavailable action ran or failed to explain why")
	}
	m.running = false
	typeKeys(m, "enter", "esc")
	if m.pal.input.Value() != "/model" || m.input.Value() != "keep this unsent draft" {
		t.Fatal("back lost query or draft")
	}
	// Restore a nonzero selection too, across a deeper breadcrumb.
	m.pal.input.SetValue("s")
	m.pal.filter()
	for i, item := range m.pal.view {
		if item.title == "Settings" {
			m.pal.sel = i
			break
		}
	}
	sel := m.pal.sel
	typeKeys(m, "enter")
	box, _, _, _ = m.paletteBox(100, 30)
	if !strings.Contains(ansi.Strip(box), "Commands › Settings") {
		t.Fatal("missing breadcrumb")
	}
	typeKeys(m, "esc")
	if m.pal.sel != sel || m.pal.input.Value() != "s" {
		t.Fatal("back lost selected result")
	}
}
