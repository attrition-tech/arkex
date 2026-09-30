package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	Browser   string `json:"browser,omitempty"`
}

func (*Bash) Name() string { return "bash" }
func (*Bash) Description() string {
	return "Run a shell command in a private working copy and return its combined output. Completed commands publish changed files to the workspace and scratch; conflicts or unsupported filesystem entries block publication. On macOS use relative workspace paths and $TMPDIR: original absolute paths are read-only. For macOS browser automation set browser=chromium and attach with Playwright chromium.connectOverCDP(process.env.ARKEX_BROWSER_WS_ENDPOINT, {isLocal:true}); ordinary chromium.launch() is blocked. Arkex downloads a verified stock browser, gives it only its own IPC endpoint, and stops it when this call ends. This is CDP attachment, not full Playwright launch compatibility. Linux uses normal Playwright launch without this option. Long-running or interactive commands are not supported; set timeout_ms for slow commands (max 10 minutes)."
}

func (*Bash) Schema() map[string]any {
	return schema(map[string]any{
		"command":    prop("string", "Shell command to run"),
		"workdir":    prop("string", "Starting directory, absolute or workspace-relative; defaults to the workspace. Use instead of cd. Supports ~/ but not shell expansions."),
		"timeout_ms": prop("integer", "Execution timeout in milliseconds, including managed browser setup but excluding sandbox preparation and publication. Default 120000; maximum 600000. Cancellation still stops the whole operation."),
		"browser":    map[string]any{"type": "string", "enum": []string{"chromium"}, "description": "macOS only: start an isolated, verified stock Chromium for this call. Attach through ARKEX_BROWSER_WS_ENDPOINT. Requires session scratch; the first call downloads about 100 MB, cached and reverified in scratch. No custom executable, launch arguments or persistent browser session."},
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
	if in.Browser != "" && (in.Browser != "chromium" || t.TempDir == "") {
		return Result{}, errors.New("browser must be chromium and requires active session scratch")
	}
	timeout := defaultBashTimeout
	if in.TimeoutMS > 0 {
		timeout = min(time.Duration(in.TimeoutMS)*time.Millisecond, maxBashTimeout)
	}
	// Bind the command to a cancelable context now, but arm its timer only
	// after preparation. Filesystem work still obeys the caller's context.
	executionCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

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
	run, err := sandboxCommand(executionCtx, t.Dir, t.TempDir, dir, sh, "-c", in.Command)
	if err != nil {
		return Result{}, err
	}
	defer run.Close()
	cmd := run.cmd
	cmd.Stdin = nil
	env := cmd.Environ()
	if t.TempDir != "" {
		env = append(env, "TMPDIR="+t.TempDir, "TMP="+t.TempDir, "TEMP="+t.TempDir)
		// npm/npx need a writable cache even for project-local installs. Leave
		// HOME, XDG_CACHE_HOME and browser discovery intact: changing them would
		// hide existing installations. Other download locations can use $TMPDIR.
		env = append(env, "npm_config_cache="+filepath.Join(t.TempDir, "npm-cache"))
	}
	cmd.Env = run.environment(env)
	buf := limitedOutput{limit: maxBashOutput}
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	timeoutDone := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		cancel(context.DeadlineExceeded)
		close(timeoutDone)
	})
	defer timer.Stop()
	err = executeShellSandbox(executionCtx, run, t.TempDir, in.Browser)
	if !timer.Stop() {
		<-timeoutDone
	}
	executionErr := context.Cause(executionCtx)
	dur := time.Since(start).Round(time.Millisecond)
	// Best-effort child cleanup; write safety does not depend on descendants
	// exiting. Publication copies captured bytes, never execution inodes.
	if cmd.Process != nil {
		_ = cmd.Cancel()
	}
	var publishErr error
	if executionErr == nil && cmd.ProcessState != nil {
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
		if err == nil {
			fmt.Fprintf(&sb, "[command succeeded; sandbox publication failed: %v]", publishErr)
		} else {
			fmt.Fprintf(&sb, "[command exit: %v; sandbox publication failed: %v]", err, publishErr)
		}
		return Result{Output: sb.String(), Summary: summary}, publishErr
	case executionErr == context.DeadlineExceeded:
		fmt.Fprintf(&sb, "[command timed out after %s]\n[unpublished command changes discarded]", timeout)
		return Result{Output: sb.String(), Summary: summary}, errors.New("timed out")
	case executionErr != nil:
		sb.WriteString("[command canceled; unpublished command changes discarded]")
		return Result{Output: sb.String(), Summary: summary}, executionErr
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
