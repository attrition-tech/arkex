package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"
)

// Mouse selection in the transcript. With mouse reporting on, the terminal
// no longer selects text itself, so a press-drag-release over the transcript
// highlights the cells here and, once the pointer has rested for copyDelay,
// copies the text to the clipboard (OSC 52 for the terminal, plus the system
// clipboard when available) and flashes a confirmation over the transcript.

const copyDelay = time.Second

// pos is a cell in transcript content coordinates: line index into the
// wrapped lines and display column.
type pos struct{ line, col int }

func (p pos) before(q pos) bool { return p.line < q.line || (p.line == q.line && p.col < q.col) }

type selection struct {
	anchor, head pos
	dragging     bool // button still held
	gen          int  // bumped on every change; stale copy timers compare it
}

// empty is true while nothing has been dragged over.
func (s *selection) empty() bool { return s.anchor == s.head }

// bounds returns the ordered range; end is exclusive on its column.
func (s *selection) bounds() (start, end pos) {
	start, end = s.anchor, s.head
	if end.before(start) {
		start, end = end, start
	}
	return start, end
}

type copySelectionMsg struct{ gen int }

var markStyle lipgloss.Style

// selectPress starts a selection at the pressed cell.
func (m *model) selectPress(x, y int) {
	m.sel = &selection{dragging: true, gen: m.selGen()}
	m.sel.anchor = pos{line: m.vp.YOffset() + y, col: x}
	m.sel.head = m.sel.anchor
}

// selectDrag extends the selection to the pointer while the button is held,
// scrolling when it leaves the window.
func (m *model) selectDrag(x, y int) {
	if m.sel == nil || !m.sel.dragging {
		return
	}
	switch {
	case y < 0:
		m.vp.ScrollUp(1)
		m.stickBottom = false
		y = 0
	case y >= m.vp.Height():
		m.vp.ScrollDown(1)
		m.stickBottom = m.vp.AtBottom()
		y = m.vp.Height() - 1
	}
	m.sel.head = pos{line: m.vp.YOffset() + y, col: max(0, x)}
	m.sel.gen = m.selGen()
}

// selectRelease ends the drag. A release without movement is a click; the
// caller handles that. Otherwise the copy timer starts.
func (m *model) selectRelease() (clicked bool, cmd tea.Cmd) {
	if m.sel == nil {
		return false, nil
	}
	m.sel.dragging = false
	if m.sel.empty() {
		m.sel = nil
		return true, nil
	}
	gen := m.sel.gen
	return false, tea.Tick(copyDelay, func(time.Time) tea.Msg { return copySelectionMsg{gen: gen} })
}

func (m *model) selGen() int {
	m.selectionGen++
	return m.selectionGen
}

func (m *model) clearSelection() {
	if m.sel != nil {
		m.sel = nil
	}
}

// selectedText extracts the highlighted text from the wrapped transcript
// lines, without styling and without trailing spaces.
func (m *model) selectedText() string {
	if m.sel == nil || m.sel.empty() {
		return ""
	}
	start, end := m.sel.bounds()
	lines := m.vp.lines
	var out []string
	for i := start.line; i <= end.line && i < len(lines); i++ {
		if i < 0 {
			continue
		}
		plain := ansi.Strip(lines[i])
		left, right := 2, ansi.StringWidth(plain) // exclude the transcript's screen margin
		for _, span := range m.spans {
			if i >= span.top && i < span.bottom {
				if span.b.kind == blockUser {
					left = 1
				}
				break
			}
		}
		if i == start.line {
			left = max(left, start.col)
		}
		if i == end.line {
			right = min(right, end.col+1)
		}
		if left >= right {
			out = append(out, "")
			continue
		}
		out = append(out, strings.TrimRight(ansi.Cut(plain, left, right), " "))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// copySelection writes the selection to the clipboard and flashes the
// result. Returns the command that sets the terminal clipboard (OSC 52).
func (m *model) copySelection() tea.Cmd {
	return m.copyText(m.selectedText())
}

func (m *model) copyActiveSelection() tea.Cmd {
	if m.input.HasSelection() {
		return m.copyText(m.input.SelectedText())
	}
	return m.copySelection()
}

func (m *model) copyText(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	lines := strings.Count(text, "\n") + 1
	if lines == 1 {
		m.flash(fmt.Sprintf("✓ copied %d chars", ansi.StringWidth(text)))
	} else {
		m.flash(fmt.Sprintf("✓ copied %d lines", lines))
	}
	return tea.Batch(tea.SetClipboard(text), func() tea.Msg {
		// A system clipboard helper can block; never run it on the UI loop.
		_ = clipboard.WriteAll(text)
		return nil
	})
}

// highlight applies the selection to the visible transcript lines.
func (m *model) highlight(view string) string {
	if m.sel == nil || m.sel.empty() {
		return view
	}
	start, end := m.sel.bounds()
	rows := strings.Split(view, "\n")
	off := m.vp.YOffset()
	for y := range rows {
		i := off + y
		if i < start.line || i > end.line {
			continue
		}
		line := rows[y]
		width := ansi.StringWidth(line)
		left, right := 0, max(width, m.vp.Width())
		if i == start.line {
			left = start.col
		}
		if i == end.line {
			right = end.col + 1
		}
		if left >= right {
			continue
		}
		// Pad so a selection can be seen past the end of a short line.
		if width < right {
			line += strings.Repeat(" ", right-width)
		}
		mid := markStyle.Render(ansi.Strip(ansi.Cut(line, left, right)))
		rows[y] = ansi.Truncate(line, left, "") + mid + ansi.Cut(line, right, max(right, width))
	}
	return strings.Join(rows, "\n")
}
