package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgentsFilesWalksUpToRepoRootOutermostFirst(t *testing.T) {
	root := t.TempDir()
	// Above the repo: must be ignored because .git stops the walk.
	write(t, filepath.Join(root, "AGENTS.md"), "outside")
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "AGENTS.md"), "repo")
	// A middle directory with no AGENTS.md, then a leaf with one.
	leaf := filepath.Join(repo, "internal", "tui")
	write(t, filepath.Join(leaf, "AGENTS.md"), "leaf")

	got := AgentsFiles(leaf)
	want := []string{filepath.Join(repo, "AGENTS.md"), filepath.Join(leaf, "AGENTS.md")}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("AgentsFiles = %v, want %v", got, want)
	}
	// From the repo root itself only the root file applies.
	if got := AgentsFiles(repo); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("at root = %v", got)
	}
}

func TestAgentsFilesWithoutGitStopsAtFilesystemRoot(t *testing.T) {
	dir := t.TempDir()
	// No .git anywhere under TempDir; the walk must terminate and return
	// nothing from a directory tree that has no AGENTS.md files.
	for _, f := range AgentsFiles(dir) {
		if strings.HasPrefix(f, dir) {
			t.Fatalf("unexpected file %s", f)
		}
	}
}

func TestBuildIncludesEnvironmentAndProjectInstructions(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "AGENTS.md"), "Always run go vet.")
	sub := filepath.Join(repo, "cmd")
	write(t, filepath.Join(sub, "AGENTS.md"), "cmd rules")

	p := Build(Options{Cwd: sub, Now: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC), Model: "local/m"})

	for _, want := range []string{
		"You are arkex",
		"Working directory: " + sub + "\n",
		"Date: 2026-09-18\n",
		"Model: local/m\n",
		"# Project instructions from " + filepath.Join(repo, "AGENTS.md") + "\nAlways run go vet.\n",
		"# Project instructions from " + filepath.Join(sub, "AGENTS.md") + "\ncmd rules\n",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Index(p, "Always run go vet.") > strings.Index(p, "cmd rules") {
		t.Fatal("outer AGENTS.md must come before the inner one")
	}

	// Zero time and empty model leave their lines out entirely.
	p = Build(Options{Cwd: t.TempDir()})
	if strings.Contains(p, "Date:") || strings.Contains(p, "Model:") || strings.Contains(p, "Project instructions") {
		t.Fatalf("optional lines present:\n%s", p)
	}
}
