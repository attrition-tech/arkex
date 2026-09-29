package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// Release gate for the promised outside-write boundary. Read-only mounts of
// outside paths must also protect an inode aliased inside the workspace.
func TestSandboxShellCannotModifyOutsideHardlink(t *testing.T) {
	workspace, scratch, outside := t.TempDir(), t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(target, []byte("outside-original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(workspace, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	result, err := runBash(t, &Bash{Dir: workspace, TempDir: scratch, Shell: "/bin/sh"}, map[string]any{"command": "printf changed-from-workspace > linked.txt"})
	if err != nil {
		t.Fatalf("ordinary workspace write failed: %v: %s", err, result.Output)
	}
	inside, err := os.ReadFile(filepath.Join(workspace, "linked.txt"))
	if err != nil || string(inside) != "changed-from-workspace" {
		t.Fatalf("workspace change not published: %q: %v", inside, err)
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "outside-original" {
		t.Fatalf("outside file changed through pre-existing workspace hard link: %q (shell err=%v output=%q)", data, err, result.Output)
	}
}
