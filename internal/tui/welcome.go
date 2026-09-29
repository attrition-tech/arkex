package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/sanitize"
	"github.com/attrition-tech/arkex/internal/session"
)

const welcomePlaceholder = "Ask a question, describe a task, or paste something…"

type welcomeScreen struct {
	greeting string
	recent   []session.Summary
	focus    int // 0: input, 1: model, 2 onward: recent sessions
	hover    int
}

// The welcome screen is only an empty conversation, never a transcript page.
func (m *model) onWelcome() bool {
	return m.conv == nil && !m.running && m.pending == nil && m.editingPrompt == nil &&
		len(m.blocks) == 1 && m.blocks[0] == m.welcome
}

func (m *model) welcomeWidth() int { return max(3, min(72, m.width-2)) }

func (m *model) composerWidth() int {
	if m.onWelcome() {
		return m.welcomeWidth() - 2
	}
	return max(1, m.width-4)
}

// Rendering, cursor placement and mouse hits share these cell coordinates.
// Spend space on the input first, then the header and recent sessions, then gaps.
type welcomeGeometry struct {
	x, width                                       int
	brand, workspace, greeting, chips, input, rule int
	model, recent, count                           int
}

func (m *model) welcomeGeometry() welcomeGeometry {
	w := m.welcomeWidth()
	g := welcomeGeometry{x: (m.width - w) / 2, width: w, brand: -1, workspace: -1, greeting: -1, chips: -1}
	base := m.inputBoxRows() + 2 // input, bottom rule, model selector
	room := max(0, m.height-base)
	header := room >= 3
	if header {
		room -= 3
	}
	g.count = min(len(m.start.recent), room)
	room -= g.count
	// Two gaps around the greeting, one above the model, two above sessions.
	headerGap, inputGap, modelGap, recentGap := 0, 0, 0, 0
	if header && room >= 3 {
		headerGap, inputGap = 2, 1
		room -= 3
	}
	if room > 0 {
		modelGap = 1
		room--
	}
	if g.count > 0 {
		recentGap = min(2, room)
		room -= recentGap
	}
	y := room / 3
	if header {
		g.brand, g.workspace = y, y+1
		g.greeting = y + 2 + headerGap
		y = g.greeting + 1 + inputGap
	}
	if len(m.attachments) > 0 {
		g.chips = y
		y++
	}
	g.input = y
	g.rule = y + m.input.Height()
	g.model = g.rule + 1 + modelGap
	g.recent = g.model + 1 + recentGap
	return g
}

func welcomeText(s string) string {
	return strings.Join(strings.Fields(sanitize.Terminal(s)), " ")
}

func (m *model) welcomeModelLabel() string {
	label := "Connect a model"
	if m.sess.Agent != nil {
		label = welcomeText(m.sess.Name)
	}
	return " " + ansi.Truncate(label, max(1, m.welcomeWidth()-4), "…") + " ▾ "
}

func (m *model) welcomeView() tea.View {
	g := m.welcomeGeometry()
	lines := make([]string, m.height)
	put := func(y, x int, s string) {
		if y >= 0 && y < len(lines) {
			lines[y] = strings.Repeat(" ", max(0, x)) + ansi.Truncate(s, max(0, m.width-x), "")
		}
	}
	center := func(y int, s string) {
		s = ansi.Truncate(s, g.width, "…")
		put(y, (m.width-ansi.StringWidth(s))/2, s)
	}
	center(g.brand, titleStyle.Render("arkex"))
	center(g.workspace, dimStyle.Render(welcomeText(m.o.Cwd)))
	center(g.greeting, textStyle.Render(m.start.greeting))
	if g.chips >= 0 {
		put(g.chips, g.x+2, m.chipRow(m.input.Width()))
	}
	for i, line := range strings.Split(m.input.View(), "\n") {
		put(g.input+i, g.x, titleStyle.Render("│")+" "+line)
	}
	put(g.rule, g.x, dimStyle.Render(strings.Repeat("─", g.width)))
	label := m.welcomeModelLabel()
	if m.start.focus == 1 || m.start.hover == 1 {
		label = fillRow(textStyle.Render(label), ansi.StringWidth(label))
	} else {
		label = dimStyle.Render(label)
	}
	center(g.model, label)
	for i, s := range m.start.recent[:g.count] {
		age := session.Age(s.Updated, time.Now())
		inner := max(1, g.width-4)
		age = ansi.Truncate(age, max(0, inner-4), "")
		title := ansi.Truncate(welcomeText(s.Title), max(1, inner-ansi.StringWidth(age)-2), "…")
		row := "  " + textStyle.Render(title) + strings.Repeat(" ", max(0, inner-ansi.StringWidth(title+age))) + dimStyle.Render(age) + "  "
		if m.start.focus == i+2 || m.start.hover == i+2 {
			row = fillRow(row, g.width)
		}
		put(g.recent+i, g.x, row)
	}
	content := strings.Join(lines, "\n")
	v := tea.NewView(content)
	v.AltScreen, v.MouseMode = true, m.mouseMode()
	if m.pal != nil {
		if box, x, y, cursor := m.paletteBox(m.width, m.height); box != "" {
			v.Content = overlay(muteBackdrop(content), box, x, y)
			v.Cursor = cursor
		}
		return v
	}
	if popup := m.renderPopup(g.width); popup != "" {
		// Keep file completion visible even when a short terminal has no
		// room above the input. The normal completion keys still own it.
		y := max(0, g.input-len(strings.Split(popup, "\n")))
		v.Content = overlay(muteBackdrop(content), popup, g.x, y)
		if y+len(strings.Split(popup, "\n")) > g.input {
			return v // Don't draw the input cursor over a covering popup.
		}
	} else if box, x, y := m.noticeBox(); box != "" {
		v.Content = overlay(content, box, x, y)
	}
	if m.start.focus == 0 {
		if c := m.input.Cursor(); c != nil {
			c.X += g.x + 2
			c.Y += g.input
			if c.Y >= g.input && c.Y < g.rule && c.X < m.width {
				v.Cursor = c
			}
		}
	}
	return v
}

