package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dantearo/arkex/internal/agent"
)

// The footer is everything under the transcript:
//
//	╭──────────────────────────────────────────────────────╮
//	│ › Ask anything…                                      │  input box
//	╰──────────────────────────────────────────────────────╯
//	 AUTO ▾  Qwen3.8-Flash ▾  write the page ▾   ctx ▰▰▱▱ 45% · 1.2M↑ 30k↓   toolbar
//	 enter send · shift+enter newline · / commands           status / hints
//
// Every toolbar pill is also the control for what it shows: clicking opens
// the matching picker. footerHits records where the pills landed in the
// last render so a click can be mapped back to one.

// footerAction is what a toolbar pill does when clicked.
type footerAction int

const (
	actNone footerAction = iota
	actMode
	actModel
	actEffort
	actSession
	actContext
)

type footerHit struct {
	x0, x1 int // column range, x1 exclusive
	act    footerAction
}

// footerKey captures everything the two footer rows depend on; elapsed is
// in whole seconds because that is the displayed precision.
type footerKey struct {
	status, name, conv string
	effort             string
	running, paused    bool
	elapsed            int64
	usageIn, usageOut  int64
	lastInput          int64
	ctx, width, gen    int
	assumed            bool
	compacting         bool
	compactFrame       int
	mode               agent.Mode
	inputRows          int    // when the input text is taller than its box
	inputEmpty         bool   // hints show only while nothing is typed
	panel              string // panel legend replaces the hints while a panel is open
	pending            bool   // approval prompt up: its keys replace the hints
	retry              bool
	hover              footerAction
}

// footerRows is the height of the toolbar plus the status line.
const footerRows = 2

// boxRows is the height of the input box including its borders; 0 while
// the Connections panel replaces it.
func (m *model) boxRows() int {
	if m.panel != nil {
		return 0
	}
	if m.pending != nil {
		return m.approvalRows()
	}
	return m.inputBoxRows() + 2
}

// Rows of the footer parts, counted from the top of the screen.
func (m *model) chipRowY() int { return m.vp.Height() + 1 }
func (m *model) toolbarY() int { return m.vp.Height() + m.boxRows() }
func (m *model) statusY() int  { return m.toolbarY() + 1 }

