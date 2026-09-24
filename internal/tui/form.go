package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// A small form widget for the Connections panel: a vertical list of
// fields with one focus, driven by tab/shift+tab/↑↓, enter acting on the
// focused field, and clicks mapped through the rectangles the last render
// recorded. It knows nothing about connections; the panel builds fields
// and reacts to the action ids the form returns.

type fieldKind int

const (
	fText fieldKind = iota
	fSecret
	fChoice
	fCheck
	fButton
	fNote // static text, never focused
)

// choiceOpt is one option of a segmented choice.
type choiceOpt struct {
	label    string
	disabled bool // shown dim with "soon", not selectable
}

// rect is a clickable area in form-relative coordinates.
type rect struct{ x, y, w, h int }

func (r rect) contains(x, y int) bool {
	return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h
}

type field struct {
	id      string
	kind    fieldKind
	label   string // left column; empty for buttons
	help    string // dim text under the field
	err     string // red text under the field
	inline  bool   // render on the same row as the previous field (buttons, show/hide)
	hidden  bool   // skipped entirely
	gap     bool   // blank line above: starts a new group
	primary bool   // button drawn filled: the action this screen is for

	input   textinput.Model // fText, fSecret
	options []choiceOpt     // fChoice
	sel     int             // fChoice
	on      bool            // fCheck
	text    string          // fNote

	rects []rect // last render; one per option for choices, else one
}

type form struct {
	fields []*field
	focus  int
	width  int // inner width used by the last render
	narrow bool
	hx, hy int // pointer position from the last motion event, form-relative; -1 when outside
}

const formNarrow = 56

func newForm() *form { return &form{hx: -1, hy: -1} }

// setHover records the pointer position so the next render lights the
// option or button under it.
func (f *form) setHover(x, y int) { f.hx, f.hy = x, y }

func (f *form) hovering(r rect) bool { return f.hx >= 0 && r.contains(f.hx, f.hy) }

func (f *form) add(fl *field) *field {
	f.fields = append(f.fields, fl)
	return fl
}

func textField(id, label, value, placeholder string) *field {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = placeholder
	ti.SetVirtualCursor(false)
	ti.SetStyles(fieldStyles())
	ti.SetValue(value)
	ti.CursorEnd()
	return &field{id: id, kind: fText, label: label, input: ti}
}

// fieldStyles underlines the whole field so its extent is visible; the
// focused state also takes the accent colour. Styling has to go through
// the textinput because its view resets attributes between segments.
func fieldStyles() textinput.Styles {
	var s textinput.Styles
	s.Focused.Text = formFieldFoc
	s.Focused.Placeholder = formFieldFoc.Foreground(theme.Muted)
	s.Blurred.Text = formField
	s.Blurred.Placeholder = formField.Foreground(theme.Muted)
	s.Cursor.Color = theme.Accent
	return s
}

func secretField(id, label, value, placeholder string) *field {
	fl := textField(id, label, value, placeholder)
	fl.kind = fSecret
	fl.input.EchoMode = textinput.EchoPassword
	fl.input.EchoCharacter = '•'
	return fl
}

func choiceField(id, label string, opts []choiceOpt, sel int) *field {
	return &field{id: id, kind: fChoice, label: label, options: opts, sel: sel}
}

func checkField(id, label string, on bool) *field {
	return &field{id: id, kind: fCheck, label: label, on: on}
}

func buttonField(id, label string) *field {
	return &field{id: id, kind: fButton, label: label}
}

func noteField(text string) *field {
	return &field{kind: fNote, text: text}
}

func (f *form) get(id string) *field {
	for _, fl := range f.fields {
		if fl.id == id {
			return fl
		}
	}
	return nil
}

func (f *form) value(id string) string {
	if fl := f.get(id); fl != nil {
		return strings.TrimSpace(fl.input.Value())
	}
	return ""
}

