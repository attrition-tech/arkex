package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

func filesystemRun(t *testing.T, tool Tool, input map[string]any) string {
	t.Helper()
	result, err := tool.Run(context.Background(), args(t, input))
	if err != nil {
		t.Fatal(err)
	}
	return result.Output
}

func filesystemFail(t *testing.T, tool Tool, input map[string]any) {
	t.Helper()
	if result, err := tool.Run(context.Background(), args(t, input)); err == nil {
		t.Fatalf("unexpected success: %+v", result)
	}
}

func filesystemMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s still exists: %v", path, err)
	}
}

func TestFilesystemInventory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "abc")
	writeFile(t, filepath.Join(root, ".hidden", "note.txt"), "hidden")
	writeFile(t, filepath.Join(root, "node_modules", "generated.txt"), "generated")
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "outside-only"), "secret")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	ls := &List{Root: root}
	out := filesystemRun(t, ls, nil)
	for _, want := range []string{"file\t3\t\"main.go\"", "dir\t", "\".hidden\"", "\"node_modules\"", "symlink\t"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "note.txt") {
		t.Fatalf("default listing recursed: %s", out)
	}
	out = filesystemRun(t, ls, map[string]any{"recursive": true, "pattern": "*.txt"})
	if !strings.Contains(out, ".hidden/note.txt") || !strings.Contains(out, "node_modules/generated.txt") || strings.Contains(out, "main.go") || strings.Contains(out, "outside-only") {
		t.Fatal(out)
	}
	out = filesystemRun(t, ls, map[string]any{"path": ".hidden", "pattern": "note.*"})
	if !strings.Contains(out, "file\t6\t\"note.txt\"") {
		t.Fatal(out)
	}
	out = filesystemRun(t, ls, map[string]any{"path": "main.go"})
	if !strings.Contains(out, "file\t3\t\"main.go\"") {
		t.Fatal(out)
	}
	out = filesystemRun(t, ls, map[string]any{"path": "main.go", "limit": 1})
	if strings.Contains(out, "truncated") {
		t.Fatal("exactly fitting listing incorrectly reported truncation:", out)
	}
	out = filesystemRun(t, ls, map[string]any{"limit": 1})
	if !strings.Contains(out, "truncated") || strings.Count(out, "\t") != 2 {
		t.Fatal(out)
	}
}

func TestFilesystemSearchAsymmetric(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"), "absent\nneedle ALPHA\nother\nneedle BETA")
	writeFile(t, filepath.Join(root, "b.go"), "needle GAMMA\n")
	writeFile(t, filepath.Join(root, ".cache", "generated.go"), "skip\nneedle HIDDEN\n")
	writeFile(t, filepath.Join(root, "wrong.txt"), "needle EXCLUDED\n")
	grep := &Search{Root: root}
	out := filesystemRun(t, grep, map[string]any{"pattern": "needle (ALPHA|GAMMA|HIDDEN)$", "glob": "*.go"})
	for _, want := range []string{"a.go:2:needle ALPHA", "b.go:1:needle GAMMA", ".cache/generated.go:2:needle HIDDEN"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "BETA") || strings.Contains(out, "EXCLUDED") || strings.Contains(out, "truncated") {
		t.Fatal(out)
	}
	out = filesystemRun(t, grep, map[string]any{"path": "a.go", "pattern": "BETA"})
	if out != "a.go:4:needle BETA\n" {
		t.Fatal(out)
	}
	out = filesystemRun(t, grep, map[string]any{"pattern": "needle", "glob": ".cache/*.go"})
	if out != ".cache/generated.go:2:needle HIDDEN\n" {
		t.Fatal(out)
	}
	out = filesystemRun(t, grep, map[string]any{"path": "b.go", "pattern": "not present"})
	if !strings.Contains(out, "no matches") {
		t.Fatal(out)
	}
	out = filesystemRun(t, grep, map[string]any{"path": "a.go", "pattern": "needle", "limit": 1})
	if !strings.Contains(out, "a.go:2:needle ALPHA") || strings.Contains(out, "BETA") || !strings.Contains(out, "truncated") {
		t.Fatal(out)
	}
	writeFile(t, filepath.Join(root, "long"), strings.Repeat("x", 9000)+"needle AT_END")
	out = filesystemRun(t, grep, map[string]any{"path": "long", "pattern": "needle"})
	if !strings.Contains(out, "long:1:… ") || !strings.Contains(out, "needle AT_END") || len(out) > 2200 {
		t.Fatal("long-line excerpt lost matching text:", out)
	}
}

