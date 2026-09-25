package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/provider"
)

func TestIdleHintRightAligned(t *testing.T) {
	m, _ := testModel(t)
	const hint = "/ or cmd+p for command palette"
	for _, width := range []int{120, 60, 40, 20} {
		m.width = width
		got := ansi.Strip(m.statusLine(footerKey{inputEmpty: true}))
		if ansi.StringWidth(got) != width || strings.Contains(got, "enter send") {
			t.Fatalf("width %d: %q", width, got)
		}
		if width >= len(hint) && got != strings.Repeat(" ", width-len(hint))+hint {
			t.Fatalf("hint not right aligned: %q", got)
		}
	}
	m.width = 80
	if got := ansi.Strip(m.statusLine(footerKey{inputEmpty: true, status: "cancelling…"})); got != " cancelling…" {
		t.Fatalf("status must remain left aligned: %q", got)
	}
	if got := m.statusLine(footerKey{}); got != "" {
		t.Fatalf("hint must hide while typing: %q", got)
	}
}

func TestHumanTokens(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 950: "950", 999: "999", 1000: "1k", 12345: "12.3k", 262144: "262k", 99_999: "100k", 1_200_000: "1.2M", 1_000_000: "1M"} {
		if got := humanTokens(n); got != want {
			t.Errorf("humanTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

// footerModel is a test model with a connected agent so every pill and the
// gauge render.
func footerModel(t *testing.T) *model {
	m, _ := testModel(t)
	m.o.Connect = nil
	m.sess.Name = "local/deepseek-v4-flash"
	lm, err := provider.Open(t.Context(), config.ModelRef{ConnID: "local", Conn: config.Connection{API: config.APIOpenAICompat, BaseURL: "http://localhost:1/v1"}, Model: config.Model{ID: "deepseek-v4-flash"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.sess.Agent = &agent.Agent{Model: lm}
	m.sess.Agent.SetContextWindow(1000)
	m.lastInput = 450
	m.usageIn, m.usageOut = 42, 7
	m.layout()
	return m
}

// hitCenter is the screen column in the middle of the pill for act.
func hitCenter(t *testing.T, m *model, act footerAction) int {
	t.Helper()
	m.footer()
	for _, h := range m.footerHits {
		if h.act == act {
			return (h.x0 + h.x1) / 2
		}
	}
	t.Fatalf("no hit for action %d in %+v", act, m.footerHits)
	return 0
}

func TestToolbarHitsMatchRenderedPills(t *testing.T) {
	m := footerModel(t)
	line := ansi.Strip(strings.SplitN(m.footer(), "\n", 2)[0])
	cells := []rune(line)
	want := map[footerAction]string{actMode: "BUILD ▾", actModel: "local/deepseek-v4-flash ▾", actEffort: "Reasoning: Default ▾", actSession: "new session ▾", actContext: "ctx ▰▰▰▰▱▱▱▱ 45%"}
	for _, h := range m.footerHits {
		got := strings.TrimSpace(string(cells[h.x0:h.x1]))
		if got != want[h.act] {
			t.Errorf("hit %d covers %q, want %q (line %q)", h.act, got, want[h.act], line)
		}
		// The column just outside the range must not belong to the pill.
		if h.x0 > 0 && cells[h.x0-1] != ' ' {
			t.Errorf("hit %d starts inside text: %q", h.act, string(cells[h.x0-1:h.x1]))
		}
	}
	if m.footerHitAt(0) != actNone {
		t.Error("the margin column must not be a control")
	}
}

func TestToolbarEffortIsNotDuplicated(t *testing.T) {
	m := footerModel(t)
	m.sess.Name = "local/model:variant:high"
	m.sess.Agent.Model.Ref.Thinking = "high"
	line := ansi.Strip(m.footer())
	if !strings.Contains(line, "local/model:variant ▾") || !strings.Contains(line, "Reasoning: High ▾") || strings.Contains(line, "variant:high") {
		t.Fatalf("duplicate effort or damaged model ID: %q", line)
	}
	if m.sess.Name != "local/model:variant:high" {
		t.Fatal("display change mutated session identity")
	}
	m.sess.Name = "local/model:high"
	m.sess.Agent.Model.Ref.Thinking = ""
	if line := ansi.Strip(m.footer()); !strings.Contains(line, "local/model:high ▾") {
		t.Fatalf("stripped a model ID suffix without an effort override: %q", line)
	}
}

func TestToolbarClicksOpenTheRightPicker(t *testing.T) {
	m := footerModel(t)
	y := m.toolbarY()
	for act, title := range map[footerAction]string{actMode: "Mode", actModel: "Switch model", actEffort: "Reasoning effort", actSession: "Session"} {
		click(m, hitCenter(t, m, act), y)
		if m.pal == nil || m.pal.levels[len(m.pal.levels)-1].title != title {
			t.Fatalf("clicking pill %d: palette = %+v", act, m.pal)
		}
		m.closePalette()
	}
	// A click on the gauge with an unknown window explains how it is learned.
	m.sess.Agent.SetContextWindow(0)
	click(m, hitCenter(t, m, actContext), y)
	if m.flashText != assumedContextNotice {
		t.Fatalf("gauge click notice missing: %q", m.flashText)
	}
	// Clicks on the hints row and in the gap do nothing.
	before := len(m.blocks)
	click(m, 2, m.statusY())
	click(m, hitCenter(t, m, actSession)+8, y)
	if m.pal != nil || len(m.blocks) != before {
		t.Fatal("hints row / gap must be inert")
	}
}

func TestGaugeWarnsFromCompactionThreshold(t *testing.T) {
	m := footerModel(t)
	m.lastInput = 799
	if f := m.footer(); strings.Contains(f, gaugeWarnStyle.Render("▰▰▰▰▰▰▰▰")) || !strings.Contains(ansi.Strip(f), "79%") {
		t.Fatalf("79%% must not warn: %q", ansi.Strip(f))
	}
	m.lastInput = 800
	if f := m.footer(); !strings.Contains(f, gaugeWarnStyle.Render("▰▰▰▰▰▰▱▱")) || !strings.Contains(ansi.Strip(f), "80%") {
		t.Fatalf("80%% must warn in amber: %q", ansi.Strip(f))
	}
	m.sess.Agent.SetContextWindow(0)
	if f := ansi.Strip(m.footer()); !strings.Contains(f, "ctx*") || !strings.Contains(f, "0%") {
		t.Fatalf("unknown window: %q", f)
	}
}

func TestToolbarDropsDetailWhenNarrow(t *testing.T) {
	m := footerModel(t)
	m.width = 120
	if f := ansi.Strip(m.footer()); !strings.Contains(f, "450/1k") {
		t.Fatalf("wide footer should show used/window: %q", f)
	}
	m.width = 100
	if f := ansi.Strip(m.footer()); strings.Contains(f, "450/1k") || !strings.Contains(f, "new session") {
		t.Fatalf("100 cols drops the token detail only: %q", f)
	}
	m.width = 70
	f := ansi.Strip(m.footer())
	if strings.Contains(f, "new session") || !strings.Contains(f, "45%") {
		t.Fatalf("70 cols drops the session pill, keeps the gauge: %q", f)
	}
	m.width = 44
	f = ansi.Strip(m.footer())
	if strings.Contains(f, "42↑") || !strings.Contains(f, "deepseek-v4-flash ▾") {
		t.Fatalf("44 cols drops usage, keeps the model: %q", f)
	}
	for _, l := range strings.Split(m.footer(), "\n") {
		if w := ansi.StringWidth(l); w > m.width {
			t.Fatalf("footer line %d wide at width %d: %q", w, m.width, ansi.Strip(l))
		}
	}
}

func TestLongModelNameDropsConnectionPrefix(t *testing.T) {
	m := footerModel(t)
	m.sess.Name = "my-long-connection/Qwen3.8-Flash-Next-FP8"
	if f := ansi.Strip(m.footer()); strings.Contains(f, "my-long-connection/") || !strings.Contains(f, "Qwen3.8-Flash-Next-FP8 ▾") {
		t.Fatalf("footer = %q", f)
	}
}

func TestCompactToolbarPreservesContextAndHitBounds(t *testing.T) {
	m := footerModel(t)
	m.sess.Name = "connection/" + strings.Repeat("long-model-", 8)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	line := ansi.Strip(strings.Split(m.footer(), "\n")[0])
	if !strings.Contains(line, "ctx 45%") || !strings.Contains(line, "… ▾") {
		t.Fatalf("long model crowded out context: %q", line)
	}
	for _, h := range m.footerHits {
		if h.x1 > 60 || h.x0 >= h.x1 {
			t.Fatalf("invisible hit: %+v", h)
		}
	}
	click(m, hitCenter(t, m, actModel), m.toolbarY())
	if m.pal == nil {
		t.Fatal("compact model pill not clickable")
	}
	frameLines(t, m, "compact palette")
}

func motion(m *model, x, y int) {
	m.handleMouse(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseNone})
}

func TestHoverLightsPillsAndCards(t *testing.T) {
	m := footerModel(t)
	b := newBlock(blockTool, "")
	b.name, b.status, b.output = "run", "ok", "hello"
	b.args = map[string]any{"command": "echo hello"}
	m.blocks = append(m.blocks, newBlock(blockAssistant, "before"), b)
	m.refresh()

	plain := m.footer()
	motion(m, hitCenter(t, m, actModel), m.toolbarY())
	if m.hoverAct != actModel {
		t.Fatalf("hoverAct = %d", m.hoverAct)
	}
	lit := m.footer()
	if lit == plain || !strings.Contains(lit, pillHoverStyle.Render("local/deepseek-v4-flash ▾")) {
		t.Fatalf("hovered pill not lit:\n%q", lit)
	}
	if ansi.Strip(lit) != ansi.Strip(plain) {
		t.Fatal("hover must not change the text")
	}
	// Leaving the toolbar clears it.
	motion(m, 2, m.statusY())
	if m.hoverAct != actNone || m.footer() != plain {
		t.Fatal("hover must clear when the pointer leaves the pill")
	}

	// Hovering the tool card fills its header; the text stays the same.
	var cardLine int
	for _, s := range m.spans {
		if s.b == b {
			cardLine = s.top
		}
	}
	base := m.vp.View()
	motion(m, 3, cardLine-m.vp.YOffset())
	if m.hoverBlock != b {
		t.Fatalf("hoverBlock = %v", m.hoverBlock)
	}
	hov := m.vp.View()
	if hov == base || trimLines(ansi.Strip(hov)) != trimLines(ansi.Strip(base)) {
		t.Fatalf("card hover must restyle without changing text:\n%s", ansi.Strip(hov))
	}
	if !strings.Contains(hov, hoverStyle.Render("")) && !strings.Contains(hov, "\x1b[48;5;236m") {
		t.Fatalf("no hover background in:\n%q", hov)
	}
	// Motion within the same card is free: no cache miss.
	key := b.key
	motion(m, 10, cardLine-m.vp.YOffset())
	if b.key != key {
		t.Fatal("moving inside the card must not re-render it")
	}
	// Plain text blocks do not light up; leaving restores the base view.
	motion(m, 3, 0)
	if m.hoverBlock != nil || m.vp.View() != base {
		t.Fatal("hover must clear over the assistant block")
	}
}

func TestHoverMovesPaletteAndPanelSelection(t *testing.T) {
	m := footerModel(t)
	typeKeys(m, "ctrl+p")
	m.View() // records the box geometry
	p := m.pal
	if len(p.box.rows) < 3 {
		t.Fatalf("palette rows = %v", p.box.rows)
	}
	// Find the second selectable row and hover it.
	var row, want int
	seen := 0
	for i, idx := range p.box.rows {
		if idx >= 0 {
			seen++
			if seen == 2 {
				row, want = i, idx
				break
			}
		}
	}
	motion(m, p.box.x+3, p.box.y+1+paletteHeaderRows+row)
	if p.sel != want {
		t.Fatalf("palette sel = %d, want %d", p.sel, want)
	}
	// Outside the box nothing moves.
	motion(m, p.box.x+3, p.box.y+p.box.h+1)
	if p.sel != want {
		t.Fatal("hover outside the box must not move the selection")
	}
	m.closePalette()

	m.openModels("")
	m.View()
	pn := m.panel
	g := pn.box
	if len(g.buttons) == 0 {
		t.Fatalf("panel buttons = %+v", g)
	}
	bt := g.buttons[1]
	motion(m, g.x+1+bt.x+1, g.y+1+bt.y)
	if pn.hoverBtn != g.buttonIDs[1] {
		t.Fatalf("hoverBtn = %q, want %q", pn.hoverBtn, g.buttonIDs[1])
	}
	// Compare the rendered fill, not only hover state; brackets are no longer buttons.
	if v := m.View().Content; !strings.Contains(v, formButtonHv.Render("  Edit  ")) || strings.Contains(v, "[ Edit ]") {
		t.Fatalf("hovered button not lit (or the wrong one):\n%s", v)
	}
	motion(m, g.x+g.w+2, g.y)
	if pn.hoverBtn != "" {
		t.Fatal("hoverBtn must clear outside the box")
	}
}