func (f *form) focused() *field {
	if f.focus >= 0 && f.focus < len(f.fields) {
		return f.fields[f.focus]
	}
	return nil
}

func (fl *field) focusable() bool { return !fl.hidden && fl.kind != fNote }

// setFocus moves focus to index i (clamped to a focusable field), blurring
// and focusing text inputs as needed.
func (f *form) setFocus(i int) tea.Cmd {
	if len(f.fields) == 0 {
		return nil
	}
	if i < 0 || i >= len(f.fields) || !f.fields[i].focusable() {
		// Fall back to the first focusable field.
		i = -1
		for j, fl := range f.fields {
			if fl.focusable() {
				i = j
				break
			}
		}
		if i < 0 {
			return nil
		}
	}
	var cmd tea.Cmd
	for j, fl := range f.fields {
		if fl.kind != fText && fl.kind != fSecret {
			continue
		}
		if j == i {
			cmd = fl.input.Focus()
		} else {
			fl.input.Blur()
		}
	}
	f.focus = i
	return cmd
}

// focusID focuses the field with the given id.
func (f *form) focusID(id string) tea.Cmd {
	for i, fl := range f.fields {
		if fl.id == id {
			return f.setFocus(i)
		}
	}
	return nil
}

// move shifts focus by dir, skipping hidden fields and clamping at the ends.
func (f *form) move(dir int) tea.Cmd {
	i := f.focus
	for {
		i += dir
		if i < 0 || i >= len(f.fields) {
			return nil
		}
		if f.fields[i].focusable() {
			return f.setFocus(i)
		}
	}
}

// key handles a key press. It returns the id of the button pressed (or of
// a choice/check whose value changed, prefixed "changed:"), and a command.
func (f *form) key(k tea.KeyPressMsg) (action string, cmd tea.Cmd) {
	fl := f.focused()
	if fl == nil {
		return "", nil
	}
	s := k.String()
	switch s {
	case "tab", "down":
		return "", f.move(1)
	case "shift+tab", "up":
		return "", f.move(-1)
	}
	isText := fl.kind == fText || fl.kind == fSecret
	switch fl.kind {
	case fChoice:
		switch s {
		case "left", "h":
			fl.pick(-1)
			return "changed:" + fl.id, nil
		case "right", "l", "space", " ":
			fl.pick(1)
			return "changed:" + fl.id, nil
		case "enter":
			return "", f.move(1)
		}
		return "", nil
	case fCheck:
		switch s {
		case "space", " ", "enter", "x":
			fl.on = !fl.on
			return "changed:" + fl.id, nil
		}
		return "", nil
	case fButton:
		switch s {
		case "enter", "space", " ":
			return fl.id, nil
		}
		return "", nil
	}
	if isText {
		if s == "enter" {
			return "", f.move(1)
		}
		fl.input, cmd = fl.input.Update(k)
		return "", cmd
	}
	return "", nil
}

// update forwards non-key messages (paste, clipboard) to the focused text
// field.
func (f *form) update(msg tea.Msg) tea.Cmd {
	fl := f.focused()
	if fl == nil || (fl.kind != fText && fl.kind != fSecret) {
		return nil
	}
	var cmd tea.Cmd
	fl.input, cmd = fl.input.Update(msg)
	return cmd
}

// click maps a form-relative click to a field. Text fields take focus;
// checks toggle; choices select the option under the pointer; buttons
// return their id.
func (f *form) click(x, y int) (action string, cmd tea.Cmd) {
	for i, fl := range f.fields {
		if !fl.focusable() {
			continue
		}
		for oi, r := range fl.rects {
			if !r.contains(x, y) {
				continue
			}
			cmd = f.setFocus(i)
			switch fl.kind {
			case fChoice:
				if oi < len(fl.options) && !fl.options[oi].disabled && fl.sel != oi {
					fl.sel = oi
					return "changed:" + fl.id, cmd
				}
				return "", cmd
			case fCheck:
				fl.on = !fl.on
				return "changed:" + fl.id, cmd
			case fButton:
				return fl.id, cmd
			}
			return "", cmd
		}
	}
	return "", nil
}