func TestFilesystemSearchReadAndOutputBounds(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "binary"), "needle\n"+strings.Repeat("x", 9000)+"\x00")
	writeFile(t, filepath.Join(root, "large"), "needle"+strings.Repeat("x", (1<<20)+1))
	writeFile(t, filepath.Join(root, "exact"), "needle"+strings.Repeat("x", (1<<20)-6))
	writeFile(t, filepath.Join(root, "text"), "needle safe\n")
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret"), "needle SECRET")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape-file")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := (&Search{Root: root}).Run(ctx, args(t, map[string]any{"pattern": "needle"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"text:1:needle safe", "exact:1:needle", "line shortened", "skipped 2", "1 MiB"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("missing %q: %s", want, res.Output)
		}
	}
	for _, absent := range []string{"binary:1:", "large:1:", "SECRET", "fifo:1:"} {
		if strings.Contains(res.Output, absent) {
			t.Fatal(res.Output)
		}
	}
	writeFile(t, filepath.Join(root, "many"), strings.Repeat("needle "+strings.Repeat("界", 700)+"\n", 300))
	out := filesystemRun(t, &Search{Root: root}, map[string]any{"path": "many", "pattern": "needle", "limit": 2000})
	if len(out) > 206000 || !utf8.ValidString(out) || !strings.Contains(out, "output truncated") {
		t.Fatalf("output bound/encoding failed: length=%d", len(out))
	}
}

func TestFilesystemTraversalAndTotalReadBounds(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10005; i++ {
		writeFile(t, filepath.Join(root, fmt.Sprintf("f%05d", i)), "")
	}
	for _, tool := range []Tool{&List{Root: root}, &Search{Root: root}} {
		in := map[string]any{"pattern": "never-matches"}
		out := filesystemRun(t, tool, in)
		if !strings.Contains(out, "traversal reached 10000") {
			t.Fatal(out)
		}
	}
	deep := t.TempDir()
	writeFile(t, filepath.Join(deep, strings.Repeat("d/", 65), "leaf"), "needle")
	for _, tool := range []Tool{&List{Root: deep}, &Search{Root: deep}} {
		in := map[string]any{"pattern": "needle"}
		if tool.Name() == "ls" {
			in["recursive"] = true
		}
		out := filesystemRun(t, tool, in)
		if !strings.Contains(out, "depth 64") {
			t.Fatal(out)
		}
	}
	budget := t.TempDir()
	for i := 0; i < 32; i++ {
		writeFile(t, filepath.Join(budget, fmt.Sprintf("f%d", i)), "needle\n"+strings.Repeat("x", (1<<20)-7))
	}
	out := filesystemRun(t, &Search{Root: budget}, map[string]any{"pattern": "needle"})
	if strings.Count(out, ":1:needle\n") != 32 || strings.Contains(out, "truncated") {
		t.Fatal("complete file at the exact total byte budget was skipped:", out)
	}
	writeFile(t, filepath.Join(budget, "extra"), "needle\n"+strings.Repeat("x", (1<<20)-7))
	out = filesystemRun(t, &Search{Root: budget}, map[string]any{"pattern": "needle"})
	if strings.Count(out, ":1:needle\n") != 32 || !strings.Contains(out, "total read budget (32 MiB) reached") {
		t.Fatal(out)
	}
}

