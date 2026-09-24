package tui

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// command is one slash command. The registry drives /help; the palette
// (palette.go) exposes the same commands as rows.
type command struct {
	name string
	args string // placeholder shown after the name, e.g. "<provider/model>"
	desc string
}

var commands = []command{
	{name: "/connections", desc: "list, add, disable or remove LLM servers, API keys and models"},
	{name: "/models", desc: "same as /connections"},
	{name: "/model", args: "<provider/model>", desc: "switch model (the conversation continues)"},
	{name: "/mode", args: "[plan|build|auto]", desc: "switch mode"},
	{name: "/clear", desc: "start a fresh conversation (also /new)"},
	{name: "/resume", args: "[id]", desc: "resume a saved session from this directory (also /sessions)"},
	{name: "/rename", args: "[title]", desc: "rename the current saved session"},
	{name: "/compact", desc: "summarise the conversation so far to free context"},
	{name: "/continue", desc: "let the model keep going after a run paused"},
	{name: "/mouse", desc: "toggle mouse support (wheel, clicks)"},
	{name: "/theme", args: "[name]", desc: "pick a colour theme (saved to ui.theme)"},
	{name: "/help", desc: "list commands and keys"},
	{name: "/quit", desc: "exit arkex"},
}

func helpText() string {
	var sb strings.Builder
	for _, c := range commands {
		head := c.name
		if c.args != "" {
			head += " " + c.args
		}
		fmt.Fprintf(&sb, "%-26s %s\n", head, c.desc)
	}
	sb.WriteString("\nmodes: plan = read-only tools, the model writes a plan · build = edits and commands ask per config · auto = trusted scope runs quietly. Explicit config denies apply in every mode.\n")
	sb.WriteString("keys: enter send · shift+enter newline · tab highlights older sent messages from empty input, shift+tab newer then composer · up/down input history · / or ctrl+p command palette · @file attach a file (png/jpg/gif/webp become image chips; backspace on an empty input removes the last) · !cmd run a shell command · /mode switch mode · ctrl+l clear screen · esc or ctrl+c twice within 2s stops a run; ctrl+c twice exits when idle (selected text copies instead) · pgup/pgdown, ctrl+up/down, ctrl+home/end scroll · wheel scrolls · click toggles a card (one detail open at a time)")
	return sb.String()
}

type compItem struct {
	value string
	desc  string
}

// completion is the open @file popup: what is being completed and where.
type completion struct {
	items []compItem
	sel   int
	word  string // text being replaced, including the leading / or @
	row   int    // cursor row in the textarea
	start int    // rune offset of word within the row
	end   int
}

func (c *completion) selected() compItem {
	if c.sel >= 0 && c.sel < len(c.items) {
		return c.items[c.sel]
	}
	return compItem{}
}

const maxCompRows = 8

// wordAt returns the whitespace-delimited word containing the cursor and
// its rune offsets within line.
func wordAt(line []rune, col int) (word string, start, end int) {
	if col > len(line) {
		col = len(line)
	}
	start = col
	for start > 0 && !unicode.IsSpace(line[start-1]) {
		start--
	}
	end = col
	for end < len(line) && !unicode.IsSpace(line[end]) {
		end++
	}
	return string(line[start:end]), start, end
}

// updateCompletion recomputes the popup from the input state. It is called
// after every key that reaches the textarea.
func (m *model) updateCompletion() tea.Cmd {
	rows := strings.Split(m.input.Value(), "\n")
	row := m.input.Line()
	if row >= len(rows) {
		m.comp = nil
		return nil
	}
	line := []rune(rows[row])
	word, start, end := wordAt(line, m.input.Column())
	// The cursor must sit at the end of the word being completed.
	if m.input.Column() != end {
		m.comp = nil
		return nil
	}
	if word != "" && word == m.dismissed {
		m.comp = nil
		return nil
	}
	m.dismissed = ""

	if !strings.HasPrefix(word, "@") {
		m.comp = nil
		return nil
	}
	c := &completion{word: word, row: row, start: start, end: end}
	if m.files == nil {
		c.items = []compItem{{value: word, desc: "loading files…"}}
		m.comp = c
		if m.filesLoading {
			return nil
		}
		m.filesLoading = true
		cwd := m.o.Cwd
		return func() tea.Msg { return filesMsg{files: listFiles(cwd)} }
	}
	c.items = fuzzyFiles(m.files, word[1:], 50)
	if len(c.items) == 0 {
		m.comp = nil
		return nil
	}
	if m.comp != nil && m.comp.sel < len(c.items) {
		c.sel = m.comp.sel
	}
	m.comp = c
	return nil
}