// pick moves a choice selection by dir, skipping disabled options.
func (fl *field) pick(dir int) {
	n := len(fl.options)
	if n == 0 {
		return
	}
	for step := 1; step <= n; step++ {
		i := (fl.sel + dir*step + n*step) % n
		if !fl.options[i].disabled {
			fl.sel = i
			return
		}
	}
}

// cursor returns the terminal cursor of the focused text field, in
// form-relative coordinates, or nil.
func (f *form) cursor() *tea.Cursor {
	fl := f.focused()
	if fl == nil || (fl.kind != fText && fl.kind != fSecret) || len(fl.rects) == 0 {
		return nil
	}
	c := fl.input.Cursor()
	if c == nil {
		return nil
	}
	c.X += fl.rects[0].x
	c.Y += fl.rects[0].y
	return c
}

// ---- rendering ----

var (
	formLabel    lipgloss.Style
	formField    lipgloss.Style
	formFieldFoc lipgloss.Style
	formButton   lipgloss.Style
	formButtonFc lipgloss.Style
	formButtonHv lipgloss.Style
	formChoice   lipgloss.Style
	formChoiceOn lipgloss.Style
	formChoiceHv lipgloss.Style
)

// render draws the form at width and records click rectangles. Rows are
// "label  field" side by side, or label above field when narrow.
func (f *form) render(width int) []string {
	f.width = width
	f.narrow = width < formNarrow
	labelW := 0
	for _, fl := range f.fields {
		if !fl.hidden && fl.label != "" && fl.kind != fButton && fl.kind != fCheck {
			labelW = max(labelW, lipgloss.Width(fl.label))
		}
	}
	labelW = min(labelW, 14)
	indent := 2 // room for the focus marker
	fieldX := indent + labelW + 2
	if f.narrow {
		fieldX = indent
	}
	fieldW := max(8, width-fieldX-1)

	var lines []string
	for _, fl := range f.fields {
		fl.rects = fl.rects[:0]
	}
	i := 0
	for i < len(f.fields) {
		fl := f.fields[i]
		if fl.hidden {
			i++
			continue
		}
		// Gather an inline group: this field plus following inline ones.
		group := []*field{fl}
		j := i + 1
		for j < len(f.fields) && (f.fields[j].hidden || f.fields[j].inline) {
			if !f.fields[j].hidden {
				group = append(group, f.fields[j])
			}
			j++
		}
		i = j

		focused := false
		for _, g := range group {
			if g == f.focused() {
				focused = true
			}
		}
		marker := "  "
		if focused {
			marker = selStyle.Render("› ")
		}
		if fl.gap && len(lines) > 0 {
			lines = append(lines, "")
		}

		switch fl.kind {
		case fNote:
			for _, l := range strings.Split(ansi.Wordwrap(fl.text, max(width-2, 8), " "), "\n") {
				lines = append(lines, "  "+dimStyle.Render(l))
			}
			continue
		case fButton:
			// A row of buttons.
			x := indent
			var parts []string
			y := len(lines)
			for _, g := range group {
				txt := "  " + g.label + "  "
				w := lipgloss.Width(txt)
				if x+w > width && len(parts) > 0 {
					lines = append(lines, marker+strings.Join(parts, "  "))
					parts, x, y = nil, indent, len(lines)
					marker = "  "
				}
				r := rect{x: x, y: y, w: w, h: 1}
				g.rects = append(g.rects, r)
				parts = append(parts, f.buttonStyle(g, r).Render(txt))
				x += w + 2
			}
			lines = append(lines, marker+strings.Join(parts, "  "))
			continue
		}

		// Labelled field.
		label := ""
		if fl.kind != fCheck {
			label = formLabel.Render(padRight(ansi.Truncate(fl.label, labelW, "…"), labelW)) + "  "
		}
		if f.narrow && fl.kind != fCheck {
			lines = append(lines, marker+strings.TrimRight(label, " "))
			marker = "  "
		}
		y := len(lines)
		prefix := marker
		if !f.narrow && fl.kind != fCheck {
			prefix += label
		}
		x := lipgloss.Width(prefix)

		switch fl.kind {
		case fText, fSecret:
			// Trailing inline fields (e.g. the show/hide button) share the row.
			extra := 0
			for _, g := range group[1:] {
				extra += lipgloss.Width("  "+g.label+"  ") + 1
			}
			w := max(8, fieldW-extra)
			fl.input.SetStyles(fieldStyles())
			fl.input.SetWidth(w - 1) // the view is Width+1 wide
			view := ansi.Truncate(fl.input.View(), w, "")
			pad := formField
			if fl == f.focused() {
				pad = formFieldFoc
			}
			if n := w - ansi.StringWidth(view); n > 0 {
				view += pad.Render(strings.Repeat(" ", n))
			}
			fl.rects = append(fl.rects, rect{x: x, y: y, w: w, h: 1})
			line := prefix + view
			bx := x + w + 1
			for _, g := range group[1:] {
				txt := "  " + g.label + "  "
				bw := lipgloss.Width(txt)
				r := rect{x: bx, y: y, w: bw, h: 1}
				g.rects = append(g.rects, r)
				line += " " + f.buttonStyle(g, r).Render(txt)
				bx += bw + 1
			}
			lines = append(lines, line)

		case fChoice:
			// Options flow left to right, wrapping onto new rows.
			line := prefix
			cx := x
			for oi, opt := range fl.options {
				txt := " " + opt.label + " "
				if opt.disabled {
					txt = " " + opt.label + " · soon "
				}
				w := lipgloss.Width(txt)
				if cx+w > width && cx > x {
					lines = append(lines, line)
					y = len(lines)
					line = strings.Repeat(" ", x)
					cx = x
				}
				r := rect{x: cx, y: y, w: w, h: 1}
				fl.rects = append(fl.rects, r)
				switch {
				case opt.disabled:
					line += dimStyle.Render(txt)
				case f.hovering(r):
					line += formChoiceHv.Bold(oi == fl.sel).Render(txt)
				case oi == fl.sel:
					line += formChoiceOn.Render(txt)
				default:
					line += formChoice.Render(txt)
				}
				line += " "
				cx += w + 1
			}
			lines = append(lines, line)

		case fCheck:
			box := "[ ]"
			if fl.on {
				box = okStyle.Render("[x]")
			}
			txt := box + " " + fl.label
			r := rect{x: x, y: y, w: lipgloss.Width(txt), h: 1}
			fl.rects = append(fl.rects, r)
			if f.hovering(r) {
				txt = fillRow(txt, r.w)
			}
			lines = append(lines, prefix+txt)
		}

		if fl.err != "" {
			lines = append(lines, strings.Repeat(" ", fieldX)+errStyle.Render(ansi.Truncate(fl.err, width-fieldX, "…")))
		} else if fl.help != "" {
			lines = append(lines, strings.Repeat(" ", fieldX)+dimStyle.Render(ansi.Truncate(fl.help, width-fieldX, "…")))
		}
	}
	return lines
}

// buttonStyle picks the button look: focused buttons fill with the accent,
// the primary action is bold, and the pointer lights whatever it is over.
func (f *form) buttonStyle(g *field, r rect) lipgloss.Style {
	switch {
	case f.hovering(r):
		return formButtonHv.Bold(g.primary || g == f.focused())
	case g == f.focused():
		return formButtonFc
	case g.primary:
		return formButton.Bold(true)
	}
	return formButton
}

func padRight(s string, w int) string {
	if n := w - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
