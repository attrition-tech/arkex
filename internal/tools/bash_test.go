//go:build unix

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runBash(t *testing.T, b *Bash, in map[string]any) (Result, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	return b.Run(context.Background(), json.RawMessage(raw))
}

func TestBashScratchEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", "old")
	t.Setenv("TMP", "old")
	t.Setenv("TEMP", "old")
	res, err := runBash(t, &Bash{Dir: t.TempDir(), TempDir: dir, Shell: "/bin/sh"}, map[string]any{"command": `test "$TMPDIR" = "$TMP" && test "$TMP" = "$TEMP" && printf kept > "$TMPDIR/output"`})
	if err != nil {
		t.Fatalf("%v: %s", err, res.Output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "output"))
	if err != nil || string(data) != "kept" {
		t.Fatal("shell did not write into scratch")
	}
	if os.Getenv("TMPDIR") != "old" {
		t.Fatal("changed host environment")
	}
}

func TestBashCombinesStreamsAndRunsInDir(t *testing.T) {
	dir := t.TempDir()
	b := &Bash{Dir: dir, Shell: "/bin/sh"}
	res, err := runBash(t, b, map[string]any{"command": "echo out; echo err 1>&2; pwd"})
	if err != nil {
		t.Fatal(err)
	}
	// Stdout and stderr share one buffer, in order.
	if !strings.HasPrefix(res.Output, "out\nerr\n") {
		t.Fatalf("output = %q", res.Output)
	}
	real, _ := filepath.EvalSymlinks(dir)
	printed := strings.TrimSpace(strings.TrimPrefix(res.Output, "out\nerr\n"))
	printed, _ = filepath.EvalSymlinks(printed)
	if printed != real {
		t.Fatalf("pwd not in Dir: %q", res.Output)
	}
	if res.Summary != "echo out; echo err 1>&2; pwd" {
		t.Fatalf("summary = %q", res.Summary)
	}
}

func TestBashGuardedHeredocStopsAfterFailedCD(t *testing.T) {
	b := &Bash{Dir: t.TempDir(), Shell: "/bin/sh"}
	for _, tc := range []struct {
		name, command string
		wantRan       bool
	}{
		{"independent follow-up", "cd missing && cat <<'EOF'\nunused\nEOF\nprintf 'FOLLOW_UP_RAN'", true},
		{"guarded follow-up", "cd missing && {\ncat <<'EOF'\nunused\nEOF\nprintf 'FOLLOW_UP_RAN'\n}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := runBash(t, b, map[string]any{"command": tc.command})
			if got := strings.Contains(res.Output, "FOLLOW_UP_RAN"); got != tc.wantRan {
				t.Fatalf("follow-up ran = %v, want %v: %s", got, tc.wantRan, res.Output)
			}
			if (err == nil) != tc.wantRan {
				t.Fatalf("unexpected exit result: %v", err)
			}
		})
	}
}

func TestBashExitStatusIsAnErrorWithOutputKept(t *testing.T) {
	b := &Bash{Dir: t.TempDir(), Shell: "/bin/sh"}
	res, err := runBash(t, b, map[string]any{"command": "echo partial; exit 3"})
	if err == nil || err.Error() != "exit status 3" {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasPrefix(res.Output, "partial\n[exit status 3 in ") {
		t.Fatalf("output = %q", res.Output)
	}
}

func TestBashEmptyAndSilentCommands(t *testing.T) {
	b := &Bash{Dir: t.TempDir(), Shell: "/bin/sh"}
	if _, err := runBash(t, b, map[string]any{"command": "   "}); err == nil {
		t.Fatal("blank command accepted")
	}
	res, err := runBash(t, b, map[string]any{"command": "true"})
	if err != nil || res.Output != "(no output)" {
		t.Fatalf("silent: %q err=%v", res.Output, err)
	}
}

func TestBashTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	b := &Bash{Dir: dir, Shell: "/bin/sh"}
	start := time.Now()
	// A background grandchild that would outlive the shell if only the shell
	// were killed.
	res, err := runBash(t, b, map[string]any{
		"command":    "sleep 30 & echo $! > child.pid; echo started; wait",
		"timeout_ms": 300,
	})
	if err == nil || err.Error() != "timed out" {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s; WaitDelay/kill not effective", time.Since(start))
	}
	if !strings.HasPrefix(res.Output, "started\n") || !strings.Contains(res.Output, "[command timed out after 300ms]") {
		t.Fatalf("output = %q", res.Output)
	}
	pidText, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if err != nil {
		t.Fatal(err)
	}
	// Process-group kill: the grandchild must be gone (or a zombie being
	// reaped) shortly after the tool returns.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // ESRCH: gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("background child %d survived the timeout", pid)
}

func TestBashTruncatesHugeOutput(t *testing.T) {
	b := &Bash{Dir: t.TempDir(), Shell: "/bin/sh"}
	res, err := runBash(t, b, map[string]any{"command": "head -c 200000 /dev/zero | tr '\\0' 'x'"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Output) > maxBashOutput+200 || !strings.Contains(res.Output, "[output truncated: 51200 of 200000 bytes shown]") {
		t.Fatalf("len=%d tail=%q", len(res.Output), res.Output[len(res.Output)-80:])
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("  ls -la\nwc -l\n"); got != "ls -la …" {
		t.Fatalf("multi-line = %q", got)
	}
	long := strings.Repeat("a", 100)
	if got := firstLine(long); len(got) != 77+len("…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("long = %q (%d)", got, len(got))
	}
}

func TestBashOutputBoundedAndMatchesTruncation(t *testing.T) {
	for _, input := range []string{
		"", "stdout\nstderr\n", strings.Repeat("x", maxBashOutput),
		strings.Repeat("x", maxBashOutput+1),
		strings.Repeat("x", maxBashOutput-1) + "界tail",
		strings.Repeat("x", maxBashOutput-3) + "界tail",
		strings.Repeat("data", maxBashOutput),
	} {
		for _, chunk := range []int{1, 7, 32768, len(input) + 1} {
			out := limitedOutput{limit: maxBashOutput}
			for pos := 0; pos < len(input); pos += chunk {
				p := input[pos:min(pos+chunk, len(input))]
				n, err := io.Copy(&out, strings.NewReader(p))
				if err != nil || n != int64(len(p)) {
					t.Fatalf("short write: %d, %v", n, err)
				}
				if out.buf.Len() > maxBashOutput+1 {
					t.Fatal("output exceeded retention limit")
				}
			}
			if got, want := out.String(), truncate(input, maxBashOutput); got != want {
				t.Fatalf("length %d, chunk %d: output differs from existing truncation", len(input), chunk)
			}
			if out.total != int64(len(input)) {
				t.Fatalf("count = %d, want %d", out.total, len(input))
			}
		}
	}
}

// Compare retention for the same 8 MiB child output, in pipe-sized chunks.
func BenchmarkBashOutput(b *testing.B) {
	chunk := bytes.Repeat([]byte("x"), 32*1024)
	for _, bounded := range []bool{false, true} {
		name := "previous-unbounded"
		if bounded {
			name = "bounded"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if bounded {
					out := limitedOutput{limit: maxBashOutput}
					for range 256 {
						_, _ = out.Write(chunk)
					}
					_ = out.String()
				} else {
					var out bytes.Buffer
					for range 256 {
						out.Write(chunk)
					}
					_ = truncate(out.String(), maxBashOutput)
				}
			}
		})
	}
}
