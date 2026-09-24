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

	"github.com/dantearo/arkex/internal/agent"
	"github.com/dantearo/arkex/internal/config"
)

// paletteItem is one row of the command palette. Commands share the typed
// slash-command path; local dialogs use action, and sub opens a nested list.
type paletteItem struct {
	group     string
	title     string
	hint      string // key or command shown right-aligned
	cmd       string // slash command run on enter
	sub       func(m *model) []paletteItem
	action    func(*model) tea.Cmd
	sessionID string // session row: enter resumes, ctrl+o / Actions manages
	detail    string // session model and last activity, shown below the title
}

// paletteLevel is one list in the palette: the top level or a nested one.
type paletteLevel struct {
	title string
	items []paletteItem
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
	paletteMinRows    = 4
	paletteHeaderRows = 3 // title, search, rule
)

func (m *model) openPalette() tea.Cmd {
	if m.pal != nil && m.pal.cancel != nil {
		m.pal.cancel()
	}
	ti := textinput.New()
	ti.Prompt = "  "
	ti.Placeholder = "Search"
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
	p.levels = append(p.levels, l)
	p.input.Reset()
	p.filter()
}

func (p *palette) level() paletteLevel { return p.levels[len(p.levels)-1] }

// filter recomputes the visible rows. With no search text rows keep their
// registration order (grouped); with text they are ranked by fuzzy score
// against the title, falling back to group and hint.
func (p *palette) filter() {
	q := strings.TrimSpace(p.input.Value())
	items := p.level().items
	if q == "" || p.editing || p.busy {
		p.view = items
	} else {
		type scored struct {
			it    paletteItem
			score int
		}
		var hits []scored
		seen := map[string]bool{} // Suggested repeats rows from other groups
		for _, it := range items {
			identity := it.title + "\x00" + it.sessionID
			if seen[identity] {
				continue
			}
			seen[identity] = true
			s, ok := fuzzyScore(q, it.title)
			if !ok {
				s, ok = fuzzyScore(q, it.group+" "+it.title+" "+it.hint+" "+it.detail)
				s -= 500
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
			p.input.Reset()
			p.sel = 0
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
	if it.action != nil {
		return it.action(m)
	}
	if it.sub != nil {
		items := it.sub(m)
		if len(items) == 0 {
			items = []paletteItem{{group: it.group, title: "nothing configured", cmd: "/connections", hint: "/connections"}}
		}
		p.sel = 0
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
	next := m.mode().Next()
	var items []paletteItem
	if m.sess.Agent == nil {
		items = append(items, paletteItem{group: "Suggested", title: "Connections", hint: "/connections", cmd: "/connections"})
	} else {
		items = append(items, paletteItem{group: "Suggested", title: "Switch model", hint: "/model", sub: modelItems})
	}
	items = append(items,
		paletteItem{group: "Suggested", title: "Switch to " + string(next) + " mode", hint: "/mode", cmd: "/mode " + string(next)},
		paletteItem{group: "Suggested", title: "New conversation", hint: "/clear", cmd: "/clear"},
	)
	if m.conv == nil && m.hasSessions() {
		items = append(items, paletteItem{group: "Suggested", title: "Resume session", hint: "/resume", sub: sessionItems})
	}

	mouse := "Turn mouse on"
	if m.mouse {
		mouse = "Turn mouse off"
	}
	selection := m.input.SelectedText()
	if selection == "" {
		selection = m.selectedText()
	}
	items = append(items,
		paletteItem{group: "Copy", title: "Copy selection", hint: "ctrl+shift+c", action: func(m *model) tea.Cmd {
			return tea.Batch(m.closePalette(), m.copyText(selection))
		}},
		paletteItem{group: "Copy", title: "Copy entire prompt", action: func(m *model) tea.Cmd {
			return tea.Batch(m.closePalette(), m.copyText(m.input.Value()))
		}},
		paletteItem{group: "Copy", title: "Copy response or code", sub: responseCopyItems},
		paletteItem{group: "Session", title: "New conversation", hint: "/clear", cmd: "/clear"},
		paletteItem{group: "Session", title: "Resume session", hint: "/resume", sub: sessionItems},
		paletteItem{group: "Session", title: "Edit sent prompt", hint: "tab · enter", sub: promptEditItems},
		paletteItem{group: "Model", title: "Reasoning effort", hint: "/effort", sub: effortItems},
		paletteItem{group: "Session", title: "Run timing", hint: "/timing", sub: timingItems},
		paletteItem{group: "Session", title: mouse, hint: "/mouse", cmd: "/mouse"},
		paletteItem{group: "Session", title: "Change theme", hint: "/theme", sub: themeItems},
		paletteItem{group: "Session", title: "Help", hint: "/help", cmd: "/help"},
		paletteItem{group: "Session", title: "Quit", hint: "ctrl+c twice", cmd: "/quit"},

		paletteItem{group: "Models", title: "Switch model", hint: "/model", sub: modelItems},
		paletteItem{group: "Models", title: "Connections", hint: "/connections", cmd: "/connections"},
	)
	for _, md := range agent.Modes {
		hint := "/mode " + string(md)
		if md == m.mode() {
			hint = "current"
		}
		items = append(items, paletteItem{group: "Mode", title: modeTitle(md), hint: hint, cmd: "/mode " + string(md)})
	}
	return items
}

func modeTitle(md agent.Mode) string {
	switch md {
	case agent.ModePlan:
		return "Plan mode — read-only tools, the model writes a plan"
	case agent.ModeAuto:
		return "Auto mode — trusted scope runs quietly; config denies apply"
	}
	return "Build mode — edits and commands ask per config"
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
	if inner < 24 || height < paletteMinRows+5 {
		return "", 0, 0, nil
	}
	maxRows := max(paletteMinRows, min(height-6, 16))
	p.input.SetWidth(inner - lipgloss.Width(p.input.Prompt) - 1)

	// Rows: group headers interleaved with items (headers only when the
	// list is unfiltered, so ranking is not broken up).
	type row struct {
		text string
		item int // -1 for headers
	}
	var rows []row
	selRow := 0
	grouped := strings.TrimSpace(p.input.Value()) == ""
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
		if it.detail != "" {
			style := popupDesc
			if i == p.sel {
				style = popupSelDsc
			}
			rows = append(rows, row{text: style.Width(inner).Render("  " + ansi.Truncate(it.detail, inner-3, "…")), item: i})
		}
	}
	if len(rows) == 0 {
		rows = append(rows, row{text: popupDesc.Render("  no match — enter runs it as a command"), item: -1})
	}
	top := 0
	if selRow >= maxRows {
		top = selRow - maxRows + 1
	}
	shown := rows[top:min(len(rows), top+maxRows)]

	title := p.level().title
	if len(p.levels) > 1 {
		title = "‹ " + title
	}
	esc := popupDesc.Render("esc")
	head := " " + paletteTitle.Render(ansi.Truncate(title, inner-6, "…"))
	head += strings.Repeat(" ", max(1, inner-lipgloss.Width(head)-lipgloss.Width(esc)-1)) + esc

	var lines []string
	lines = append(lines, head, p.input.View(), paletteRule.Render(strings.Repeat("─", inner)))
	for _, r := range shown {
		lines = append(lines, r.text)
	}
	if rest := len(rows) - top - len(shown); rest > 0 {
		lines = append(lines, popupDesc.Render(fmt.Sprintf("  … %d more", rest)))
	}
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
	hint := ansi.Truncate(it.hint, max(0, inner/2-2), "…")
	if it.sessionID != "" {
		hint = ansi.Truncate(it.hint, max(0, inner/2-10), "…") + " Actions"
	}
	titleW := inner - 4 - lipgloss.Width(hint)
	title := ansi.Truncate(it.title, max(8, titleW), "…")
	gap := max(1, inner-2-lipgloss.Width(title)-lipgloss.Width(hint))
	if selected {
		return popupSel.Render(" "+title) + popupSelDsc.Render(strings.Repeat(" ", gap)+hint+" ")
	}
	return "  " + textStyle.Render(title) + strings.Repeat(" ", gap-1) + popupDesc.Render(hint) + " "
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