// accept replaces the completed word with the selected item. It returns
// the accepted value, or "" when nothing was selected.
func (m *model) accept() string {
	c := m.comp
	if c == nil {
		return ""
	}
	it := c.selected()
	if it.value == "" {
		return ""
	}
	rows := strings.Split(m.input.Value(), "\n")
	line := []rune(rows[c.row])
	replacement := it.value + " "
	// An @image becomes a chip above the input instead of an inline mention.
	if name, ok := strings.CutPrefix(it.value, "@"); ok && imageMediaType(name) != "" {
		if err := m.attach(name); err == nil {
			replacement = ""
		}
	}
	rows[c.row] = string(line[:c.start]) + replacement + string(line[c.end:])
	col := c.start + len([]rune(replacement))
	m.input.SetValue(strings.Join(rows, "\n"))
	for m.input.Line() > c.row {
		m.input.CursorUp()
	}
	m.input.SetCursorColumn(col)
	m.comp = nil
	m.resizeInput()
	return it.value
}

// compKey handles keys while the popup is open. It reports whether the key
// was consumed.
func (m *model) compKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	c := m.comp
	if c == nil {
		return nil, false
	}
	switch k.String() {
	case "up":
		c.sel = (c.sel - 1 + len(c.items)) % len(c.items)
		return nil, true
	case "down":
		c.sel = (c.sel + 1) % len(c.items)
		return nil, true
	case "esc", "ctrl+c":
		m.dismissed = c.word
		m.comp = nil
		return nil, true
	case "tab":
		m.accept()
		return m.updateCompletion(), true
	case "enter":
		it := c.selected()
		if it.value == c.word || strings.HasSuffix(it.desc, "…") {
			return nil, false // nothing to complete; let enter send
		}
		m.accept()
		return nil, true
	}
	return nil, false
}

// ---- files ----

type filesMsg struct{ files []string }

const maxFiles = 20000

// listFiles returns repository files relative to cwd: git's view when
// available, otherwise a bounded walk that skips hidden and vendor dirs.
func listFiles(cwd string) []string {
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = cwd
	if out, err := cmd.Output(); err == nil {
		var files []string
		for _, f := range bytes.Split(out, []byte{0}) {
			if len(f) > 0 {
				files = append(files, string(f))
			}
			if len(files) >= maxFiles {
				break
			}
		}
		return files
	}
	var files []string
	_ = filepath.WalkDir(cwd, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != cwd && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "target" || name == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(cwd, path)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		if len(files) >= maxFiles {
			return filepath.SkipAll
		}
		return nil
	})
	return files
}

