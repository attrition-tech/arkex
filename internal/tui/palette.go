package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/config"
)

// paletteItem is one row of the command palette. Commands share the typed
// slash-command path; local dialogs use action, and sub opens a nested list.
type paletteItem struct {
	group       string
	title       string
	hint        string // key or command shown right-aligned
	cmd         string // slash command run on enter
	sub         func(m *model) []paletteItem
	action      func(*model) tea.Cmd
	sessionID   string // session row: enter resumes, ctrl+o / Actions manages
	detail      string // session model and last activity, shown below the title
	searchOnly  bool   // available by search, without crowding the default list
	aliases     string
	unavailable func(*model) string // checked again on activation, not just opening
}

// paletteLevel is one list in the palette: the top level or a nested one.
type paletteLevel struct {
	title string
	items []paletteItem
	query string
	sel   int
}

// palette is the centered overlay opened with ctrl+p, cmd+p or "/" on an
// empty input.
type palette struct {
	input   textinput.Model
	levels  []paletteLevel // last is the visible one
	view    []paletteItem  // current level filtered by the search text
	sel     int
	box     paletteGeom // where the last render put things, for clicks
	editing bool        // input is a value, not a search query
	busy    bool
	cancel  context.CancelFunc
}

// paletteGeom is the rendered box position inside the transcript area and
// the view index of each visible row (-1 for group headers).
type paletteGeom struct {
	x, y, w, h int
	rows       []int
}

const (
	paletteWidth      = 64
	paletteHeaderRows = 3 // title, search, rule
)

func (m *model) openPalette() tea.Cmd {
	if m.pal != nil && m.pal.cancel != nil {
		m.pal.cancel()
	}
	ti := textinput.New()
	ti.Prompt = "  "
	ti.Placeholder = "Search all commands…"
	ti.SetVirtualCursor(false)
	p := &palette{input: ti}
	m.pal = p
	m.comp = nil
	m.input.Blur()
	p.push(paletteLevel{title: "Commands", items: m.paletteItems()})
	return p.input.Focus()
}

// openPaletteSub opens the palette directly on a nested list, e.g. /resume.
func (m *model) openPaletteSub(title string, items []paletteItem) tea.Cmd {
	cmd := m.openPalette()
	m.pal.push(paletteLevel{title: title, items: items})
	return cmd
}

func (m *model) closePalette() tea.Cmd {
	if m.pal != nil && m.pal.cancel != nil {
		m.pal.cancel()
	}
	m.pal = nil
	return m.input.Focus()
}

func (p *palette) push(l paletteLevel) {
	if len(p.levels) > 0 {
		parent := &p.levels[len(p.levels)-1]
		parent.query, parent.sel = p.input.Value(), p.sel
	}
	p.levels = append(p.levels, l)
	p.input.Reset()
	p.sel = 0
	p.filter()
}

func (p *palette) level() paletteLevel { return p.levels[len(p.levels)-1] }

