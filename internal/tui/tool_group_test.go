package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"
	"github.com/dantearo/arkex/internal/agent"
)

func groupTool(name, path string) *block {
	return &block{kind: blockTool, name: name, status: "ok", args: map[string]any{"path": path}, output: "contents of " + path}
}

func groupText(m *model) string {
	return ansi.Strip(strings.Join(m.renderBlocks(m.width), "\n"))
}

func groupClick(t *testing.T, m *model, b *block, header bool) {
	t.Helper()
	for _, s := range m.spans {
		if s.b == b && s.header == header {
			m.vp.ScrollDown(s.top - m.vp.YOffset() - 2)
			click(m, 5, s.top-m.vp.YOffset())
			return
		}
	}
	t.Fatal("missing click target")
}

func TestToolGroupBoundaries(t *testing.T) {
	for _, barrier := range []string{"assistant", "empty assistant", "reasoning", "write", "error", "denied", "approved", "pending"} {
		t.Run(barrier, func(t *testing.T) {
			m, _ := testModel(t)
			mid := groupTool("read", "middle.go")
			switch barrier {
			case "assistant":
				mid = newBlock(blockAssistant, "Next.")
			case "empty assistant":
				mid = newBlock(blockAssistant, "")
			case "reasoning":
				mid = newBlock(blockReasoning, "Thinking.")
			case "write":
				mid.name = "write"
			case "error", "denied":
				mid.status = barrier
			case "approved":
				mid.note = "by you"
			case "pending":
				mid.id, mid.status = "ask", "running"
				m.pending = &approval{call: agent.ToolCall{ID: "ask"}}
			}
			m.blocks = []*block{groupTool("read", "a.go"), groupTool("read", "b.go"), mid, groupTool("read", "c.go"), groupTool("read", "d.go")}
			got := groupText(m)
			if strings.Count(got, "read 2 files") != 2 || strings.Contains(got, "read 5 files") {
				t.Fatalf("group crossed %s: %s", barrier, got)
			}
			if barrier != "empty assistant" && len(m.spans) != 3 {
				t.Fatalf("barrier hidden: %+v", m.spans)
			}
		})
	}
}

func TestToolGroupCountsAndScope(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = []*block{groupTool("read", "src/a.go"), groupTool("read", "src/deep/b.go"), groupTool("read", filepath.Join(m.o.Cwd, "src/a.go"))}
	if got := groupText(m); !strings.Contains(got, "3 reads · 2 files · src ▸") {
		t.Fatal(got)
	}
	m.blocks[2] = groupTool("write", "other.go")
	if got := groupText(m); !strings.Contains(got, "read 2 files · src ▸") || !strings.Contains(got, "write other.go") {
		t.Fatal(got)
	}
	// A path-prefix is not necessarily a directory boundary.
	m.blocks = []*block{groupTool("edit", "src/a.go"), groupTool("edit", "src-other/b.go")}
	if got := groupText(m); strings.Contains(got, "· src") || !strings.Contains(got, "edit 2 files") {
		t.Fatal(got)
	}
	m.blocks = []*block{groupTool("bash", ""), groupTool("bash", "")}
	if got := groupText(m); !strings.Contains(got, "bash 2 calls") || strings.Contains(got, "files") {
		t.Fatal(got)
	}
	m.blocks = []*block{groupTool("read", "src\nodd/a.go"), groupTool("read", "src\nodd/b.go")}
	if got := groupText(m); strings.ContainsAny(got, "\n\r\t") || !strings.Contains(got, "src odd") {
		t.Fatalf("path escaped its one-line header: %q", got)
	}
}

func TestToolGroupInspectionAndHover(t *testing.T) {
	m, _ := testModel(t)
	a, b := groupTool("read", "src/a.go"), groupTool("read", "src/b.go")
	c, d := groupTool("edit", "src/c.go"), groupTool("edit", "src/d.go")
	c.detail = "-old line\n+new line"
	r := newBlock(blockReasoning, "Reasoning detail")
	m.blocks = []*block{a, b, c, d, r}
	m.layout()
	groupClick(t, m, a, true)
	if m.openGroup != a || m.openDetail != nil || len(m.spans) != 5 {
		t.Fatal("group did not open its two member rows")
	}
	groupClick(t, m, a, false)
	if got := groupText(m); !strings.Contains(got, a.output) || strings.Contains(got, b.output) {
		t.Fatal(got)
	}
	groupClick(t, m, b, false)
	if got := groupText(m); strings.Contains(got, a.output) || !strings.Contains(got, b.output) || m.openGroup != a {
		t.Fatal(got)
	}
	groupClick(t, m, c, true)
	if m.openGroup != c || m.openDetail != nil {
		t.Fatal("second group did not replace first inspection")
	}
	groupClick(t, m, c, false)
	if got := groupText(m); !strings.Contains(got, "+new line") || strings.Contains(got, b.output) {
		t.Fatal(got)
	}
	groupClick(t, m, r, false)
	if m.openGroup != nil || m.openDetail != r {
		t.Fatal("reasoning did not replace group")
	}
	groupClick(t, m, a, true)
	groupClick(t, m, a, true)
	if m.openGroup != nil || m.openDetail != nil {
		t.Fatal("second header click did not close group")
	}
	m.hoverAt(4, m.spans[0].top-m.vp.YOffset())
	if m.hoverGroup != a || m.hoverBlock != nil {
		t.Fatal("group hover was confused with first member")
	}
	row := m.vp.lines[m.spans[0].top]
	if !strings.Contains(row, "48;") || strings.Contains(row, "\x1b[4m") {
		t.Fatalf("expected filled hover without underline: %q", row)
	}
}

