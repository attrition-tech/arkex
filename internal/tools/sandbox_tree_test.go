package tools

import (
	"context"
	"errors"
	"fmt"
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

// Model dependency trees with many small files and deep paths, rather than
// a few large files: rooted path traversal was the dominant cost here.
func sandboxDependencies(t testing.TB, workspace string) {
	t.Helper()
	data := []byte(strings.Repeat("export type Dependency = string;\n", 32))
	for pkg := range 200 {
		dir := filepath.Join(workspace, "web/node_modules", fmt.Sprint(pkg), "node_modules/library/dist/types/parsers")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for file := range 50 {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.d.ts", file)), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestBashDependencyTreeDoesNotConsumeCommandTimeout(t *testing.T) {
	workspace := t.TempDir()
	sandboxDependencies(t, workspace)
	sandboxFixture(t, filepath.Join(workspace, "source"), "matched\n")
	start := time.Now()
	res, err := runBash(t, &Bash{Dir: workspace, Shell: "/bin/sh"}, map[string]any{
		"command": "cat source; printf published > result", "timeout_ms": 500,
	})
	t.Logf("10,000 dependency files: %s", time.Since(start))
	if err != nil || res.Output != "matched\n" {
		t.Fatalf("short command failed due to sandbox overhead: %v: %s", err, res.Output)
	}
	sandboxContents(t, filepath.Join(workspace, "result"), "published")
}

func BenchmarkSandboxDependencyTree(b *testing.B) {
	workspace := b.TempDir()
	sandboxDependencies(b, workspace)
	for b.Loop() {
		run, err := sandboxCommand(b.Context(), workspace, "", workspace, "/bin/sh", "-c", "true")
		if err != nil {
			b.Fatal(err)
		}
		err = run.cmd.Run()
		if err == nil {
			err = run.Publish(b.Context())
		}
		run.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestSandboxSelectiveCaptureStillHashesUnchangedMetadata(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	sandboxFixture(t, filepath.Join(workspace, "deep/file"), "original")
	sandboxFixture(t, filepath.Join(workspace, "unchanged"), "keep")
	run := preparedSandbox(t, workspace, "", "true")
	executeSandbox(t, run)
	path := filepath.Join(run.trees[0].execution, "deep/file")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sandboxFixture(t, path, "modified") // Same length and restored mtime: stat-only detection misses it.
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := run.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	sandboxContents(t, filepath.Join(workspace, "deep/file"), "modified")
	sandboxContents(t, filepath.Join(run.temp, "capture-0/deep/file"), "modified")
	if _, err := os.Stat(filepath.Join(run.temp, "capture-0/unchanged")); !os.IsNotExist(err) {
		t.Fatalf("unchanged payload was copied: %v", err)
	}
	// Captured and published files must both be detached from execution FDs.
	sandboxFixture(t, path, "lateedit")
	sandboxContents(t, filepath.Join(workspace, "deep/file"), "modified")
	sandboxContents(t, filepath.Join(run.temp, "capture-0/deep/file"), "modified")
}

func TestSandboxNoopDoesNotScanOrCopyHostAgain(t *testing.T) {
	workspace := t.TempDir()
	sandboxFixture(t, filepath.Join(workspace, "file"), "before")
	run := preparedSandbox(t, workspace, "", "true")
	executeSandbox(t, run)
	sandboxFixture(t, filepath.Join(workspace, "file"), "host edit")
	// This would reject a host snapshot, but a no-op has nothing to publish.
	if err := unix.Mkfifo(filepath.Join(workspace, "host-fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	sandboxContents(t, filepath.Join(workspace, "file"), "host edit")
	entries, err := os.ReadDir(filepath.Join(run.temp, "capture-0"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("no-op created capture payloads: %v: %v", entries, err)
	}
}

// Use existing context checkpoints to deterministically mutate a file between
// the comparison read and the trusted capture read, without production hooks.
type sandboxMutationContext struct {
	context.Context
	check func()
}

func (c sandboxMutationContext) Err() error {
	c.check()
	return c.Context.Err()
}

func TestSandboxCaptureDetectsChangingSource(t *testing.T) {
	for _, mutation := range []string{"same-length", "truncate", "grow", "replace-with-symlink"} {
		t.Run(mutation, func(t *testing.T) {
			workspace := t.TempDir()
			sandboxFixture(t, filepath.Join(workspace, "file"), "original")
			run := preparedSandbox(t, workspace, "", "printf modified > file")
			executeSandbox(t, run)
			path := filepath.Join(run.trees[0].execution, "file")
			mutated := false
			ctx := sandboxMutationContext{Context: t.Context(), check: func() {
				if mutated {
					return
				}
				if _, err := os.Stat(filepath.Join(run.temp, "capture-0/file")); err != nil {
					return
				}
				mutated = true
				switch mutation {
				case "same-length":
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					sandboxFixture(t, path, "tampered")
					if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				case "truncate":
					sandboxFixture(t, path, "short")
				case "grow":
					sandboxFixture(t, path, "longer than captured")
				case "replace-with-symlink":
					outside := filepath.Join(t.TempDir(), "outside")
					sandboxFixture(t, outside, "must not capture")
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, path); err != nil {
						t.Fatal(err)
					}
				}
			}}
			err := run.Publish(ctx)
			if !mutated {
				t.Fatal("did not exercise the capture race")
			}
			if mutation == "replace-with-symlink" {
				if err != nil {
					t.Fatal(err)
				}
				sandboxContents(t, filepath.Join(workspace, "file"), "modified")
			} else {
				if err == nil || !strings.Contains(err.Error(), "file changed during capture") {
					t.Fatalf("mutation was not rejected: %v", err)
				}
				sandboxContents(t, filepath.Join(workspace, "file"), "original")
			}
		})
	}
}

func TestSandboxDirectorySubstitution(t *testing.T) {
	for _, replacement := range []string{"fifo", "directory", "outside-symlink", "inside-symlink"} {
		t.Run(replacement, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, "dir")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			source, err := os.OpenRoot(workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = source.Close() }()
			held, err := source.OpenFile("dir", os.O_RDONLY|unix.O_DIRECTORY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Close() }()
			if err := os.Rename(path, filepath.Join(workspace, "moved")); err != nil {
				t.Fatal(err)
			}
			switch replacement {
			case "fifo":
				err = unix.Mkfifo(path, 0o600)
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "outside-symlink":
				err = os.Symlink(t.TempDir(), path)
			case "inside-symlink":
				err = os.Symlink("moved", path)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				root, err := snapshotDirectory(source, "dir", held)
				if root != nil {
					_ = root.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				// A contained alias to the exact pinned directory grants no new access.
				if (err == nil) != (replacement == "inside-symlink") {
					t.Fatalf("substitution result: %v", err)
				}
			case <-time.After(2 * time.Second):
				// Unblock an accidentally blocking FIFO open so test cleanup completes.
				if replacement == "fifo" {
					if fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0); err == nil {
						_ = unix.Close(fd)
					}
				}
				<-done
				t.Fatal("directory substitution blocked snapshot traversal")
			}
		})
	}
}

func TestBashParentCancellationStillStopsPreparation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := (&Bash{Dir: t.TempDir(), Shell: "/bin/sh"}).Run(ctx, []byte(`{"command":"printf must-not-run"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation was lost: %v", err)
	}
}
