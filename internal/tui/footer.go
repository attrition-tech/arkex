package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The footer keeps model and effort controls beneath the input box. Its
// second row is reserved for actionable status, so notices do not reflow
// the transcript. Statistics live in the model's hover panel instead.
type footerAction int

const (
	actNone footerAction = iota
	actModel
	actEffort
)

type footerHit struct {
	x0, x1 int // column range, x1 exclusive
	act    footerAction
}

// Statistics do not invalidate the toolbar cache on every usage update.
type footerKey struct {
	status, name    string
	effort          string
	running, paused bool
	width, gen      int
	inputRows       int
	panel           string
	pending, retry  bool
	hover           footerAction
	inbox           string
	messageFocus    bool
	link            string
}

const footerRows = 2

// boxRows includes the input's borders; approvals replace the input box.
func (m *model) boxRows() int {
	if m.panel != nil {
		return 0
	}
	if m.pending != nil {
		return m.approvalRows()
	}
	return m.inputBoxRows() + 2
}

func (m *model) chipRowY() int { return m.vp.Height() + 1 }
func (m *model) toolbarY() int { return m.vp.Height() + m.boxRows() }
func (m *model) statusY() int  { return m.toolbarY() + 1 }

func (m *model) footer() string {
	name := m.connection.Name
	if m.connection.Agent != nil && m.connection.Agent.Model != nil && m.connection.Agent.Model.Ref.Thinking != "" {
		name = strings.TrimSuffix(name, ":"+m.connection.Agent.Model.Ref.Thinking)
	}
	key := footerKey{
		status: m.status, name: name, running: m.running, paused: m.paused,
		effort: m.effortLabel(), width: m.width, gen: themeGen,
		hover: m.hoverAct, pending: m.pending != nil,
		retry:        m.pending != nil && m.pending.title != "",
		messageFocus: m.promptFocus != nil,
		link:         m.hoveredLink(),
	}
	if m.session != nil {
		steers := 0
		for _, input := range m.session.Pending {
			if input.Steer {
				steers++
			}
		}
		if n := len(m.session.Pending); n > 0 {
			if m.session.QueuePaused {
				key.inbox = fmt.Sprintf("Paused pending: %d · /queue", n)
			} else {
				key.inbox = fmt.Sprintf("Steering pending: %d · Queued: %d · /queue", steers, n-steers)
			}
		}
	}
	if m.inboxSaving {
		key.inbox = "Saving pending message…"
	}
	if m.inputRows > m.input.Height() {
		key.inputRows = m.inputRows
	}
	if m.compacting && key.status == "" {
		key.status = fmt.Sprintf("Compacting · %s · %s · esc twice cancels", m.compactStage, time.Since(m.compactStarted).Round(time.Second))
	}
	if m.panel != nil {
		key.panel = m.panelBottom()
	}
	if c := &m.footerCache; c.view != "" && c.key == key {
		return c.view
	}
	view := m.toolbar(key) + "\n" + m.statusLine(key)
	m.footerCache.key, m.footerCache.view = key, view
	return view
}

// statusBar is the footer's status line alone, used by tests.
func (m *model) statusBar() string {
	m.footer()
	_, line, _ := strings.Cut(m.footerCache.view, "\n")
	return line
}

// sessionTitle supplies the session name for the terminal window title.
func (m *model) sessionTitle() string {
	if m.session == nil {
		return ""
	}
	return m.session.Title
}

// Reserve room for effort before truncating the model. Hit ranges cover
// only visible controls, including in very narrow terminals.
func (m *model) toolbar(key footerKey) string {
	m.footerHits = m.footerHits[:0]
	var sb strings.Builder
	x := 1
	pill := func(act footerAction, s string) {
		if sb.Len() > 0 {
			sb.WriteByte(' ')
			x++
		}
		w := lipgloss.Width(s)
		sb.WriteString(s)
		m.footerHits = append(m.footerHits, footerHit{x0: x, x1: x + w, act: act})
		x += w
	}
	model := key.name
	if i := strings.IndexByte(model, '/'); i >= 0 {
		model = model[i+1:] // the stats panel includes the connection
	}
	effort := ""
	if key.effort != "" {
		effort = pillStyleFor(key.hover == actEffort).Render("Effort: " + key.effort + " ▾")
		if m.width < lipgloss.Width(effort)+9 {
			effort = pillStyleFor(key.hover == actEffort).Render(key.effort + " ▾")
		}
		if m.width < lipgloss.Width(effort)+9 {
			effort = ""
		}
	}
	budget := m.width - 2
	if effort != "" {
		budget -= lipgloss.Width(effort) + 1
	}
	if budget >= 5 {
		pill(actModel, pillStyleFor(key.hover == actModel).Render(ansi.Truncate(model, budget-4, "…")+" ▾"))
	}
	if effort != "" {
		pill(actEffort, effort)
	}
	return ansi.Truncate(" "+sb.String(), m.width, "")
}

// Keep approvals, pending messages and recovery visible without hovering.
func (m *model) statusLine(key footerKey) string {
	if key.panel != "" {
		return " " + ansi.Truncate(key.panel, m.width-1, "…")
	}
	if key.pending {
		if key.retry {
			return " " + hintsStyle.Render(ansi.Truncate("←/→ choose · enter confirm · esc twice cancels · ctrl+c twice stops", m.width-1, "…"))
		}
		return " " + hintsStyle.Render(ansi.Truncate(approvalHint(m.width), m.width-1, "…"))
	}
	if key.link != "" {
		label := "Click to open · "
		if remoteTerminal() {
			label = "SSH: terminal link gesture · "
		}
		return " " + textStyle.Foreground(theme.Tool).Render(ansi.Truncate(label+key.link, m.width-1, "…"))
	}
	if key.messageFocus {
		return " " + hintsStyle.Render(ansi.Truncate("enter actions · tab older · shift+tab newer · esc composer", m.width-1, "…"))
	}
	var parts []string
	if key.inbox != "" {
		parts = append(parts, key.inbox)
	}
	switch {
	case key.status != "":
		parts = append(parts, key.status)
	case key.paused:
		parts = append(parts, "paused — enter continues")
	}
	if key.inputRows > 0 {
		parts = append(parts, fmt.Sprintf("input %d lines", key.inputRows))
	}
	if len(parts) == 0 {
		if key.running {
			return " " + hintsStyle.Render(ansi.Truncate("enter steer · alt+enter queue · shift+enter newline", m.width-1, "…"))
		}
		return ""
	}
	return " " + ansi.Truncate(strings.Join(parts, dimStyle.Render(" · ")), m.width-1, "…")
}

func (m *model) footerClick(x, y int) (tea.Cmd, bool) {
	if y != m.toolbarY() {
		return nil, false
	}
	switch m.footerHitAt(x) {
	case actModel:
		if m.connection.Agent == nil {
			m.openModels("")
			return nil, true
		}
		return m.openPaletteSub("Switch model", modelItems(m)), true
	case actEffort:
		return m.openPaletteSub("Reasoning effort", effortItems(m)), true
	}
	return nil, false
}

func (m *model) footerHitAt(x int) footerAction {
	for _, h := range m.footerHits {
		if x >= h.x0 && x < h.x1 {
			return h.act
		}
	}
	return actNone
}

func pillStyleFor(hover bool) lipgloss.Style {
	if hover {
		return pillHoverStyle
	}
	return pillStyle
}

// humanTokens renders a token count compactly: 950, 12.3k, 262k, 1.2M.
func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000_000)) + "M"
	case n >= 100_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1000)) + "k"
	}
	return fmt.Sprint(n)
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }
