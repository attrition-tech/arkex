package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	fsEntries      = 10000
	fsDepth        = 64
	fsFileBytes    = 1 << 20
	fsSearchBytes  = 32 << 20
	fsDefaultLimit = 200
	fsMaxLimit     = 2000
)

// List inventories entries without following symbolic links.
type List struct{ Root string }

func (*List) Name() string   { return "ls" }
func (*List) ReadOnly() bool { return true }
func (*List) Description() string {
	return "List filesystem entries with type and size, including hidden/generated files. Does not follow symlinks. Defaults to workspace; recursive is false. Bounded to 10000 entries, depth 64 and 200 KiB output. pattern is a Go filepath glob on relative paths (or basenames if no slash); ** is not a recursive wildcard."
}
func (*List) Schema() map[string]any {
	return filesystemSchema(map[string]any{
		"path":      prop("string", "File or directory; defaults to workspace"),
		"recursive": prop("boolean", "Descend into directories; default false"),
		"pattern":   prop("string", "Optional Go filepath glob"),
		"limit":     prop("integer", "Maximum entries, 1-2000; default 200"),
	})
}

type listInput struct {
	Path      string          `json:"path"`
	Recursive bool            `json:"recursive"`
	Pattern   string          `json:"pattern"`
	Limit     json.RawMessage `json:"limit"`
}

func filesystemSchema(props map[string]any, required ...string) map[string]any {
	s := schema(props, required...)
	s["additionalProperties"] = false
	if limit, ok := props["limit"].(map[string]any); ok {
		limit["minimum"], limit["maximum"], limit["default"] = 1, fsMaxLimit, fsDefaultLimit
	}
	return s
}

func filesystemLimit(limit json.RawMessage, glob string) (int, error) {
	n := fsDefaultLimit
	if len(limit) > 0 {
		var value int
		if err := json.Unmarshal(limit, &value); err != nil {
			return 0, fmt.Errorf("limit must be an integer between 1 and %d", fsMaxLimit)
		}
		n = value // Explicit null is not the same as an omitted limit.
	}
	if n < 1 || n > fsMaxLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", fsMaxLimit)
	}
	if _, err := filepath.Match(glob, ""); err != nil {
		return 0, fmt.Errorf("invalid glob: %w", err)
	}
	return n, nil
}

func filesystemMatch(glob, path string) bool {
	if glob == "" {
		return true
	}
	if !strings.ContainsRune(glob, filepath.Separator) {
		path = filepath.Base(path)
	}
	ok, _ := filepath.Match(glob, path)
	return ok
}

var errFilesystemBound = errors.New("filesystem bound reached")

