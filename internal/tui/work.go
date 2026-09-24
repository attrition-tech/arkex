package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// A work section ends before the final assistant answer. It is derived from
// user-turn boundaries, so old sessions need no migration or hidden messages.
type workSection struct {
	start, end int
	head       *block
	label      string
	live       bool
}

// Live history can span warnings and in-flight calls without owning them.
// Use the same membership rule for rendering and inspection preservation.
func (w workSection) contains(m *model, i int) bool {
	if i < w.start || i >= w.end {
		return false
	}
	if !w.live {
		return true
	}
	b := m.blocks[i]
	return b.kind != blockSystem && !m.toolInFlight(b)
}

func (m *model) toolInFlight(b *block) bool {
	return b.kind == blockTool && (b.status == "running" ||
		(m.pending != nil && b.id == m.pending.call.ID))
}

func (m *model) workSections() []workSection {
	return m.workSectionsFrom(0)
}

func (m *model) workSectionsFrom(start int) []workSection {
	var sections []workSection
	for start < len(m.blocks) {
		if m.blocks[start].kind != blockUser {
			start++
			continue
		}
		end := start + 1
		for end < len(m.blocks) && m.blocks[end].kind != blockUser {
			end++
		}
		if end == len(m.blocks) && (m.running || m.pending != nil || m.paused) {
			sections = append(sections, m.liveSections(start+1, end)...)
			break
		}
		// A trailing tool/reasoning block means there is no final answer yet.
		final := end - 1
		for final > start && (m.blocks[final].kind == blockSystem ||
			(m.blocks[final].kind == blockAssistant && strings.TrimSpace(m.blocks[final].text.String()) == "")) {
			final--
		}
		if final > start+1 && m.blocks[final].kind == blockAssistant {
			first := start + 1
			unfinished := false
			// Keep persistent errors, cancellation and compaction notices visible.
			// Only the contiguous work after the last system block can be folded.
			for i := first; i < final; i++ {
				if m.blocks[i].kind == blockTool && m.blocks[i].status == "running" {
					unfinished = true
				}
				if m.blocks[i].kind == blockSystem {
					first = i + 1
				}
			}
			if first < final && !unfinished {
				sections = append(sections, workSection{start: first, end: final, head: m.blocks[first], label: workLabel(m.blocks[first:final])})
			}
		}
		start = end
	}
	return sections
}

func workLabel(blocks []*block) string {
	var edits, reads, commands, other, failed, denied, thinking, updates int
	for _, b := range blocks {
		switch b.kind {
		case blockTool:
			switch b.name {
			case "edit", "write":
				edits++
			case "read", "grep", "find", "ls":
				reads++
			case "bash":
				commands++
			default:
				other++
			}
			switch b.status {
			case "error":
				failed++
			case "denied":
				denied++
			}
		case blockReasoning:
			thinking++
		case blockAssistant:
			updates++
		}
	}
	parts := []string{"Work"}
	for _, item := range []struct {
		n         int
		one, many string
	}{{edits, "edit", "edits"}, {reads, "read/search", "reads/searches"}, {commands, "command", "commands"}, {other, "tool call", "tool calls"}, {failed, "failed attempt", "failed attempts"}, {denied, "denial", "denials"}} {
		if item.n > 0 {
			word := item.many
			if item.n == 1 {
				word = item.one
			}
			parts = append(parts, fmt.Sprintf("%d %s", item.n, word))
		}
	}
	if len(parts) == 1 {
		if thinking > 0 {
			parts = append(parts, "Thinking")
		}
		if updates > 0 {
			parts = append(parts, "Progress")
		}
	}
	return strings.Join(parts, " · ")
}

func (m *model) workHeader(w workSection, width int) string {
	arrow := "▸ "
	if m.openWork == w.head {
		arrow = "▾ "
	}
	line := dimStyle.Render(ansi.Truncate(arrow+w.label, width, "…"))
	if m.hoverWork == w.head {
		line = fillRow(line, width)
	}
	return "  " + line
}
