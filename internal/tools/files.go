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
	"strings"
)

const (
	maxReadBytes  = 200 * 1024
	defaultLimit  = 2000
	maxLineLength = 2000
)

// resolvePath makes p absolute relative to root. A leading ~ is the home
// directory, as the policy layer reads it, so "~/x" reaches the same file
// the user approved.
func resolvePath(root, p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand %s: %w", p, err)
		}
		p = home + p[1:]
	}
	// On Windows, filepath.IsAbs reports a rooted path such as \Windows as
	// incomplete because it has no volume. It is nevertheless absolute on the
	// current root's drive and must not be joined beneath the workspace.
	if volume := filepath.VolumeName(root); volume != "" && filepath.VolumeName(p) == "" && (strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`)) {
		p = volume + p
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return filepath.Clean(p), nil
}

// Read returns file contents with line numbers.
type Read struct{ Root string }

type readInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func (*Read) Name() string   { return "read" }
func (*Read) ReadOnly() bool { return true }
func (*Read) Description() string {
	return "Read a text file. Returns numbered lines. Use offset (1-based line) and limit for large files."
}

func (*Read) Schema() map[string]any {
	return schema(map[string]any{
		"path":   prop("string", "File path, absolute or relative to the working directory"),
		"offset": prop("integer", "First line to return (1-based). Default 1"),
		"limit":  prop("integer", "Maximum number of lines to return. Default 2000"),
	}, "path")
}

func (t *Read) Run(_ context.Context, input json.RawMessage) (result Result, err error) {
	var in readInput
	if err := decode(input, &in); err != nil {
		return Result{}, err
	}
	path, err := resolvePath(t.Root, in.Path)
	if err != nil {
		return Result{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	start := max(in.Offset, 1)
	limit := in.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	last := start - 1 + limit
	lineNo := 1
	lineBytes := 0
	var line []byte
	out := limitedOutput{limit: maxReadBytes}
	writeLine := func() {
		if lineNo >= start && lineNo <= last {
			text := string(line)
			if lineBytes > maxLineLength {
				text = text[:cutAt(text, maxLineLength)] + "…"
			}
			out.writeString(fmt.Sprintf("%6d\t", lineNo))
			out.writeString(text)
			out.writeString("\n")
		}
	}

	buf := make([]byte, 32*1024)
	fileBytes := 0
	binary := false
	for {
		n, readErr := f.Read(buf)
		if fileBytes < 8000 && bytes.IndexByte(buf[:min(n, 8000-fileBytes)], 0) >= 0 {
			binary = true
		}
		fileBytes += n
		for chunk := buf[:n]; len(chunk) > 0; {
			part, rest, newline := bytes.Cut(chunk, []byte{'\n'})
			lineBytes += len(part)
			if lineNo >= start && lineNo <= last {
				keep := min(len(part), maxLineLength+1-len(line))
				line = append(line, part[:keep]...)
			}
			if newline {
				writeLine()
				lineNo++
				lineBytes = 0
				line = line[:0]
			}
			chunk = rest
		}
		if readErr != nil {
			if readErr != io.EOF {
				return Result{}, readErr
			}
			break
		}
	}
	if lineBytes > 0 {
		writeLine()
	} else {
		lineNo-- // a final newline does not create another line
	}
	if binary {
		return Result{}, fmt.Errorf("%s looks like a binary file (%d bytes)", in.Path, fileBytes)
	}
	totalLines := lineNo
	if start > totalLines {
		return Result{Output: fmt.Sprintf("(file has %d lines; offset %d is past the end)", totalLines, start)}, nil
	}
	end := min(last, totalLines)
	if end < totalLines {
		out.writeString(fmt.Sprintf("\n(%d more lines; continue with offset=%d)", totalLines-end, end+1))
	}
	return Result{
		Output:  out.String(),
		Summary: fmt.Sprintf("%s (%d lines)", in.Path, end-start+1),
	}, nil
}

func isBinary(b []byte) bool {
	n := min(len(b), 8000)
	for _, c := range b[:n] {
		if c == 0 {
			return true
		}
	}
	return false
}

// Write creates or overwrites a file.
type Write struct{ Root string }

type writeInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (*Write) Name() string { return "write" }
func (*Write) Description() string {
	return "Create or overwrite a file with the given content. Parent directories are created."
}

func (*Write) Schema() map[string]any {
	return schema(map[string]any{
		"path":    prop("string", "File path, absolute or relative to the working directory"),
		"content": prop("string", "Full file content"),
	}, "path", "content")
}

func (t *Write) Run(_ context.Context, input json.RawMessage) (Result, error) {
	var in writeInput
	if err := decode(input, &in); err != nil {
		return Result{}, err
	}
	path, err := resolvePath(t.Root, in.Path)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
		return Result{}, err
	}
	n := strings.Count(in.Content, "\n")
	return Result{
		Output:  fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path),
		Summary: fmt.Sprintf("%s (%d lines)", in.Path, n),
		Detail:  DiffDetail("", in.Content),
	}, nil
}

// Edit replaces an exact string in a file.
type Edit struct{ Root string }

type editInput struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

func (*Edit) Name() string { return "edit" }
func (*Edit) Description() string {
	return "Replace an exact substring in a file. old_string must match exactly once unless replace_all is true. Include enough surrounding context to make it unique."
}

func (*Edit) Schema() map[string]any {
	return schema(map[string]any{
		"path":        prop("string", "File path, absolute or relative to the working directory"),
		"old_string":  prop("string", "Exact text to find"),
		"new_string":  prop("string", "Replacement text"),
		"replace_all": prop("boolean", "Replace every occurrence instead of requiring a unique match"),
	}, "path", "old_string", "new_string")
}

func (t *Edit) Run(_ context.Context, input json.RawMessage) (Result, error) {
	var in editInput
	if err := decode(input, &in); err != nil {
		return Result{}, err
	}
	if in.OldString == "" {
		return Result{}, errors.New("old_string must not be empty")
	}
	if in.OldString == in.NewString {
		return Result{}, errors.New("old_string and new_string are identical")
	}
	path, err := resolvePath(t.Root, in.Path)
	if err != nil {
		return Result{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	src := string(b)
	count := strings.Count(src, in.OldString)
	switch {
	case count == 0:
		return Result{}, errors.New("old_string not found in file")
	case count > 1 && !in.ReplaceAll:
		return Result{}, fmt.Errorf("old_string matches %d times; add context to make it unique or set replace_all", count)
	}
	var out string
	if in.ReplaceAll {
		out = strings.ReplaceAll(src, in.OldString, in.NewString)
	} else {
		out = strings.Replace(src, in.OldString, in.NewString, 1)
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return Result{}, err
	}
	return Result{
		Output:  fmt.Sprintf("replaced %d occurrence(s) in %s", count, in.Path),
		Summary: in.Path,
		Detail:  DiffDetail(in.OldString, in.NewString),
	}, nil
}

// DiffDetail renders old and new text as removed/added lines for the UI.
// It is a plain before/after listing, not a minimal diff.
func DiffDetail(oldText, newText string) string {
	var sb strings.Builder
	write := func(prefix, text string) {
		if text == "" {
			return
		}
		for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			sb.WriteString(prefix)
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}
	write("-", oldText)
	write("+", newText)
	return strings.TrimRight(sb.String(), "\n")
}
