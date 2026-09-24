package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dantearo/arkex/internal/config"
	"github.com/dantearo/arkex/internal/tools"
)

func TestWorkspaceExplicitDeniesInEveryMode(t *testing.T) {
	for _, mode := range Modes {
		for _, tool := range []string{"read", "bash", "write"} {
			t.Run(string(mode)+"/"+tool, func(t *testing.T) {
				s, _, home := testScope(t)
				s.trust(home, ReachOutsideWrite)
				grants := &Grants{}
				grants.Add(tool)
				ask := &scriptedAsker{answers: []Answer{AllowSession}}
				cfg := &config.Config{Permissions: map[string]config.Permission{tool: config.PermissionDeny}}
				p := NewModePolicy(mode, ConfigPolicy{Config: cfg, Asker: ask, Grants: grants})
				p.SetScope(s, ask)
				for _, input := range []string{`{"path":"local"}`, `{"path":"~/secret","command":"cat ~/secret"}`} {
					d, err := p.Decide(context.Background(), ToolCall{Name: tool, Input: input})
					if err != nil || d.Allowed || d.Reason != "denied by config" || len(ask.calls) != 0 {
						t.Fatalf("explicit deny bypassed: %+v %v %+v", d, err, ask.calls)
					}
				}
			})
		}
	}
}

func TestWorkspaceTrustBoundaries(t *testing.T) {
	s, _, home := testScope(t)
	if err := os.MkdirAll(filepath.Join(home, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	ask := &scriptedAsker{answers: []Answer{AllowOnce, Deny, AllowSession, Deny, AllowSession, Deny}}
	p := NewModePolicy(ModeAuto, AllowAll{})
	p.SetScope(s, ask)
	check := func(name, input string, allowed bool, prompts int) {
		t.Helper()
		d, err := p.Decide(context.Background(), ToolCall{Name: name, Input: input})
		if err != nil || d.Allowed != allowed || len(ask.calls) != prompts {
			t.Fatalf("%s %s: %+v %v, prompts=%d want %d", name, input, d, err, len(ask.calls), prompts)
		}
	}
	check("read", `{"path":"~/shared/a"}`, true, 1)
	check("read", `{"path":"~/shared/a"}`, false, 2) // once must not persist
	check("read", `{"path":"~/shared/a"}`, true, 3)  // explicit read trust
	check("read", `{"path":"~/shared/sub/b"}`, true, 3)
	check("write", `{"path":"~/shared/b"}`, false, 4) // read is not change trust
	check("bash", `{"command":"cat ~/shared/a; cat ~/other/b"}`, false, 6)
	// First directory is now trusted for changes, second was denied.
	check("write", `{"path":"~/shared/c"}`, true, 6)
	if err := os.Symlink(filepath.Join(home, "other"), filepath.Join(home, "shared/link")); err != nil {
		t.Fatal(err)
	}
	check("write", `{"path":"~/shared/link/new"}`, false, 7)
	// A new process has no grants.
	p.SetScope(NewScope(s.Root, home), ask)
	check("read", `{"path":"~/shared/a"}`, false, 8)
}

func TestWorkspaceBashWorkdirExecutesSiblingHeredoc(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	s, root, home := testScope(t)
	for _, d := range []string{"falak", "frontend/.tooling"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "frontend/.tooling/activate"), []byte("printf 'SIBLING_OK\\n'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ask := &scriptedAsker{}
	p := NewModePolicy(ModeAuto, ConfigPolicy{Config: &config.Config{}})
	p.SetScope(s, ask)
	command := "cat <<'EOF'\nHEREDOC_OK\nEOF\n. ../frontend/.tooling/activate\npwd"
	for _, dir := range []string{"falak", filepath.Join(root, "falak")} {
		raw, _ := json.Marshal(map[string]any{"command": command, "workdir": dir})
		d, err := p.Decide(context.Background(), ToolCall{Name: "bash", Input: string(raw)})
		if err != nil || !d.Allowed || len(ask.calls) != 0 {
			t.Fatalf("sibling command prompted: %+v %v", d, err)
		}
		res, err := (&tools.Bash{Dir: root, Shell: "/bin/sh"}).Run(context.Background(), raw)
		want := "HEREDOC_OK\nSIBLING_OK\n" + filepath.Join(root, "falak") + "\n"
		if err != nil || res.Output != want {
			t.Fatalf("execution mismatch: %q want %q, %v", res.Output, want, err)
		}
	}
	for _, dir := range []string{"missing", "frontend/.tooling/activate"} {
		raw, _ := json.Marshal(map[string]any{"command": "touch SHOULD_NOT_EXIST", "workdir": dir})
		if _, err := (&tools.Bash{Dir: root, Shell: "/bin/sh"}).Run(context.Background(), raw); err == nil {
			t.Fatal("invalid workdir ran command")
		}
	}
	if _, err := os.Stat(filepath.Join(root, "SHOULD_NOT_EXIST")); !os.IsNotExist(err) {
		t.Fatal("command ran despite invalid workdir")
	}
	// The selected directory itself requires approval, even for `pwd`.
	raw, _ := json.Marshal(map[string]any{"command": "pwd", "workdir": home})
	if d, _ := p.Decide(context.Background(), ToolCall{Name: "bash", Input: string(raw)}); d.Allowed || len(ask.calls) != 1 {
		t.Fatal("outside workdir bypassed approval")
	}
	if !strings.Contains(ask.calls[0].Workdir, home) {
		t.Fatalf("missing starting directory: %+v", ask.calls[0])
	}
}
