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
	TempDir string // conversation-owned scratch
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
	return "Run a shell command in a private working copy and return its combined output. Completed commands publish changed files to the workspace and scratch; conflicts or unsupported filesystem entries block publication. On macOS use relative workspace paths and $TMPDIR: original absolute paths are read-only. Long-running or interactive commands are not supported; set timeout_ms for slow commands (max 10 minutes)."
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
	dir := t.Dir
	if in.Workdir != "" {
		resolved, err := resolvePath(t.Dir, in.Workdir)
		if err != nil {
			return Result{}, err
		}
		dir = resolved
	}
	run, err := sandboxCommand(ctx, t.Dir, t.TempDir, dir, sh, "-c", in.Command)
	if err != nil {
		return Result{}, err
	}
	defer run.Close()
	cmd := run.cmd
	cmd.Stdin = nil
	env := cmd.Environ()
	if t.TempDir != "" {
		env = append(env, "TMPDIR="+t.TempDir, "TMP="+t.TempDir, "TEMP="+t.TempDir)
	}
	cmd.Env = run.environment(env)
	buf := limitedOutput{limit: maxBashOutput}
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err = cmd.Run()
	dur := time.Since(start).Round(time.Millisecond)
	// Best-effort child cleanup; write safety does not depend on descendants
	// exiting. Publication copies captured bytes, never execution inodes.
	if cmd.Process != nil {
		_ = cmd.Cancel()
	}
	var publishErr error
	if ctx.Err() == nil && cmd.ProcessState != nil {
		publishErr = run.Publish(ctx)
	}

	out := buf.String()
	var sb strings.Builder
	sb.WriteString(out)
	if out != "" && !strings.HasSuffix(out, "\n") {
		sb.WriteString("\n")
	}
	summary := firstLine(in.Command)
	switch {
	case publishErr != nil:
		fmt.Fprintf(&sb, "[command exit: %v; %v]", err, publishErr)
		return Result{Output: sb.String(), Summary: summary}, publishErr
	case ctx.Err() == context.DeadlineExceeded:
		fmt.Fprintf(&sb, "[command timed out after %s]\n[unpublished command changes discarded]", timeout)
		return Result{Output: sb.String(), Summary: summary}, errors.New("timed out")
	case ctx.Err() != nil:
		sb.WriteString("[command canceled; unpublished command changes discarded]")
		return Result{Output: sb.String(), Summary: summary}, ctx.Err()
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
