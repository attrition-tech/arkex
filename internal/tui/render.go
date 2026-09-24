package tui

import (
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"time"

	glamouransi "charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/dantearo/arkex/internal/sanitize"
)

// Banner is the Arkex wordmark used by the update command.
const Banner = `
  ▄▀█ █▀█ █▄▀ █▀▀ ▀▄▀
  █▀█ █▀▄ █ █ ██▄ █ █`

var (
	textStyle      lipgloss.Style
	userBarStyle   lipgloss.Style
	reasoningStyle lipgloss.Style
	toolStyle      lipgloss.Style
	okStyle        lipgloss.Style
	errStyle       lipgloss.Style
	dimStyle       lipgloss.Style
	addStyle       lipgloss.Style
	delStyle       lipgloss.Style
	pillStyle      lipgloss.Style
	pillHoverStyle lipgloss.Style
	gaugeStyle     lipgloss.Style
	gaugeWarnStyle lipgloss.Style
	hintsStyle     lipgloss.Style
	hoverStyle     lipgloss.Style
	hoverSeq       string // SGR prefix of hoverStyle, see fillRow
	modeBuildStyle lipgloss.Style
	modePlanStyle  lipgloss.Style
	modeAutoStyle  lipgloss.Style
	borderStyle    lipgloss.Style
	toolBodyStyle  lipgloss.Style
	toolBodyDimmed lipgloss.Style
)

// markdown renders completed assistant text with Glamour. One renderer is
// kept per width; blocks cache their rendered output.
type markdown struct {
	width int
	gen   int // themeGen the renderer was built for
	r     *tableMarkdownRenderer
}

func (md *markdown) render(text string, width int) string {
	if width < 10 {
		return textStyle.Render(text)
	}
	if md.r == nil || md.width != width || md.gen != themeGen {
		r := newMarkdownRenderer(theme.Markdown, width, theme.Muted)
		md.r, md.width, md.gen = r, width, themeGen
	}
	out, err := md.r.Render(text)
	if err != nil {
		return textStyle.Width(width).Render(text)
	}
	return tidy(out)
}

func newMarkdownRenderer(st glamouransi.StyleConfig, width int, border color.Color) *tableMarkdownRenderer {
	zero := uint(0)
	st.Document.Margin = &zero
	bold := true
	for _, heading := range []*glamouransi.StyleBlock{&st.H1, &st.H2, &st.H3, &st.H4, &st.H5, &st.H6} {
		heading.Prefix, heading.Suffix = "", ""
		heading.BackgroundColor = nil
		heading.Color = st.Heading.Color
		heading.Bold = &bold
	}
	return &tableMarkdownRenderer{styles: st, width: width, border: border}
}

// tidy strips Glamour's trailing per-line padding and outer blank lines so
// the result composes with other blocks.
func tidy(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		plain := strings.TrimRight(ansi.Strip(line), " ")
		lines[i] = dropEmptyStyles(ansi.Truncate(line, lipgloss.Width(plain), ""))
	}
	start := 0
	for start < len(lines) && strings.TrimSpace(ansi.Strip(lines[start])) == "" {
		start++
	}
	end := len(lines)
	for end > start && strings.TrimSpace(ansi.Strip(lines[end-1])) == "" {
		end--
	}
	return strings.Join(lines[start:end], "\n")
}

// dropEmptyStyles removes SGR sequences that cannot affect any cell: within
// a run of adjacent SGR sequences (no text between them), everything before
// the last reset is overridden by that reset. Glamour styles every padding
// space separately, so truncating its padding leaves dozens of zero-width
// ESC[38;5;252m ESC[m pairs per line; the renderer would otherwise parse
// them on every frame.
func dropEmptyStyles(s string) string {
	if !strings.Contains(s, "m\x1b[") {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	for len(s) > 0 {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			break
		}
		out.WriteString(s[:i])
		s = s[i:]
		// Keep the suffix starting at the last reset, without allocating
		// a slice of each sequence in this contiguous run.
		run := s
		consumed, keep := 0, 0
		for strings.HasPrefix(s, "\x1b[") {
			end := sgrEnd(s)
			if end < 0 {
				break
			}
			if isSGRReset(s[:end]) {
				keep = consumed
			}
			consumed += end
			s = s[end:]
		}
		if consumed == 0 {
			// Not an SGR sequence: pass the introducer through untouched.
			out.WriteString(s[:2])
			s = s[2:]
			continue
		}
		out.WriteString(run[keep:consumed])
	}
	out.WriteString(s)
	return out.String()
}

