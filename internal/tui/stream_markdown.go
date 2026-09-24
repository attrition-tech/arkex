package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Render immutable snapshots off the UI goroutine. Only one job runs at a
// time, at most eight per second. Aim for a 5% render duty cycle, but cap
// the pause at half a second so long replies keep advancing. The existing
// working tick flushes pending text without an idle timer.
type streamMarkdownMsg struct {
	b        *block
	run, gen int
	width, n int
	lines    []string
	duration time.Duration
}

func (m *model) streamMarkdown() tea.Cmd {
	if !m.running || m.markdownBusy || len(m.blocks) == 0 || time.Now().Before(m.markdownNext) {
		return nil
	}
	b := m.blocks[len(m.blocks)-1]
	width := max(1, m.width-4)
	if b.kind != blockAssistant || b.text.Len() == 0 {
		return nil
	}
	if prev := b.streamMD; prev != nil && prev.n == b.text.Len() && prev.width == width && prev.gen == themeGen {
		return nil
	}
	m.markdownBusy = true
	msg := streamMarkdownMsg{b: b, run: m.runGen, gen: themeGen, width: width, n: b.text.Len()}
	text := strings.TrimSpace(b.text.String())
	style, fallback, border := theme.Markdown, textStyle, theme.Muted
	return func() tea.Msg {
		started := time.Now()
		r := newMarkdownRenderer(style, width, border)
		out, err := r.Render(text)
		if err != nil {
			out = fallback.Width(width).Render(text)
		}
		if out = tidy(out); out != "" {
			msg.lines = strings.Split(out, "\n")
		}
		msg.duration = time.Since(started)
		return msg
	}
}

func (m *model) finishStreamMarkdown(msg streamMarkdownMsg) {
	m.markdownBusy = false
	m.markdownNext = time.Now().Add(max(workingInterval, min(500*time.Millisecond, msg.duration*19)))
	if !m.running || msg.run != m.runGen || msg.gen != themeGen || msg.width != max(1, m.width-4) ||
		len(m.blocks) == 0 || m.blocks[len(m.blocks)-1] != msg.b {
		return
	}
	msg.b.streamMD = &msg
	msg.b.lines = nil
	m.refresh()
}
