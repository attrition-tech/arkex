package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"
)

func workTurn() []*block {
	failed := groupTool("bash", "")
	failed.args = toolArgs(`{"command":"pnpm test"}`)
	failed.status, failed.output = "error", "missing pointer API"
	edit := groupTool("edit", "src/test/setup.ts")
	edit.detail = "-old setup\n+new setup"
	check := groupTool("bash", "")
	check.args, check.output = toolArgs(`{"command":"pnpm verify"}`), "91 tests passed"
	return []*block{newBlock(blockUser, "Fix the sidebar"), newBlock(blockAssistant, "First checking the tests."), failed,
		newBlock(blockReasoning, "Need the pointer API."), edit, newBlock(blockAssistant, "Now verifying."), check,
		newBlock(blockAssistant, "Sidebar updated. `pnpm verify` passed.")}
}

func clickWork(t *testing.T, m *model, head *block) {
	t.Helper()
	for _, s := range m.spans {
		if s.workHeader && s.b == head {
			m.vp.ScrollDown(s.top - m.vp.YOffset() - 2)
			click(m, 5, s.top-m.vp.YOffset())
			return
		}
	}
	t.Fatal("work header missing")
}

func TestCompletedWorkFoldsWithoutLosingHistory(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = workTurn()
	m.layout()
	got := groupText(m)
	if !strings.Contains(got, "▸ Work · 1 edit · 2 commands · 1 failed attempt") || !strings.Contains(got, "Sidebar updated.") || strings.Contains(got, "Now verifying") || strings.Contains(got, "Thinking") {
		t.Fatal(got)
	}
	if len(m.blocks) != 8 || len(m.spans) != 3 {
		t.Fatal("fold changed history or hid final answer")
	}
	clickWork(t, m, m.blocks[1])
	got = groupText(m)
	if !strings.Contains(got, "First checking") || !strings.Contains(got, "▸ Thinking") || !strings.Contains(got, "✗ bash") {
		t.Fatal(got)
	}
	groupClick(t, m, m.blocks[4], false)
	if m.openDetail != m.blocks[4] || !strings.Contains(groupText(m), "+new setup") {
		t.Fatal("inline diff did not open")
	}
	groupClick(t, m, m.blocks[2], false)
	if m.openDetail != m.blocks[2] || strings.Contains(groupText(m), "+new setup") || !strings.Contains(groupText(m), "missing pointer API") {
		t.Fatal("details did not replace")
	}
	clickWork(t, m, m.blocks[1])
	if m.openWork != nil || m.openDetail != nil || strings.Contains(groupText(m), "missing pointer API") {
		t.Fatal("work failed to close")
	}
}

func TestFoldedWorkReleasesCachesAndReopensIdentically(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = workTurn()
	m.layout()
	head, detail := m.blocks[1], m.blocks[2]
	clickWork(t, m, head)
	groupClick(t, m, detail, false)
	before := groupText(m)
	if len(detail.lines) == 0 || !strings.Contains(before, "missing pointer API") {
		t.Fatal("detail was not rendered")
	}
	clickWork(t, m, head)
	for _, b := range m.blocks[1:7] {
		if b.lines != nil || b.wrapLines != nil || b.streamMD != nil {
			t.Fatal("folded work retained render caches")
		}
	}
	if detail.output != "missing pointer API" {
		t.Fatal("source output was discarded")
	}
	clickWork(t, m, head)
	groupClick(t, m, detail, false)
	if after := groupText(m); after != before {
		t.Fatalf("reopened display changed:\n%s\n%s", before, after)
	}
}

func TestWorkCompletionPreservesInspectionAndSeparatesTurns(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = workTurn()
	m.running = true
	m.layout()
	if len(m.workSections()) != 0 || strings.Contains(groupText(m), "First checking") || !strings.Contains(groupText(m), "pnpm verify") {
		t.Fatal("live progress not compact")
	}
	groupClick(t, m, m.blocks[4], false)
	m.running = false
	m.refresh()
	if m.openWork != m.blocks[1] || m.openDetail != m.blocks[4] {
		t.Fatal("completion collapsed inspection")
	}
	second := workTurn()
	m.blocks = append(m.blocks, second...)
	m.refresh()
	clickWork(t, m, second[1])
	if m.openWork != second[1] || m.openDetail != nil || strings.Count(groupText(m), "First checking") != 1 {
		t.Fatal("two work sections open")
	}
	for _, width := range []int{20, 60, 100} {
		for _, line := range m.renderBlocks(width) {
			if ansi.StringWidth(line) > width {
				t.Fatalf("overflow at %d: %q", width, line)
			}
		}
	}
}

