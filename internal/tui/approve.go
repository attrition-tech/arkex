package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/sanitize"
	"github.com/attrition-tech/arkex/internal/tools"
)

// While a tool call waits for the user, the input box becomes the question.
// Nothing can be typed anyway, so the frame the eye already rests on shows
// what the tool wants to do, why arkex is asking, and the answers:
//
//	╭──────────────────────────────────────────────────────────────╮
//	│ ● bash needs your permission                                 │
//	│   $ grep -rn level /logging/config.yaml                      │
//	│   ⚠ command names a path outside the workspace: /logging     │
//	│                                                              │
//	│   ▶ Allow  enter   Allow this session  a   Deny  n / esc     │
//	╰──────────────────────────────────────────────────────────────╯
//
// ←/→ or tab move between the answers, enter takes the marked one, and each
// answer has its own key. The pointer lights an answer and a click takes it.

// approval is the pending prompt plus its interaction state.
type approval struct {
	title     string // optional non-permission dialog title
	retry     bool
	call      agent.ToolCall
	reply     chan agent.Answer
	buttons   []approveButton
	sel       int // marked answer; enter takes it
	hover     int // answer under the pointer, -1 for none
	hits      []approveHit
	buttonRow int    // first action row within the box content
	rows      int    // content rows drawn last time, for layout
	preview   string // proposed text only; never read a file before approval
	caption   string
	lines     []string // wrapped preview, cached by width
	width     int
	offset    int
	page      int
}

type approveButton struct {
	label  string
	keys   string // shown after the label, e.g. "enter", "n / esc"
	answer agent.Answer
}

type approveHit struct {
	x0, x1, idx int
	y           int // relative to the first action row
}

// approveBodyLines caps how many rows the command or path may take.
const approveBodyLines = 3

// newApproval builds the prompt for call. The session answer is offered
// only when the policy said it would honour it.
func newApproval(call agent.ToolCall, reply chan agent.Answer) *approval {
	a := &approval{call: call, reply: reply, hover: -1}
	a.buttons = []approveButton{{label: "Allow", keys: "enter", answer: agent.AllowOnce}}
	if call.Grantable {
		label := "Allow this session"
		if call.TrustDirectory != "" {
			a.buttons[0].label = "Allow once"
			label = "Trust directory"
		}
		a.buttons = append(a.buttons, approveButton{label: label, keys: "a", answer: agent.AllowSession})
	}
	a.buttons = append(a.buttons, approveButton{label: "Deny", keys: "n / esc", answer: agent.Deny})
	args := toolArgs(call.Input)
	str := func(key string) string {
		v, _ := args[key].(string)
		return sanitize.Terminal(strings.ReplaceAll(v, "\t", "    "))
	}
	switch call.Name {
	case "bash":
		a.caption = "Full command · shell paths are heuristic, not a sandbox"
		a.preview = "Starting directory: " + sanitize.Terminal(call.Workdir) + "\n$ " + str("command")
	case "edit":
		a.preview = tools.DiffDetail(str("old_string"), str("new_string"))
		a.caption = "Proposed replacement"
		if all, _ := args["replace_all"].(bool); all {
			a.caption += " · all matches"
		}
	case "write":
		a.preview = tools.DiffDetail("", str("content"))
		a.caption = "New contents · replaces entire file"
		if a.preview == "" {
			a.preview = "(empty file)"
		}
	}
	if call.TrustDirectory != "" {
		a.preview = "Trust directory: " + sanitize.Terminal(call.TrustDirectory) +
			"\nAccess: " + call.TrustAccess + "\nIncludes descendants; expires when arkex exits.\n" + a.preview
		if a.caption == "" {
			a.caption = "Approval details"
		}
	}
	if call.Reason != "" && (call.TrustDirectory != "" || strings.ContainsAny(call.Reason, "\n\r")) {
		a.preview = sanitize.Terminal(call.Reason) + "\n" + a.preview
		if a.caption == "" {
			a.caption = "Approval details"
		}
	}
	return a
}

// answer resolves the prompt. It is safe to call once; the channel is
// buffered so the agent goroutine never blocks on the UI.
func (a *approval) answer(ans agent.Answer) { a.reply <- ans }

// move shifts the marked answer by d, wrapping.
func (a *approval) move(d int) {
	n := len(a.buttons)
	a.sel = ((a.sel+d)%n + n) % n
}

