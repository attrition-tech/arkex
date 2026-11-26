package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/attrition-tech/arkex/internal/sanitize"
	"github.com/charmbracelet/x/ansi"
)

const statsHoverDelay = 350 * time.Millisecond

type statsPanel struct {
	open, pinned, hovered bool
	gen, offset           int
}

type statsHoverMsg struct {
	gen  int
	show bool
}

func (m *model) dismissStats() {
	m.stats.open, m.stats.pinned, m.stats.offset = false, false, 0
	m.stats.gen++ // a delayed hover must not reopen a dismissed panel
}

func (m *model) statsAllowed() bool {
	return m.pal == nil && m.panel == nil && m.pending == nil && m.comp == nil && m.flashText == "" &&
		!m.inputDragging && (m.sel == nil || !m.sel.dragging)
}

// statsMouse uses the same geometry as rendering. A short departure delay
// lets the pointer cross the composer into the panel without flickering.
func (m *model) statsMouse(msg tea.MouseMsg) (tea.Cmd, bool) {
	if !m.statsAllowed() {
		m.dismissStats()
		return nil, false
	}
	mo := msg.Mouse()
	hit := m.statsHit(mo.X, mo.Y)
	switch msg.(type) {
	case tea.MouseMotionMsg:
		if mo.Button != tea.MouseNone {
			m.dismissStats()
			return nil, false
		}
		modelHit := mo.Y == m.toolbarY() && m.footerHitAt(mo.X) == actModel
		if m.onWelcome() {
			modelHit = m.welcomeHit(mo.X, mo.Y) == 1
		}
		hovered := hit || modelHit
		if hovered != m.stats.hovered {
			m.stats.hovered = hovered
			m.stats.gen++
			if !m.stats.pinned && m.mouse {
				gen := m.stats.gen
				return tea.Tick(statsHoverDelay, func(time.Time) tea.Msg {
					return statsHoverMsg{gen: gen, show: hovered}
				}), hit
			}
		}
	case tea.MouseWheelMsg:
		if hit {
			switch mo.Button {
			case tea.MouseWheelUp:
				m.stats.offset = max(0, m.stats.offset-1)
			case tea.MouseWheelDown:
				m.stats.offset++
			}
		}
	case tea.MouseClickMsg:
		if hit && mo.Button == tea.MouseLeft {
			m.stats.pinned = true
			m.stats.gen++
		} else if !hit {
			m.dismissStats()
		}
	}
	// Never activate links, tools or selections covered by the stats panel.
	return nil, hit
}

func (m *model) statsLines(width int) []string {
	var lines []string
	line := func(s string) { lines = append(lines, strings.Split(ansi.Wrap(s, width, ""), "\n")...) }
	value := func(label, v string) {
		gap := width - ansi.StringWidth(label) - ansi.StringWidth(v)
		if gap < 1 {
			line(dimStyle.Render(label))
			line(v)
		} else {
			line(dimStyle.Render(label) + strings.Repeat(" ", gap) + v)
		}
	}
	line(sanitize.Terminal(m.connection.Name))
	limit := m.contextWindow()
	context := "No request measured"
	if m.lastInput > 0 && limit > 0 {
		pct := m.lastInput * 100 / int64(limit)
		context = fmt.Sprintf("%d / %d · %d%%", m.lastInput, limit, pct)
		if pct >= 80 {
			context = gaugeWarnStyle.Render(context)
		}
	} else if limit > 0 {
		context = fmt.Sprintf("— / %d tokens", limit)
	}
	value("Context · last request", context)
	if m.connection.Agent != nil && m.connection.Agent.ContextWindowAssumed() {
		line(gaugeWarnStyle.Render("Limit assumed · set contextWindow"))
	}
	in, out := m.usageIn, m.usageOut
	if m.session != nil {
		in += m.session.TitleUsageIn
		out += m.session.TitleUsageOut
	}
	value("Session input ↑", fmt.Sprint(in))
	value("Session output ↓", fmt.Sprint(out))
	line(dimStyle.Render("Includes title generation"))
	first, llm, answer := m.responseTiming.values(time.Now(), m.running)
	value("First answer", first)
	value("LLM first answer", llm)
	value("Answer elapsed", answer)
	elapsed := "—"
	if m.running {
		elapsed = responseDuration(time.Since(m.startedAt)) + "…"
	} else if !m.responseTiming.start.IsZero() {
		elapsed = responseDuration(m.runDuration)
	}
	value("Run elapsed", elapsed)
	line(dimStyle.Render("/stats keeps open · /timing request details"))
	return lines
}

// Keep every statistic reachable on short screens, without covering the
// composer or changing transcript height. Pinned panels also scroll by key.
func (m *model) statsBox() (box string, x, y int) {
	bottom := m.vp.Height()
	if m.onWelcome() {
		bottom = m.welcomeGeometry().input - 1
	}
	if !m.stats.open || !m.statsAllowed() || m.width < 10 || bottom < 3 {
		return "", 0, 0
	}
	inner := min(48, m.width-6)
	lines := m.statsLines(inner)
	rows := min(len(lines), bottom-2)
	m.stats.offset = min(max(0, m.stats.offset), len(lines)-rows)
	box = borderStyle.Width(inner + 4).Render(strings.Join(lines[m.stats.offset:m.stats.offset+rows], "\n"))
	box = overlay(box, textStyle.Render(ansi.Truncate(" Session stats ", inner, "…")), 2, 0)
	hint := " esc close "
	if rows < len(lines) {
		hint = fmt.Sprintf(" esc · pgup/pgdn %d/%d ", m.stats.offset+1, len(lines))
	}
	box = overlay(box, dimStyle.Render(ansi.Truncate(hint, inner, "…")), 2, lipgloss.Height(box)-1)
	return box, 1, bottom - lipgloss.Height(box)
}

func (m *model) statsHit(x, y int) bool {
	box, left, top := m.statsBox()
	return box != "" && x >= left && x < left+lipgloss.Width(box) && y >= top && y < top+lipgloss.Height(box)
}
