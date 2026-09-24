package tui

import (
	"strings"
	"testing"

	glamouransi "charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestMarkdownDocumentColourDoesNotStyleDiscardedPadding(t *testing.T) {
	red := "1"
	styles := glamouransi.StyleConfig{}
	styles.Document.Color = &red
	r := &tableMarkdownRenderer{styles: styles, width: 80}
	out, err := r.Render("plain **strong** text")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(ansi.Strip(out)); got != "plain strong text" {
		t.Fatalf("text semantics changed: %q", got)
	}
	// The inherited document colour must still reach text, while right padding
	// remains unstyled. Glamour otherwise emits one SGR pair per padding cell.
	if !strings.Contains(out, "\x1b[") {
		t.Fatal("document foreground was lost")
	}
	if n := strings.Count(out, "\x1b["); n > 20 {
		t.Fatalf("discarded padding is styled (%d SGR sequences): %q", n, out)
	}
}

func TestMarkdownTableOutline(t *testing.T) {
	t.Cleanup(func() { applyTheme(themes[0]) })
	for _, th := range themes {
		applyTheme(th)
		for _, width := range []int{30, 90} {
			var md markdown
			out := md.render("| Component | AED |\n|:---|---:|\n| **Long component name wraps safely** | 18.25 |\n| `literal` | 3 |", width)
			plain := ansi.Strip(out)
			if !strings.HasPrefix(plain, "╭") || !strings.HasSuffix(plain, "╯") || !strings.Contains(plain, "├") || !strings.Contains(plain, "┤") {
				t.Fatalf("missing outline %s/%d: %q", th.Name, width, plain)
			}
			if !strings.Contains(out, lipgloss.NewStyle().Foreground(th.Muted).Render("╭")) {
				t.Fatalf("border does not use input colour %s: %q", th.Name, out)
			}
			for _, line := range strings.Split(plain, "\n") {
				if ansi.StringWidth(line) != width {
					t.Fatalf("wrong width %d: %q", width, line)
				}
			}
			if !strings.Contains(plain, "18.25") || !strings.Contains(plain, "literal") {
				t.Fatal("lost cell contents")
			}
		}
	}
}

func TestMarkdownTableReferencesAndCode(t *testing.T) {
	var md markdown
	out := ansi.Strip(md.render("Before.\n\n| Link | Escaped |\n|---|---|\n| [site][ref] | a\\|b |\n\n[ref]: https://example.com\n\nAfter.\n\n```text\n| Not | table |\n|---|---|\n```", 70))
	if strings.Count(out, "╭") != 1 || !strings.Contains(out, "https://example.com") || !strings.Contains(out, "a|b") || !strings.Contains(out, "After.") || !strings.Contains(out, "| Not | table |") {
		t.Fatalf("Markdown changed: %q", out)
	}
}

func TestMarkdownTablesNestedAndStreaming(t *testing.T) {
	const body = "| Name | Cost |\n|---|---:|\n| alpha | 123 |\n| beta | 7 |"
	var md markdown
	out := ansi.Strip(md.render("> "+strings.ReplaceAll(body, "\n", "\n> ")+"\n\n"+body, 50))
	if strings.Count(out, "╭") != 2 || strings.Count(out, "╯") != 2 {
		t.Fatalf("nested outline lost: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if ansi.StringWidth(line) > 50 {
			t.Fatalf("nested table overflow: %q", line)
		}
	}
	m, _ := testModel(t)
	m.running = true
	b := newBlock(blockAssistant, body)
	m.blocks = []*block{b}
	m.Update(m.streamMarkdown()())
	if !strings.Contains(strings.Join(b.lines, "\n"), "╭") {
		t.Fatal("streaming table lacks outline")
	}
	plain := ansi.Strip(md.render("| Text |\n|---|\n| &amp;amp; |", 40))
	if !strings.Contains(plain, "&amp;") {
		t.Fatalf("text entity decoded twice: %q", plain)
	}
}
