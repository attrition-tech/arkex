package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Packages separates model-selected dependencies from runtime-owned commands.
// Only supported installation operations may leave the ordinary write sandbox.
type Packages struct{ Root, TempDir string }

func (*Packages) Name() string { return "packages" }
func (*Packages) Description() string {
	return "Inspect the OS and supported installed package managers, search packages, read package/version information, or install a necessary prerequisite. Inspect first and choose based on project requirements, existing installations, OS and scope. Homebrew supports official formula bottles and missing runtime dependencies; apt/dnf provide cached read-only queries, not administrator installation. Project-local dependencies should use the project's existing manager through bash within the workspace/scratch boundary. No sudo, manager bootstrap, custom repositories, arbitrary commands/flags, removals, source builds, or unrelated upgrades. Package-manager software and official installation scripts are trusted host code."
}
func (*Packages) Schema() map[string]any {
	return schema(map[string]any{
		"action":  map[string]any{"type": "string", "enum": []string{"inspect", "search", "info", "install"}},
		"manager": map[string]any{"type": "string", "enum": []string{"brew", "apt", "dnf"}},
		"package": prop("string", "Package name, or a name fragment for search. No URLs, paths, taps, globs or flags. Required except for inspect."),
		"version": prop("string", "Optional exact Homebrew package version for install, including revision suffix when present. Must be installed already or match the available stable bottle; no automatic upgrade/downgrade or version-range interpretation. Use info to resolve versions and versioned formula names."),
	}, "action")
}

type packageInput struct {
	Action  string `json:"action"`
	Manager string `json:"manager,omitempty"`
	Package string `json:"package,omitempty"`
	Version string `json:"version,omitempty"`
}

var packageName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._@-]*$`)
var packageVersion = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._:~-]*$`)

func (in packageInput) validate() error {
	if in.Action == "inspect" {
		if in.Manager != "" || in.Package != "" || in.Version != "" {
			return errors.New("inspect takes no manager, package or version")
		}
		return nil
	}
	if in.Action != "search" && in.Action != "info" && in.Action != "install" {
		return errors.New("action must be inspect, search, info or install")
	}
	if in.Manager != "brew" && in.Manager != "apt" && in.Manager != "dnf" {
		return errors.New("unsupported manager; inspect available managers first")
	}
	if !packageName.MatchString(in.Package) || len(in.Package) > 200 {
		return errors.New("package must be a plain package name or search fragment, not a path, URL, flag or expression")
	}
	if in.Manager != "brew" && strings.Contains(in.Package, "@") {
		return errors.New("versioned @ formula names are supported only for Homebrew")
	}
	if in.Version != "" && (in.Action != "install" || in.Manager != "brew" || !packageVersion.MatchString(in.Version) || len(in.Version) > 100) {
		return errors.New("version is supported only as an exact Homebrew install version")
	}
	return nil
}

type packageManager struct {
	Name       string   `json:"name"`
	Executable string   `json:"executable,omitempty"`
	Available  bool     `json:"available"`
	Version    string   `json:"version,omitempty"`
	Operations []string `json:"operations"`
	Scope      string   `json:"scope"`
	Limit      string   `json:"limitation,omitempty"`
}

type packageRunner func(program string, args ...string) (string, error)

func (t *Packages) Run(ctx context.Context, input json.RawMessage) (Result, error) {
	var in packageInput
	if err := decode(input, &in); err != nil {
		return Result{}, err
	}
	if err := in.validate(); err != nil {
		return Result{}, err
	}
	if t.TempDir == "" {
		return Result{}, errors.New("packages requires the active session scratch directory")
	}
	if err := CheckSandbox(ctx, t.Root, t.TempDir); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, maxBashTimeout)
	defer cancel()
	if in.Action == "inspect" {
		managers := []packageManager{}
		for _, name := range []string{"brew", "apt", "dnf"} {
			m := t.discoverManager(name)
			if m.Available {
				run, cleanup, err := t.packageRunner(ctx, m, false)
				if err == nil {
					m.Version, err = run(m.Executable, "--version")
					cleanup()
				}
				if err != nil {
					m.Available, m.Operations = false, nil
					m.Limit = "manager probe failed: " + err.Error()
				}
				m.Version = strings.TrimSpace(m.Version)
			}
			managers = append(managers, m)
		}
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		report := struct {
			OS           string            `json:"os"`
			Architecture string            `json:"architecture"`
			Distribution map[string]string `json:"distribution"`
			Managers     []packageManager  `json:"managers"`
		}{runtime.GOOS, runtime.GOARCH, osDistribution(), managers}
		data, err := json.MarshalIndent(report, "", "  ")
		return Result{Summary: "Package manager capabilities", Output: string(data)}, err
	}
	m := t.discoverManager(in.Manager)
	if !m.Available {
		return Result{}, fmt.Errorf("%s unavailable: %s", m.Name, m.Limit)
	}
	if in.Action == "install" && m.Name != "brew" {
		return Result{}, errors.New("system installation through apt/dnf is not supported: administrator access is not granted, and this tool never invokes sudo; inspection remains available")
	}
	run, cleanup, err := t.packageRunner(ctx, m, in.Action == "install")
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	if in.Action == "install" {
		return installFormula(ctx, m.Executable, in.Package, in.Version, run)
	}
	return queryPackage(m, in, run)
}

