package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultBashTimeout = 2 * time.Minute
	maxBashTimeout     = 10 * time.Minute
	maxBashOutput      = 50 * 1024
)

// Bash runs a shell command in Dir.
type Bash struct {
	Dir     string
	TempDir string // conversation-owned scratch, empty preserves inherited environment
	// Shell overrides the shell binary; defaults to $SHELL or a discovered
	// POSIX shell.
	Shell string
}

type bashInput struct {
	Command   string `json:"command"`
	Workdir   string `json:"workdir,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

func (*Bash) Name() string { return "bash" }
func (*Bash) Description() string {
	return "Run a shell command in the working directory and return its combined output. Long-running or interactive commands are not supported; set timeout_ms for slow commands (max 10 minutes)."
}

func (*Bash) Schema() map[string]any {
	return schema(map[string]any{
		"command":    prop("string", "Shell command to run"),
		"workdir":    prop("string", "Starting directory, absolute or workspace-relative; defaults to the workspace. Use instead of cd. Supports ~/ but not shell expansions."),
		"timeout_ms": prop("integer", "Timeout in milliseconds. Default 120000"),
	}, "command")
}

func (t *Bash) Run(ctx context.Context, input json.RawMessage) (Result, error) {
	var in bashInput
	if err := decode(input, &in); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(in.Command) == "" {
		return Result{}, errors.New("command is required")
	}
	timeout := defaultBashTimeout
	if in.TimeoutMS > 0 {
		timeout = min(time.Duration(in.TimeoutMS)*time.Millisecond, maxBashTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sh, err := resolveShell(t.Shell, os.Getenv("SHELL"))
	if err != nil {
		return Result{}, err
	}
	cmd := exec.CommandContext(ctx, sh, "-c", in.Command)
	cmd.Dir = t.Dir
	if in.Workdir != "" {
		dir, err := resolvePath(t.Dir, in.Workdir)
		if err != nil {
			return Result{}, err
		}
		cmd.Dir = dir
	}
	cmd.Stdin = nil
	if t.TempDir != "" {
		cmd.Env = append(cmd.Environ(), "TMPDIR="+t.TempDir, "TMP="+t.TempDir, "TEMP="+t.TempDir)
	}
	buf := limitedOutput{limit: maxBashOutput}
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err = cmd.Run()
	dur := time.Since(start).Round(time.Millisecond)

	out := buf.String()
	var sb strings.Builder
	sb.WriteString(out)
	if out != "" && !strings.HasSuffix(out, "\n") {
		sb.WriteString("\n")
	}
	summary := firstLine(in.Command)
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		fmt.Fprintf(&sb, "[command timed out after %s]", timeout)
		return Result{Output: sb.String(), Summary: summary}, errors.New("timed out")
	case err != nil:
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			fmt.Fprintf(&sb, "[exit status %d in %s]", exit.ExitCode(), dur)
			return Result{Output: sb.String(), Summary: summary}, fmt.Errorf("exit status %d", exit.ExitCode())
		}
		return Result{Output: sb.String(), Summary: summary}, err
	}
	if buf.total == 0 {
		sb.WriteString("(no output)")
	}
	return Result{Output: sb.String(), Summary: summary}, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 80 {
		s = s[:77] + "…"
	}
	return s
}
