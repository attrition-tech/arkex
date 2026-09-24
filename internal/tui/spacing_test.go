package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/dantearo/arkex/internal/agent"
)

func TestUserMessageHasHalfTheOuterMargin(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = []*block{newBlock(blockUser, "hello"), newBlock(blockAssistant, "reply")}
	for _, width := range []int{40, 100} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 34})
		user := ansi.Strip(m.vp.lines[0])
		if !strings.HasPrefix(user, " ┃ ") || ansi.StringWidth(user) != width-1 {
			t.Fatalf("user margins at %d: %q", width, user)
		}
		assistant := ansi.Strip(m.vp.lines[m.spans[1].top])
		if !strings.HasPrefix(assistant, "  reply") {
			t.Fatalf("reply moved: %q", assistant)
		}
		if m.spans[0].bottom-m.spans[0].top != 1 {
			t.Fatal("single-line user message has an extra label row")
		}
		m.sel = &selection{anchor: pos{line: 0, col: 0}, head: pos{line: 0, col: width - 1}}
		if got := m.selectedText(); got != "┃ hello" {
			t.Fatalf("copy lost accent or text: %q", got)
		}
	}
}

func TestSymmetricTranscriptAndInputSpacing(t *testing.T) {
	m, _ := testModel(t)
	for _, width := range []int{40, 80, 121} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 34})
		m.blocks = []*block{newBlock(blockSystem, strings.Repeat("x", width-4))}
		m.refresh()
		line := ansi.Strip(m.vp.lines[0])
		if line != "  "+strings.Repeat("x", width-4) || ansi.StringWidth(line) != width-2 {
			t.Fatalf("width %d: transcript margins: %q", width, line)
		}
		m.input.SetValue(strings.Repeat("x", width-4))
		m.resizeInput()
		rows := strings.Split(ansi.Strip(m.inputView()), "\n")
		for _, row := range rows {
			if ansi.StringWidth(row) != width {
				t.Fatalf("input width %d: %q", width, row)
			}
		}
		if rows[1] != "│ "+strings.Repeat("x", width-4)+" │" {
			t.Fatalf("asymmetric input content at %d: %q", width, rows[1])
		}
		if c := m.View().Cursor; c == nil || c.X < 2 || c.X >= width-2 {
			t.Fatalf("cursor outside padded input: %+v", c)
		}
	}
}

func TestWhitespaceAnswerBetweenThinkingAndToolHasNoRows(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = nil
	m.applyEvent(agent.ReasoningDelta{Text: "Inspect the files."})
	m.applyEvent(agent.TextDelta{Text: "\n\n "})
	m.applyEvent(agent.ToolCall{ID: "read", Name: "bash", Input: `{"command":"ls"}`})
	lines := m.renderBlocks(80)
	if len(lines) != 3 || strings.TrimSpace(lines[1]) != "" {
		t.Fatalf("want thinking, one separator, tool; got %d lines: %q", len(lines), lines)
	}
}

func TestEmptyMarkdownHasNoTranscriptRows(t *testing.T) {
	for _, body := range []string{"", "\n\n ", "<!-- hidden -->"} {
		for _, streaming := range []bool{false, true} {
			m, _ := testModel(t)
			m.running = streaming
			b := newBlock(blockAssistant, body)
			m.blocks = []*block{b}
			if cmd := m.streamMarkdown(); cmd != nil {
				m.Update(cmd())
			}
			if lines := m.blockLines(b, 76, streaming); len(lines) != 0 {
				t.Fatalf("body %q streaming=%v: invisible Markdown took %d rows", body, streaming, len(lines))
			}
		}
	}
}

func TestTableBlockSpacing(t *testing.T) {
	var md markdown
	out := ansi.Strip(md.render("## Files\n\n| Path | Purpose |\n|---|---|\n| `src/a` | first |\n| `src/b` | second |\n\nAfter.", 60))
	if !strings.HasPrefix(out, "Files\n\n╭") || !strings.HasSuffix(out, "╯\n\nAfter.") || strings.Contains(out, "\n\n\n") {
		t.Fatalf("want one blank line around the table: %q", out)
	}
}

func TestTranscriptPreservesCodeBlankLines(t *testing.T) {
	m, _ := testModel(t)
	b := newBlock(blockAssistant, "Before.\n\n```text\nalpha\n\n\nbeta\n```\n\nAfter.")
	lines := m.blockLines(b, 76, false)
	for i, line := range lines {
		if strings.TrimSpace(ansi.Strip(line)) == "alpha" {
			if i+3 >= len(lines) || strings.TrimSpace(ansi.Strip(lines[i+1])) != "" || strings.TrimSpace(ansi.Strip(lines[i+2])) != "" || strings.TrimSpace(ansi.Strip(lines[i+3])) != "beta" {
				t.Fatalf("code blank lines changed: %q", lines)
			}
			return
		}
	}
	t.Fatalf("code missing: %q", lines)
}
