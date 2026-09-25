// Package prompt assembles the system prompt: a short built-in core plus
// any AGENTS.md files found walking up from the working directory.
package prompt

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const core = `You are arkex, a coding agent running in the user's terminal.

Work directly in the repository using the provided tools. Read before you edit. Prefer small, verifiable changes. Run the project's own build and test commands to check your work and report results honestly, including failures.

Do not invent files, APIs, or results you did not observe. When a task is ambiguous, pick a sensible default, state it briefly, and continue. Keep replies concise and lead with the outcome.

AGENTS.md instructions apply only to their containing directory and descendants; more specific instructions take precedence within that scope. Tools may return newly discovered instructions instead of executing an action. Read them, then reconsider and repeat the action if appropriate. For shell work in a subdirectory, set workdir so its instructions can be discovered; before accessing other directories through shell commands, read their applicable AGENTS.md files. Shell scripts can compute paths that cannot be discovered automatically.

Each bash call starts in the workspace unless you set its workdir field (absolute or workspace-relative; ~/ is supported, shell expansions are not). Prefer workdir over cd, especially for multiline scripts and heredocs: workdir="falak" makes ../frontend resolve to the workspace's frontend sibling. If workdir does not exist, the command does not run. Directory changes do not persist between calls. If you must use cd, guard every dependent command with cd subdir && { ...; }. Do not bypass an outside-workspace approval: correct unintended path resolution, or request permission when the task genuinely needs outside access.

Never execute destructive commands (rm -rf, git push --force, git reset --hard, dropping data) without the user asking for exactly that.`

// Options controls prompt assembly.
type Options struct {
	Cwd   string
	Now   time.Time
	Model string
}

// Build returns the full system prompt.
func Build(o Options) string {
	var sb strings.Builder
	sb.WriteString(core)
	sb.WriteString("\n\n# Environment\n")
	sb.WriteString("Working directory: " + o.Cwd + "\n")
	sb.WriteString("Tool paths are resolved against it; pass them relative (app/views/home.html.erb), not absolute.\n")
	sb.WriteString("It is the workspace: outside access needs permission unless the user explicitly trusts that directory for this process. Read-only trust does not permit changes or shell calls. Explicit config denies apply in every mode. Shell path checking is heuristic, not isolation. Stay inside unless the task requires otherwise.\n")
	sb.WriteString("OS: " + runtime.GOOS + "/" + runtime.GOARCH + "\n")
	if !o.Now.IsZero() {
		sb.WriteString("Date: " + o.Now.Format("2006-01-02") + "\n")
	}
	if o.Model != "" {
		sb.WriteString("Model: " + o.Model + "\n")
	}
	for _, f := range AgentsFiles(o.Cwd) {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		sb.WriteString("\n" + Instructions(f, string(b)))
	}
	return sb.String()
}

// Instructions uses the same delimited form at startup and during tool access.
// The end marker distinguishes a complete file from an older content prefix.
func Instructions(path, body string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return "# Project instructions from " + path + "\n" + body + "\n# End project instructions from " + path + "\n" +
		"Scope: " + filepath.Dir(path) + " and its descendants only. More specific directory instructions take precedence within their scope.\n"
}

// ScratchNote supplies the active conversation's disposable directory.
func ScratchNote(dir string) string {
	return "\n\n# Temporary files\nYour temporary workspace is " + dir + ". Use this exact absolute path for disposable scripts, downloads, logs, and intermediate output. Keep project changes and user deliverables in the working directory. This folder is private to this conversation during this Arkex run; old scratch paths in resumed history may no longer exist. Do not rely on temporary files surviving a restart. Do not modify or delete other sessions' or applications' temporary files. Arkex cleans its owned scratch folders on normal exit. Bash receives TMPDIR, TMP and TEMP pointing here. This scratch folder does not require an outside-workspace approval, but access still follows the current mode and configured permissions; it does not bypass Plan mode or explicit denies.\n"
}

// PlanNote is appended to the system prompt while the user is in plan mode.
const PlanNote = `

# Mode: plan
The user has put you in plan mode. Only read-only tools (read, grep, find, ls) will run; edit, write and bash are refused. Investigate the code and reply with a concrete plan: which files change, what changes, how to verify, and the open questions. Do not attempt edits or commands. If the user wants the plan carried out, tell them to switch to build mode.`

// AgentsFiles returns AGENTS.md paths from the filesystem root down to cwd
// (outermost first), stopping at the git repository root if one is found.
func AgentsFiles(cwd string) []string {
	var found []string
	dir := filepath.Clean(cwd)
	for {
		if p := filepath.Join(dir, "AGENTS.md"); fileExists(p) {
			found = append(found, p)
		}
		if fileExists(filepath.Join(dir, ".git")) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
		found[i], found[j] = found[j], found[i]
	}
	return found
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