func TestWorkKeepsTerminalFailuresAndIncompleteRunsVisible(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = workTurn()[:7] // no final assistant answer
	m.blocks = append(m.blocks, newBlock(blockSystem, "error: provider disconnected"))
	if len(m.workSections()) != 0 || !strings.Contains(groupText(m), "provider disconnected") {
		t.Fatal("incomplete run folded")
	}
	m.blocks = workTurn()
	m.blocks = append(m.blocks, newBlock(blockSystem, "cancelled"))
	if !strings.Contains(groupText(m), "cancelled") {
		t.Fatal("terminal status folded")
	}
	m.paused = true
	if len(m.workSections()) != 0 {
		t.Fatal("paused turn folded")
	}
}

func TestWorkRestoredFromSavedConversation(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = blocksFromMessages([]fantasy.Message{
		fantasy.NewUserMessage("Check tests"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Checking."}, fantasy.ToolCallPart{ToolCallID: "x", ToolName: "bash", Input: `{"command":"go test"}`}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "x", Output: fantasy.ToolResultOutputContentText{Text: "passed"}}}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "All tests passed."}}},
	})
	if got := groupText(m); !strings.Contains(got, "Work · 1 command") || !strings.Contains(got, "All tests passed.") || strings.Contains(got, "Checking.") {
		t.Fatal(got)
	}
}

func TestCommandPreviewRetainsFullCommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	command := fmt.Sprintf("cd '%s' && source .tooling/activate && pnpm test && pnpm lint", filepath.ToSlash(filepath.Join(root, "frontend")))
	b := groupTool("bash", "")
	b.args = map[string]any{"command": command}
	got := ansi.Strip(renderTool(b, root, 100, false, false))
	if !strings.Contains(got, "$ pnpm test && pnpm lint · frontend") || strings.Contains(got, "activate") {
		t.Fatal(got)
	}
	got = ansi.Strip(renderTool(b, root, 200, true, false))
	if !strings.Contains(got, command) {
		t.Fatal("full command missing: " + got)
	}
	for _, command := range []string{`cd "$DYNAMIC" && pnpm test`, `cd a || pnpm test`, `(cd a) && pnpm test`, `cd a; pnpm test`, `! cd a && pnpm test`} {
		if short, dir := commandPreview(command, root); short != command || dir != "" {
			t.Fatalf("ambiguous command shortened: %s", command)
		}
	}
}

func TestInlineCodeUsesToolColorWithoutBackground(t *testing.T) {
	old := theme
	t.Cleanup(func() { applyTheme(old) })
	for _, th := range themes {
		applyTheme(th)
		if theme.Markdown.Code.BackgroundColor != nil || theme.Markdown.Code.Color == nil {
			t.Fatal("inline code theme not updated")
		}
		var md markdown
		got := md.render("Run `pnpm verify` now.", 80)
		if strings.Contains(got, "\x1b[48;") || !strings.Contains(ansi.Strip(got), "pnpm verify") {
			t.Fatalf("inline code background: %q", got)
		}
	}
}

func TestWorkContainsToolGroupAndResetsOnClear(t *testing.T) {
	m, _ := testModel(t)
	a, b := groupTool("read", "a.go"), groupTool("read", "b.go")
	m.blocks = []*block{newBlock(blockUser, "Read both"), a, b, newBlock(blockAssistant, "Both read.")}
	m.layout()
	clickWork(t, m, a)
	groupClick(t, m, a, true)
	groupClick(t, m, b, false)
	if m.openWork != a || m.openGroup != a || m.openDetail != b || !strings.Contains(groupText(m), b.output) {
		t.Fatal("nested group inspection failed")
	}
	typeKeys(m, "ctrl+l")
	if m.openWork != nil || m.openDetail != nil || m.openGroup != nil {
		t.Fatal("cleared transcript retained Work inspection")
	}
}

func BenchmarkCompletedWork(b *testing.B) {
	m, _ := testModel(b)
	for i := 0; i < 100; i++ {
		m.blocks = append(m.blocks, workTurn()...)
	}
	m.renderBlocks(100)
	b.ResetTimer()
	for b.Loop() {
		m.renderBlocks(100)
	}
}
