package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/attrition-tech/arkex/internal/tools"
)

func TestPlanNoteUsesRegisteredTools(t *testing.T) {
	for _, tc := range []struct {
		name             string
		registry         *tools.Registry
		allowed, refused string
	}{
		{"default", tools.Default(""), "read", "write, edit, bash"},
		{"subset", tools.NewRegistry(&tools.Bash{}, &tools.Read{}), "read", "bash"},
		{"mutating only", tools.NewRegistry(&tools.Edit{}), "none", "edit"},
		{"empty", nil, "none", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanNote(tc.registry)
			want := "Read-only tools permitted by this mode: " + tc.allowed + ". Registered tools refused by this mode: " + tc.refused + "."
			if !strings.Contains(got, want) {
				t.Fatalf("want %q in %q", want, got)
			}
		})
	}
}

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
	rootInstructions, err := filepath.EvalSymlinks(filepath.Join(repo, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	subInstructions, err := filepath.EvalSymlinks(filepath.Join(sub, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"You are arkex",
		"Working directory: " + sub + "\n",
		"Date: 2026-09-18\n",
		"Model: local/m\n",
		"# Project instructions from " + rootInstructions + "\nAlways run go vet.\n",
		"# Project instructions from " + subInstructions + "\ncmd rules\n",
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

func TestInstructionsNormalizeDirectoryAliases(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "real", "AGENTS.md"), "same rules")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "real"), alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if Instructions(filepath.Join(alias, "AGENTS.md"), "same rules") != Instructions(filepath.Join(root, "real", "AGENTS.md"), "same rules") {
		t.Fatal("same instruction file has different identities through directory alias")
	}
}