// byKey maps a shortcut to an answer; ok is false for keys that are not
// answers (they are swallowed, not typed).
func (a *approval) byKey(k string) (agent.Answer, bool) {
	switch k {
	case "enter":
		return a.buttons[a.sel].answer, true
	case "y", "Y":
		return agent.AllowOnce, true
	case "a", "A":
		if a.call.Grantable {
			return agent.AllowSession, true
		}
	case "n", "N", "esc":
		return agent.Deny, true
	}
	return agent.Deny, false
}

// hitAt returns the answer under a column and action-row offset, or -1.
func (a *approval) hitAt(x, y int) int {
	for _, h := range a.hits {
		if y == h.y && x >= h.x0 && x < h.x1 {
			return h.idx
		}
	}
	return -1
}

// approvalRows is the height of the box including its borders.
func (m *model) approvalRows() int {
	if m.pending == nil {
		return 0
	}
	m.approvalView() // recompute on resize too, before sizing the transcript
	return m.pending.rows + 2
}

// approvalView draws the box at the terminal width and records the button
// hit ranges (screen columns) for the mouse.
func (m *model) approvalView() string {
	a := m.pending
	inner := max(1, m.width-4) // two borders and equal one-cell inner padding
	call := a.call
	buttons := a.buttonLines(inner, m.width < 80)

	var lines []string
	head := errStyle.Render("●") + " " + toolStyle.Bold(true).Render(call.Name) + " needs your permission"
	if a.title != "" {
		head = toolStyle.Bold(true).Render(a.title)
	}
	if call.Grantable && call.Reason == "" && m.width >= 80 {
		head += dimStyle.Render("  (permission \"ask\" in config)")
	}
	lines = append(lines, ansi.Truncate(head, inner, "…"))

	if body := approvalBody(call, m.o.Cwd); body != "" && call.Name != "bash" {
		wrapped := strings.Split(ansi.Wrap(body, max(1, inner-2), ""), "\n")
		limit := approveBodyLines
		if m.height <= 20 {
			limit = 1
		}
		if len(wrapped) > limit {
			wrapped = wrapped[:limit]
			wrapped[limit-1] = ansi.Truncate(wrapped[limit-1], inner-3, "") + "…"
		}
		for _, l := range wrapped {
			lines = append(lines, "  "+l)
		}
	}
	if call.Reason != "" {
		if a.title != "" {
			wrapped := strings.Split(ansi.Wrap(sanitize.Terminal(call.Reason), max(1, inner-2), ""), "\n")
			for _, line := range wrapped[:min(3, len(wrapped))] {
				lines = append(lines, "  "+line)
			}
		} else {
			// Truncate limits columns, not embedded newlines. Keep this
			// summary physically one row; full multiline reasons live in
			// the scrollable preview so actions remain reachable.
			reason := strings.Join(strings.Fields(sanitize.Terminal(call.Reason)), " ")
			lines = append(lines, "  "+gaugeWarnStyle.Render("⚠ ")+ansi.Truncate(reason, max(1, inner-4), "…"))
		}
	}
	if call.TrustDirectory != "" && m.height > 20 {
		lines = append(lines, "  "+dimStyle.Render(ansi.Truncate("Trust directory = "+call.TrustAccess+" · until arkex exits", inner-2, "…")))
	}
	if a.preview != "" {
		// Reserve borders, actions, footer and some transcript context.
		transcriptRows := min(5, max(1, m.height/4))
		room := min(8, m.height-footerRows-2-len(buttons)-transcriptRows-len(lines)-2)
		if room > 0 {
			lines = append(lines, a.previewLines(max(1, inner-2), room)...)
		}
	}
	if m.height > 20 {
		lines = append(lines, "")
	}
	// Actions have priority over preview/context on short terminals.
	budget := max(1, m.height-footerRows-2-len(buttons))
	if len(lines) > budget {
		lines = lines[:budget]
	}
	a.buttonRow = len(lines)
	lines = append(lines, buttons...)
	a.rows = len(lines)
	return borderStyle.BorderForeground(theme.Warn).Width(m.width).Render(strings.Join(lines, "\n"))
}

