package tui

import (
	"sort"

	tea "charm.land/bubbletea/v2"
)

// span records where a block's lines sit in the transcript content so a
// click can be mapped back to the block.
type span struct {
	top, bottom int // content line range, bottom exclusive
	b           *block
	group       *block // first member, nil for a standalone block
	header      bool   // group header rather than a member's row
	work        *block // containing completed turn
	workHeader  bool
}

const wheelLines = 1

// Rendered spans are ordered and nonoverlapping. Separators and animation
// rows between them are not interactive.
func (m *model) spanAt(line int) (span, bool) {
	i := sort.Search(len(m.spans), func(i int) bool { return m.spans[i].bottom > line })
	if i < len(m.spans) && m.spans[i].top <= line {
		return m.spans[i], true
	}
	return span{}, false
}

// handleMouse routes wheel, motion and left-click events. Mouse reporting
// is on while m.mouse is set (View sets MouseModeAllMotion); /mouse turns
// it off so terminals that need it for native text selection get it back.
func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	mo := msg.Mouse()
	if m.inputDragging {
		switch msg.(type) {
		case tea.MouseMotionMsg:
			m.input.ExtendSelection(mo.X-2, mo.Y-m.inputY())
			return m, nil
		case tea.MouseReleaseMsg:
			m.input.EndSelection()
			m.inputDragging = false
			return m, nil
		}
	}
	m.scrollHover = m.scrollHit(mo.X, mo.Y)
	if _, wheel := msg.(tea.MouseWheelMsg); !wheel && m.scrollHover {
		m.clearSelection()
		if _, pressed := msg.(tea.MouseClickMsg); pressed && mo.Button == tea.MouseLeft {
			m.vp.GotoBottom()
			m.stickBottom = true
			m.scrollHover = false
		}
		return m, nil
	}
	if _, wheel := msg.(tea.MouseWheelMsg); !wheel && m.noticeHit(mo.X, mo.Y) {
		// The covered transcript must not receive clicks or selections.
		m.clearSelection()
		return m, nil
	}
	switch msg.(type) {
	case tea.MouseWheelMsg:
		dir := 0
		switch mo.Button {
		case tea.MouseWheelUp:
			dir = -1
		case tea.MouseWheelDown:
			dir = 1
		default:
			return m, nil
		}
		if m.pending == nil && m.pal == nil && m.panel == nil && mo.Y >= m.inputY() && mo.Y < m.inputY()+m.input.Height() {
			// Scroll one visual row. The textarea keeps its cursor visible, so
			// move it to the edge first rather than scrolling the transcript.
			if dir < 0 {
				if c := m.input.Cursor(); c != nil && c.Y > 0 {
					m.input.PageUp()
				}
				m.input.CursorUp()
			} else {
				if c := m.input.Cursor(); c != nil && c.Y < m.input.Height()-1 {
					m.input.PageDown()
				}
				m.input.CursorDown()
			}
		} else {
			m.wheel(dir)
		}
	case tea.MouseMotionMsg:
		switch mo.Button {
		case tea.MouseLeft:
			m.selectDrag(mo.X, mo.Y)
		case tea.MouseNone:
			m.hoverAt(mo.X, mo.Y)
		}
	case tea.MouseReleaseMsg:
		if mo.Button != tea.MouseLeft {
			return m, nil
		}
		clicked, cmd := m.selectRelease()
		if clicked {
			if s, ok := m.spanAt(m.vp.YOffset() + mo.Y); ok && s.b.kind == blockUser && s.b.prompt != 0 {
				index := s.b.prompt
				return m, m.openPaletteSub("Sent prompt", []paletteItem{
					{title: "Edit and resend…", action: func(m *model) tea.Cmd { return m.requestPromptEdit(index) }},
					{title: "Remove from here…", action: func(m *model) tea.Cmd { return m.confirmPromptRemove(index) }},
					{title: "Close", action: func(m *model) tea.Cmd { return m.closePalette() }},
				})
			}
			m.transcriptClick(m.vp.YOffset() + mo.Y)
		}
		return m, cmd
	case tea.MouseClickMsg:
		if mo.Button != tea.MouseLeft {
			return m, nil
		}
		if m.pending != nil {
			m.approvalClick(mo.X, mo.Y)
			return m, nil
		}
		m.clearSelection()
		if m.pal == nil && m.panel == nil && mo.Y >= m.inputY() && mo.Y < m.inputY()+m.input.Height() {
			m.input.BeginSelection(mo.X-2, mo.Y-m.inputY())
			m.inputDragging = true
			return m, nil
		}
		m.input.ClearSelection()
		if mo.Y >= m.vp.Height() {
			// Below the transcript: the chip row's × buttons and the
			// toolbar pills are live.
			if m.pal == nil && m.panel == nil && len(m.attachments) > 0 && mo.Y == m.chipRowY() {
				m.chipClick(mo.X - 2) // inside the left border and padding
				return m, nil
			}
			if m.pal == nil && m.pending == nil {
				cmd, _ := m.footerClick(mo.X, mo.Y)
				return m, cmd
			}
			return m, nil
		}
		switch {
		case m.pal != nil:
			return m, m.paletteClick(mo.X, mo.Y)
		case m.panel != nil:
			return m, m.panelClick(mo.X, mo.Y)
		default:
			// Cards toggle on release (see MouseReleaseMsg) so a drag can
			// start on one without flipping it.
			m.selectPress(mo.X, mo.Y)
		}
	}
	return m, nil
}

func (m *model) inputY() int {
	y := m.vp.Height() + 1
	if len(m.attachments) > 0 {
		y++
	}
	return y
}

