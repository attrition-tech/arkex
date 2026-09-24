package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// A two-cell dot pattern animates beside the current activity and in the
// terminal title. A fixed, scattered sequence keeps both surfaces in sync
// without generating randomness on every View. The shared 8 fps tick runs
// only during a live run; the banner, input and completed blocks stay cached.

// workingMsg advances the strip by one frame.
type workingMsg struct{ gen int }

const (
	workingFPS      = 8
	workingInterval = time.Second / workingFPS
)

var dotFrames = [...]string{"⠋⡐", "⡒⠢", "⠔⡙", "⡡⠌", "⠢⡒", "⡘⠥", "⠥⡈", "⡃⠔"}

const loaderFrames = len(dotFrames)

// workingTick schedules the next frame for the current run.
func (m *model) workingTick() tea.Cmd {
	gen := m.runGen
	return tea.Tick(workingInterval, func(time.Time) tea.Msg { return workingMsg{gen: gen} })
}

// workingFrame handles a tick: advance, redraw, re-arm while the run that
// started the animation is still going.
func (m *model) workingFrame(msg workingMsg) tea.Cmd {
	if !m.running || msg.gen != m.runGen {
		return nil
	}
	m.frame++
	m.refresh()
	return tea.Batch(m.workingTick(), m.streamMarkdown())
}

// loader returns plain Unicode: terminal titles cannot contain ANSI styling.
func loader(f int) string {
	return dotFrames[f%loaderFrames]
}

// workingStrip is the full line at the given inner width: loader, activity,
// and the step number on the right when it fits.
func (m *model) workingStrip(width int) string {
	act := m.activity()
	left := toolStyle.Render(loader(m.frame)) + "  " + textStyle.Render(act)
	if m.step > 0 {
		step := dimStyle.Render(fmt.Sprintf("step %d", m.step))
		if gap := width - lipgloss.Width(left) - lipgloss.Width(step); gap >= 2 {
			return left + strings.Repeat(" ", gap) + step
		}
	}
	return ansi.Truncate(left, width, "…")
}

// activity says what the agent is doing, from the state of the last block:
// a running tool leaves the strip label empty, streamed reasoning is
// thinking, streamed text is writing, and between those the model is
// being waited on.
func (m *model) activity() string {
	if m.pending != nil {
		return "waiting for your answer"
	}
	if m.status != "" {
		return m.status
	}
	if !m.retryUntil.IsZero() {
		return fmt.Sprintf("Connection interrupted · retrying in %s · %d/3", max(time.Duration(0), time.Until(m.retryUntil)).Round(time.Second), m.retryAttempt)
	}
	n := len(m.blocks)
	if n == 0 {
		return "thinking…"
	}
	b := m.blocks[n-1]
	switch b.kind {
	case blockTool:
		if b.status != "running" {
			return "thinking…"
		}
		return ""
	case blockReasoning:
		return "thinking…"
	case blockAssistant:
		return "writing…"
	}
	return "thinking…"
}