// walkFilesystem holds each directory open and pins traversal to that handle.
// ReadDir batches bound memory even in huge directories. No ignore rules or
// symlink following; the callback can stop traversal at its own output bound.
func walkFilesystem(ctx context.Context, path string, recursive bool, maxEntries int, visit func(*os.Root, string, string, os.FileInfo) error) (string, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	entries := 0
	note := ""
	var walk func(*os.Root, string, string, int, bool) error
	walk = func(parent *os.Root, name, display string, depth int, top bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entries >= maxEntries {
			note = fmt.Sprintf("truncated: traversal reached %d entries", maxEntries)
			return errFilesystemBound
		}
		entries++
		info, err := parent.Lstat(name)
		if err != nil {
			return err
		}
		if !top || !info.IsDir() {
			if err := visit(parent, name, display, info); err != nil {
				return err
			}
		}
		if !info.IsDir() || (!top && !recursive) {
			return nil
		}
		if depth >= fsDepth {
			note = "truncated: traversal reached depth 64"
			return nil
		}
		dir, err := parent.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer func() { _ = dir.Close() }()
		child, err := snapshotDirectory(parent, name, dir)
		if err != nil {
			return err
		}
		defer func() { _ = child.Close() }()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			batch, readErr := dir.ReadDir(128)
			for _, entry := range batch {
				if err := walk(child, entry.Name(), filepath.Join(display, entry.Name()), depth+1, false); err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	name := filepath.Base(path)
	if path == string(filepath.Separator) {
		name = "."
	}
	info, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	display := filepath.Base(path)
	if info.IsDir() {
		display = ""
	}
	err = walk(root, name, display, 0, true)
	if errors.Is(err, errFilesystemBound) {
		err = nil
	}
	return note, err
}

func filesystemPath(root, path string) (string, error) {
	if path == "" || path == "." {
		// The configured workspace itself may be reached through a symlink.
		// Canonicalize this trusted start, not descendant entries in the walk.
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(abs)
	}
	p, err := resolvePath(root, path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

func (t *List) Run(ctx context.Context, input json.RawMessage) (Result, error) {
	var in listInput
	if err := decode(input, &in); err != nil {
		return Result{}, err
	}
	limit, err := filesystemLimit(in.Limit, in.Pattern)
	if err != nil {
		return Result{}, err
	}
	path, err := filesystemPath(t.Root, in.Path)
	if err != nil {
		return Result{}, err
	}
	out := limitedOutput{limit: maxReadBytes}
	count := 0
	stopped := false
	note, err := walkFilesystem(ctx, path, in.Recursive, fsEntries, func(_ *os.Root, _, display string, info os.FileInfo) error {
		if !filesystemMatch(in.Pattern, display) {
			return nil
		}
		if count >= limit || out.total >= maxReadBytes {
			stopped = true
			return errFilesystemBound
		}
		kind := "file"
		switch {
		case info.IsDir():
			kind = "dir"
		case info.Mode()&os.ModeSymlink != 0:
			kind = "symlink"
		case !info.Mode().IsRegular():
			kind = "special"
		}
		out.writeString(fmt.Sprintf("%s\t%d\t%q\n", kind, info.Size(), display))
		count++
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if stopped || out.total > maxReadBytes {
		note = "truncated: entry/output limit reached; narrow path/pattern or increase limit"
	}
	if count == 0 {
		out.writeString("(no entries matched)\n")
	}
	if note != "" {
		out.writeString("[" + note + "]\n")
	}
	return Result{Output: out.String(), Summary: fmt.Sprintf("%d entries", count)}, nil
}

// Search is a bounded, shell-independent, line-oriented regular expression search.
type Search struct{ Root string }

func (*Search) Name() string   { return "grep" }
func (*Search) ReadOnly() bool { return true }
func (*Search) Description() string {
	return "Search text with a Go regular expression, returning filename:line:matching text. Defaults to workspace, recursively including hidden/generated files. No symlinks or special files are read. Skips files over 1 MiB and files containing NUL anywhere; total read budget 32 MiB, traversal 10000 entries/depth 64, output 200 KiB. Matching lines are shortened to 2000 bytes. glob uses Go filepath matching on relative paths (basenames without slash), not recursive **. Reports skips/truncation; no shell required."
}
func (*Search) Schema() map[string]any {
	return filesystemSchema(map[string]any{
		"path":    prop("string", "File or directory; defaults to workspace"),
		"pattern": prop("string", "Required nonempty Go regular expression"),
		"glob":    prop("string", "Optional Go filepath glob selecting files"),
		"limit":   prop("integer", "Maximum matching lines, 1-2000; default 200"),
	}, "pattern")
}

type searchInput struct {
	Path    string          `json:"path"`
	Pattern string          `json:"pattern"`
	Glob    string          `json:"glob"`
	Limit   json.RawMessage `json:"limit"`
}

func (t *Search) Run(ctx context.Context, input json.RawMessage) (Result, error) {
	var in searchInput
	if err := decode(input, &in); err != nil {
		return Result{}, err
	}
	limit, err := filesystemLimit(in.Limit, in.Glob)
	if err != nil {
		return Result{}, err
	}
	if in.Pattern == "" {
		return Result{}, errors.New("pattern is required and must not be empty")
	}
	rx, err := regexp.Compile(in.Pattern)
	if err != nil {
		return Result{}, fmt.Errorf("invalid regex: %w", err)
	}
	path, err := filesystemPath(t.Root, in.Path)
	if err != nil {
		return Result{}, err
	}
	out := limitedOutput{limit: maxReadBytes}
	count, skipped := 0, 0
	remaining := int64(fsSearchBytes)
	stopped := ""
	note, err := walkFilesystem(ctx, path, true, fsEntries, func(parent *os.Root, name, display string, info os.FileInfo) error {
		if !info.Mode().IsRegular() || !filesystemMatch(in.Glob, display) {
			return nil
		}
		if info.Size() > fsFileBytes {
			skipped++
			return nil
		}
		if remaining <= 0 {
			stopped = "total read budget (32 MiB) reached"
			return errFilesystemBound
		}
		f, err := parent.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		opened, err := f.Stat()
		if err != nil {
			return err
		}
		if !opened.Mode().IsRegular() || opened.Size() > fsFileBytes {
			skipped++
			return nil
		}
		// Read one extra byte to detect growth, and never search partial files.
		b, err := io.ReadAll(io.LimitReader(sandboxReader{ctx, f}, min(int64(fsFileBytes+1), remaining+1)))
		if err != nil {
			return err
		}
		if int64(len(b)) > remaining {
			stopped = "total read budget (32 MiB) reached"
			return errFilesystemBound
		}
		remaining -= int64(len(b))
		if len(b) > fsFileBytes {
			skipped++
			return nil
		}
		if bytes.IndexByte(b, 0) >= 0 {
			skipped++
			return nil
		}
		lineNo := 0
		for len(b) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			line, rest, _ := bytes.Cut(b, []byte{'\n'})
			b = rest
			lineNo++
			match := rx.FindIndex(line)
			if match == nil {
				continue
			}
			if count >= limit || out.total >= maxReadBytes {
				stopped = "match/output limit reached; narrow path/glob/pattern or increase limit"
				return errFilesystemBound
			}
			prefix := ""
			if match[0] >= maxLineLength/2 {
				// Keep the actual match visible rather than returning an unrelated
				// prefix when a long line matches near its end.
				line = line[match[0]-maxLineLength/4:]
				prefix = "… "
			}
			text := prefix + strings.ToValidUTF8(string(line), "�")
			if len(text) > maxLineLength {
				text = text[:cutAt(text, maxLineLength)] + "… [line shortened]"
			}
			out.writeString(fmt.Sprintf("%s:%d:%s\n", display, lineNo, text))
			count++
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if count == 0 {
		out.writeString("(no matches in searched files)\n")
	}
	if skipped > 0 {
		out.writeString(fmt.Sprintf("[skipped %d binary/oversized/non-regular files; size bound 1 MiB]\n", skipped))
	}
	if stopped != "" {
		note = "truncated: " + stopped
	}
	if note != "" {
		out.writeString("[" + note + "]\n")
	}
	return Result{Output: out.String(), Summary: fmt.Sprintf("%d matching lines", count)}, nil
}

// Remove and Move mutate directory entries, never linked file contents.
type Remove struct{ Root, TempDir string }
type Move struct{ Root, TempDir string }

func (*Remove) Name() string { return "remove" }
func (*Remove) Description() string {
	return "Delete a file, symlink, or empty directory in workspace/active scratch. recursive=true permanently deletes a directory tree without following symlinks. Destructive: requires appropriate user authorization. Never deletes a write root."
}
func (*Remove) Schema() map[string]any {
	return filesystemSchema(map[string]any{"path": prop("string", "Required path to delete"), "recursive": prop("boolean", "Delete nonempty directories; default false")}, "path")
}
func (*Move) Name() string { return "move" }
func (*Move) Description() string {
	return "Move/rename a file, symlink, or directory within the same authorized workspace or active scratch write root. Destination must not exist; parents must exist. Destructive: requires appropriate user authorization. Never moves or replaces a write root; symlink parent paths are rejected."
}
func (*Move) Schema() map[string]any {
	return filesystemSchema(map[string]any{"path": prop("string", "Required source path"), "destination": prop("string", "Required destination path (not an existing directory container)")}, "path", "destination")
}

// Pin every parent, refusing symlink ancestors even when they point inside.
// This also gives renameat2 directory handles, not unchecked absolute paths.
func filesystemParent(root *os.Root, name string) (*os.File, error) {
	parent := filepath.Dir(name)
	f, err := root.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(parent, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		fd, openErr := unix.Openat(int(f.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = f.Close()
		if openErr != nil {
			return nil, openErr
		}
		f = os.NewFile(uintptr(fd), part)
	}
	return f, nil
}

func filesystemWriteTarget(workspace, scratch, path string) (*os.Root, string, error) {
	root, name, err := writeRoot(workspace, scratch, path)
	if err != nil {
		return nil, "", err
	}
	reject := func(err error) (*os.Root, string, error) { _ = root.Close(); return nil, "", err }
	if name == "." {
		return reject(errors.New("cannot modify a write root"))
	}
	// Also protect an active scratch root nested under a requested directory.
	// Parent symlinks are rejected by filesystemParent; final symlinks are
	// directory entries, so deleting/renaming them does not touch their target.
	info, statErr := root.Lstat(name)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return reject(statErr)
	}
	if info != nil && info.IsDir() {
		for _, dir := range []string{workspace, scratch} {
			if dir == "" {
				continue
			}
			real, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return reject(err)
			}
			real, err = filepath.Abs(real)
			if err != nil {
				return reject(err)
			}
			if within(filepath.Join(root.Name(), name), real) {
				return reject(errors.New("cannot modify a write root or its ancestor"))
			}
		}
	}
	return root, name, nil
}

func (t *Remove) Run(ctx context.Context, input json.RawMessage) (Result, error) {
	var in struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := decode(input, &in); err != nil {
		return Result{}, err
	}
	path, err := resolvePath(t.Root, in.Path)
	if err != nil {
		return Result{}, err
	}
	root, name, err := filesystemWriteTarget(t.Root, t.TempDir, path)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = root.Close() }()
	parent, err := filesystemParent(root, name)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = parent.Close() }()
	pinned, err := snapshotDirectory(root, filepath.Dir(name), parent)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = pinned.Close() }()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	base := filepath.Base(name)
	if _, err := pinned.Lstat(base); err != nil {
		return Result{}, err
	}
	if in.Recursive {
		err = pinned.RemoveAll(base)
	} else {
		err = pinned.Remove(base)
	}
	if err != nil {
		return Result{}, err
	}
	return Result{Output: fmt.Sprintf("removed %s", in.Path), Summary: in.Path}, nil
}

func (t *Move) Run(ctx context.Context, input json.RawMessage) (Result, error) {
	var in struct {
		Path        string `json:"path"`
		Destination string `json:"destination"`
	}
	if err := decode(input, &in); err != nil {
		return Result{}, err
	}
	source, err := resolvePath(t.Root, in.Path)
	if err != nil {
		return Result{}, err
	}
	destination, err := resolvePath(t.Root, in.Destination)
	if err != nil {
		return Result{}, err
	}
	root, name, err := filesystemWriteTarget(t.Root, t.TempDir, source)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = root.Close() }()
	destRoot, destName, err := filesystemWriteTarget(t.Root, t.TempDir, destination)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = destRoot.Close() }()
	a, err := root.Stat(".")
	if err != nil {
		return Result{}, err
	}
	b, err := destRoot.Stat(".")
	if err != nil {
		return Result{}, err
	}
	if !os.SameFile(a, b) {
		return Result{}, errors.New("move requires the same authorized write root")
	}
	oldParent, err := filesystemParent(root, name)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = oldParent.Close() }()
	newParent, err := filesystemParent(destRoot, destName)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = newParent.Close() }()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	err = filesystemRenameExclusive(oldParent, filepath.Base(name), newParent, filepath.Base(destName))
	if err != nil {
		return Result{}, err
	}
	return Result{Output: fmt.Sprintf("moved %s to %s", in.Path, in.Destination), Summary: in.Path + " → " + in.Destination}, nil
}