func (m *model) wheel(dir int) {
	switch {
	case m.pending != nil:
		m.pending.scroll(dir * wheelLines)
	case m.pal != nil:
		m.pal.move(dir)
	case m.panel != nil:
		m.panel.wheel(dir)
	case dir < 0:
		m.vp.ScrollUp(wheelLines)
		m.stickBottom = m.vp.AtBottom()
	default:
		m.vp.ScrollDown(wheelLines)
		m.stickBottom = m.vp.AtBottom()
	}
}

// transcriptClick toggles the tool card or reasoning block on the given
// content line.
func (m *model) transcriptClick(line int) {
	if s, ok := m.spanAt(line); ok {
		if !s.workHeader && s.b.kind != blockTool && s.b.kind != blockReasoning {
			return
		}
		// Keep the clicked header at its screen row when an earlier detail
		// closes or this one grows, instead of jumping to the transcript end.
		y := s.top - m.vp.YOffset()
		m.stickBottom = false
		defer func() {
			for _, next := range m.spans {
				if next.b == s.b && next.header == s.header && next.workHeader == s.workHeader {
					m.vp.ScrollDown(next.top - y - m.vp.YOffset())
					break
				}
			}
			m.stickBottom = m.openWork == nil && m.openGroup == nil && m.openDetail == nil && m.vp.AtBottom()
		}()
		if s.workHeader {
			if m.openWork == s.work {
				m.openWork = nil
			} else {
				m.openWork = s.work
			}
			m.openDetail, m.openGroup = nil, nil
			m.refresh()
			return
		}
		m.openWork = s.work
		if s.header {
			if m.openGroup == s.group {
				m.openGroup = nil
			} else {
				m.openGroup = s.group
			}
			m.openDetail = nil
			m.refresh()
			return
		}
		if s.b.kind == blockTool || s.b.kind == blockReasoning {
			m.openGroup = s.group
			m.toggleDetail(s.b)
		}
		return
	}
}

func (m *model) toggleDetail(b *block) {
	if m.openDetail == b {
		m.openDetail = nil
	} else {
		m.openDetail = b
	}
	m.refresh()
}

// paletteClick runs the row under x,y, or closes the palette when the
// click lands outside the box.
func (m *model) paletteClick(x, y int) tea.Cmd {
	p := m.pal
	if p.box.w == 0 {
		return nil
	}
	if x < p.box.x || x >= p.box.x+p.box.w || y < p.box.y || y >= p.box.y+p.box.h {
		return m.closePalette()
	}
	row := y - p.box.y - paletteHeaderRows - 1 // top border + title, search, rule
	if row < 0 || row >= len(p.box.rows) || p.box.rows[row] < 0 {
		return nil
	}
	p.sel = p.box.rows[row]
	if id := p.view[p.sel].sessionID; id != "" && x >= p.box.x+p.box.w-10 {
		return m.openSessionActions(id)
	}
	return p.activate(m)
}

// hoverAt records what sits under the pointer so it can be drawn lit. Only
// a change of target touches state, so sweeping the mouse across a card
// costs one re-render at each edge, not one per cell.
func (m *model) hoverAt(x, y int) {
	act := actNone
	m.hoverChip = 0
	var hb *block
	var hg *block
	var hw *block
	switch {
	case m.pal != nil:
		m.paletteHover(x, y)
	case m.panel != nil:
		m.panelHover(x, y)
	case m.pending != nil:
		m.approvalHover(x, y)
	case len(m.attachments) > 0 && y == m.chipRowY():
		for _, h := range m.chipHits {
			if x-2 >= h.x0 && x-2 < h.x1 {
				m.hoverChip = h.i + 1
				break
			}
		}
	case y == m.toolbarY():
		act = m.footerHitAt(x)
	case y < m.vp.Height():
		line := m.vp.YOffset() + y
		if s, ok := m.spanAt(line); ok {
			if s.workHeader {
				hw = s.work
			} else if s.header {
				hg = s.group
			} else if s.b.kind == blockTool || s.b.kind == blockReasoning {
				hb = s.b
			}
		}
	}
	m.hoverAct = act // footer re-renders through its cache key
	if hb != m.hoverBlock || hg != m.hoverGroup || hw != m.hoverWork {
		m.hoverBlock = hb
		m.hoverGroup = hg
		m.hoverWork = hw
		m.refresh()
	}
}

// paletteHover moves the selection to the row under the pointer.
func (m *model) paletteHover(x, y int) {
	p := m.pal
	if p.box.w == 0 || x < p.box.x || x >= p.box.x+p.box.w {
		return
	}
	row := y - p.box.y - paletteHeaderRows - 1
	if row < 0 || row >= len(p.box.rows) || p.box.rows[row] < 0 {
		return
	}
	p.sel = p.box.rows[row]
}

// panelHover moves the list or picker cursor to the row under the pointer
// and lights the button under it.
func (m *model) panelHover(x, y int) {
	p := m.panel
	b := p.box
	p.hoverBtn = ""
	if p.form != nil {
		p.form.setHover(-1, -1)
	}
	if b.w == 0 || x < b.x || x >= b.x+b.w || y < b.y || y >= b.y+b.h {
		return
	}
	ix, iy := x-b.x-1, y-b.y-1
	if p.mode == pmAdd || p.mode == pmEdit {
		p.form.setHover(ix, iy-b.formY+b.formTop)
		return
	}
	for i, r := range b.buttons {
		if r.contains(ix, iy) {
			p.hoverBtn = b.buttonIDs[i]
			if p.mode == pmPick {
				p.pick = len(p.visible) + i
			}
			return
		}
	}
	li := iy - b.bodyY
	if li < 0 || li >= len(b.rows) || b.rows[li] < 0 {
		return
	}
	switch p.mode {
	case pmList:
		p.cursor = b.rows[li]
	case pmPick:
		p.pick = b.rows[li]
	}
}
