package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

func copyItem(title, value string) paletteItem {
	return paletteItem{title: title, action: func(m *model) tea.Cmd {
		return tea.Batch(m.closePalette(), m.copyText(value))
	}}
}

// Offer exact source text, not the wrapped/styled screen representation.
func responseCopyItems(m *model) []paletteItem {
	var items []paletteItem
	for i := len(m.blocks) - 1; i >= 0; i-- {
		b := m.blocks[i]
		if b.kind != blockAssistant || b.text.Len() == 0 {
			continue
		}
		source := b.text.String()
		label := ansi.Truncate(strings.Join(strings.Fields(source), " "), 60, "…")
		items = append(items, paletteItem{title: label, sub: func(*model) []paletteItem {
			out := []paletteItem{copyItem("Copy response as Markdown", source)}
			for i, code := range codeBlocks(source) {
				out = append(out, copyItem(fmt.Sprintf("Copy code block %d", i+1), code))
			}
			return out
		}})
	}
	if len(items) == 0 {
		items = append(items, paletteItem{title: "No responses yet"})
	}
	return items
}

func codeBlocks(source string) []string {
	data := []byte(source)
	doc := goldmark.DefaultParser().Parse(text.NewReader(data))
	var blocks []string
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && (n.Kind() == ast.KindFencedCodeBlock || n.Kind() == ast.KindCodeBlock) {
			var code strings.Builder
			for i := 0; i < n.Lines().Len(); i++ {
				line := n.Lines().At(i)
				code.Write(line.Value(data))
			}
			blocks = append(blocks, code.String())
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return blocks
}