// footer renders the toolbar and the status line, reusing the previous
// render while nothing they show has changed (View runs on every message).
func (m *model) footer() string {
	var elapsed time.Duration
	if m.running {
		elapsed = time.Since(m.startedAt).Round(time.Second)
	}
	name := m.sess.Name
	if m.sess.Agent != nil && m.sess.Agent.Model != nil && m.sess.Agent.Model.Ref.Thinking != "" {
		name = strings.TrimSuffix(name, ":"+m.sess.Agent.Model.Ref.Thinking)
	}
	key := footerKey{
		status: m.status, name: name, conv: m.convTitle(), running: m.running, paused: m.paused,
		effort:  m.effortLabel(),
		elapsed: int64(elapsed / time.Second),
		usageIn: m.usageIn, usageOut: m.usageOut, lastInput: m.lastInput,
		ctx: m.contextWindow(), width: m.width, gen: themeGen, mode: m.mode(),
		assumed:    m.sess.Agent != nil && m.sess.Agent.ContextWindowAssumed(),
		compacting: m.compacting,
		inputEmpty: m.input.Value() == "" && len(m.attachments) == 0,
		hover:      m.hoverAct, pending: m.pending != nil,
		retry: m.pending != nil && m.pending.title != "",
	}
	if m.inputRows > m.input.Height() {
		key.inputRows = m.inputRows
	}
	if m.compacting {
		key.compactFrame = m.frame % 8
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

// convTitle is the current conversation's title for the session pill.
func (m *model) convTitle() string {
	if m.conv == nil || m.conv.Title == "" {
		return ""
	}
	return m.conv.Title
}

// toolbar draws the pills on the left and the context gauge plus token
// usage on the right, dropping right-hand detail first when the width is
// short. It records footerHits.
func (m *model) toolbar(key footerKey) string {
	m.footerHits = m.footerHits[:0]
	var sb strings.Builder
	x := 1 // screen column: the line starts with one space of margin
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

	pill(actMode, m.modeBadge(key.hover == actMode))
	model := key.name
	if i := strings.IndexByte(model, '/'); i >= 0 && (lipgloss.Width(model) > 28 || m.width < 80) {
		model = model[i+1:] // the connection is in the Connections panel; the model matters here
	}
	if m.width < 80 {
		// Reserve a readable context percentage before spending space on
		// the model name. Hit ranges cover only the rendered pill.
		budget := max(4, m.width-1-x-1-4-lipgloss.Width(m.gauge(key))-2)
		model = ansi.Truncate(model, budget, "…")
	}
	pill(actModel, pillStyleFor(key.hover == actModel).Render(model+" ▾"))
	if key.effort != "" && m.width >= 80 && (!key.compacting || m.width >= 110) {
		pill(actEffort, pillStyleFor(key.hover == actEffort).Render("Reasoning: "+key.effort+" ▾"))
	}
	conv := key.conv
	if conv == "" {
		conv = "new session"
	}
	if m.width >= 80 && !key.compacting {
		pill(actSession, pillStyleFor(key.hover == actSession).Render(ansi.Truncate(conv, 24, "…")+" ▾"))
	}

	// Right side: gauge, then usage; each dropped when it does not fit.
	right := m.gauge(key)
	usage := humanTokens(key.usageIn) + "↑ " + humanTokens(key.usageOut) + "↓"
	if key.running {
		usage = fmt.Sprintf("%ds · %s", key.elapsed, usage)
	}
	sep := dimStyle.Render(" · ")
	if lipgloss.Width(right) > 0 {
		right += sep
	}
	right += dimStyle.Render(usage)
	gap := m.width - 1 - x - lipgloss.Width(right)
	if gap < 2 {
		right = m.gauge(key)
		gap = m.width - 1 - x - lipgloss.Width(right)
	}
	if gap < 2 {
		right = ""
		gap = 0
	}
	if right != "" {
		gx := x + gap
		m.footerHits = append(m.footerHits, footerHit{x0: gx, x1: gx + lipgloss.Width(right), act: actContext})
	}
	line := " " + sb.String() + strings.Repeat(" ", gap) + right
	return ansi.Truncate(line, m.width, "")
}

// gauge is the context-window meter: eight cells filled in proportion to
// the last request's input tokens, amber from the compaction threshold on.
func (m *model) gauge(key footerKey) string {
	label := "ctx "
	if key.assumed {
		label = "ctx* "
	}
	if key.compacting {
		wave := []rune("▁▂▄▆█▆▄▂")
		var bar strings.Builder
		cells := 8
		if m.width < 80 {
			cells = 4
		}
		if m.width < 50 {
			cells = 2
		}
		for i := 0; i < cells; i++ {
			bar.WriteRune(wave[(i-key.compactFrame+8)%8])
		}
		return dimStyle.Render(label) + gaugeStyle.Render(bar.String()) + dimStyle.Render(" compacting")
	}
	if key.ctx <= 0 || key.lastInput <= 0 {
		return dimStyle.Render(label + "?")
	}
	pct := int(key.lastInput * 100 / int64(key.ctx))
	cells := min((pct*8+50)/100, 8)
	bar := strings.Repeat("▰", cells) + strings.Repeat("▱", 8-cells)
	style := gaugeStyle
	if pct >= 80 {
		style = gaugeWarnStyle
	}
	if m.width < 80 {
		return dimStyle.Render(label) + style.Render(fmt.Sprintf("%d%%", pct))
	}
	s := dimStyle.Render(label) + style.Render(bar) + " " + fmt.Sprintf("%d%%", pct)
	if m.width >= 110 {
		s += dimStyle.Render(" · " + humanTokens(key.lastInput) + "/" + humanTokens(int64(key.ctx)))
	}
	return s
}

// statusLine shows, in order of urgency: what the agent is
// doing and input overflow hints; with nothing to report and an
// empty input box, the key hints.
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
	var parts []string
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
		if !key.inputEmpty {
			return ""
		}
		hint := ansi.Truncate("/ or cmd+p for command palette", m.width, "…")
		return hintsStyle.Width(m.width).Align(lipgloss.Right).Render(hint)
	}
	return " " + ansi.Truncate(strings.Join(parts, dimStyle.Render(" · ")), m.width-1, "…")
}

// footerClick handles a click on the toolbar row; y is relative to the
// screen. It reports whether the click landed on a control.
func (m *model) footerClick(x, y int) (tea.Cmd, bool) {
	if y != m.toolbarY() {
		return nil, false
	}
	switch m.footerHitAt(x) {
	case actMode:
		return m.openPaletteSub("Mode", modeItems(m)), true
	case actModel:
		if m.sess.Agent == nil {
			m.openModels("")
			return nil, true
		}
		return m.openPaletteSub("Switch model", modelItems(m)), true
	case actSession:
		items := append([]paletteItem{{title: "New conversation", hint: "/clear", cmd: "/clear"}}, sessionItems(m)...)
		return m.openPaletteSub("Session", items), true
	case actEffort:
		return m.openPaletteSub("Reasoning effort", effortItems(m)), true
	case actContext:
		if m.sess.Agent != nil && m.sess.Agent.ContextWindowAssumed() {
			m.flash(assumedContextNotice)
		}
		return nil, true
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

// modeItems lists the three modes, the current one marked.
func modeItems(m *model) []paletteItem {
	var items []paletteItem
	for _, md := range agent.Modes {
		hint := "/mode " + string(md)
		if md == m.mode() {
			hint = "current"
		}
		items = append(items, paletteItem{title: modeTitle(md), hint: hint, cmd: "/mode " + string(md)})
	}
	return items
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
