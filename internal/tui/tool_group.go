package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dantearo/arkex/internal/sanitize"
	"github.com/dantearo/arkex/internal/tools"
)

// Groups are a view of adjacent calls, never a change to conversation history.
// Permission prompts and exceptional outcomes are barriers, even for the same tool.
func (m *model) groupable(b *block) bool {
	if m.running && b.status == "running" {
		return false // keep the live loader on a visible, clickable tool row
	}
	if b.kind != blockTool || (b.status != "ok" && b.status != "running") || b.note != "" {
		return false
	}
	if m.pending != nil && m.pending.call.ID == b.id {
		return false
	}
	return tools.IsBuiltin(b.name)
}

func (m *model) toolGroupEnd(start int) int {
	first := m.blocks[start]
	end := start + 1
	if m.groupable(first) {
		for end < len(m.blocks) && m.blocks[end].name == first.name && m.groupable(m.blocks[end]) {
			end++
		}
	}
	return end
}

// Cache path aggregation on the first member. Membership only changes when
// a run grows or a failure/prompt splits it; animation never rescans outputs.
type toolGroupSummary struct {
	count int
	label string
	scope string
}

func groupSummary(members []*block, cwd string) toolGroupSummary {
	n := len(members)
	name := members[0].name
	s := toolGroupSummary{count: n, label: fmt.Sprintf("%s %d calls", name, n)}
	if name != "read" && name != "write" && name != "edit" {
		return s
	}
	paths := make(map[string]bool, n)
	dir := ""
	for _, b := range members {
		p, _ := b.args["path"].(string)
		if p == "" {
			return s // malformed input is not a file
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		paths[p] = true
		d := filepath.Dir(p)
		if dir == "" {
			dir = d
		}
		for dir != d && !strings.HasPrefix(d, dir+string(filepath.Separator)) && filepath.Dir(dir) != dir {
			dir = filepath.Dir(dir)
		}
	}
	s.label = fmt.Sprintf("%s %d files", name, n)
	if len(paths) != n {
		unit := "files"
		if len(paths) == 1 {
			unit = "file"
		}
		s.label = fmt.Sprintf("%d %ss · %d %s", n, name, len(paths), unit)
	}
	s.scope = strings.Join(strings.Fields(sanitize.Terminal(displayPath(cwd, dir))), " ")
	if s.scope == "." || dir == filepath.Dir(dir) {
		s.scope = ""
	}
	return s
}

func (m *model) toolGroupHeader(members []*block, width int) string {
	first := members[0]
	if first.groupSummary.count != len(members) {
		first.groupSummary = groupSummary(members, m.o.Cwd)
	}
	s := first.groupSummary
	done := 0
	var active *block
	for _, b := range members {
		if b.status == "ok" {
			done++
		} else if active == nil {
			active = b
		}
	}
	mark := okStyle.Render("✓")
	label, tail := s.label, s.scope
	if active != nil {
		mark = toolStyle.Render(loader(m.frame))
		label = fmt.Sprintf("%s %d/%d done", first.name, done, len(members))
		tail = toolTitle(active.name, active.args, m.o.Cwd, width)
	}
	arrow := " ▸"
	if m.openGroup == first {
		arrow = " ▾"
	}
	head := mark + " " + toolStyle.Bold(true).Render(label)
	if tail != "" {
		head += dimStyle.Render(" · " + tail)
	}
	head = ansi.Truncate(head, max(1, width-lipgloss.Width(arrow)), "…") + dimStyle.Render(arrow)
	head = ansi.Truncate(head, width, "")
	if m.hoverGroup == first {
		head = fillRow(head, width)
	}
	return head
}