// Discover only runtime-owned locations. A same-named executable found through
// a project-controlled PATH is not trusted to perform host installations.
func (t *Packages) discoverManager(name string) packageManager {
	m := packageManager{Name: name, Scope: "system", Operations: []string{}}
	var paths []string
	switch name {
	case "brew":
		m.Scope = "Homebrew prefix"
		paths = []string{"/home/linuxbrew/.linuxbrew/bin/brew"}
		if runtime.GOOS == "darwin" {
			paths = []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"}
		}
	case "apt":
		if runtime.GOOS == "linux" {
			paths = []string{"/usr/bin/apt-cache"}
		}
	case "dnf":
		if runtime.GOOS == "linux" {
			paths = []string{"/usr/bin/dnf"}
		}
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := t.trustedManager(name, path); err != nil {
			m.Limit = err.Error()
			continue
		}
		m.Available, m.Executable = true, path
		m.Operations = []string{"search", "info"}
		m.Limit = "Cached metadata only; no refresh or system installation under the no-administrator-access policy."
		if name == "brew" {
			m.Operations = append(m.Operations, "install")
			m.Limit = "Official core formula bottles only. No casks, taps, source builds or upgrades of installed formulas."
		}
		return m
	}
	if m.Limit == "" {
		m.Limit = "Not installed at a supported location on this OS; automatic bootstrap and custom executable paths are not supported."
	}
	return m
}

func (t *Packages) trustedManager(name, path string) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return errors.New("package manager is not an executable regular file")
	}
	prefix, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		return err
	}
	if !within(prefix, real) {
		return errors.New("package manager resolves outside its supported installation prefix")
	}
	for _, root := range []string{t.Root, t.TempDir} {
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		if within(root, prefix) || within(prefix, root) {
			return errors.New("package manager installation overlaps a session write root")
		}
	}
	if name == "brew" {
		// brew.env loads even with a clean environment and can override it.
		for _, config := range []string{"/etc/homebrew/brew.env", filepath.Join(prefix, "etc/homebrew/brew.env")} {
			if _, err := os.Lstat(config); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("bounded installation requires no Homebrew environment file at %s", config)
			}
		}
	}
	return nil
}

// Queries run inside the ordinary sandbox. Only the fixed Homebrew install
// sequence receives a host runner, with private caches beyond model write access.
func (t *Packages) packageRunner(ctx context.Context, m packageManager, host bool) (packageRunner, func(), error) {
	parent := t.TempDir
	if host {
		parent = ""
	}
	temp, err := os.MkdirTemp(parent, "arkex-packages-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(temp) }
	real, err := filepath.EvalSymlinks(temp)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if host {
		for _, root := range []string{t.Root, t.TempDir} {
			root, err = filepath.EvalSymlinks(root)
			if err != nil || within(root, real) {
				cleanup()
				return nil, nil, errors.New("installer temporary directory overlaps or cannot resolve a session write root")
			}
		}
	}
	env := packageEnvironment(m, real)
	run := func(program string, args ...string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var cmd *exec.Cmd
		var sandbox *sandboxRun
		cache := real
		if host {
			cmd = exec.CommandContext(ctx, program)
			cmd.Dir, cmd.Env = real, env
		} else {
			var err error
			sandbox, err = sandboxCommand(ctx, t.Root, t.TempDir, real, program)
			if err != nil {
				return "", err
			}
			defer sandbox.Close()
			cmd = sandbox.cmd
			cmd.Env = sandbox.environment(env)
			cache = sandbox.runtimePath(real)
		}
		if m.Name == "dnf" {
			// This config spelling works in DNF4 and DNF5.
			args = append([]string{"--setopt=plugins=False", "--setopt=logdir=" + cache, "--cacheonly"}, args...)
		}
		cmd.Args = append(cmd.Args, args...)
		setProcessGroup(cmd)
		cmd.WaitDelay = 2 * time.Second
		out, diagnostics := &limitedOutput{limit: maxBashOutput}, &limitedOutput{limit: maxBashOutput}
		cmd.Stdout, cmd.Stderr = out, diagnostics
		err := cmd.Run()
		if cmd.Process != nil {
			_ = cmd.Cancel()
		}
		if sandbox != nil && ctx.Err() == nil && cmd.ProcessState != nil {
			err = errors.Join(err, sandbox.Publish(ctx))
		}
		if err != nil {
			return out.String(), fmt.Errorf("%w: %s", err, diagnostics.String())
		}
		if out.total > maxBashOutput {
			return out.String(), errors.New("package manager output exceeded the limit; narrow the query (no partial installation plan is executed)")
		}
		return out.String(), nil
	}
	return run, cleanup, nil
}

func packageEnvironment(m packageManager, temp string) []string {
	env := []string{"HOME=" + temp, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR=" + temp, "LC_ALL=C", "XDG_CACHE_HOME=" + temp, "XDG_CONFIG_HOME=" + temp}
	if m.Name == "dnf" {
		env = append(env, "DNF5_PLUGINS_DIR=")
	}
	if m.Name == "brew" {
		env[1] = "PATH=" + filepath.Dir(m.Executable) + ":/usr/bin:/bin:/usr/sbin:/sbin"
		env = append(env, "HOMEBREW_CACHE="+filepath.Join(temp, "cache"), "HOMEBREW_LOGS="+filepath.Join(temp, "logs"), "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ANALYTICS=1", "HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_INSTALL_CLEANUP=1", "HOMEBREW_NO_INSTALL_UPGRADE=1", "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1", "NONINTERACTIVE=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	}
	return env
}

func osDistribution() map[string]string {
	out := map[string]string{}
	if runtime.GOOS != "linux" {
		return out
	}
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return out
	}
	// Parse identity fields only; never source the file as shell code.
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && (key == "ID" || key == "ID_LIKE" || key == "VERSION_ID" || key == "PRETTY_NAME") {
			out[key] = strings.Trim(value, `"'`)
		}
	}
	return out
}
