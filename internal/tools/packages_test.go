package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestPackagesInput(t *testing.T) {
	for _, input := range []string{
		`{"action":"inspect"}`,
		`{"action":"install","manager":"brew","package":"python@3.12","version":"3.12.9_1"}`,
		`{"action":"info","manager":"apt","package":"libstdc++6"}`,
		`{"action":"search","manager":"dnf","package":"python3"}`,
	} {
		var in packageInput
		if err := decode(json.RawMessage(input), &in); err != nil {
			t.Fatal(err)
		}
		if err := in.validate(); err != nil {
			t.Fatalf("valid input %s: %v", input, err)
		}
	}
	for _, input := range []string{
		`{"action":"inspect","package":"python"}`,
		`{"action":"upgrade","manager":"brew","package":"python"}`,
		`{"action":"install","manager":"unknown","package":"python"}`,
		`{"action":"install","manager":"brew","package":"--build-from-source"}`,
		`{"action":"install","manager":"brew","package":"other/tap/python"}`,
		`{"action":"install","manager":"brew","package":"https://host/pkg.rb"}`,
		`{"action":"install","manager":"brew","package":"../pkg.rb"}`,
		`{"action":"install","manager":"brew","package":"python; touch /tmp/escape"}`,
		`{"action":"install","manager":"brew","package":"python","version":">=3"}`,
		`{"action":"info","manager":"brew","package":"python","version":"3"}`,
		`{"action":"install","manager":"apt","package":"python3","version":"3"}`,
		`{"action":"search","manager":"dnf","package":"*"}`,
		`{"action":"install","manager":"brew","package":"python","args":["--HEAD"]}`,
		`{"action":"inspect","executable":"/tmp/brew"}`,
	} {
		if _, err := (&Packages{}).Run(t.Context(), json.RawMessage(input)); err == nil || strings.Contains(err.Error(), "scratch") {
			t.Fatalf("input not rejected before execution: %s: %v", input, err)
		}
	}
}

func TestPackageQueries(t *testing.T) {
	for _, tc := range []struct {
		manager, action, name string
		args                  []string
	}{
		{"brew", "search", "python", []string{"search", "--formula", "python"}},
		{"brew", "info", "python@3.12", []string{"info", "--json=v2", "--formula", "homebrew/core/python@3.12"}},
		{"apt", "search", "libstdc++", []string{"-o", "Dir::Cache::pkgcache=", "-o", "Dir::Cache::srcpkgcache=", "search", "--names-only", `libstdc\+\+`}},
		{"apt", "info", "bash", []string{"-o", "Dir::Cache::pkgcache=", "-o", "Dir::Cache::srcpkgcache=", "policy", "bash"}},
		{"dnf", "info", "python3", []string{"info", "python3"}},
		{"dnf", "search", "python", []string{"search", "python"}},
	} {
		t.Run(tc.manager+"/"+tc.action, func(t *testing.T) {
			fake := &packageFake{t: t, replies: []packageReply{{out: "package information"}}}
			res, err := queryPackage(packageManager{Name: tc.manager, Executable: "/trusted/manager"}, packageInput{Action: tc.action, Package: tc.name}, fake.run)
			if err != nil || res.Output != "package information" {
				t.Fatalf("query: %+v %v", res, err)
			}
			want := []packageCall{{"/trusted/manager", tc.args}}
			if !reflect.DeepEqual(fake.calls, want) {
				t.Fatalf("commands: %#v, want %#v", fake.calls, want)
			}
		})
	}
}

