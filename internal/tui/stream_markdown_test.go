package tui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestStreamingMarkdownSnapshotAndFinalFlush(t *testing.T) {
	m, _ := testModel(t)
	m.running = true
	b := newBlock(blockAssistant, "## Heading\n\n- first\n\n```go\n// ## literal\n")
	m.blocks = []*block{b}
	cmd := m.streamMarkdown()
	if cmd == nil || m.streamMarkdown() != nil {
		t.Fatal("expected exactly one render in flight")
	}
	// Deltas arriving during the render belong to the next immutable snapshot.
	result := make(chan streamMarkdownMsg, 1)
	go func() { result <- cmd().(streamMarkdownMsg) }()
	b.text.WriteString("```\n\nFinal **words**.")
	msg := <-result
	m.Update(msg)
	got := ansi.Strip(strings.Join(b.lines, "\n"))
	if strings.Contains(got, "## Heading") || !strings.Contains(got, "Heading") ||
		!strings.Contains(got, "• first") || !strings.Contains(got, "// ## literal") || strings.Contains(got, "Final") {
		t.Fatalf("bad streaming snapshot: %q", got)
	}
	if m.streamMarkdown() != nil {
		t.Fatal("render did not back off")
	}
	// The working tick must schedule pending text even if no more tokens arrive.
	m.markdownNext = time.Time{}
	m.workingFrame(workingMsg{gen: m.runGen})
	if !m.markdownBusy {
		t.Fatal("working tick did not flush pending Markdown")
	}
	m.running = false
	m.refresh()
	final := strings.Join(b.lines, "\n")
	if !strings.Contains(ansi.Strip(final), "Final words.") {
		t.Fatalf("completion lost pending text: %q", final)
	}
	m.Update(msg)
	if strings.Join(b.lines, "\n") != final || b.streamMD != nil {
		t.Fatal("late snapshot overwrote completed reply")
	}
}

func TestStreamingMarkdownRejectsStaleResults(t *testing.T) {
	for _, change := range []string{"width", "theme", "run", "block"} {
		t.Run(change, func(t *testing.T) {
			m, _ := testModel(t)
			m.running = true
			b := newBlock(blockAssistant, "## old")
			m.blocks = []*block{b}
			cmd := m.streamMarkdown()
			switch change {
			case "width":
				m.width++
			case "theme":
				applyTheme(themes[1])
				t.Cleanup(func() { applyTheme(themes[0]) })
			case "run":
				m.runGen++
			case "block":
				m.blocks = []*block{newBlock(blockAssistant, "replacement")}
			}
			m.Update(cmd())
			if b.streamMD != nil || m.markdownBusy {
				t.Fatal("accepted stale result or failed to release render slot")
			}
			m.markdownNext = time.Time{}
			if m.streamMarkdown() == nil {
				t.Fatal("new state cannot render")
			}
		})
	}
}

func BenchmarkStreamingMarkdownSnapshot(b *testing.B) {
	for _, size := range []int{10, 100} {
		text := strings.Repeat("## Heading\n\nSome **bold** text and `code`.\n\n- first\n- second\n\n", size)
		b.Run(strconv.Itoa(len(text))+"bytes", func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			for b.Loop() {
				r := newMarkdownRenderer(theme.Markdown, 100, theme.Muted)
				out, err := r.Render(text)
				if err != nil {
					b.Fatal(err)
				}
				_ = tidy(out)
			}
		})
	}
}
