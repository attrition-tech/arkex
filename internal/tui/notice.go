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

const flashDelay = 5 * time.Second
const confirmDelay = 2 * time.Second

const assumedContextNotice = "Assumed context limit: 256k (262,144 tokens). No model limit is available. Auto-compaction starts at 80%. Set contextWindow to your server's actual limit. The token usage count is server-reported."

type flashClearMsg struct{ gen int }

func (m *model) disarmConfirmation() {
	if m.confirmKey != "" && m.confirmFlash == m.flashGen {
		m.flashText = ""
	}
	m.confirmKey = ""
	m.confirmUntil = time.Time{}
}

// Only the same key, inside the deadline and without an intervening action,
// confirms. Run completion and dialog transitions also disarm this state.
func (m *model) confirmDanger(key string) bool {
	if m.confirmKey == key && time.Now().Before(m.confirmUntil) {
		m.disarmConfirmation()
		return true
	}
	label, action := "Esc", "stop this run"
	if key == "ctrl+c" {
		label = "Ctrl+C"
	}
	if !m.running {
		action = "exit arkex"
	}
	m.flash("Press " + label + " again within 2s to " + action)
	m.confirmKey, m.confirmUntil, m.confirmFlash = key, time.Now().Add(confirmDelay), m.flashGen
	return false
}

func (m *model) confirmQuit() tea.Cmd {
	m.disarmConfirmation()
	return m.openPaletteSub("Exit arkex?", []paletteItem{
		{title: "Stay", action: func(m *model) tea.Cmd { return m.closePalette() }},
		{title: "Exit", action: func(m *model) tea.Cmd {
			if m.cancel != nil {
				m.cancel()
			}
			return tea.Quit
		}},
	})
}

// flash replaces the current notice. Update schedules its expiry, so callers
// can report success without adding transcript blocks or moving focus.
func (m *model) flash(s string) {
	m.disarmConfirmation()
	m.flashGen++
	m.flashText = strings.Join(strings.Fields(sanitize.Terminal(s)), " ")
}

func (m *model) flashTimer() tea.Cmd {
	if m.flashText == "" {
		return nil
	}
	gen := m.flashGen
	delay := flashDelay
	if m.confirmKey != "" && m.confirmFlash == gen {
		delay = max(0, time.Until(m.confirmUntil))
	}
	return tea.Tick(delay, func(time.Time) tea.Msg { return flashClearMsg{gen: gen} })
}

// Notices cover only the transcript. Confirmation notices remain visible
// above approvals; ordinary notices yield to dialogs.
func (m *model) noticeBox() (box string, x, y int) {
	message := m.flashText
	if message == "" && m.hoverAct == actContext && m.sess.Agent != nil && m.sess.Agent.ContextWindowAssumed() {
		message = assumedContextNotice
	}
	if message == "" || m.pal != nil || m.panel != nil || (m.pending != nil && m.confirmKey == "") || m.comp != nil || m.width < 10 || m.vp.Height() < 4 {
		return "", 0, 0
	}
	w := min(64, m.width-4)
	rows := 2
	if message == assumedContextNotice {
		rows = min(8, m.vp.Height()-2)
	}
	text := ansi.Truncate(message, rows*(w-4), "…")
	text = ansi.Wrap(text, w-4, "")
	lines := strings.Split(text, "\n")
	if len(lines) > rows {
		text = strings.Join(lines[:rows-1], "\n") + "\n" + ansi.Truncate(strings.Join(lines[rows-1:], " "), w-4, "…")
	}
	box = lipgloss.NewStyle().Foreground(theme.Text).
		Border(lipgloss.RoundedBorder()).BorderForeground(theme.Muted).
		Padding(0, 1).Render(text)
	x = m.width - lipgloss.Width(box) - 2
	y = m.vp.Height() - lipgloss.Height(box)
	return box, x, y
}

func (m *model) noticeHit(x, y int) bool {
	box, left, top := m.noticeBox()
	return box != "" && x >= left && x < left+lipgloss.Width(box) && y >= top && y < top+lipgloss.Height(box)
}

// The persistent jump-to-latest control covers text without taking any rows
// from the transcript. Notices keep their corner; lift this box if they overlap.
func (m *model) scrollBox() (box string, x, y int) {
	below := m.vp.maxOff() - m.vp.YOffset()
	if below <= 0 || m.pal != nil || m.panel != nil || m.pending != nil || m.comp != nil || m.width < 10 || m.vp.Height() < 4 {
		return "", 0, 0
	}
	unit := "lines"
	if below == 1 {
		unit = "line"
	}
	text := ansi.Truncate(fmt.Sprintf("↓ %d %s below · Latest", below, unit), m.width-6, "…")
	if m.scrollHover {
		text = fillRow(text, lipgloss.Width(text))
	}
	box = lipgloss.NewStyle().Foreground(theme.Text).
		Border(lipgloss.RoundedBorder()).BorderForeground(theme.Muted).
		Padding(0, 1).Render(text)
	x = (m.width - lipgloss.Width(box)) / 2
	y = m.vp.Height() - lipgloss.Height(box)
	if notice, nx, ny := m.noticeBox(); notice != "" && x < nx+lipgloss.Width(notice) && x+lipgloss.Width(box) > nx {
		y = ny - lipgloss.Height(box) - 1
	}
	if y < 0 {
		return "", 0, 0
	}
	return box, x, y
}

func (m *model) scrollHit(x, y int) bool {
	box, left, top := m.scrollBox()
	return box != "" && x >= left && x < left+lipgloss.Width(box) && y >= top && y < top+lipgloss.Height(box)
}