func TestPackageRunnerConfinementAndEnvironment(t *testing.T) {
	t.Setenv("BASH_ENV", "/untrusted/hook")
	t.Setenv("RUBYOPT", "-runtrusted")
	t.Setenv("HOMEBREW_CURL_PATH", "/untrusted/curl")
	t.Setenv("APT_CONFIG", "/untrusted/apt.conf")
	p := &Packages{Root: t.TempDir(), TempDir: t.TempDir()}
	m := packageManager{Name: "brew", Executable: "/trusted/bin/brew"}
	env := strings.Join(packageEnvironment(m, "/private/cache"), "\n")
	for _, bad := range []string{"BASH_ENV", "RUBYOPT", "HOMEBREW_CURL_PATH", "APT_CONFIG", "untrusted"} {
		if strings.Contains(env, bad) {
			t.Fatalf("inherited unsafe environment: %s", bad)
		}
	}
	for _, want := range []string{"HOME=/private/cache", "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_INSTALL_UPGRADE=1", "HOMEBREW_NO_INSTALL_CLEANUP=1", "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1", "GIT_CONFIG_GLOBAL=/dev/null"} {
		if !strings.Contains(env, want) {
			t.Fatalf("missing installer restriction %s", want)
		}
	}
	run, cleanup, err := p.packageRunner(t.Context(), packageManager{Name: "apt"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	out, err := run("/bin/sh", "-c", `printf scratch-ok > "$HOME/cache"; cat "$HOME/cache"`)
	if err != nil || out != "scratch-ok" {
		t.Fatalf("query cache: %q %v", out, err)
	}
	outside := filepath.Join(t.TempDir(), "forbidden")
	if _, err := run("/bin/sh", "-c", `printf escaped > "$1"`, "probe", outside); err == nil {
		t.Fatal("query escaped sandbox")
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("outside file created:", err)
	}
	// stderr warnings cannot corrupt JSON metadata parsed by the installer.
	out, err = run("/bin/sh", "-c", `printf '{"ok":true}'; printf warning >&2`)
	if err != nil || out != `{"ok":true}` {
		t.Fatalf("mixed metadata and stderr: %q %v", out, err)
	}
	if _, err := run("/bin/sh", "-c", "head -c 52000 /dev/zero"); err == nil {
		t.Fatal("truncated output accepted as a complete installation plan")
	}
}

func TestPackageRunnerDNFRestrictions(t *testing.T) {
	p := &Packages{Root: t.TempDir(), TempDir: t.TempDir()}
	probe := filepath.Join(p.Root, "probe")
	script := `#!/bin/sh
test "$1" = '--setopt=plugins=False' || exit 1
test "$2" = "--setopt=logdir=$HOME" || exit 2
test "$3" = '--cacheonly' || exit 3
test "$4" = 'info' || exit 4
test "$5" = 'python3' || exit 5
test "$#" = 5 || exit 6
test "${DNF5_PLUGINS_DIR-unset}" = '' || exit 7
printf checked
`
	if err := os.WriteFile(probe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DNF5_PLUGINS_DIR", "/untrusted/plugins")
	run, cleanup, err := p.packageRunner(t.Context(), packageManager{Name: "dnf"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if out, err := run(probe, "info", "python3"); err != nil || out != "checked" {
		t.Fatalf("DNF restrictions: %q: %v", out, err)
	}
}

func TestPackageHostRunnerPrivateTemporaryDirectory(t *testing.T) {
	p := &Packages{Root: t.TempDir(), TempDir: t.TempDir()}
	run, cleanup, err := p.packageRunner(t.Context(), packageManager{Name: "brew", Executable: "/trusted/bin/brew"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	// Exercise only a harmless host probe, never an actual installation.
	out, err := run("/bin/sh", "-c", `printf '%s' "$HOME"`)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{p.Root, p.TempDir} {
		real, err := filepath.EvalSymlinks(root)
		if err != nil || within(real, out) {
			t.Fatalf("installer cache is model-writable: %q: %v", out, err)
		}
	}
	if st, err := os.Stat(out); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("private directory permissions: %v: %v", st, err)
	}
	cleanup()
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("installer temporary directory not cleaned: %v", err)
	}
	t.Setenv("TMPDIR", p.TempDir)
	if _, cleanup, err := p.packageRunner(t.Context(), packageManager{Name: "brew"}, true); err == nil {
		cleanup()
		t.Fatal("accepted a model-writable host temporary directory")
	}
}

func TestManagerTrustAndPathShadowing(t *testing.T) {
	p := &Packages{Root: t.TempDir(), TempDir: t.TempDir()}
	prefix := t.TempDir()
	bin := filepath.Join(prefix, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bin, "apt-cache")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.trustedManager("apt", path); err != nil {
		t.Fatal(err)
	}
	p.Root = prefix
	if err := p.trustedManager("apt", path); err == nil {
		t.Fatal("workspace-controlled manager trusted")
	}
	p.Root = t.TempDir()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", path); err != nil {
		t.Fatal(err)
	}
	if err := p.trustedManager("apt", path); err == nil {
		t.Fatal("manager symlink escaping prefix trusted")
	}
	t.Setenv("PATH", bin)
	if got := p.discoverManager("apt"); got.Executable == path {
		t.Fatal("discovery used shadowing PATH executable")
	}
}

func TestPackagesNativeInspectAndAPT(t *testing.T) {
	p := &Packages{Root: t.TempDir(), TempDir: t.TempDir()}
	res, err := p.Run(t.Context(), json.RawMessage(`{"action":"inspect"}`))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		OS       string           `json:"os"`
		Managers []packageManager `json:"managers"`
	}
	if err := json.Unmarshal([]byte(res.Output), &report); err != nil || report.OS != runtime.GOOS || len(report.Managers) != 3 {
		t.Fatalf("inspection: %s %v", res.Output, err)
	}
	if runtime.GOOS != "linux" {
		return
	}
	if _, err := os.Stat("/usr/bin/apt-cache"); err != nil {
		t.Skip("APT is not installed")
	}
	if !report.Managers[1].Available || report.Managers[1].Version == "" || !reflect.DeepEqual(report.Managers[1].Operations, []string{"search", "info"}) {
		t.Fatalf("APT discovery: %+v", report.Managers[1])
	}
	wantVersion, err := exec.Command("/usr/bin/dpkg-query", "-W", "-f=${Version}", "bash").Output()
	if err != nil {
		t.Fatal(err)
	}
	res, err = p.Run(t.Context(), json.RawMessage(`{"action":"info","manager":"apt","package":"bash"}`))
	if err != nil || !strings.Contains(res.Output, "Installed: "+string(wantVersion)) {
		t.Fatalf("installed version: %s %v", res.Output, err)
	}
	res, err = p.Run(t.Context(), json.RawMessage(`{"action":"search","manager":"apt","package":"bash"}`))
	if err != nil || !strings.Contains(res.Output, "bash - ") {
		t.Fatalf("search: %s %v", res.Output, err)
	}
	if _, err := p.Run(t.Context(), json.RawMessage(`{"action":"install","manager":"apt","package":"bash"}`)); err == nil || !strings.Contains(err.Error(), "administrator access is not granted") {
		t.Fatalf("administrator installation was not blocked: %v", err)
	}
}

func TestInstallFormulaGenericWorkflow(t *testing.T) {
	const metadata = `{"formulae":[{"full_name":"ripgrep","tap":"homebrew/core","revision":2,"versions":{"stable":"14.1.1"}}]}`
	const brew = "/trusted/bin/brew"
	base := []packageReply{{out: "pcre2 10.45\npython@3.12 3.12.9\n"}, {out: metadata}, {out: "homebrew/core/pcre2\nlibuv\n"}, {}, {}, {out: "ripgrep 14.1.1_2\n"}}
	fake := &packageFake{t: t, replies: base}
	res, err := installFormula(t.Context(), brew, "ripgrep", "14.1.1_2", fake.run)
	if err != nil || !strings.Contains(res.Output, "ripgrep 14.1.1_2 installed") {
		t.Fatalf("install: %+v %v", res, err)
	}
	want := []packageCall{
		{brew, []string{"list", "--formula", "--versions"}},
		{brew, []string{"info", "--json=v2", "--formula", "homebrew/core/ripgrep"}},
		{brew, []string{"deps", "--formula", "--topological", "--full-name", "homebrew/core/ripgrep"}},
		{brew, []string{"install", "--formula", "--force-bottle", "--ignore-dependencies", "--no-ask", "homebrew/core/libuv"}},
		{brew, []string{"install", "--formula", "--force-bottle", "--ignore-dependencies", "--no-ask", "homebrew/core/ripgrep"}},
		{brew, []string{"list", "--formula", "--versions", "homebrew/core/ripgrep"}},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("commands: %#v, want %#v", fake.calls, want)
	}
	for _, tc := range []struct {
		name, version string
		replies       []packageReply
		calls         int
		errorText     string
	}{
		{"installed no-op", "", []packageReply{{out: "ripgrep 13.0.0"}}, 1, ""},
		{"installed version mismatch", "14.1.1_2", []packageReply{{out: "ripgrep 13.0.0"}}, 1, "automatic upgrades/downgrades"},
		{"unavailable version", "14.1.1", base[:2], 2, "not the available stable"},
		{"malformed metadata", "", []packageReply{{}, {out: "not JSON"}}, 2, "invalid formula metadata"},
		{"foreign formula", "", []packageReply{{}, {out: strings.ReplaceAll(metadata, "homebrew/core", "other/tap")}}, 2, "canonical official"},
		{"alias", "", []packageReply{{}, {out: strings.ReplaceAll(metadata, `"ripgrep"`, `"ripgrep-new"`)}}, 2, "canonical official"},
		{"foreign dependency", "", []packageReply{{}, {out: metadata}, {out: "libuv\nother/tap/bad"}}, 3, "unsupported dependency"},
		{"dependency failure", "", []packageReply{base[0], base[1], base[2], {err: errors.New("no bottle")}}, 4, "earlier dependencies may remain"},
		{"verification mismatch", "", append(append([]packageReply{}, base[:5]...), packageReply{out: "ripgrep 13.0.0"}), 6, "verification failed"},
		{"inventory failure", "", []packageReply{{err: errors.New("offline")}}, 1, "cannot inspect installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &packageFake{t: t, replies: tc.replies}
			_, err := installFormula(t.Context(), brew, "ripgrep", tc.version, fake.run)
			if tc.errorText == "" && err != nil || tc.errorText != "" && (err == nil || !strings.Contains(err.Error(), tc.errorText)) {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(fake.calls) != tc.calls {
				t.Fatalf("executed %d commands, want %d", len(fake.calls), tc.calls)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fake = &packageFake{t: t}
	if _, err := installFormula(ctx, brew, "ripgrep", "", fake.run); !errors.Is(err, context.Canceled) || len(fake.calls) != 0 {
		t.Fatal("cancelled installation executed", err)
	}
}

type packageCall struct {
	program string
	args    []string
}
type packageReply struct {
	out string
	err error
}
type packageFake struct {
	t       *testing.T
	calls   []packageCall
	replies []packageReply
}

func (f *packageFake) run(program string, args ...string) (string, error) {
	f.t.Helper()
	f.calls = append(f.calls, packageCall{program, append([]string(nil), args...)})
	if len(f.calls) > len(f.replies) {
		f.t.Fatalf("unexpected command: %s %q", program, args)
	}
	r := f.replies[len(f.calls)-1]
	return r.out, r.err
}
