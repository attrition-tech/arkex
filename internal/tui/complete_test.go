package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dantearo/arkex/internal/agent"
)

func TestWordAt(t *testing.T) {
	line := []rune("/model loc al")
	w, s, e := wordAt(line, 10)
	if w != "loc" || s != 7 || e != 10 {
		t.Fatalf("got %q %d %d", w, s, e)
	}
	w, s, e = wordAt(line, 7) // cursor right after the space
	if w != "loc" || s != 7 || e != 10 {
		t.Fatalf("at word start: got %q %d %d", w, s, e)
	}
	w, s, e = wordAt([]rune("/model "), 7)
	if w != "" || s != 7 || e != 7 {
		t.Fatalf("after trailing space: got %q %d %d", w, s, e)
	}
}

func TestFuzzyFilesRanking(t *testing.T) {
	files := []string{"internal/tui/app.go", "cmd/arkex/main.go", "internal/agent/mode.go", "README.md", "internal/tui/models.go"}
	got := fuzzyFiles(files, "mod", 10)
	if len(got) != 2 {
		t.Fatalf("expected mode.go and models.go only, got %+v", got)
	}
	if got := fuzzyFiles([]string{"a/very/long/path/mode.go", "x/mode.go"}, "mode", 10); got[0].value != "@x/mode.go" {
		t.Fatalf("shorter path first, got %+v", got)
	}
	if got := fuzzyFiles([]string{"models/x.go", "a/model.go"}, "model", 10); got[0].value != "@a/model.go" {
		t.Fatalf("match in file name should beat match in directory, got %+v", got)
	}
	if hits := fuzzyFiles(files, "zzz", 10); len(hits) != 0 {
		t.Fatalf("no match expected, got %+v", hits)
	}
	if hits := fuzzyFiles(files, "tapp", 10); len(hits) != 1 || hits[0].value != "@internal/tui/app.go" {
		t.Fatalf("subsequence t…app should match app.go only, got %+v", hits)
	}
}

func TestExpandMentions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := expandMentions(dir, "look at @a.txt, and @sub and @missing.go")
	if !strings.HasPrefix(got, "look at @a.txt, and @sub and @missing.go") {
		t.Fatalf("original text must be preserved: %q", got)
	}
	if !strings.Contains(got, `<file path="a.txt">`+"\nalpha\n</file>") {
		t.Fatalf("file body missing: %q", got)
	}
	if strings.Contains(got, "sub\"") || strings.Contains(got, "missing") && strings.Count(got, "missing") > 1 {
		t.Fatalf("directories and missing files must not be attached: %q", got)
	}
	if plain := expandMentions(dir, "no mentions"); plain != "no mentions" {
		t.Fatalf("text without mentions must pass through, got %q", plain)
	}
}

func TestFileCompletionFlow(t *testing.T) {
	m := newModel(Options{Cwd: t.TempDir(), Mode: agent.NewModePolicy(agent.ModeBuild, nil)})
	m.width, m.height = 100, 30
	m.files = []string{"internal/tui/app.go", "README.md"}

	m.input.SetValue("look at @app")
	m.updateCompletion()
	if m.comp == nil || len(m.comp.items) != 1 || m.comp.items[0].value != "@internal/tui/app.go" {
		t.Fatalf("expected one file match, got %+v", m.comp)
	}
	m.accept()
	if m.input.Value() != "look at @internal/tui/app.go " || m.comp != nil {
		t.Fatalf("value = %q comp = %v", m.input.Value(), m.comp)
	}

	// Slash commands no longer open the bottom popup; the palette owns them.
	for _, v := range []string{"/mo", "/mode p", "hello /mo"} {
		m.input.SetValue(v)
		m.updateCompletion()
		if m.comp != nil {
			t.Fatalf("%q must not open the popup: %+v", v, m.comp)
		}
	}
}