func (a *approval) buttonLines(inner int, compact bool) []string {
	// Buttons: accent marks the selection, neutral fill marks hover.
	a.hits = a.hits[:0]
	rows := []string{"  "}
	x := 4 // left border, padding, then the two-space indent
	indent := 4
	if inner < 30 {
		rows[0], x, indent = "", 2, 2
	}
	for i, b := range a.buttons {
		style := formButton
		switch i {
		case a.hover:
			style = formButtonHv.Bold(i == a.sel)
		case a.sel:
			style = formButtonFc
		}
		label := "   " + b.label + " " // stable width when selection moves
		if i == a.sel {
			label = " ▶ " + b.label + " "
		}
		if inner < 30 {
			name := b.label
			if b.answer == agent.AllowSession {
				name = "Session"
				if a.call.TrustDirectory != "" {
					name = "Trust"
				}
			}
			label = " " + ansi.Truncate(name, max(1, inner-2), "…") + " "
		}
		s := style.Render(label)
		if !compact {
			s += dimStyle.Render(" " + b.keys)
		}
		w := lipgloss.Width(s)
		if x > indent && x-2+w > inner {
			rows = append(rows, strings.Repeat(" ", indent-2))
			x = indent
		}
		a.hits = append(a.hits, approveHit{x0: x, x1: x + w, idx: i, y: len(rows) - 1})
		rows[len(rows)-1] += s + " "
		x += w + 1
	}
	for i := range rows {
		rows[i] = strings.TrimRight(rows[i], " ")
	}
	return rows
}

func (a *approval) previewLines(width, height int) []string {
	if a.lines == nil || a.width != width {
		a.lines = nil
		for _, line := range strings.Split(a.preview, "\n") {
			// Keep the +/- marker on continuation rows, so scroll position
			// never makes removed text look like an addition (or vice versa).
			prefix := " "
			if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "+") {
				prefix, line = line[:1], line[1:]
			}
			for _, part := range strings.Split(ansi.Hardwrap(line, max(1, width-2), true), "\n") {
				a.lines = append(a.lines, prefix+" "+part)
			}
		}
		a.width = width
	}
	a.page = min(height, len(a.lines))
	a.scroll(0)
	rows := []string{"  " + dimStyle.Render(ansi.Truncate(a.caption, width, "…"))}
	for _, line := range a.lines[a.offset : a.offset+a.page] {
		style := dimStyle
		if strings.HasPrefix(line, "-") {
			style = delStyle
		} else if strings.HasPrefix(line, "+") {
			style = addStyle
		}
		rows = append(rows, "  "+style.Render(line))
	}
	if len(a.lines) > a.page {
		label := fmt.Sprintf("%d–%d of %d · pgup/pgdn or wheel", a.offset+1, a.offset+a.page, len(a.lines))
		rows = append(rows, "  "+dimStyle.Render(ansi.Truncate(label, width, "…")))
	}
	return rows
}

func (a *approval) scroll(delta int) {
	a.offset = max(0, min(a.offset+delta, len(a.lines)-a.page))
}

// approvalBody is what the tool is about to do, in one line before
// wrapping: the command for bash, the path for file tools, the raw
// arguments otherwise.
func approvalBody(call agent.ToolCall, cwd string) string {
	args := toolArgs(call.Input)
	str := func(k string) string {
		v, _ := args[k].(string)
		return v
	}
	var s string
	switch call.Name {
	case "bash":
		s = "$ " + strings.TrimSpace(str("command"))
	case "read", "edit", "write", "ls":
		s = displayPath(cwd, str("path"))
	default:
		s = toolTitle(call.Name, args, cwd, 1<<20)
	}
	return sanitize.Terminal(strings.ReplaceAll(s, "\t", "    "))
}

// approvalHint is the status line under the box while a prompt is up.
func approvalHint(width int) string {
	if width < 80 {
		return "←/→ pick · enter confirm · esc deny"
	}
	return "←/→ pick · enter confirm · y allow · n / esc deny · ctrl+c twice stops the run"
}

// approvalButtonY is the first action row; compact layouts may wrap actions.
func (m *model) approvalButtonY() int { return m.vp.Height() + 1 + m.pending.buttonRow }

// approvalClick takes the answer under the pointer, if any.
func (m *model) approvalClick(x, y int) {
	if i := m.pending.hitAt(x, y-m.approvalButtonY()); i >= 0 {
		m.answerPending(m.pending.buttons[i].answer)
	}
}

// approvalHover lights the answer under the pointer.
func (m *model) approvalHover(x, y int) {
	m.pending.hover = m.pending.hitAt(x, y-m.approvalButtonY())
}