func TestFilesystemValidationAndCancellation(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep"), "WIP")
	for _, tool := range []Tool{&List{Root: root}, &Search{Root: root}, &Remove{Root: root}, &Move{Root: root}} {
		t.Run(tool.Name(), func(t *testing.T) {
			filesystemFail(t, tool, map[string]any{"unknown": true})
			if tool.Schema()["additionalProperties"] != false {
				t.Fatal("schema allows unknown fields")
			}
			if _, ok := tool.Schema()["properties"].(map[string]any)["path"]; !ok {
				t.Fatal("path not exposed")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			in := map[string]any{"path": "keep"}
			if tool.Name() == "grep" {
				in["pattern"] = "WIP"
			}
			if tool.Name() == "move" {
				in["destination"] = "moved"
			}
			_, err := tool.Run(ctx, args(t, in))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
			if got := readFile(t, filepath.Join(root, "keep")); got != "WIP" {
				t.Fatal(got)
			}
		})
	}
	for _, tool := range []Tool{&List{Root: root}, &Search{Root: root}} {
		for _, limit := range []any{0, -1, 2001, 1.5, "2", nil, false} {
			filesystemFail(t, tool, map[string]any{"pattern": "x", "limit": limit})
		}
	}
	filesystemFail(t, &List{Root: root}, map[string]any{"pattern": "["})
	filesystemFail(t, &Search{Root: root}, map[string]any{"pattern": "["})
	filesystemFail(t, &Search{Root: root}, map[string]any{"pattern": "x", "glob": "["})
	filesystemFail(t, &Search{Root: root}, nil)
	filesystemFail(t, &Remove{Root: root}, nil)
	filesystemFail(t, &Move{Root: root}, map[string]any{"path": "keep"})
}

func TestFilesystemRemoveAndMoveEffects(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "tree", "child"), "child data")
	writeFile(t, filepath.Join(root, "unrelated"), "uncommitted WIP")
	rm, mv := &Remove{Root: root}, &Move{Root: root}
	filesystemFail(t, rm, map[string]any{"path": "tree"})
	if got := readFile(t, filepath.Join(root, "tree", "child")); got != "child data" {
		t.Fatal(got)
	}
	out := filesystemRun(t, mv, map[string]any{"path": "tree", "destination": "renamed"})
	if !strings.Contains(out, "moved tree to renamed") {
		t.Fatal(out)
	}
	filesystemMissing(t, filepath.Join(root, "tree"))
	if got := readFile(t, filepath.Join(root, "renamed", "child")); got != "child data" {
		t.Fatal(got)
	}
	filesystemRun(t, mv, map[string]any{"path": "renamed/child", "destination": "moved-file"})
	filesystemMissing(t, filepath.Join(root, "renamed", "child"))
	filesystemRun(t, rm, map[string]any{"path": "renamed"})
	filesystemRun(t, rm, map[string]any{"path": "moved-file"})
	writeFile(t, filepath.Join(root, "recursive", "a", "b"), "delete me")
	out = filesystemRun(t, rm, map[string]any{"path": "recursive", "recursive": true})
	if !strings.Contains(out, "removed recursive") {
		t.Fatal(out)
	}
	filesystemMissing(t, filepath.Join(root, "recursive"))
	if got := readFile(t, filepath.Join(root, "unrelated")); got != "uncommitted WIP" {
		t.Fatal(got)
	}
}

func TestFilesystemDestinationPreserved(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "source"), "source WIP")
			dest := filepath.Join(root, "dest")
			switch kind {
			case "file":
				writeFile(t, dest, "destination WIP")
			case "directory":
				if err := os.Mkdir(dest, 0o755); err != nil {
					t.Fatal(err)
				}
			default:
				target := "source"
				if kind == "dangling" {
					target = "absent"
				}
				if err := os.Symlink(target, dest); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(dest)
			if err != nil {
				t.Fatal(err)
			}
			filesystemFail(t, &Move{Root: root}, map[string]any{"path": "source", "destination": "dest"})
			after, err := os.Lstat(dest)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) {
				t.Fatal("destination replaced")
			}
			if kind == "file" && readFile(t, dest) != "destination WIP" {
				t.Fatal("destination changed")
			}
			if readFile(t, filepath.Join(root, "source")) != "source WIP" {
				t.Fatal("source changed")
			}
		})
	}
}

func TestFilesystemMoveConcurrentDestination(t *testing.T) {
	root := t.TempDir()
	mv := &Move{Root: root}
	for i := 0; i < 100; i++ {
		source, dest := fmt.Sprintf("source-%d", i), fmt.Sprintf("dest-%d", i)
		writeFile(t, filepath.Join(root, source), "source WIP")
		created := make(chan error, 1)
		go func() {
			f, err := os.OpenFile(filepath.Join(root, dest), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err == nil {
				_, err = f.WriteString("concurrent WIP")
				err = errors.Join(err, f.Close())
			}
			created <- err
		}()
		_, moveErr := mv.Run(context.Background(), args(t, map[string]any{"path": source, "destination": dest}))
		createErr := <-created
		if createErr == nil {
			if moveErr == nil || readFile(t, filepath.Join(root, dest)) != "concurrent WIP" || readFile(t, filepath.Join(root, source)) != "source WIP" {
				t.Fatal("concurrent destination was replaced or source lost")
			}
		} else {
			if !errors.Is(createErr, os.ErrExist) || moveErr != nil || readFile(t, filepath.Join(root, dest)) != "source WIP" {
				t.Fatalf("unexpected concurrent move results: create=%v move=%v", createErr, moveErr)
			}
			filesystemMissing(t, filepath.Join(root, source))
		}
	}
}

func TestFilesystemSymlinkedWorkspace(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "file"), "needle")
	alias := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if out := filesystemRun(t, &List{Root: alias}, nil); !strings.Contains(out, "\"file\"") {
		t.Fatal(out)
	}
	if out := filesystemRun(t, &Search{Root: alias}, map[string]any{"pattern": "needle"}); out != "file:1:needle\n" {
		t.Fatal(out)
	}
	filesystemFail(t, &Remove{Root: alias}, map[string]any{"path": root, "recursive": true})
	filesystemRun(t, &Move{Root: alias}, map[string]any{"path": "file", "destination": "renamed"})
	filesystemRun(t, &Remove{Root: alias}, map[string]any{"path": "renamed"})
	filesystemMissing(t, filepath.Join(root, "renamed"))
}

