package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeAsker records prompts and answers with a scripted verdict.
type fakeAsker struct {
	answer bool
	calls  []ToolCall
}

func (a *fakeAsker) Ask(_ context.Context, call ToolCall) (Answer, error) {
	a.calls = append(a.calls, call)
	if a.answer {
		return AllowOnce, nil
	}
	return Deny, nil
}

func testScope(t *testing.T) (*Scope, string, string) {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "code", "proj")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewScope(root, home)
	return s, s.Root, s.Home
}

func toolInput(t *testing.T, values map[string]string) string {
	t.Helper()
	b, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestScopeClassify(t *testing.T) {
	s, root, home := testScope(t)
	systemReach, systemWhere := ReachInside, ""
	if runtime.GOOS == "windows" {
		systemReach = ReachOutsideWrite
		systemWhere = s.display(canonical(s.resolve("/etc/hosts")))
	}
	cases := []struct {
		name, tool, input string
		want              Reach
		where             string
	}{
		{"relative read", "read", `{"path":"src/a.go"}`, ReachInside, ""},
		{"absolute inside", "edit", toolInput(t, map[string]string{"path": filepath.Join(root, "src/a.go")}), ReachInside, ""},
		{"dot-dot that stays inside", "read", `{"path":"src/../go.mod"}`, ReachInside, ""},
		{"sibling sharing a prefix", "read", toolInput(t, map[string]string{"path": root + "2/a.go"}), ReachOutsideRead, "~/code/proj2/a.go"},
		{"parent", "read", `{"path":"../other/x"}`, ReachOutsideRead, "~/code/other/x"},
		{"tilde write", "write", `{"path":"~/notes.md"}`, ReachOutsideWrite, "~/notes.md"},
		{"absolute outside home", "edit", `{"path":"/etc/hosts"}`, ReachOutsideWrite, s.display(canonical(s.resolve("/etc/hosts")))},
		{"grep without path", "grep", `{"pattern":"x"}`, ReachInside, ""},
		{"bash inside", "bash", `{"command":"go test ./... && ls src"}`, ReachInside, ""},
		{"bash system prefix", "bash", `{"command":"cat /etc/hosts; ls /usr/bin | head"}`, systemReach, systemWhere},
		{"bash arbitrary temp", "bash", `{"command":"cp a /tmp/b"}`, ReachOutsideWrite, s.display(canonical(s.resolve("/tmp/b")))},
		{"bash url is not a path", "bash", `{"command":"curl https://example.com/x/y"}`, ReachInside, ""},
		{"bash tilde", "bash", `{"command":"rm -rf ~/Downloads/x"}`, ReachOutsideWrite, "~/Downloads/x"},
		{"bash HOME var", "bash", `{"command":"echo hi > $HOME/out.txt"}`, ReachOutsideWrite, "~/out.txt"},
		{"bash cd parent", "bash", `{"command":"cd .. && ls"}`, ReachOutsideWrite, "~/code"},
		{"bash absolute inside", "bash", toolInput(t, map[string]string{"command": `ls "` + filepath.ToSlash(root) + `/src"`}), ReachInside, ""},
		{"bash sibling prefix", "bash", toolInput(t, map[string]string{"command": `ls "` + filepath.ToSlash(root) + `-old"`}), ReachOutsideWrite, "~/code/proj-old"},
		{"bash assignment value", "bash", toolInput(t, map[string]string{"command": `OUT="` + filepath.ToSlash(home) + `/x" make`}), ReachOutsideWrite, "~/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, where := s.Classify(ToolCall{Name: tc.tool, Input: tc.input})
			if got != tc.want || where != tc.where {
				t.Fatalf("Classify = (%d, %q), want (%d, %q)", got, where, tc.want, tc.where)
			}
		})
	}
}