// sgrEnd returns the length of the SGR sequence at the start of s (which
// must begin with ESC[), or -1 if it is not a complete SGR sequence.
func sgrEnd(s string) int {
	for j := 2; j < len(s); j++ {
		c := s[j]
		if c == 'm' {
			return j + 1
		}
		if (c < '0' || c > '9') && c != ';' && c != ':' {
			return -1
		}
	}
	return -1
}

func isSGRReset(seq string) bool { return seq == "\x1b[m" || seq == "\x1b[0m" }

// toolArgs decodes a tool's JSON input for display; failures yield nil.
func toolArgs(input string) map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(input), &m) != nil {
		return nil
	}
	return m
}

// toolTitle is the one-line description of a call shown in its card header.
// File paths are shown relative to cwd; see displayPath.
func toolTitle(name string, args map[string]any, cwd string, maxWidth int) string {
	str := func(k string) string {
		v, _ := args[k].(string)
		return v
	}
	var s string
	switch name {
	case "bash":
		s = commandTitle(str("command"))
	case "read", "edit", "write", "ls":
		s = displayPath(cwd, str("path"))
	case "grep":
		s = str("pattern")
		if p := str("path"); p != "" {
			s += "  " + displayPath(cwd, p)
		}
	case "find":
		s = str("pattern")
	}
	if s == "" && args != nil {
		var parts []string
		for k, v := range args {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
		s = strings.Join(parts, " ")
	}
	return compact(sanitize.Terminal(s), max(maxWidth, 12))
}

// displayPath shortens an absolute path for the card header: relative
// when it is inside the project (cwd), ~/… when it is elsewhere under the
// home directory, untouched otherwise — so a path that leaves the project
// stays visibly different from one that does not.
func displayPath(cwd, p string) string {
	if p == "" || !filepath.IsAbs(p) || cwd == "" {
		return filepath.ToSlash(p)
	}
	if rel, err := filepath.Rel(cwd, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if rel == "." {
			return "."
		}
		return filepath.ToSlash(rel)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(p)
}

// renderTool draws a tool card. Collapsed it is exactly one line: status
// mark, tool name, what it ran on, then a tail with the outcome (+added
// −removed for edits and writes, the exit status for failed commands) and
// the duration. Expanded, the full output or diff follows. hover fills the
// header line, the part a click toggles, with the theme's hover colour.
func renderTool(b *block, cwd string, width int, expanded, hover bool) string {
	return renderToolRow(b, cwd, width, expanded, hover, true, "", "")
}

func renderToolRow(b *block, cwd string, width int, expanded, hover, showName bool, runningMark, elapsed string) string {
	var mark string
	switch b.status {
	case "running":
		mark = toolStyle.Render("⋯")
	case "ok":
		mark = okStyle.Render("✓")
	default:
		mark = errStyle.Render("✗")
	}
	body, dimmed := toolBody(b)
	tail := toolTail(b)
	if b.status == "running" && runningMark != "" {
		mark = toolStyle.Render(runningMark)
		tail += " · " + elapsed
	}
	var preview, command string
	if b.name == "bash" {
		command, _ = b.args["command"].(string)
		short, directory := commandPreview(command, cwd)
		preview = commandTitle(short)
		if directory != "" {
			tail = " · " + compact(sanitize.Terminal(directory), max(12, width/3)) + tail
		}
	}
	hasDetails := body != "" || b.name == "bash"
	if hasDetails {
		if expanded {
			tail += " ▾"
		} else {
			tail += " ▸"
		}
	}
	head := mark
	if showName {
		head += " " + toolStyle.Bold(true).Render(b.name)
	}
	titleRoom := width - lipgloss.Width(head) - 1 - lipgloss.Width(tail)
	var t string
	if preview != "" {
		t = compact(sanitize.Terminal(preview), max(12, titleRoom))
	} else {
		t = toolTitle(b.name, b.args, cwd, titleRoom)
	}
	if t != "" {
		head += " " + t
	}
	head += dimStyle.Render(tail)
	head = ansi.Truncate(head, max(1, width), "…")
	if hover {
		head = fillRow(head, width)
	}
	if !hasDetails || !expanded {
		return head
	}

	lines := []string{head}
	style := toolBodyStyle
	if dimmed {
		style = toolBodyDimmed
	}
	if b.name == "bash" {
		// Even silent successful commands expose the source. Only build and
		// wrap it when expanded, not on every live spinner frame.
		body = "command:\n" + sanitize.Terminal(command) + "\n\n" + body
		body = ansi.Hardwrap(strings.ReplaceAll(body, "\t", "    "), max(1, width-3), true)
	}
	for _, l := range strings.Split(body, "\n") {
		l = ansi.Truncate(strings.ReplaceAll(l, "\t", "    "), width-3, "…")
		switch {
		case b.detail != "" && strings.HasPrefix(l, "+"):
			l = addStyle.Render(l)
		case b.detail != "" && strings.HasPrefix(l, "-"):
			l = delStyle.Render(l)
		}
		lines = append(lines, style.Render(l))
	}
	return strings.Join(lines, "\n")
}

// toolBody is what an expanded card shows under its header: the diff for
// edits and writes, the output of commands and failures, the reason for a
// denial. dimmed asks for the muted body style.
func toolBody(b *block) (body string, dimmed bool) {
	body = b.detail
	switch {
	case b.status == "denied":
		body, dimmed = b.summary, true
	case b.status == "error":
		body = b.output
	case body == "":
		body, dimmed = b.output, true
	}
	return strings.TrimRight(body, "\n"), dimmed
}

// toolTail is the outcome shown at the end of the header line, each part
// preceded by " · ".
func toolTail(b *block) string {
	var parts []string
	if b.note != "" {
		parts = append(parts, b.note)
	}
	switch {
	case b.status == "denied":
		parts = append(parts, "denied")
	case b.status == "error":
		if e := bashExit(b.output); e != "" {
			parts = append(parts, e)
		} else {
			parts = append(parts, "error")
		}
	case b.name == "read" && b.summary != "":
		// "path (N lines)" → keep the count only; the path is the title.
		if i := strings.LastIndex(b.summary, "("); i >= 0 {
			parts = append(parts, strings.Trim(b.summary[i:], "()"))
		}
	case b.detail != "":
		add, del := diffCounts(b.detail)
		parts = append(parts, fmt.Sprintf("%s %s", addStyle.Render(fmt.Sprintf("+%d", add)), delStyle.Render(fmt.Sprintf("−%d", del))))
	}
	if d := b.dur.Round(time.Millisecond); d > 0 {
		parts = append(parts, d.String())
	}
	if len(parts) == 0 {
		return ""
	}
	return " · " + strings.Join(parts, " · ")
}

// diffCounts counts the added and removed lines of a diffDetail listing.
func diffCounts(detail string) (add, del int) {
	for _, l := range strings.Split(detail, "\n") {
		switch {
		case strings.HasPrefix(l, "+"):
			add++
		case strings.HasPrefix(l, "-"):
			del++
		}
	}
	return add, del
}

// bashExit extracts "exit 1" from the bash tool's trailing
// "[exit status 1 in 12ms]" note, or "" when there is none.
func bashExit(output string) string {
	output = strings.TrimRight(output, "\n")
	i := strings.LastIndex(output, "\n[")
	if i < 0 && strings.HasPrefix(output, "[") {
		i = -1
	} else if i < 0 {
		return ""
	}
	note := strings.Trim(output[i+1:], "[]")
	switch {
	case strings.HasPrefix(note, "exit status "):
		code, _, _ := strings.Cut(strings.TrimPrefix(note, "exit status "), " ")
		return "exit " + code
	case strings.HasPrefix(note, "command timed out"):
		return "timed out"
	}
	return ""
}

func compact(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return ansi.Truncate(s, max, "…")
}

// fillRow lights a rendered row with the hover background out to width.
// Styled segments inside the row end with a reset that would drop the
// fill, so the background is re-armed after each one.
func fillRow(s string, width int) string {
	s = strings.ReplaceAll(s, "\x1b[m", "\x1b[m"+hoverSeq)
	s = strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+hoverSeq)
	return hoverStyle.Width(width).Render(s)
}
