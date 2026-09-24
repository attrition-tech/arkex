package tui

import (
	"strings"
	"unicode"

	rw "github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"
)

// inputMaxRows is how tall the input box grows before it scrolls.
const inputMaxRows = 5

// inputRows returns how many display rows value occupies inside the textarea
// at the given inner width: every logical line word-wrapped the way the
// textarea does it. The textarea only exposes the wrapped height of the
// cursor's line, so the box would otherwise stay one row tall while a long
// pasted line wraps out of sight above it.
func inputRows(value string, width int) int {
	if width <= 0 {
		return 1
	}
	rows := 0
	for _, line := range strings.Split(value, "\n") {
		rows += wrapRows([]rune(line), width)
	}
	return max(1, rows)
}

// wrapRows is bubbles/textarea's wrap algorithm reduced to a row count. It
// must stay in step with textarea.wrap (see TestInputRowsMatchTextarea).
func wrapRows(runes []rune, width int) int {
	var (
		rows   = 1
		cur    int // display width of the current row
		word   []rune
		spaces int
	)
	for _, r := range runes {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}
		if spaces > 0 {
			w := uniseg.StringWidth(string(word))
			if cur+w+spaces > width {
				rows++
				cur = w + spaces
			} else {
				cur += w + spaces
			}
			spaces = 0
			word = nil
			continue
		}
		lastCharLen := rw.RuneWidth(word[len(word)-1])
		if w := uniseg.StringWidth(string(word)); w+lastCharLen > width {
			if cur > 0 {
				rows++
			}
			cur = w
			word = nil
		}
	}
	if cur+uniseg.StringWidth(string(word))+spaces >= width {
		rows++
	}
	return rows
}
