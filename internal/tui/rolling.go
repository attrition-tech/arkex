package tui

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
)

const rollDuration = 150 * time.Millisecond

type rollMsg struct{ gen int }

// Only advance the live fold while following the bottom, never under a reader.
func (m *model) advanceRoll() {
	if !m.running || !m.stickBottom || m.openWork != nil || m.openDetail != nil || m.openGroup != nil || m.sel != nil {
		return
	}
	if !m.rollPaused.IsZero() {
		m.rollAt = m.rollAt.Add(time.Since(m.rollPaused))
		m.rollPaused = time.Time{}
	}
	start, total, finished := 0, 0, 0
	cut := len(m.blocks)
	for i := len(m.blocks) - 1; i >= 0; i-- {
		b := m.blocks[i]
		if b.kind == blockUser {
			start = i + 1
			break
		}
		if b.kind == blockTool {
			total++
			if !m.toolInFlight(b) {
				finished++
				if finished <= 2 {
					cut = i
				}
			}
		}
	}
	if total < 5 || finished < 2 || cut <= m.liveCut {
		return
	}
	m.rollFrom = max(start, m.liveCut)
	m.liveCut = cut
	m.rollAt = time.Now()
}

func (m *model) liveSections(start, end int) []workSection {
	w := workSection{start: start, end: min(m.liveCut, end), live: true}
	var actions, failed, denied int
	for i := w.start; i < w.end; i++ {
		if !w.contains(m, i) {
			continue
		}
		b := m.blocks[i]
		if b.kind == blockTool {
			actions++
			switch b.status {
			case "error":
				failed++
			case "denied":
				denied++
			}
		}
	}
	if actions == 0 {
		return nil
	}
	w.head = m.blocks[start]
	w.label = fmt.Sprintf("Earlier work · %d actions", actions)
	if failed > 0 {
		w.label += fmt.Sprintf(" · %d failed", failed)
	}
	if denied > 0 {
		w.label += fmt.Sprintf(" · %d denied", denied)
	}
	return []workSection{w}
}

func (m *model) rollTick() tea.Cmd {
	gen := m.runGen
	return tea.Tick(frameInterval, func(time.Time) tea.Msg { return rollMsg{gen} })
}

// Keep a shrinking tail of the old rows during the transition. No persistent
// height changes or animation timers remain once the fold settles.
func (m *model) rollingTail(s workSection, width int) []string {
	if m.rollAt.IsZero() || s.end != m.liveCut {
		return nil
	}
	now := time.Now()
	if !m.rollPaused.IsZero() {
		now = m.rollPaused
	}
	remaining := rollDuration - now.Sub(m.rollAt)
	if remaining <= 0 {
		return nil
	}
	var lines []string
	for i := max(s.start, m.rollFrom); i < s.end; i++ {
		b := m.blocks[i]
		if !s.contains(m, i) || b.kind == blockReasoning {
			continue // reasoning has its own latest-only view and archive
		}
		lines = append(lines, "")
		for _, line := range m.blockLines(b, width, false) {
			lines = append(lines, "  "+line)
		}
	}
	n := int(float64(len(lines)) * float64(remaining) / float64(rollDuration))
	return lines[len(lines)-n:]
}
