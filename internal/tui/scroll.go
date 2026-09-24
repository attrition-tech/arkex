package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// scroller is a vertical window over pre-wrapped lines. It replaces
// bubbles/viewport for the transcript because that one measures the display
// width of every line on each SetContent — O(transcript) per streamed token.
// Lines here are already wrapped to the width by renderBlocks, so the
// scroller only slices and joins. The renderer clears its cell buffer before
// every frame, so lines need no padding to the width.
type scroller struct {
	width, height int
	lines         []string
	yOff          int
}

func (s *scroller) SetWidth(w int)  { s.width = w }
func (s *scroller) SetHeight(h int) { s.height = h; s.clamp() }
func (s *scroller) Width() int      { return s.width }
func (s *scroller) Height() int     { return s.height }

// SetLines replaces the content. The offset is clamped, so a transcript that
// shrinks (ctrl+l, /new) never leaves the window past the end.
func (s *scroller) SetLines(lines []string) { s.lines = lines; s.clamp() }

func (s *scroller) maxOff() int      { return max(0, len(s.lines)-s.height) }
func (s *scroller) clamp()           { s.yOff = min(max(0, s.yOff), s.maxOff()) }
func (s *scroller) YOffset() int     { return s.yOff }
func (s *scroller) AtBottom() bool   { return s.yOff >= s.maxOff() }
func (s *scroller) GotoBottom()      { s.yOff = s.maxOff() }
func (s *scroller) ScrollUp(n int)   { s.yOff -= n; s.clamp() }
func (s *scroller) ScrollDown(n int) { s.yOff += n; s.clamp() }

// View returns exactly height lines: the visible slice, each clipped to the
// width, then blank lines so the status bar below always lands on the same
// row.
func (s *scroller) View() string {
	if s.width <= 0 || s.height <= 0 {
		return ""
	}
	end := min(len(s.lines), s.yOff+s.height)
	var sb strings.Builder
	for i := s.yOff; i < end; i++ {
		line := s.lines[i]
		// Byte length is a free upper bound on display width; only measure
		// (and clip) lines that could actually be too wide.
		if len(line) > s.width && ansi.StringWidth(line) > s.width {
			line = ansi.Truncate(line, s.width, "")
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	for i := end - s.yOff; i < s.height; i++ {
		sb.WriteByte('\n')
	}
	out := sb.String()
	return out[:len(out)-1] // drop the final newline: height lines, height-1 separators
}

// scrollbar occupies an existing margin cell, never a text column. width is
// the one-based column of the indicator; any border to its right is retained.
func scrollbar(view string, width, height, total, offset int) string {
	if total <= height || height <= 0 || width <= 0 {
		return view
	}
	rows := strings.Split(view, "\n")
	thumb := max(1, height*height/total)
	start := min(height-thumb, max(0, offset)*(height-thumb)/max(1, total-height))
	for y := 0; y < min(height, len(rows)); y++ {
		glyph := "│"
		if y >= start && y < start+thumb {
			glyph = "┃"
		}
		left := ansi.Truncate(rows[y], width-1, "")
		left += strings.Repeat(" ", max(0, width-1-ansi.StringWidth(left)))
		rows[y] = left + dimStyle.Render(glyph) + ansi.Cut(rows[y], width, ansi.StringWidth(rows[y]))
	}
	return strings.Join(rows, "\n")
}
