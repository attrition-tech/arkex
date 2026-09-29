package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func sandboxFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sandboxContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("%s: got %q (%v), want %q", path, data, err, want)
	}
}

func preparedSandbox(t *testing.T, workspace, scratch, command string) *sandboxRun {
	t.Helper()
	run, err := sandboxCommand(t.Context(), workspace, scratch, workspace, "/bin/sh", "-c", command)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	env := run.cmd.Environ()
	if scratch != "" {
		env = append(env, "TMPDIR="+scratch)
	}
	run.cmd.Env = run.environment(env)
	return run
}

func executeSandbox(t *testing.T, run *sandboxRun) {
	t.Helper()
	if out, err := run.cmd.CombinedOutput(); err != nil {
		t.Fatalf("command failed: %v: %s", err, out)
	}
}

func TestSandboxHardlinksAddedAfterPreparation(t *testing.T) {
	workspace, scratch, outside := t.TempDir(), t.TempDir(), t.TempDir()
	for _, root := range []string{workspace, scratch} {
		sandboxFixture(t, filepath.Join(root, "file"), "original")
	}
	run := preparedSandbox(t, workspace, scratch, `printf workspace > file; printf scratch > "$TMPDIR/file"`)
	// A scan/detach-before-exec implementation would miss these new aliases.
	for i, root := range []string{workspace, scratch} {
		if err := os.Link(filepath.Join(root, "file"), filepath.Join(outside, strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	executeSandbox(t, run)
	sandboxContents(t, filepath.Join(workspace, "file"), "original")
	if err := run.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	sandboxContents(t, filepath.Join(workspace, "file"), "workspace")
	sandboxContents(t, filepath.Join(scratch, "file"), "scratch")
	for _, name := range []string{"0", "1"} {
		sandboxContents(t, filepath.Join(outside, name), "original")
	}
}

func TestSandboxPublicationDoesNotTransferWritableInodes(t *testing.T) {
	workspace := t.TempDir()
	run := preparedSandbox(t, workspace, "", "printf captured > result")
	executeSandbox(t, run)
	// Simulate a descendant retaining a writable descriptor past command exit.
	file, err := os.OpenFile(filepath.Join(run.trees[0].execution, "result"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if err := run.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("tampered"), 0); err != nil {
		t.Fatal(err)
	}
	sandboxContents(t, filepath.Join(workspace, "result"), "captured")
}

func TestSandboxCannotCreateHardlinks(t *testing.T) {
	workspace, scratch := t.TempDir(), t.TempDir()
	source := filepath.Join(t.TempDir(), "outside")
	sandboxFixture(t, source, "protected")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The child exercises link and linkat, rather than relying on ln's choice.
	command := strconv.Quote(exe) + " -test.run=^TestSandboxHardlinkChild$ -- arkex-link-probe " + strconv.Quote(source)
	res, err := runBash(t, &Bash{Dir: workspace, TempDir: scratch, Shell: "/bin/sh"}, map[string]any{"command": command})
	if err != nil {
		t.Fatalf("hardlink denial probe: %v: %s", err, res.Output)
	}
	sandboxContents(t, source, "protected")
}

func TestSandboxHardlinkChild(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "arkex-link-probe" {
		return
	}
	source := os.Args[len(os.Args)-1]
	for _, dest := range []string{"link", filepath.Join(os.Getenv("TMPDIR"), "link")} {
		if err := unix.Link(source, dest); err == nil {
			t.Fatal("link allowed an outside alias")
		}
		if err := unix.Linkat(unix.AT_FDCWD, source, unix.AT_FDCWD, dest, 0); err == nil {
			t.Fatal("linkat allowed an outside alias")
		}
	}
}

func TestSandboxPublishesChangesWithoutTouchingUnchangedFiles(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	sandboxFixture(t, filepath.Join(workspace, "unchanged"), "keep")
	sandboxFixture(t, filepath.Join(workspace, "delete"), "remove")
	sandboxFixture(t, filepath.Join(outside, "mode"), "metadata")
	if err := os.Link(filepath.Join(outside, "mode"), filepath.Join(workspace, "mode")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(filepath.Join(workspace, "unchanged"))
	res, err := runBash(t, &Bash{Dir: workspace, Shell: "/bin/sh"}, map[string]any{"command": "mkdir -p new/sub; printf nested > new/sub/file; ln -s sub/file new/link; rm delete; chmod 755 mode; exit 3"})
	if err == nil || err.Error() != "exit status 3" {
		t.Fatalf("nonzero command: %v: %s", err, res.Output)
	}
	sandboxContents(t, filepath.Join(workspace, "new/link"), "nested")
	if _, err := os.Stat(filepath.Join(workspace, "delete")); !os.IsNotExist(err) {
		t.Fatal("deletion not published", err)
	}
	after, _ := os.Stat(filepath.Join(workspace, "unchanged"))
	if !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
		t.Fatal("unchanged file was replaced or touched")
	}
	inside, _ := os.Stat(filepath.Join(workspace, "mode"))
	other, _ := os.Stat(filepath.Join(outside, "mode"))
	if inside.Mode().Perm() != 0o755 || other.Mode().Perm() != 0o600 {
		t.Fatal("mode change affected the outside hardlink or was not published")
	}
}

func TestSandboxPublicationConflicts(t *testing.T) {
	for _, change := range []string{"edit", "new", "deleted-child", "symlink-parent", "scratch"} {
		t.Run(change, func(t *testing.T) {
			workspace, scratch, outside := t.TempDir(), t.TempDir(), t.TempDir()
			sandboxFixture(t, filepath.Join(workspace, "existing"), "before")
			if err := os.Mkdir(filepath.Join(workspace, "dir"), 0o700); err != nil {
				t.Fatal(err)
			}
			sandboxFixture(t, filepath.Join(workspace, "dir/child"), "before")
			run := preparedSandbox(t, workspace, scratch, `printf after > existing; printf new > new; rm -r dir; printf scratch > "$TMPDIR/new"`)
			executeSandbox(t, run)
			switch change {
			case "edit":
				sandboxFixture(t, filepath.Join(workspace, "existing"), "host-edit")
			case "new":
				sandboxFixture(t, filepath.Join(workspace, "new"), "host-edit")
			case "deleted-child":
				sandboxFixture(t, filepath.Join(workspace, "dir/added"), "host-edit")
			case "symlink-parent":
				if err := os.RemoveAll(filepath.Join(workspace, "dir")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(workspace, "dir")); err != nil {
					t.Fatal(err)
				}
			case "scratch":
				sandboxFixture(t, filepath.Join(scratch, "new"), "host-edit")
			}
			if err := run.Publish(t.Context()); err == nil || !strings.Contains(err.Error(), "changed during execution") {
				t.Fatalf("conflict not rejected: %v", err)
			}
			want := "before"
			if change == "edit" {
				want = "host-edit"
			}
			sandboxContents(t, filepath.Join(workspace, "existing"), want)
		})
	}
}

func TestSandboxUnrelatedHostEditSurvivesPublication(t *testing.T) {
	workspace := t.TempDir()
	sandboxFixture(t, filepath.Join(workspace, "host"), "old")
	run := preparedSandbox(t, workspace, "", "printf shell > result")
	executeSandbox(t, run)
	sandboxFixture(t, filepath.Join(workspace, "host"), "new")
	if err := run.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	sandboxContents(t, filepath.Join(workspace, "host"), "new")
	sandboxContents(t, filepath.Join(workspace, "result"), "shell")
}

func TestSandboxRejectsUnsafeOutputBeforePublishing(t *testing.T) {
	for _, command := range []string{"mkfifo fifo", "ln -s PRIVATE_COPY private-link"} {
		t.Run(command, func(t *testing.T) {
			workspace := t.TempDir()
			run := preparedSandbox(t, workspace, "", "printf result > ordinary")
			executeSandbox(t, run)
			if strings.HasPrefix(command, "mkfifo") {
				if err := unix.Mkfifo(filepath.Join(run.trees[0].execution, "fifo"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(run.trees[0].execution, filepath.Join(run.trees[0].execution, "private-link")); err != nil {
				t.Fatal(err)
			}
			if err := run.Publish(t.Context()); err == nil {
				t.Fatal("unsafe output accepted")
			}
			if _, err := os.Stat(filepath.Join(workspace, "ordinary")); !os.IsNotExist(err) {
				t.Fatal("preflight failure published some output")
			}
		})
	}
}

func TestSandboxCanceledPublicationDoesNotApply(t *testing.T) {
	workspace := t.TempDir()
	run := preparedSandbox(t, workspace, "", "printf result > result")
	executeSandbox(t, run)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := run.Publish(ctx); err == nil {
		t.Fatal("canceled publication succeeded")
	}
	if _, err := os.Stat(filepath.Join(workspace, "result")); !os.IsNotExist(err) {
		t.Fatal("canceled publication changed the workspace")
	}
}

func TestSandboxPublishesTimestampsAndReadOnlyDirectories(t *testing.T) {
	workspace := t.TempDir()
	sandboxFixture(t, filepath.Join(workspace, "touched"), "same bytes")
	if err := os.MkdirAll(filepath.Join(workspace, "removed/sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	sandboxFixture(t, filepath.Join(workspace, "removed/sub/file"), "delete")
	run := preparedSandbox(t, workspace, "", "mkdir -p readonly/sub; printf value > readonly/sub/file; chmod 555 readonly/sub readonly; rm -r removed")
	executeSandbox(t, run)
	stamp := time.Unix(1234567890, 0)
	if err := os.Chtimes(filepath.Join(run.trees[0].execution, "touched"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(workspace, "readonly"), 0o700)
		_ = os.Chmod(filepath.Join(workspace, "readonly/sub"), 0o700)
	})
	if err := run.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(workspace, "touched"))
	if err != nil || !info.ModTime().Equal(stamp) {
		t.Fatalf("touch was not published: %v: %v", info, err)
	}
	sandboxContents(t, filepath.Join(workspace, "readonly/sub/file"), "value")
	for _, dir := range []string{"readonly", "readonly/sub"} {
		info, err := os.Stat(filepath.Join(workspace, dir))
		if err != nil || info.Mode().Perm() != 0o555 {
			t.Fatalf("directory mode not published: %v: %v", info, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "removed")); !os.IsNotExist(err) {
		t.Fatalf("directory deletion not published: %v", err)
	}
	run.Close()
	if _, err := os.Stat(run.temp); !os.IsNotExist(err) {
		t.Fatalf("read-only execution files prevented cleanup: %v", err)
	}
}

func TestSandboxRejectsOversizedInputBeforeExecution(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "sparse")
	sandboxFixture(t, path, "")
	if err := os.Truncate(path, sandboxTreeBytes+1); err != nil {
		t.Fatal(err)
	}
	_, err := sandboxCommand(t.Context(), workspace, "", workspace, "/bin/sh", "-c", "touch must-not-run")
	if err == nil || !strings.Contains(err.Error(), "exceeds 8 GiB") {
		t.Fatalf("oversized input not rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "must-not-run")); !os.IsNotExist(err) {
		t.Fatal("setup failure ran the command")
	}
}