// fuzzyFiles ranks files by fuzzyScore and returns the best n as items.
func fuzzyFiles(files []string, pattern string, n int) []compItem {
	type scored struct {
		path  string
		score int
	}
	var hits []scored
	for _, f := range files {
		if s, ok := fuzzyScore(pattern, f); ok {
			hits = append(hits, scored{f, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return len(hits[i].path) < len(hits[j].path)
	})
	if len(hits) > n {
		hits = hits[:n]
	}
	out := make([]compItem, len(hits))
	for i, h := range hits {
		out[i] = compItem{value: "@" + h.path}
	}
	return out
}

// fuzzyScore matches pattern as a case-insensitive subsequence of s. Runs
// of consecutive matches and matches at path-segment or word starts score
// higher; an exact substring match scores highest.
func fuzzyScore(pattern, s string) (int, bool) {
	if pattern == "" {
		return 0, true
	}
	p, t := strings.ToLower(pattern), strings.ToLower(s)
	if i := strings.Index(t, p); i >= 0 {
		score := 1000
		if i == 0 || t[i-1] == '/' {
			score += 100 // segment start
		}
		if !strings.Contains(t[i:], "/") {
			score += 50 // in the file name, not a directory
		}
		return score - len(t)/10, true
	}
	score, pi, prev := 0, 0, -2
	for ti := 0; ti < len(t) && pi < len(p); ti++ {
		if t[ti] != p[pi] {
			continue
		}
		score += 10
		if ti == prev+1 {
			score += 15
		}
		if ti == 0 || t[ti-1] == '/' || t[ti-1] == '.' || t[ti-1] == '_' || t[ti-1] == '-' {
			score += 20
		}
		prev = ti
		pi++
	}
	if pi < len(p) {
		return 0, false
	}
	return score - len(t)/10, true
}

// ---- mentions ----

const (
	maxMentionBytes = 32 * 1024
	maxMentions     = 8
)

// expandMentions appends the contents of @file mentions that name real
// files under cwd. The returned text is what the model receives; the
// transcript still shows what the user typed.
func expandMentions(cwd, text string) string {
	var sb strings.Builder
	seen := map[string]bool{}
	for _, w := range strings.Fields(text) {
		if !strings.HasPrefix(w, "@") || len(w) < 2 {
			continue
		}
		rel := strings.TrimRight(w[1:], ".,:;)")
		if seen[rel] || len(seen) >= maxMentions {
			continue
		}
		path := rel
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, rel)
		}
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		seen[rel] = true
		note := ""
		if len(b) > maxMentionBytes {
			b = b[:maxMentionBytes]
			note = fmt.Sprintf(" (truncated to %d bytes; use the read tool for more)", maxMentionBytes)
		}
		fmt.Fprintf(&sb, "\n\n<file path=%q%s>\n%s\n</file>", rel, note, strings.TrimRight(string(b), "\n"))
	}
	if sb.Len() == 0 {
		return text
	}
	return text + sb.String()
}

// ---- view ----

var (
	popupBorder lipgloss.Style
	popupSel    lipgloss.Style
	popupDesc   lipgloss.Style
	popupSelDsc lipgloss.Style
)

// renderPopup draws the completion list, at most maxCompRows tall, no wider
// than width.
func (m *model) renderPopup(width int) string {
	c := m.comp
	if c == nil || len(c.items) == 0 {
		return ""
	}
	inner := min(width-4, 72)
	if inner < 20 {
		return ""
	}
	// Keep the selection in view.
	top := 0
	if c.sel >= maxCompRows {
		top = c.sel - maxCompRows + 1
	}
	rows := c.items[top:min(len(c.items), top+maxCompRows)]
	nameW := 0
	for _, it := range rows {
		nameW = max(nameW, lipgloss.Width(it.value))
	}
	nameW = min(nameW, inner-4)
	var lines []string
	for i, it := range rows {
		name := ansi.Truncate(it.value, nameW, "…")
		name += strings.Repeat(" ", nameW-lipgloss.Width(name))
		desc := ansi.Truncate(it.desc, max(0, inner-nameW-3), "…")
		if top+i == c.sel {
			line := popupSel.Render(" "+name+"  ") + popupSelDsc.Render(desc)
			lines = append(lines, line+popupSelDsc.Render(strings.Repeat(" ", max(0, inner-lipgloss.Width(line)))))
		} else {
			lines = append(lines, " "+name+"  "+popupDesc.Render(desc))
		}
	}
	if len(c.items) > len(rows) {
		lines = append(lines, popupDesc.Render(fmt.Sprintf(" … %d more", len(c.items)-len(rows))))
	}
	return popupBorder.Width(inner + 2).Render(strings.Join(lines, "\n")) // Width includes the border
}