// filter recomputes the visible rows. With no search text rows keep their
// registration order (grouped); with text they are ranked by fuzzy score
// against the title, falling back to group and hint.
func (p *palette) filter() {
	q := strings.TrimPrefix(strings.TrimSpace(p.input.Value()), "/")
	items := p.level().items
	if q == "" || p.editing || p.busy {
		p.view = nil
		for _, it := range items {
			if !it.searchOnly || p.editing || p.busy {
				p.view = append(p.view, it)
			}
		}
	} else {
		type scored struct {
			it    paletteItem
			score int
		}
		var hits []scored
		for _, it := range items {
			s, ok := fuzzyScore(q, it.title)
			if !ok {
				s, ok = fuzzyScore(q, it.group+" "+it.title+" "+it.hint+" "+it.aliases+" "+it.detail)
				s -= 500
			}
			for _, alias := range strings.Fields(it.hint + " " + it.aliases) {
				if strings.EqualFold(q, strings.TrimPrefix(alias, "/")) {
					s, ok = 10000, true
					break
				}
			}
			if ok {
				hits = append(hits, scored{it, s})
			}
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
		p.view = make([]paletteItem, 0, len(hits))
		for _, h := range hits {
			p.view = append(p.view, h.it)
		}
	}
	if p.sel >= len(p.view) {
		p.sel = 0
	}
}

// paletteKey handles keys while the palette is open.
func (m *model) paletteKey(k tea.KeyPressMsg) tea.Cmd {
	p := m.pal
	if p.busy {
		if k.String() == "esc" || k.String() == "ctrl+c" || k.String() == "enter" || k.String() == "ctrl+p" || k.String() == "super+p" {
			return m.closePalette()
		}
		return nil
	}
	switch k.String() {
	case "esc":
		if p.editing {
			return m.closePalette()
		}
		if len(p.levels) > 1 {
			p.levels = p.levels[:len(p.levels)-1]
			parent := p.level()
			p.input.SetValue(parent.query)
			p.sel = parent.sel
			p.filter()
			return nil
		}
		return m.closePalette()
	case "ctrl+c", "ctrl+p", "super+p":
		return m.closePalette()
	case "up", "shift+tab":
		p.move(-1)
		return nil
	case "down", "tab":
		p.move(1)
		return nil
	case "enter":
		return p.activate(m)
	case "ctrl+o":
		if len(p.view) > 0 && p.view[p.sel].sessionID != "" {
			return m.openSessionActions(p.view[p.sel].sessionID)
		}
		return nil
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(k)
	p.sel = 0
	p.filter()
	return cmd
}

// move shifts the selection by dir, wrapping around.
func (p *palette) move(dir int) {
	if n := len(p.view); n > 0 {
		p.sel = (p.sel + dir + n) % n
	}
}

// activate runs the selected row: enters a sub-list, runs its command, or
// with no rows runs the search text as a typed command so "model local/x"
// runs /model local/x.
func (p *palette) activate(m *model) tea.Cmd {
	if len(p.view) == 0 {
		q := strings.TrimSpace(p.input.Value())
		cmd := m.closePalette()
		if q == "" {
			return cmd
		}
		_, run := m.command("/" + strings.TrimPrefix(q, "/"))
		return tea.Batch(cmd, run)
	}
	it := p.view[p.sel]
	if it.unavailable != nil && it.unavailable(m) != "" {
		return nil
	}
	if it.action != nil {
		return it.action(m)
	}
	if it.sub != nil {
		items := it.sub(m)
		if len(items) == 0 {
			items = []paletteItem{{group: it.group, title: "nothing configured", cmd: "/connections", hint: "/connections"}}
		}
		p.push(paletteLevel{title: it.title, items: items})
		return nil
	}
	if it.cmd == "" {
		return nil // informational rows have no action
	}
	cmd := m.closePalette()
	_, run := m.command(it.cmd)
	return tea.Batch(cmd, run)
}

// paletteItems builds the top level from the current state.
func (m *model) paletteItems() []paletteItem {
	items := []paletteItem{
		{group: "Session", title: "New conversation", hint: "/clear", aliases: "/new", cmd: "/clear", unavailable: idleRequired},
		{group: "Session", title: "Resume session", hint: "/resume", aliases: "/sessions", sub: sessionItems, unavailable: idleRequired},
		{group: "Session", title: "Pending messages", hint: "/queue", sub: pendingItems},
		{group: "Model", title: "Switch model", hint: "/model", sub: modelItems, unavailable: idleRequired},
		{group: "Model", title: "Connections", hint: "/connections", aliases: "/models", cmd: "/connections"},
		{group: "More", title: "Settings", sub: settingsItems},
		{group: "More", title: "Help", hint: "/help", cmd: "/help"},
	}
	selection := m.input.SelectedText()
	if selection == "" {
		selection = m.selectedText()
	}
	if selection != "" {
		item := copyItem("Copy selection", selection)
		item.group, item.hint = "Selection", "ctrl+shift+c"
		items = append(items, item)
	}
	items = append(items,
		paletteItem{group: "Copy", title: "Copy draft", searchOnly: true, unavailable: func(m *model) string {
			if m.input.Value() == "" {
				return "The composer is empty"
			}
			return ""
		}, action: func(m *model) tea.Cmd {
			return tea.Batch(m.closePalette(), m.copyText(m.input.Value()))
		}},
		paletteItem{group: "Copy", title: "Copy reply or code…", searchOnly: true, sub: replyItems, unavailable: func(m *model) string {
			for _, b := range m.blocks {
				if b.kind == blockAssistant && b.text.Len() > 0 {
					return ""
				}
			}
			return "No assistant replies yet"
		}},
		paletteItem{group: "Session", title: "Queue current draft", hint: "alt+enter", searchOnly: true, unavailable: func(m *model) string {
			if m.editingPrompt != nil {
				return "Finish editing the sent prompt first"
			}
			if strings.TrimSpace(m.input.Value()) == "" && len(m.attachments) == 0 {
				return "The composer is empty"
			}
			if m.sess.Agent == nil {
				return "Connect a model first"
			}
			return ""
		}, action: func(m *model) tea.Cmd {
			m.closePalette()
			return m.enqueueDraft(false)
		}},
		paletteItem{group: "Session", title: "Edit sent prompt", hint: "tab · e", searchOnly: true, sub: promptEditItems},
		paletteItem{group: "Session", title: "Remove from prompt…", hint: "tab · delete", searchOnly: true, sub: promptRemoveItems},
		paletteItem{group: "Session", title: "Rename session", hint: "/rename", cmd: "/rename", searchOnly: true, unavailable: func(m *model) string {
			if m.conv == nil {
				return "Start or resume a session first"
			}
			return ""
		}},
		paletteItem{group: "Session", title: "Compact conversation", hint: "/compact", cmd: "/compact", searchOnly: true, unavailable: func(m *model) string {
			if reason := idleRequired(m); reason != "" {
				return reason
			}
			if m.sess.Agent == nil || len(m.sess.Agent.Messages()) == 0 {
				return "No conversation to compact"
			}
			return ""
		}},
		paletteItem{group: "Session", title: "Continue paused run", hint: "/continue", cmd: "/continue", searchOnly: true, unavailable: func(m *model) string {
			if reason := idleRequired(m); reason != "" {
				return reason
			}
			if !m.paused {
				return "No paused run to continue"
			}
			return ""
		}},
		paletteItem{group: "Session", title: "Run timing", hint: "/timing", sub: timingItems, searchOnly: true},
		paletteItem{group: "More", title: "Quit", hint: "/quit", aliases: "/exit /q", cmd: "/quit", searchOnly: true},
	)
	for _, item := range settingsItems(m) {
		item.searchOnly = true
		items = append(items, item)
	}
	return items
}

func idleRequired(m *model) string {
	if m.running {
		return "Finish or stop the current run first"
	}
	return ""
}

func settingsItems(m *model) []paletteItem {
	mouse := "Turn mouse on"
	if m.mouse {
		mouse = "Turn mouse off"
	}
	return []paletteItem{
		{group: "Appearance", title: "Change theme", hint: "/theme", sub: themeItems},
		{group: "Input", title: mouse, hint: "/mouse", cmd: "/mouse"},
		{group: "Model", title: "Reasoning effort", hint: "/effort", sub: effortItems, unavailable: func(m *model) string {
			if reason := idleRequired(m); reason != "" {
				return reason
			}
			if m.sess.Agent == nil || m.sess.Agent.Model == nil {
				return "Connect a model first"
			}
			return ""
		}},
	}
}

// modelItems lists enabled provider/model ids from the config.
func modelItems(m *model) []paletteItem {
	cfg, err := config.Load(m.o.Cwd)
	if err != nil {
		return nil
	}
	var items []paletteItem
	for _, pid := range cfg.ConnectionIDs() {
		p := cfg.Connections[pid]
		if p.Disabled {
			continue
		}
		for _, md := range p.Models {
			if md.Disabled {
				continue
			}
			id := pid + "/" + md.ID
			hint := ""
			switch id {
			case m.sess.Name:
				hint = "current"
			case cfg.Default:
				hint = "default"
			}
			items = append(items, paletteItem{group: pid, title: id, hint: hint, cmd: "/model " + id})
		}
	}
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		items = append(items, paletteItem{group: "Profiles", title: name, hint: cfg.Profiles[name].Model, cmd: "/model " + name})
	}
	return items
}

var (
	paletteTitle  lipgloss.Style
	paletteGroup  lipgloss.Style
	paletteRule   lipgloss.Style
	paletteBorder lipgloss.Style
)

// paletteBox renders the palette and returns it with the offset at which
// it sits inside a width×height area, and the cursor position of the
// search field relative to the same area.
func (m *model) paletteBox(width, height int) (box string, x, y int, cursor *tea.Cursor) {
	p := m.pal
	p.box = paletteGeom{}
	inner := min(width-4, paletteWidth)
	if inner < 20 || height < 8 {
		return "", 0, 0, nil
	}
	maxRows := min(height-7, 16)
	p.input.SetWidth(inner - lipgloss.Width(p.input.Prompt) - 1)

	// Rows: group headers interleaved with items (headers only when the
	// list is unfiltered, so ranking is not broken up).
	type row struct {
		text string
		item int // -1 for headers
	}
	var rows []row
	selRow := 0
	grouped := strings.TrimSpace(p.input.Value()) == "" && maxRows > 1
	prev := ""
	for i, it := range p.view {
		if grouped && it.group != prev {
			rows = append(rows, row{text: paletteGroup.Render(" " + it.group), item: -1})
			prev = it.group
		}
		if i == p.sel {
			selRow = len(rows)
		}
		rows = append(rows, row{text: m.paletteRow(it, inner, i == p.sel), item: i})
		detail := it.detail
		if i == p.sel && it.unavailable != nil {
			if reason := it.unavailable(m); reason != "" {
				detail = reason
			}
		}
		if detail != "" && maxRows > 1 {
			style := popupDesc
			if i == p.sel {
				style = popupSelDsc
			}
			rows = append(rows, row{text: style.Width(inner).Render("  " + ansi.Truncate(detail, inner-3, "…")), item: i})
			if i == p.sel {
				selRow = len(rows) - 1
			}
		}
	}
	if len(rows) == 0 {
		rows = append(rows, row{text: popupDesc.Render("  " + ansi.Truncate("no match — enter runs it as a command", inner-2, "…")), item: -1})
	}
	top := 0
	if selRow >= maxRows {
		top = selRow - maxRows + 1
	}
	shown := rows[top:min(len(rows), top+maxRows)]
	if len(shown) > 1 && shown[len(shown)-1].item == -1 {
		shown = shown[:len(shown)-1]
	}

	var path []string
	for _, level := range p.levels {
		path = append(path, level.title)
	}
	title := strings.Join(path, " › ")
	if w := ansi.StringWidth(title); w > inner-6 {
		title = ansi.TruncateLeft(title, w-(inner-7), "…")
	}
	esc := popupDesc.Render("esc")
	head := " " + paletteTitle.Render(ansi.Truncate(title, inner-6, "…"))
	head += strings.Repeat(" ", max(1, inner-lipgloss.Width(head)-lipgloss.Width(esc)-1)) + esc

	var lines []string
	lines = append(lines, head, p.input.View(), paletteRule.Render(strings.Repeat("─", inner)))
	for _, r := range shown {
		lines = append(lines, r.text)
	}
	foot := "↑↓ move · enter choose · esc close"
	if len(p.levels) > 1 {
		foot = "↑↓ move · enter choose · esc back"
	}
	if inner < 40 {
		foot = "↑↓ · enter · esc"
	}
	if len(p.view) > 0 {
		foot += fmt.Sprintf(" · %d/%d", p.sel+1, len(p.view))
		if maxRows == 1 && p.view[p.sel].unavailable != nil {
			if reason := p.view[p.sel].unavailable(m); reason != "" {
				foot = reason
			}
		}
	}
	lines = append(lines, paletteRule.Render(strings.Repeat("─", inner)), popupDesc.Render(" "+ansi.Truncate(foot, inner-2, "…")))
	box = paletteBorder.Width(inner + 2).Render(strings.Join(lines, "\n")) // Width includes the border
	x = (width - inner - 2) / 2
	y = max(0, (height-lipgloss.Height(box))/2)
	p.box = paletteGeom{x: x, y: y, w: inner + 2, h: lipgloss.Height(box)}
	for _, r := range shown {
		p.box.rows = append(p.box.rows, r.item)
	}
	if c := p.input.Cursor(); c != nil {
		c.X += x + 1
		c.Y += y + 2 // top border + title row
		cursor = c
	}
	return box, x, y, cursor
}

func (m *model) paletteRow(it paletteItem, inner int, selected bool) string {
	unavailable := it.unavailable != nil && it.unavailable(m) != ""
	hint := ansi.Truncate(it.hint, max(0, inner/2-2), "…")
	if it.sessionID != "" {
		hint = ansi.Truncate(it.hint, max(0, inner/2-10), "…") + " Actions"
	}
	titleW := inner - 4 - lipgloss.Width(hint)
	title := ansi.Truncate(it.title, max(8, titleW), "…")
	gap := max(1, inner-2-lipgloss.Width(title)-lipgloss.Width(hint))
	if selected {
		// Some terminals map bold ANSI black to gray, losing contrast on the
		// selected background. The background already indicates selection.
		return popupSel.Bold(false).Render("  "+title) + popupSelDsc.Render(strings.Repeat(" ", gap-1)+hint+" ")
	}
	style := textStyle
	if unavailable {
		style = popupDesc
	}
	return "  " + style.Render(title) + strings.Repeat(" ", gap-1) + popupDesc.Render(hint) + " "
}

// muteBackdrop removes competing highlights without changing layout. Terminal
// text cannot be blurred. Use a low-contrast colour rather than relying on
// terminal-specific support for the faint attribute.
func muteBackdrop(s string) string {
	lines := strings.Split(ansi.Strip(s), "\n")
	colour := c("#45484c")
	if theme.Name == "light" {
		colour = c("#afb2b6")
	}
	style := dimStyle.Foreground(colour)
	for i := range lines {
		lines[i] = style.Render(lines[i])
	}
	return strings.Join(lines, "\n")
}

// overlay draws box at x,y on top of base, which must be width×height.
func overlay(base, box string, x, y int) string {
	return lipgloss.NewCompositor(lipgloss.NewLayer(base), lipgloss.NewLayer(box).X(x).Y(y)).Render()
}
