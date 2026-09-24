package tui

import (
	"bytes"
	"html"
	"image/color"

	glamouransi "charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Keep Glamour's parser and ANSI styles, but lay out parsed table cells with
// Lip Gloss: Glamour's built-in table renderer disables all four outer borders.
type tableMarkdownRenderer struct {
	styles glamouransi.StyleConfig
	width  int
	border color.Color
}

func (r *tableMarkdownRenderer) Render(input string) (string, error) {
	// Glamour pads every block to WordWrap using the document style. A styled
	// padding space is emitted as its own SGR pair, even though tidy removes all
	// of that right padding. Keep the foreground on actual text instead. This is
	// visually equivalent, but makes padding plain spaces and avoids hundreds of
	// thousands of tiny WrapWriter writes for ordinary replies.
	styles := r.styles
	if styles.Text.Color == nil {
		styles.Text.Color = styles.Document.Color
	}
	styles.Document.Color = nil

	md := goldmark.New(goldmark.WithExtensions(extension.GFM, extension.DefinitionList),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()))
	source := []byte(input)
	doc := md.Parser().Parse(text.NewReader(source))
	var tables []*extast.Table
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if t, ok := n.(*extast.Table); ok && entering {
			tables = append(tables, t)
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	for _, t := range tables {
		body, err := r.renderTable(t, source, styles)
		if err != nil {
			return "", err
		}
		// A TextBlock preserves line breaks without treating the rendered table
		// as Markdown again. Only our own ANSI output is added to the source.
		start := len(source)
		source = append(source, []byte("\n"+html.EscapeString(body)+"\n")...)
		block := ast.NewTextBlock()
		block.AppendChild(block, ast.NewTextSegment(text.NewSegment(start, len(source))))
		t.Parent().ReplaceChild(t.Parent(), t, block)
	}
	return r.renderNode(doc, source, r.width, styles)
}

func (r *tableMarkdownRenderer) renderNode(node ast.Node, source []byte, width int, styles glamouransi.StyleConfig) (string, error) {
	ar := glamouransi.NewRenderer(glamouransi.Options{Styles: styles, WordWrap: width})
	render := renderer.NewRenderer(renderer.WithNodeRenderers(util.Prioritized(ar, 1000)))
	var out bytes.Buffer
	err := render.Render(&out, source, node)
	return out.String(), err
}

func (r *tableMarkdownRenderer) renderTable(t *extast.Table, source []byte, styles glamouransi.StyleConfig) (string, error) {
	width := r.width
	// Reserve the enclosing block's indentation before sizing the outline.
	for parent := t.Parent(); parent != nil; parent = parent.Parent() {
		var st glamouransi.StyleBlock
		switch parent.Kind() {
		case ast.KindBlockquote:
			st = r.styles.BlockQuote
		case ast.KindList:
			st = r.styles.List.StyleBlock
			for ancestor := parent.Parent(); ancestor != nil; ancestor = ancestor.Parent() {
				if ancestor.Kind() == ast.KindList {
					indent := r.styles.List.LevelIndent
					st.Indent = &indent
					break
				}
			}
		}
		if st.Indent != nil {
			width -= int(*st.Indent)
		}
		if st.Margin != nil {
			width -= 2 * int(*st.Margin)
		}
	}
	grid := table.New().Width(max(1, width)).Wrap(true).
		Border(lipgloss.RoundedBorder()).BorderStyle(lipgloss.NewStyle().Foreground(r.border)).
		StyleFunc(func(row, col int) lipgloss.Style {
			style := lipgloss.NewStyle().Padding(0, 1)
			switch t.Alignments[col] {
			case extast.AlignRight:
				style = style.Align(lipgloss.Right)
			case extast.AlignCenter:
				style = style.Align(lipgloss.Center)
			}
			return style
		})
	for row := t.FirstChild(); row != nil; row = row.NextSibling() {
		var cells []string
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			doc := ast.NewDocument()
			p := ast.NewParagraph()
			doc.AppendChild(doc, p)
			for cell.FirstChild() != nil {
				p.AppendChild(p, cell.FirstChild())
			}
			value, err := r.renderNode(doc, source, max(1, width), styles)
			if err != nil {
				return "", err
			}
			cells = append(cells, tidy(value))
		}
		if row.Kind() == extast.KindTableHeader {
			grid.Headers(cells...)
		} else {
			grid.Row(cells...)
		}
	}
	return grid.String(), nil
}
