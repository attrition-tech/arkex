package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

func copyItem(title, value string) paletteItem {
	item := paletteItem{title: title, action: func(m *model) tea.Cmd {
		return tea.Batch(m.closePalette(), m.copyText(value))
	}}
	if value == "" {
		item.unavailable = func(*model) string { return "No text to copy" }
	}
	return item
}

// Search fallback for replies outside the viewport. Contextual menus use the
// same actions directly, without making users find their reply a second time.
func replyItems(m *model) []paletteItem {
	var items []paletteItem
	for i := len(m.blocks) - 1; i >= 0; i-- {
		b := m.blocks[i]
		if b.kind != blockAssistant || b.text.Len() == 0 {
			continue
		}
		source := b.text.String()
		label := ansi.Truncate(strings.Join(strings.Fields(source), " "), 60, "…")
		items = append(items, paletteItem{title: label, sub: func(*model) []paletteItem {
			return replyActions(source)
		}})
	}
	if len(items) == 0 {
		items = append(items, paletteItem{title: "No responses yet"})
	}
	return items
}

func replyActions(source string) []paletteItem {
	items := []paletteItem{copyItem("Copy reply as Markdown", source)}
	for i, code := range codeBlocks(source) {
		item := copyItem(fmt.Sprintf("Copy code block %d", i+1), code)
		item.detail = strings.Join(strings.Fields(code), " ")
		items = append(items, item)
	}
	if links := replyLinks(source); len(links) > 0 {
		items = append(items, paletteItem{title: "Links", hint: fmt.Sprintf("%d", len(links)), sub: func(*model) []paletteItem { return links }})
	}
	return items
}

func (m *model) messageActions(b *block) tea.Cmd {
	m.promptFocus = b
	m.refresh()
	title := "Sent message"
	items := []paletteItem{copyItem("Copy sent message", b.text.String())}
	if b.kind == blockAssistant {
		title, items = "Reply actions", replyActions(b.text.String())
	} else if b.prompt != 0 {
		items = append(items,
			paletteItem{title: "Edit and resend…", hint: "e", action: func(m *model) tea.Cmd { return m.requestPromptEdit(b.prompt) }},
			paletteItem{title: "Remove from here…", hint: "delete", action: func(m *model) tea.Cmd { return m.confirmPromptRemove(b.prompt) }},
		)
	}
	cmd := m.openPalette()
	m.pal.levels = nil
	m.pal.push(paletteLevel{title: title, items: items})
	return cmd
}

// These zero-width markers only travel inside the Markdown renderer. They let
// its nested lists/quotes and wrapping determine the exact button coordinates.
// Extract them before displaying anything; never send markers to the terminal.
const codeActionMarker = "\x1b]arkex-copy;"

type codeAction struct {
	row, x0, x1 int
	text        string
}

func codeActionLabel(index int) string { return fmt.Sprintf("Copy code %d", index+1) }

func extractCodeActions(rendered string, codes []string) (string, []codeAction) {
	lines := strings.Split(tidy(rendered), "\n")
	var actions []codeAction
	for row, line := range lines {
		for {
			start := strings.Index(line, codeActionMarker)
			if start < 0 {
				break
			}
			end := strings.IndexByte(line[start:], '\a')
			if end < 0 {
				break
			}
			end += start
			index, err := strconv.Atoi(line[start+len(codeActionMarker) : end])
			x := ansi.StringWidth(line[:start])
			line = line[:start] + line[end+1:]
			if err == nil && index >= 0 && index < len(codes) {
				actions = append(actions, codeAction{row: row, x0: x, x1: x + len(codeActionLabel(index)), text: codes[index]})
			}
		}
		lines[row] = line
	}
	return strings.Join(lines, "\n"), actions
}

func codeText(n ast.Node, data []byte) string {
	var code strings.Builder
	for i := 0; i < n.Lines().Len(); i++ {
		line := n.Lines().At(i)
		// EOF is a newline for Markdown rendering, not for copying source.
		line.ForceNewline = false
		code.Write(line.Value(data))
	}
	return code.String()
}

func codeBlocks(source string) []string {
	data := []byte(source)
	doc := goldmark.DefaultParser().Parse(text.NewReader(data))
	var blocks []string
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && (n.Kind() == ast.KindFencedCodeBlock || n.Kind() == ast.KindCodeBlock) {
			blocks = append(blocks, codeText(n, data))
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return blocks
}