func (m *model) focusWelcome(focus int) {
	m.start.focus = focus
	if focus == 0 {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
}

func (m *model) activateWelcome(target int) tea.Cmd {
	m.focusWelcome(0)
	m.start.hover = 0
	if target == 1 {
		if m.sess.Agent == nil {
			m.openModels(m.o.StartupNote)
			return nil
		}
		return m.openPaletteSub("Switch model", modelItems(m))
	}
	if target >= 2 && target-2 < len(m.start.recent) {
		return m.resume(m.start.recent[target-2].ID)
	}
	return nil
}

func (m *model) welcomeKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	if !m.onWelcome() {
		return nil, false
	}
	n := m.welcomeGeometry().count + 2
	switch k.String() {
	case "tab", "shift+tab":
		dir := 1
		if k.String() == "shift+tab" {
			dir = -1
		}
		m.focusWelcome((m.start.focus + dir + n) % n)
		return nil, true
	case "enter":
		if m.start.focus > 0 {
			return m.activateWelcome(m.start.focus), true
		}
	case "up", "down":
		if m.start.focus > 0 {
			dir := 1
			if k.String() == "up" {
				dir = -1
			}
			m.focusWelcome((m.start.focus + dir + n) % n)
			return nil, true
		}
	case "esc":
		if m.start.focus > 0 {
			m.focusWelcome(0)
			return nil, true
		}
	default:
		m.focusWelcome(0) // Typing always returns to the draft.
	}
	return nil, false
}

func (m *model) welcomeHit(x, y int) int {
	g := m.welcomeGeometry()
	if y == g.model {
		w := ansi.StringWidth(m.welcomeModelLabel())
		left := (m.width - w) / 2
		if x >= left && x < left+w {
			return 1
		}
	}
	if x >= g.x && x < g.x+g.width && y >= g.recent && y < g.recent+g.count {
		return 2 + y - g.recent
	}
	return 0
}

func (m *model) welcomeMouse(msg tea.MouseMsg) (tea.Cmd, bool) {
	if !m.onWelcome() || m.pal != nil || m.panel != nil {
		return nil, false
	}
	mo := msg.Mouse()
	g := m.welcomeGeometry()
	if m.comp != nil {
		return nil, true // Don't click through file completion into the input or controls.
	}
	inInput := mo.X >= g.x+2 && mo.X < g.x+g.width && mo.Y >= g.input && mo.Y < g.rule
	if inInput {
		m.start.hover = 0
		if _, click := msg.(tea.MouseClickMsg); click {
			m.focusWelcome(0)
		}
		return nil, false // Use the shared textarea selection and wheel handling.
	}
	switch msg.(type) {
	case tea.MouseMotionMsg:
		m.start.hover = m.welcomeHit(mo.X, mo.Y)
		m.hoverChip = 0
		if mo.Y == g.chips {
			for _, h := range m.chipHits {
				if mo.X-g.x-2 >= h.x0 && mo.X-g.x-2 < h.x1 {
					m.hoverChip = h.i + 1
				}
			}
		}
	case tea.MouseClickMsg:
		if mo.Button == tea.MouseLeft {
			m.input.ClearSelection()
			if mo.Y == g.chips {
				m.chipClick(mo.X - g.x - 2)
			} else if hit := m.welcomeHit(mo.X, mo.Y); hit != 0 {
				return m.activateWelcome(hit), true
			}
		}
	}
	return nil, true
}