func TestFilesystemWriteBoundariesAndScratch(t *testing.T) {
	root, scratch, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "keep"), "WIP")
	writeFile(t, filepath.Join(outside, "outside"), "outside contents")
	writeFile(t, filepath.Join(scratch, "temporary"), "scratch contents")
	rm, mv := &Remove{Root: root, TempDir: scratch}, &Move{Root: root, TempDir: scratch}
	for _, path := range []string{".", root, scratch, filepath.Join(outside, "outside"), filepath.Join(root, "..", filepath.Base(outside), "outside")} {
		filesystemFail(t, rm, map[string]any{"path": path, "recursive": true})
		filesystemFail(t, mv, map[string]any{"path": path, "destination": "new"})
		filesystemFail(t, mv, map[string]any{"path": "keep", "destination": path})
	}
	filesystemFail(t, mv, map[string]any{"path": "keep", "destination": filepath.Join(scratch, "new")})
	filesystemRun(t, mv, map[string]any{"path": filepath.Join(scratch, "temporary"), "destination": filepath.Join(scratch, "renamed")})
	if readFile(t, filepath.Join(scratch, "renamed")) != "scratch contents" {
		t.Fatal("scratch move changed contents")
	}
	filesystemRun(t, rm, map[string]any{"path": filepath.Join(scratch, "renamed")})
	filesystemMissing(t, filepath.Join(scratch, "renamed"))
	if readFile(t, filepath.Join(outside, "outside")) != "outside contents" || readFile(t, filepath.Join(root, "keep")) != "WIP" {
		t.Fatal("unrelated files changed")
	}
	// Protect nested active scratch and ancestors containing it, too.
	nested := filepath.Join(root, "parent", "scratch")
	writeFile(t, filepath.Join(nested, "keep"), "active")
	rm.TempDir, mv.TempDir = nested, nested
	for _, path := range []string{"parent", "parent/scratch"} {
		filesystemFail(t, rm, map[string]any{"path": path, "recursive": true})
		filesystemFail(t, mv, map[string]any{"path": path, "destination": "new"})
	}
	if readFile(t, filepath.Join(nested, "keep")) != "active" {
		t.Fatal("nested scratch changed")
	}
}

func TestFilesystemLinksDoNotChangeOutsideContents(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(outside, "keep"), "outside WIP")
	writeFile(t, filepath.Join(root, "source"), "source")
	if err := os.Link(filepath.Join(outside, "keep"), filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	rm, mv := &Remove{Root: root}, &Move{Root: root}
	for _, path := range []string{"escape/keep", "alias/source"} {
		filesystemFail(t, rm, map[string]any{"path": path, "recursive": true})
		filesystemFail(t, mv, map[string]any{"path": path, "destination": "new"})
		filesystemFail(t, mv, map[string]any{"path": "source", "destination": path})
	}
	filesystemRun(t, mv, map[string]any{"path": "hardlink", "destination": "renamed-link"})
	filesystemRun(t, rm, map[string]any{"path": "renamed-link"})
	filesystemRun(t, mv, map[string]any{"path": "escape", "destination": "renamed-escape"})
	if target, err := os.Readlink(filepath.Join(root, "renamed-escape")); err != nil || target != outside {
		t.Fatalf("symlink move: %q, %v", target, err)
	}
	filesystemRun(t, rm, map[string]any{"path": "renamed-escape", "recursive": true})
	writeFile(t, filepath.Join(root, "tree", "file"), "delete")
	if err := os.Symlink(outside, filepath.Join(root, "tree", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(outside, "keep"), filepath.Join(root, "tree", "hardlink")); err != nil {
		t.Fatal(err)
	}
	filesystemRun(t, rm, map[string]any{"path": "tree", "recursive": true})
	if readFile(t, filepath.Join(outside, "keep")) != "outside WIP" {
		t.Fatal("outside contents changed")
	}
	if readFile(t, filepath.Join(root, "source")) != "source" {
		t.Fatal("source changed")
	}
}