func TestScopeGrantCoversDirectorySubtree(t *testing.T) {
	s, _, home := testScope(t)
	lib := filepath.Join(home, "lib", "pkg")
	if err := os.MkdirAll(filepath.Join(lib, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if s.Granted("~/lib/pkg/a.go") {
		t.Fatal("nothing granted yet")
	}
	s.Grant("~/lib/pkg/a.go") // a file: grants its parent
	if !s.Granted("~/lib/pkg/b.go") || !s.Granted("~/lib/pkg/sub/c.go") {
		t.Fatal("sibling and child of a granted file's directory should be covered")
	}
	if s.Granted("~/lib/other.go") || s.Granted("~/lib/pkg2/x.go") {
		t.Fatal("parent and prefix-sibling directories must not be covered")
	}
	s.Grant(lib + "/sub") // a directory: grants itself
	if !s.Granted(lib + "/sub/deep/x") {
		t.Fatal("directory grant should cover its subtree")
	}
}

func TestShellPatternsAreNotPaths(t *testing.T) {
	s, _, _ := testScope(t)
	cases := []struct{ command, outside string }{
		{`grep -rn "/logging" .`, ""},
		{`rg -n '/logging' .`, ""},
		{`grep -A 2 -- /logging .`, ""},
		{`rg --regexp=/logging .`, ""},
		{`grep -rne /logging -e /other .`, ""},
		{`curl 'https://x.dev/logging?path=/logging'`, ""},
		{`curl --url=https://x.dev/logging`, ""},
		{`grep -rn /logging /outside/file`, "/outside/file"},
		{`grep -e /logging /outside/file`, "/outside/file"},
		{`grep -f /outside/patterns .`, "/outside/patterns"},
		{`rg --file=/outside/patterns .`, "/outside/patterns"},
		{`rg --files /outside`, "/outside"},
		{`grep /outside/file -e /logging`, "/outside/file"},
		{`cat "/outside/file with spaces"`, "/outside/file with spaces"},
		{`cat '/outside/'"file"`, "/outside/file"},
		{`cat "/outside/$NAME"`, "/outside"},
		{`cat /outside/$(echo name)`, "/outside"},
		{`cat "$HOME/$NAME"`, s.Home},
		{`grep "/logging$SUFFIX" .`, ""},
		{`grep "/logging$SUFFIX" "/outside/$NAME"`, "/outside"},
		{`grep$COMMAND /outside/file`, "/outside/file"},
		{`grep /logging . > /outside/log`, "/outside/log"},
		{`curl https://x.dev/logging --output=/outside/log`, "/outside/log"},
		{`grep "$(cat /outside/pattern)" .`, "/outside/pattern"},
		{`grep /logging .; cat /outside/log`, "/outside/log"},
		{`grep /logging . | grep /other /outside/log`, "/outside/log"},
		{`echo hi > "$HOME/out.txt"`, filepath.Join(s.Home, "out.txt")},
		{`cat '/outside/unterminated`, "/outside/unterminated"},
	}
	for _, tc := range cases {
		if runtime.GOOS == "windows" && strings.HasPrefix(tc.outside, "/") {
			tc.outside = canonical(s.resolve(tc.outside))
		}
		t.Run(tc.command, func(t *testing.T) {
			if got := s.commandEscapes(tc.command); got != tc.outside {
				t.Fatalf("outside = %q, want %q", got, tc.outside)
			}
		})
	}
}

func TestShellCDAndChainPaths(t *testing.T) {
	s, root, home := testScope(t)
	for _, dir := range []string{"falak/nested", "frontend/.tooling"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	parent := canonical(filepath.Dir(root))
	cases := []struct{ command, outside string }{
		{`cd "` + filepath.ToSlash(root) + `/falak" && source ../frontend/.tooling/activate >/dev/null 2>&1; E2E_HEADED=1 node harness/e2e/chrome-e2e.mjs quit; pnpm format:check 2>&1 | tail -1`, ""},
		{`cd falak && source ../frontend/.tooling/activate`, ""},
		// A heredoc's terminator does not extend the preceding && guard.
		{"cd falak && python3 - <<'PY'\nprint('ok')\nPY\nsource ../frontend/.tooling/activate", filepath.Join(parent, "frontend/.tooling/activate")},
		// Guarding the whole group keeps the sibling path inside the workspace.
		{"cd falak && {\npython3 - <<'PY'\nprint('ok')\nPY\nsource ../frontend/.tooling/activate\n}", ""},
		// Grouping must not hide a real escape.
		{"cd falak && {\npython3 - <<'PY'\nprint('ok')\nPY\nsource ../../frontend/.tooling/activate\n}", filepath.Join(parent, "frontend/.tooling/activate")},
		{`cd -- falak && cd nested && cat ../../frontend/config.json`, ""},
		{`cd -L falak && cat ../frontend/config.json`, ""},
		{`cd -P falak && cat ../frontend/config.json`, ""},
		{`cd falak && cat ../../secrets`, filepath.Join(parent, "secrets")},
		{`cd falak && echo ok > ../../output`, filepath.Join(parent, "output")},
		{`cd falak && grep "$(cat ../../secret-pattern)" .`, filepath.Join(parent, "secret-pattern")},
		{`cd falak && grep -rn /logging ../frontend`, ""},
		{`cd falak && cat "$HOME/private"`, filepath.Join(home, "private")},
		{`(cd falak) && cat ../outside`, filepath.Join(parent, "outside")},
		{`echo "$(cd falak)" && cat ../outside`, filepath.Join(parent, "outside")},
		{`! cd falak && cat ../outside`, filepath.Join(parent, "outside")},
		{`cd falak || cat ../outside`, filepath.Join(parent, "outside")},
		{`cd falak | cat ../outside`, filepath.Join(parent, "outside")},
		{`cd falak & cat ../outside`, filepath.Join(parent, "outside")},
		{`cd falak; cat ../outside`, filepath.Join(parent, "outside")},
		{`cd "$UNKNOWN" && cat ../outside`, filepath.Join(parent, "outside")},
		{`cd falak && cd "$UNKNOWN" && cat ../outside`, filepath.Join(parent, "outside")},
		{`cd f* && cat ../outside`, filepath.Join(parent, "outside")},
		{`cd falak && cd - && cat ../outside`, filepath.Join(parent, "outside")},
		{`(cd falak && cat ../frontend/config.json)`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			if got := s.commandEscapes(tc.command); got != tc.outside {
				t.Fatalf("outside = %q, want %q", got, tc.outside)
			}
		})
	}
	// Directory tracking must never mutate the shared workspace scope.
	if got := s.commandEscapes(`cat ../outside`); got != filepath.Join(parent, "outside") || s.Root != root {
		t.Fatalf("cd leaked into another tool call: root=%q outside=%q", s.Root, got)
	}
	if err := os.Symlink(home, filepath.Join(root, "external")); err != nil {
		t.Skip(err)
	}
	if got := s.commandEscapes(`cd external && echo ok`); got != home {
		t.Fatalf("outside cd symlink was allowed: %q", got)
	}
	if err := os.Symlink(filepath.Join(root, "falak/nested"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if got := s.commandEscapes(`cd -L link && cat ../../outside`); got != filepath.Join(parent, "outside") {
		t.Fatalf("logical cd path = %q", got)
	}
	if got := s.commandEscapes(`cd -P link && cat ../../frontend/config.json`); got != "" {
		t.Fatalf("physical cd path should stay inside: %q", got)
	}
}

func TestNilScopeIsInside(t *testing.T) {
	var s *Scope
	if r, _ := s.Classify(ToolCall{Name: "write", Input: `{"path":"/etc/hosts"}`}); r != ReachInside {
		t.Fatalf("nil scope should not classify, got %d", r)
	}
}

func TestModePolicyAutoStillAsksOutsideWorkspace(t *testing.T) {
	s, _, _ := testScope(t)
	ask := &fakeAsker{answer: true}
	p := NewModePolicy(ModeAuto, AllowAll{})
	p.SetScope(s, ask)
	ctx := context.Background()

	d, _ := p.Decide(ctx, ToolCall{Name: "write", Input: `{"path":"src/a.go"}`})
	if !d.Allowed || len(ask.calls) != 0 {
		t.Fatalf("inside write in auto should not ask: %+v asks=%d", d, len(ask.calls))
	}
	for i := 0; i < 2; i++ {
		d, _ = p.Decide(ctx, ToolCall{Name: "write", Input: `{"path":"~/notes.md"}`})
		if !d.Allowed || len(ask.calls) != i+1 {
			t.Fatalf("outside write #%d in auto should ask every time: %+v asks=%d", i+1, d, len(ask.calls))
		}
	}
	if r := ask.calls[0].Reason; r != "writes outside the workspace: ~/notes.md" {
		t.Fatalf("prompt reason = %q", r)
	}
	d, _ = p.Decide(ctx, ToolCall{Name: "bash", Input: `{"command":"ls ~/Downloads"}`})
	if !d.Allowed || len(ask.calls) != 3 || ask.calls[2].Reason != "command names a path outside the workspace: ~/Downloads" {
		t.Fatalf("outside bash in auto should ask with reason: %+v %+v", d, ask.calls[2])
	}

	ask.answer = false
	d, _ = p.Decide(ctx, ToolCall{Name: "write", Input: `{"path":"~/notes.md"}`})
	if d.Allowed || d.Reason != "denied by user" {
		t.Fatalf("declined outside write should be denied: %+v", d)
	}
}

func TestModePolicyOutsideReadAsksOncePerDirectory(t *testing.T) {
	s, _, home := testScope(t)
	if err := os.MkdirAll(filepath.Join(home, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	ask := &scriptedAsker{answers: []Answer{AllowSession, AllowOnce, Deny, AllowOnce}}
	p := NewModePolicy(ModePlan, nil) // plan: reads allowed, no base needed
	p.SetScope(s, ask)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		d, _ := p.Decide(ctx, ToolCall{Name: "read", Input: `{"path":"~/lib/a.go"}`})
		if !d.Allowed || len(ask.calls) != 1 {
			t.Fatalf("read #%d: %+v asks=%d (want one prompt total)", i+1, d, len(ask.calls))
		}
	}
	if ask.calls[0].Reason != "reads outside the workspace: ~/lib/a.go" {
		t.Fatalf("reason = %q", ask.calls[0].Reason)
	}
	d, _ := p.Decide(ctx, ToolCall{Name: "read", Input: `{"path":"~/lib/sub/b.go"}`})
	if !d.Allowed || len(ask.calls) != 1 {
		t.Fatalf("grant should cover the subtree: %+v asks=%d", d, len(ask.calls))
	}
	d, _ = p.Decide(ctx, ToolCall{Name: "read", Input: `{"path":"~/elsewhere/c.go"}`})
	if !d.Allowed || len(ask.calls) != 2 {
		t.Fatalf("another directory should ask again: %+v asks=%d", d, len(ask.calls))
	}

	// Declining does not grant.
	d, _ = p.Decide(ctx, ToolCall{Name: "read", Input: `{"path":"~/secret/k"}`})
	if d.Allowed || len(ask.calls) != 3 {
		t.Fatalf("declined read: %+v", d)
	}
	if d, _ = p.Decide(ctx, ToolCall{Name: "read", Input: `{"path":"~/secret/k"}`}); !d.Allowed || len(ask.calls) != 4 {
		t.Fatalf("declined read should be asked again next time: %+v asks=%d", d, len(ask.calls))
	}
}

func TestModePolicyPlanDeniesOutsideWriteWithoutAsking(t *testing.T) {
	s, _, _ := testScope(t)
	ask := &fakeAsker{answer: true}
	p := NewModePolicy(ModePlan, nil)
	p.SetScope(s, ask)
	d, _ := p.Decide(context.Background(), ToolCall{Name: "write", Input: `{"path":"~/x"}`})
	if d.Allowed || len(ask.calls) != 0 {
		t.Fatalf("plan should deny before prompting: %+v asks=%d", d, len(ask.calls))
	}
}

func TestModePolicyBuildPromptsOnceWithReason(t *testing.T) {
	s, _, _ := testScope(t)
	ask := &fakeAsker{answer: true}
	// A base that itself asks (like ConfigPolicy with permission "ask").
	base := PolicyFunc(func(ctx context.Context, call ToolCall) (Decision, error) {
		a, _ := ask.Ask(ctx, call)
		return Decision{Allowed: a != Deny, Asked: true}, nil
	})
	p := NewModePolicy(ModeBuild, base)
	p.SetScope(s, ask)
	d, _ := p.Decide(context.Background(), ToolCall{Name: "edit", Input: `{"path":"/etc/hosts"}`})
	if !d.Allowed || len(ask.calls) != 1 {
		t.Fatalf("base already asked; scope must not ask twice: %+v asks=%d", d, len(ask.calls))
	}
	wantReason := "writes outside the workspace: " + s.display(canonical(s.resolve("/etc/hosts")))
	if ask.calls[0].Reason != wantReason {
		t.Fatalf("base prompt should carry the scope reason, got %q", ask.calls[0].Reason)
	}

	// A base that allows silently (permission "allow") still gets a prompt.
	p.SetBase(AllowAll{})
	d, _ = p.Decide(context.Background(), ToolCall{Name: "edit", Input: `{"path":"/etc/hosts"}`})
	if !d.Allowed || len(ask.calls) != 2 {
		t.Fatalf("silent base: scope must ask itself: %+v asks=%d", d, len(ask.calls))
	}
}

func TestModePolicyNoAskerDeniesOutside(t *testing.T) {
	s, _, _ := testScope(t)
	p := NewModePolicy(ModeAuto, AllowAll{})
	p.SetScope(s, nil)
	d, _ := p.Decide(context.Background(), ToolCall{Name: "write", Input: `{"path":"~/x"}`})
	if d.Allowed {
		t.Fatalf("print mode (no asker) must deny outside writes even in auto: %+v", d)
	}
	d, _ = p.Decide(context.Background(), ToolCall{Name: "write", Input: `{"path":"src/x"}`})
	if !d.Allowed {
		t.Fatalf("inside write in auto stays allowed: %+v", d)
	}
}

func TestScopeFollowsSymlinksOutOfTheWorkspace(t *testing.T) {
	s, root, home := testScope(t)
	outside := filepath.Join(home, "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks not supported:", err)
	}
	// A new file under the link does not exist yet; its parent resolves outside.
	r, where := s.Classify(ToolCall{Name: "write", Input: `{"path":"link/new.txt"}`})
	if r != ReachOutsideWrite || where != "~/elsewhere/new.txt" {
		t.Fatalf("write through symlink = (%d, %q), want outside write of ~/elsewhere/new.txt", r, where)
	}
	if r, _ := s.Classify(ToolCall{Name: "bash", Input: toolInput(t, map[string]string{"command": `rm -rf "` + filepath.ToSlash(root) + `/link/x"`})}); r != ReachOutsideWrite {
		t.Fatalf("bash through symlink should leave the workspace, got %d", r)
	}
	// A symlink that stays inside is still inside.
	if err := os.Symlink(filepath.Join(root, "src"), filepath.Join(root, "srclink")); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Classify(ToolCall{Name: "write", Input: `{"path":"srclink/a.go"}`}); r != ReachInside {
		t.Fatalf("internal symlink should stay inside, got %d", r)
	}
}

func TestScopeRootThroughSymlinkStillContainsItself(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, "real")
	if err := os.MkdirAll(filepath.Join(real, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skip("symlinks not supported:", err)
	}
	s := NewScope(alias, home) // like a workspace under macOS /tmp
	for _, p := range []string{"src/a.go", alias + "/src/b.go", real + "/src/c.go"} {
		if r, where := s.Classify(ToolCall{Name: "write", Input: toolInput(t, map[string]string{"path": p})}); r != ReachInside {
			t.Fatalf("%s should be inside a symlinked root, got (%d, %q)", p, r, where)
		}
	}
}
