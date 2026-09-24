package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestMarkdownHeadingsWithoutMarkers(t *testing.T) {
	for level := 1; level <= 6; level++ {
		var md markdown
		out := ansi.Strip(md.render(strings.Repeat("#", level)+" Heading\n\nBody text.", 60))
		if !strings.HasPrefix(out, "Heading\n\n") || !strings.Contains(out, "Body text.") {
			t.Fatalf("heading level %d: %q", level, out)
		}
	}
	var md markdown
	out := ansi.Strip(md.render("## Heading\n\nInline `## literal`.\n\n```text\n## code heading\n```", 60))
	if !strings.Contains(out, "## literal") || !strings.Contains(out, "## code heading") {
		t.Fatalf("literal code changed: %q", out)
	}
}

func TestMarkdownRenderTidy(t *testing.T) {
	var md markdown
	out := md.render("Hello **world**\n\n- one\n- two", 40)
	if out == "" {
		t.Fatal("empty render")
	}
	lines := strings.Split(out, "\n")
	if strings.TrimSpace(ansi.Strip(lines[0])) == "" || strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		t.Fatalf("outer blank lines not trimmed:\n%q", out)
	}
	for _, l := range lines {
		if strings.HasSuffix(ansi.Strip(l), " ") {
			t.Fatalf("trailing padding left on line %q", ansi.Strip(l))
		}
	}
	if !strings.Contains(ansi.Strip(out), "Hello world") || !strings.Contains(ansi.Strip(out), "• one") {
		t.Fatalf("unexpected content:\n%s", ansi.Strip(out))
	}
	// Same renderer is reused at the same width and replaced on change.
	first := md.r
	md.render("x", 40)
	if md.r != first {
		t.Fatal("renderer rebuilt for same width")
	}
	md.render("x", 60)
	if md.r == first {
		t.Fatal("renderer not rebuilt for new width")
	}
}

func TestRenderToolCollapsesToOneLine(t *testing.T) {
	b := &block{kind: blockTool, name: "bash", status: "ok", args: toolArgs(`{"command":"seq 1 20"}`), dur: 186 * time.Millisecond}
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		sb.WriteString(strings.Repeat("x", i%3) + "line\n")
	}
	b.output = sb.String()

	collapsed := ansi.Strip(renderTool(b, "", 80, false, false))
	if collapsed != "✓ bash $ seq 1 20 · 186ms ▸" {
		t.Fatalf("collapsed card:\n%q", collapsed)
	}
	expanded := ansi.Strip(renderTool(b, "", 80, true, false))
	if strings.Count(expanded, "\n") != 23 || strings.Count(expanded, "line") < 20 || !strings.HasSuffix(strings.SplitN(expanded, "\n", 2)[0], "▾") || !strings.Contains(expanded, "command:\n  seq 1 20\n") {
		t.Fatalf("expanded card should show the header and every line:\n%s", expanded)
	}
}