func TestToolGroupLiveGrowthAndFailureSplit(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	events(m, agent.ToolCall{ID: "a", Name: "read", Input: `{"path":"a.go"}`}, agent.ToolResult{ID: "a", Output: "alpha"})
	a := m.tool("a")
	groupClick(t, m, a, false)
	events(m, agent.ToolCall{ID: "b", Name: "read", Input: `{"path":"b.go"}`})
	if got := groupText(m); !strings.Contains(got, "read 1/2 done · b.go ▾") || m.openGroup != a || m.openDetail != a {
		t.Fatal(got)
	}
	events(m, agent.ToolResult{ID: "b", Output: "beta"})
	groupClick(t, m, a, true) // explicitly collapse; growth must respect this
	events(m, agent.ToolCall{ID: "c", Name: "read", Input: `{"path":"c.go"}`})
	if got := groupText(m); !strings.Contains(got, "read 2/3 done · c.go ▸") || m.openGroup != nil {
		t.Fatal(got)
	}
	events(m, agent.ToolResult{ID: "c", Output: "permission denied", IsError: true})
	if got := groupText(m); !strings.Contains(got, "read 2 files") || !strings.Contains(got, "✗ read c.go") {
		t.Fatal(got)
	}
	// A late failure splits an open run. Only the selected member's new
	// container may remain open (not both the old prefix and new suffix).
	m.blocks = []*block{groupTool("read", "a"), groupTool("read", "b"), groupTool("read", "c"), groupTool("read", "d"), groupTool("read", "e")}
	m.openGroup, m.openDetail = m.blocks[0], m.blocks[4]
	m.blocks[2].status = "error"
	got := groupText(m)
	if m.openGroup != m.blocks[3] || strings.Count(got, "read 2 files ▾") != 1 || len(m.spans) != 5 {
		t.Fatalf("multiple open containers: %s", got)
	}
}

func TestToolGroupsOnResumeAndNarrowWidth(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = blocksFromMessages([]fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "a", ToolName: "read", Input: `{"path":"長い名前/a.go"}`},
			fantasy.ToolCallPart{ToolCallID: "b", ToolName: "read", Input: `{"path":"長い名前/b.go"}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "a", Output: fantasy.ToolResultOutputContentText{Text: "saved content"}},
		}},
	})
	if got := groupText(m); !strings.Contains(got, "read 2 files") {
		t.Fatal(got)
	}
	m.openGroup, m.openDetail = m.blocks[0], m.blocks[0]
	for _, width := range []int{20, 40, 60, 100} {
		for _, line := range m.renderBlocks(width) {
			if ansi.StringWidth(line) > width {
				t.Fatalf("width %d overflow: %q", width, line)
			}
		}
	}
	if got := groupText(m); !strings.Contains(got, "saved content") {
		t.Fatal(got)
	}
}

func TestToolGroupClickKeepsScrolledRowAnchored(t *testing.T) {
	m, _ := testModel(t)
	for i := 0; i < 20; i++ {
		m.blocks = append(m.blocks, newBlock(blockSystem, "Earlier message"))
	}
	a, b := groupTool("read", "a.go"), groupTool("read", "b.go")
	a.output = strings.Repeat("source line\n", 12)
	c, d := groupTool("write", "c.go"), groupTool("write", "d.go")
	m.blocks = append(m.blocks, a, b, c, d)
	for i := 0; i < 20; i++ {
		m.blocks = append(m.blocks, newBlock(blockSystem, "Later message"))
	}
	m.layout()
	groupClick(t, m, a, true)
	groupClick(t, m, a, false)
	groupClick(t, m, c, true) // helper places the clicked header at screen row 2
	for _, s := range m.spans {
		if s.b == c && s.header {
			if s.top-m.vp.YOffset() != 2 || m.stickBottom || m.openDetail != nil || m.openGroup != c {
				t.Fatalf("inspection jumped: row=%d, offset=%d", s.top, m.vp.YOffset())
			}
			return
		}
	}
	t.Fatal("new group disappeared")
}

func BenchmarkToolGroups(b *testing.B) {
	m, _ := testModel(b)
	for i := 0; i < 1000; i++ {
		m.blocks = append(m.blocks, groupTool("read", fmt.Sprintf("src/file%d.go", i)))
		if i%10 == 9 {
			m.blocks = append(m.blocks, newBlock(blockAssistant, "Next batch."))
		}
	}
	m.renderBlocks(100)
	b.ResetTimer()
	for b.Loop() {
		m.frame++
		m.renderBlocks(100)
	}
}
