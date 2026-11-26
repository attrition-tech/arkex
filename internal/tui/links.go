package tui

import (
	"net/url"
	"os"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/attrition-tech/arkex/internal/chatgpt"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

type linkSpan struct {
	row, x0, x1 int
	url         string
}

func webURL(target string) bool {
	if len(target) > 4096 || strings.ContainsAny(target, "\\") || strings.ContainsFunc(target, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r)
	}) {
		return false
	}
	u, err := url.Parse(target)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil
}

// Use the rendered OSC 8 destinations, not a second URL regex or Markdown
// layout. This preserves the exact targets and cell widths across wrapping,
// tables, indentation and immutable streaming snapshots. Leave OSC 8 intact.
func renderedLinks(lines []string) []linkSpan {
	var links []linkSpan
	var state byte
	target := ""
	for row, line := range lines {
		if target == "" && !strings.Contains(line, "\x1b]8;") {
			continue
		}
		for x := 0; len(line) > 0; {
			seq, width, n, next := ansi.DecodeSequence(line, state, nil)
			line, state = line[n:], next
			if data, ok := strings.CutPrefix(seq, "\x1b]8;"); ok {
				_, target, _ = strings.Cut(strings.TrimSuffix(strings.TrimSuffix(data, "\x1b\\"), "\a"), ";")
				if !webURL(target) {
					target = ""
				}
			}
			if width > 0 && target != "" {
				if i := len(links) - 1; i >= 0 && links[i].row == row && links[i].x1 == x && links[i].url == target {
					links[i].x1 += width
				} else {
					links = append(links, linkSpan{row: row, x0: x, x1: x + width, url: target})
				}
			}
			x += width
		}
	}
	return links
}

func (m *model) linkAt(x, y int) string {
	if !m.mouse || x < 0 || x >= m.width-1 || y < 0 || y >= m.vp.Height() ||
		m.pal != nil || m.panel != nil || m.pending != nil || m.comp != nil || m.noticeHit(x, y) || m.scrollHit(x, y) || m.statsHit(x, y) {
		return ""
	}
	line := m.vp.YOffset() + y
	s, ok := m.spanAt(line)
	if !ok || s.workHeader || s.b.kind != blockAssistant {
		return ""
	}
	if s.work != nil {
		x -= 2
	}
	for _, link := range s.b.links {
		if link.row == line-s.top && x >= link.x0 && x < link.x1 {
			return link.url
		}
	}
	return ""
}

func (m *model) hoveredLink() string {
	if m.pointer == nil || m.sel != nil || m.inputDragging {
		return ""
	}
	return m.linkAt(m.pointer.col, m.pointer.line)
}

func remoteTerminal() bool {
	return os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != ""
}

type linkOpenedMsg struct{ err error }

func (m *model) openLink(target string) tea.Cmd {
	if !webURL(target) {
		return nil
	}
	if remoteTerminal() {
		m.flash("SSH: use your terminal's link gesture, or Reply actions → Links → Copy URL")
		return nil
	}
	return func() tea.Msg { return linkOpenedMsg{err: chatgpt.OpenBrowser(target)} }
}

func replyLinks(source string) []paletteItem {
	data := []byte(source)
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	doc := md.Parser().Parse(text.NewReader(data))
	seen := make(map[string]bool)
	var items []paletteItem
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var target string
		switch n := n.(type) {
		case *ast.Link:
			target = string(n.Destination)
		case *ast.AutoLink:
			target = string(n.URL(data))
		}
		if !webURL(target) || seen[target] {
			return ast.WalkContinue, nil
		}
		seen[target] = true
		items = append(items, paletteItem{title: target, sub: func(*model) []paletteItem {
			return []paletteItem{
				{title: "Open in browser", detail: target, unavailable: func(*model) string {
					if remoteTerminal() {
						return "Over SSH, use your terminal's link gesture or Copy URL"
					}
					return ""
				}, action: func(m *model) tea.Cmd {
					return tea.Batch(m.closePalette(), m.openLink(target))
				}},
				copyItem("Copy URL", target),
			}
		}})
		return ast.WalkContinue, nil
	})
	return items
}