func TestRenderToolTails(t *testing.T) {
	cases := []struct {
		name string
		b    *block
		want string
	}{
		{"write counts", &block{kind: blockTool, name: "write", status: "ok", args: toolArgs(`{"path":"ApplicationsPage.jsx"}`), detail: "+a\n+b\n+c", dur: time.Millisecond}, "✓ write ApplicationsPage.jsx · +3 −0 · 1ms ▸"},
		{"edit counts", &block{kind: blockTool, name: "edit", status: "ok", args: toolArgs(`{"path":"a.go"}`), detail: "-old\n+new\n+more"}, "✓ edit a.go · +2 −1 ▸"},
		{"bash exit", &block{kind: blockTool, name: "bash", status: "error", args: toolArgs(`{"command":"make"}`), output: "boom\n[exit status 2 in 12ms]"}, "✗ bash $ make · exit 2 ▸"},
		{"bash timeout", &block{kind: blockTool, name: "bash", status: "error", args: toolArgs(`{"command":"sleep 9"}`), output: "[command timed out after 1s]"}, "✗ bash $ sleep 9 · timed out ▸"},
		{"read", &block{kind: blockTool, name: "read", status: "ok", args: toolArgs(`{"path":"a.go"}`), summary: "a.go (40 lines)"}, "✓ read a.go · 40 lines"},
		{"running", &block{kind: blockTool, name: "bash", status: "running", args: toolArgs(`{"command":"go test"}`)}, "⋯ bash $ go test ▸"},
	}
	for _, tc := range cases {
		if got := ansi.Strip(renderTool(tc.b, "", 80, false, false)); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestRenderToolDenied(t *testing.T) {
	b := &block{kind: blockTool, name: "edit", status: "denied", summary: "plan mode permits only read-only tools", args: toolArgs(`{"path":"a.go"}`)}
	out := ansi.Strip(renderTool(b, "", 80, false, false))
	if out != "✗ edit a.go · denied ▸" {
		t.Fatalf("denied card collapsed:\n%s", out)
	}
	if out = ansi.Strip(renderTool(b, "", 80, true, false)); !strings.Contains(out, "plan mode") {
		t.Fatalf("denied card expanded should show the reason:\n%s", out)
	}
}

func TestDisplayPathIsRelativeInsideTheProject(t *testing.T) {
	home, _ := os.UserHomeDir()
	cwd := filepath.Join(home, "Projects", "landing")
	cases := map[string]string{
		filepath.Join(cwd, "app", "views", "home.html.erb"): "app/views/home.html.erb",
		cwd:                                  ".",
		filepath.Join(home, "other", "x.go"): "~/other/x.go",
		filepath.Join(home, "Projects", "landing-2", "a"): "~/Projects/landing-2/a", // sibling with a shared prefix is outside
		"/etc/hosts":          "/etc/hosts",
		"relative/already.go": "relative/already.go",
		"":                    "",
	}
	for in, want := range cases {
		want = filepath.ToSlash(want)
		if got := displayPath(cwd, in); got != want {
			t.Errorf("displayPath(%q) = %q, want %q", in, got, want)
		}
	}
	b := newBlock(blockTool, "")
	b.name, b.status = "read", "ok"
	b.args = map[string]any{"path": filepath.Join(cwd, "README.md")}
	if got := ansi.Strip(renderTool(b, cwd, 80, false, false)); !strings.Contains(got, "read README.md") || strings.Contains(got, home) {
		t.Fatalf("card title = %q", got)
	}
}

func TestMultilineCommandDetailsPreserveSourceAndWrap(t *testing.T) {
	command := "python3 - <<'PY'\nvalue = '界🙂'\nprint(value)\nPY"
	b := &block{kind: blockTool, name: "bash", status: "ok", args: map[string]any{"command": command}}
	got := ansi.Strip(renderTool(b, "", 80, false, false))
	if got != "✓ bash Multiline command · 4 lines ▸" {
		t.Fatal(got)
	}
	got = ansi.Strip(renderTool(b, "", 80, true, false))
	if !strings.Contains(got, "  python3 - <<'PY'\n  value = '界🙂'\n  print(value)\n  PY") {
		t.Fatal("silent script source unavailable or newlines lost", got)
	}
	// Unlike a short sample, these unbroken tokens reveal truncation of
	// either command source or output in the expanded card.
	b.args["command"] = "printf " + strings.Repeat("abcdef", 30) + "SOURCE_END"
	b.output = strings.Repeat("xyz", 50) + "OUTPUT_END"
	for _, width := range []int{16, 24, 40, 80} {
		got = ansi.Strip(renderTool(b, "", width, true, false))
		lines := strings.Split(got, "\n")
		var content strings.Builder
		for _, line := range lines[1:] {
			if ansi.StringWidth(line) > width {
				t.Fatalf("overflow at %d: %q", width, line)
			}
			content.WriteString(strings.TrimPrefix(line, "  "))
		}
		if !strings.Contains(content.String(), b.args["command"].(string)) || !strings.Contains(content.String(), b.output) {
			t.Fatalf("expanded data lost at %d:\n%s", width, got)
		}
	}
}
