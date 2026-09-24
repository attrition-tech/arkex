package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffDetail(t *testing.T) {
	got := DiffDetail("a\nb\n", "a\nc")
	want := "-a\n-b\n+a\n+c"
	if got != want {
		t.Fatalf("diffDetail = %q, want %q", got, want)
	}
	if got := DiffDetail("", "new"); got != "+new" {
		t.Fatalf("write detail = %q", got)
	}
}

func args(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestResolvePath(t *testing.T) {
	root := t.TempDir()
	if _, err := resolvePath(root, ""); err == nil {
		t.Fatal("empty path accepted")
	}
	if got, _ := resolvePath(root, "a/../b.txt"); got != filepath.Join(root, "b.txt") {
		t.Fatalf("relative = %q", got)
	}
	absolute := filepath.Join(string(filepath.Separator), "etc", "hosts")
	wantAbsolute := absolute
	if volume := filepath.VolumeName(root); volume != "" {
		wantAbsolute = volume + absolute
	}
	if got, _ := resolvePath(root, absolute); got != wantAbsolute {
		t.Fatalf("absolute = %q", got)
	}
	home := filepath.Join(root, "home", "tester")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got, _ := resolvePath(root, "~/notes/x.md"); got != filepath.Join(home, "notes", "x.md") {
		t.Fatalf("tilde = %q", got)
	}
	if got, _ := resolvePath(root, "~other"); got != filepath.Join(root, "~other") {
		t.Fatalf("~user form is not expanded, got %q", got)
	}
}

func TestReadNumbersLinesAndPaginates(t *testing.T) {
	root := t.TempDir()
	// Trailing newline must not produce a phantom empty last line.
	writeFile(t, filepath.Join(root, "f.txt"), "one\ntwo\nthree\nfour\nfive\n")
	rd := &Read{Root: root}

	res, err := rd.Run(context.Background(), args(t, map[string]any{"path": "f.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "     1\tone\n     2\ttwo\n     3\tthree\n     4\tfour\n     5\tfive\n"; res.Output != want {
		t.Fatalf("output = %q", res.Output)
	}
	if res.Summary != "f.txt (5 lines)" {
		t.Fatalf("summary = %q", res.Summary)
	}

	// offset/limit window and the continuation hint.
	res, err = rd.Run(context.Background(), args(t, map[string]any{"path": "f.txt", "offset": 2, "limit": 2}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Output, "     2\ttwo\n     3\tthree\n") || !strings.Contains(res.Output, "(2 more lines; continue with offset=4)") {
		t.Fatalf("window = %q", res.Output)
	}
	if res.Summary != "f.txt (2 lines)" {
		t.Fatalf("window summary = %q", res.Summary)
	}

	// Last page: no hint.
	res, _ = rd.Run(context.Background(), args(t, map[string]any{"path": "f.txt", "offset": 5}))
	if res.Output != "     5\tfive\n" {
		t.Fatalf("last page = %q", res.Output)
	}

	// Past the end is a message, not an error, so the model can recover.
	res, err = rd.Run(context.Background(), args(t, map[string]any{"path": "f.txt", "offset": 6}))
	if err != nil || !strings.Contains(res.Output, "file has 5 lines; offset 6") {
		t.Fatalf("past end: %q err=%v", res.Output, err)
	}
}

func TestReadRejectsBinaryAndUnknownArgs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "bin"), "abc\x00def")
	rd := &Read{Root: root}
	if _, err := rd.Run(context.Background(), args(t, map[string]any{"path": "bin"})); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary err = %v", err)
	}
	if _, err := rd.Run(context.Background(), args(t, map[string]any{"path": "bin", "lines": 3})); err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Fatalf("unknown field err = %v", err)
	}
	if _, err := rd.Run(context.Background(), args(t, map[string]any{"path": "missing"})); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestReadTruncatesLongLinesOnRuneBoundary(t *testing.T) {
	root := t.TempDir()
	// 1999 ASCII bytes then a 2-byte rune straddling the 2000-byte cap.
	long := strings.Repeat("x", 1999) + "é" + "tail"
	writeFile(t, filepath.Join(root, "long.txt"), long+"\nshort\n")
	res, err := (&Read{Root: root}).Run(context.Background(), args(t, map[string]any{"path": "long.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(res.Output, "\n", 2)[0]
	if !strings.HasSuffix(first, "…") || strings.Contains(first, "tail") {
		t.Fatalf("long line not truncated: len=%d tail=%q", len(first), first[len(first)-12:])
	}
	if strings.Contains(first, "\uFFFD") || !strings.Contains(first, "x…") {
		t.Fatalf("truncation split a rune: %q", first[len(first)-8:])
	}
}

// legacyReadResult is an independent copy of Read's former whole-file
// formatting logic. Keep it test-only so streaming changes are checked byte
// for byte, including both truncation stages.
func legacyReadResult(path, name string, offset, limit int) (Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	if isBinary(b) {
		return Result{}, fmt.Errorf("%s looks like a binary file (%d bytes)", name, len(b))
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start := max(offset, 1)
	if limit <= 0 {
		limit = defaultLimit
	}
	if start > len(lines) {
		return Result{Output: fmt.Sprintf("(file has %d lines; offset %d is past the end)", len(lines), start)}, nil
	}
	end := min(start-1+limit, len(lines))
	var out strings.Builder
	for i := start - 1; i < end; i++ {
		line := lines[i]
		if len(line) > maxLineLength {
			line = line[:cutAt(line, maxLineLength)] + "…"
		}
		fmt.Fprintf(&out, "%6d\t%s\n", i+1, line)
	}
	if end < len(lines) {
		fmt.Fprintf(&out, "\n(%d more lines; continue with offset=%d)", len(lines)-end, end+1)
	}
	return Result{Output: truncate(out.String(), maxReadBytes), Summary: fmt.Sprintf("%s (%d lines)", name, end-start+1)}, nil
}

func TestReadStreamingMatchesLegacyBoundaries(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name          string
		content       string
		offset, limit int
	}{
		{"empty", "", 0, 0},
		{"only-newline", "\n", 0, 0},
		{"empty-lines-no-final-newline", "\n\nlast", 1, 2},
		{"final-newline", "first\nlast\n", 2, 1},
		{"past-end", "one\ntwo", 3, 1},
		{"utf8-line-boundary", strings.Repeat("x", 1999) + "étail\nnext", 0, 0},
		{"invalid-utf8-boundary", strings.Repeat("x", 1999) + "\x80\x80tail", 0, 0},
		{"output-cap-boundary", strings.Repeat(strings.Repeat("界", 650)+"\n", 120), 0, 0},
		{"many-lines-page", strings.Repeat("line\n", defaultLimit+3), defaultLimit - 1, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			writeFile(t, path, tc.content)
			want, wantErr := legacyReadResult(path, tc.name, tc.offset, tc.limit)
			got, gotErr := (&Read{Root: root}).Run(context.Background(), args(t, map[string]any{
				"path": tc.name, "offset": tc.offset, "limit": tc.limit,
			}))
			if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || got != want {
				t.Fatalf("streaming mismatch\n got: %#v, err=%v\nwant: %#v, err=%v", got, gotErr, want, wantErr)
			}
		})
	}
}

func TestReadBinaryDetection8000ByteBoundary(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name   string
		prefix int
	}{{"nul-at-7999", 7999}, {"nul-at-8000", 8000}} {
		t.Run(tc.name, func(t *testing.T) {
			content := strings.Repeat("x", tc.prefix) + "\x00tail"
			path := filepath.Join(root, tc.name)
			writeFile(t, path, content)
			want, wantErr := legacyReadResult(path, tc.name, 0, 0)
			got, gotErr := (&Read{Root: root}).Run(context.Background(), args(t, map[string]any{"path": tc.name}))
			if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || got != want {
				t.Fatalf("boundary mismatch: got %#v, %v; want %#v, %v", got, gotErr, want, wantErr)
			}
		})
	}
}

func BenchmarkReadLargeFile(b *testing.B) {
	root := b.TempDir()
	path := filepath.Join(root, "large.txt")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	line := strings.Repeat("x", 120) + "\n"
	const size = 16 << 20
	for written := 0; written < size; written += len(line) {
		if _, err := f.WriteString(line); err != nil {
			b.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	in, err := json.Marshal(map[string]any{"path": "large.txt", "offset": 50000, "limit": 20})
	if err != nil {
		b.Fatal(err)
	}
	rd := &Read{Root: root}
	for _, stream := range []bool{false, true} {
		name := "previous"
		if stream {
			name = "stream"
		}
		b.Run(name, func(b *testing.B) {
			b.SetBytes(size)
			b.ReportAllocs()
			for b.Loop() {
				var err error
				if stream {
					_, err = rd.Run(context.Background(), in)
				} else {
					_, err = legacyReadResult(path, "large.txt", 50000, 20)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestWriteCreatesParentsAndReportsLines(t *testing.T) {
	root := t.TempDir()
	res, err := (&Write{Root: root}).Run(context.Background(), args(t, map[string]any{
		"path": "deep/er/new.txt", "content": "a\nb\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root, "deep/er/new.txt")); got != "a\nb\n" {
		t.Fatalf("content = %q", got)
	}
	if res.Output != "wrote 4 bytes to deep/er/new.txt" || res.Summary != "deep/er/new.txt (2 lines)" || res.Detail != "+a\n+b" {
		t.Fatalf("result = %+v", res)
	}
	// Overwrite replaces, does not append.
	if _, err := (&Write{Root: root}).Run(context.Background(), args(t, map[string]any{"path": "deep/er/new.txt", "content": "z"})); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root, "deep/er/new.txt")); got != "z" {
		t.Fatalf("overwrite = %q", got)
	}
}

func TestEditRequiresUniqueMatchUnlessReplaceAll(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "f.go")
	writeFile(t, p, "foo bar foo\n")
	ed := &Edit{Root: root}
	run := func(old, nw string, all bool) (Result, error) {
		in := map[string]any{"path": "f.go", "old_string": old, "new_string": nw}
		if all {
			in["replace_all"] = true
		}
		return ed.Run(context.Background(), args(t, in))
	}

	if _, err := run("foo", "baz", false); err == nil || !strings.Contains(err.Error(), "matches 2 times") {
		t.Fatalf("ambiguous err = %v", err)
	}
	if got := readFile(t, p); got != "foo bar foo\n" {
		t.Fatalf("file changed on refused edit: %q", got)
	}
	if _, err := run("nope", "x", false); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("not found err = %v", err)
	}
	if _, err := run("", "x", false); err == nil {
		t.Fatal("empty old_string accepted")
	}
	if _, err := run("foo", "foo", false); err == nil {
		t.Fatal("identical strings accepted")
	}

	res, err := run("bar", "qux", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "foo qux foo\n" {
		t.Fatalf("unique edit = %q", got)
	}
	if res.Output != "replaced 1 occurrence(s) in f.go" || res.Detail != "-bar\n+qux" {
		t.Fatalf("result = %+v", res)
	}

	res, err = run("foo", "baz", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "baz qux baz\n" {
		t.Fatalf("replace_all = %q", got)
	}
	if res.Output != "replaced 2 occurrence(s) in f.go" {
		t.Fatalf("replace_all output = %q", res.Output)
	}
}

func TestEditPreservesRestOfFileExactly(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "f.txt")
	src := "line1\n\tindented\r\nline3 no newline at end"
	writeFile(t, p, src)
	_, err := (&Edit{Root: root}).Run(context.Background(), args(t, map[string]any{
		"path": p, "old_string": "\tindented\r\n", "new_string": "  spaces\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "line1\n  spaces\nline3 no newline at end" {
		t.Fatalf("edit = %q", got)
	}
}

func TestTruncateKeepsRuneBoundaryAndNotesSize(t *testing.T) {
	s := "ab" + "日本語"
	got := truncate(s, 4) // byte 4 is mid-rune (日 is bytes 2..4)
	if !strings.HasPrefix(got, "ab") || strings.Contains(got, "\uFFFD") {
		t.Fatalf("truncate = %q", got)
	}
	if !strings.Contains(got, "[output truncated: 2 of 11 bytes shown]") {
		t.Fatalf("note = %q", got)
	}
	if truncate("abc", 3) != "abc" {
		t.Fatal("exact fit was truncated")
	}
}

func TestRegistryOrderLookupAndDuplicatePanic(t *testing.T) {
	r := Default(t.TempDir())
	var names []string
	for _, tl := range r.All() {
		names = append(names, tl.Name())
	}
	if strings.Join(names, ",") != "read,write,edit,bash" {
		t.Fatalf("order = %v", names)
	}
	if _, ok := r.Get("edit"); !ok {
		t.Fatal("edit missing")
	}
	if _, ok := r.Get("rm"); ok {
		t.Fatal("unknown tool found")
	}
	if ft := r.Fantasy(); len(ft) != 4 {
		t.Fatalf("fantasy tools = %d", len(ft))
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate name did not panic")
		}
	}()
	NewRegistry(&Read{}, &Read{})
}
