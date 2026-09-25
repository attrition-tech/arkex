package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/agent"
)

func TestDragSelectsHighlightsAndCopies(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = append(m.blocks[:0], newBlock(blockSystem, "alpha beta gamma"), newBlock(blockSystem, "second line here"))
	m.refresh()
	// Content lines: 0 "alpha beta gamma", 1 "", 2 "second line here".
	if m.vp.YOffset() != 0 {
		t.Fatalf("offset %d", m.vp.YOffset())
	}

	// Screen columns include the two-cell transcript margin.
	m.handleMouse(tea.MouseClickMsg{X: 8, Y: 0, Button: tea.MouseLeft})
	m.handleMouse(tea.MouseMotionMsg{X: 7, Y: 2, Button: tea.MouseLeft})
	_, cmd := m.handleMouse(tea.MouseReleaseMsg{X: 7, Y: 2, Button: tea.MouseLeft})
	if cmd == nil {
		t.Fatal("release after a drag must start the copy timer")
	}
	if got := m.selectedText(); got != "beta gamma\n\nsecond" {
		t.Fatalf("selected text = %q", got)
	}
	// The card under the press must not have toggled and the view shows the
	// highlight (styled cells, the plain text is unchanged).
	view := m.View().Content
	if trimLines(ansi.Strip(view)) != trimLines(ansi.Strip(m.vp.View()+"\n"+m.inputView()+"\n"+m.footer())) {
		t.Fatal("highlight must not change the text")
	}
	if !strings.Contains(view, markStyle.Render("  second")) {
		t.Fatalf("selection not highlighted:\n%s", view)
	}

	// The timer fires: clipboard command + flash.
	_, cmd = m.Update(copySelectionMsg{gen: m.sel.gen})
	if cmd == nil {
		t.Fatal("copy must produce the clipboard command")
	}
	if !strings.Contains(m.flashText, "✓ copied 3 lines") {
		t.Fatalf("notice = %q", m.flashText)
	}
	// A stale timer (from before the drag continued) is ignored.
	m.flashText = ""
	if _, cmd = m.Update(copySelectionMsg{gen: m.sel.gen - 1}); cmd != nil || m.flashText != "" {
		t.Fatal("stale copy timer must be ignored")
	}
	// The flash clears on its own timer, and esc drops the highlight.
	m.flash("x")
	m.Update(flashClearMsg{gen: m.flashGen})
	if m.flashText != "" {
		t.Fatal("flash must clear")
	}
	typeKeys(m, "esc")
	if m.sel != nil {
		t.Fatal("esc must clear the selection")
	}
}

func TestSelectionRightToLeftAndSingleLine(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = append(m.blocks[:0], newBlock(blockSystem, "one two three"))
	m.refresh()
	m.handleMouse(tea.MouseClickMsg{X: 8, Y: 0, Button: tea.MouseLeft})
	m.handleMouse(tea.MouseMotionMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	m.handleMouse(tea.MouseReleaseMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	if got := m.selectedText(); got != "one two" {
		t.Fatalf("backwards selection = %q", got)
	}
	m.Update(copySelectionMsg{gen: m.sel.gen})
	if !strings.Contains(m.flashText, "copied 7 chars") {
		t.Fatalf("notice = %q", m.flashText)
	}
}

func TestPressWithoutDragStillClicks(t *testing.T) {
	m, _ := testModel(t)
	tool := &block{kind: blockTool, name: "bash", status: "ok", args: toolArgs(`{"command":"ls"}`), output: "a\nb\n"}
	m.blocks = append(m.blocks[:0], tool)
	m.refresh()
	click(m, 1, 0)
	if m.openDetail != tool || m.sel != nil {
		t.Fatalf("click must open the card and leave no selection (open=%v sel=%v)", m.openDetail == tool, m.sel)
	}
}

// A stray whitespace reasoning delta in the middle of an answer must not
// split the assistant text into two blocks.
func TestWhitespaceReasoningDeltaDoesNotSplitText(t *testing.T) {
	m, _ := testModel(t)
	m.running = true
	events(m,
		agent.TextDelta{Text: "Extern"},
		agent.ReasoningDelta{Text: "\n"},
		agent.TextDelta{Text: "ally managed"},
	)
	var kinds []blockKind
	for _, b := range m.blocks[1:] { // skip the banner
		kinds = append(kinds, b.kind)
	}
	if len(kinds) != 1 || kinds[0] != blockAssistant || m.blocks[1].text.String() != "Externally managed" {
		t.Fatalf("blocks = %v text=%q", kinds, m.blocks[1].text.String())
	}
	// Real reasoning still opens a block, and whitespace inside it is kept.
	events(m, agent.ReasoningDelta{Text: "hmm"}, agent.ReasoningDelta{Text: " "}, agent.ReasoningDelta{Text: "ok"})
	last := m.blocks[len(m.blocks)-1]
	if last.kind != blockReasoning || last.text.String() != "hmm ok" {
		t.Fatalf("reasoning block = %q", last.text.String())
	}
}

func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}
