package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// sandboxRun never exposes live workspace inodes for subprocess writes. Close
// only discards resources; publication must be requested explicitly after Run.
type sandboxRun struct {
	cmd     *exec.Cmd
	temp    string
	trees   []*sandboxTree
	release func()
}

func sandboxRoots(workspace, scratch string) ([]string, error) {
	roots := []string{}
	for _, dir := range []string{workspace, scratch} {
		if dir == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(real)
		if err != nil || !info.IsDir() || real == "/" {
			return nil, fmt.Errorf("invalid sandbox write root %q", dir)
		}
		roots = append(roots, real)
	}
	if workspace == "" {
		return nil, fmt.Errorf("sandbox requires a workspace")
	}
	if len(roots) == 2 && (within(roots[0], roots[1]) || within(roots[1], roots[0])) {
		return nil, fmt.Errorf("workspace and session scratch must not contain one another")
	}
	return roots, nil
}

func sandboxCommand(ctx context.Context, workspace, scratch, workdir, program string, args ...string) (_ *sandboxRun, err error) {
	roots, err := sandboxRoots(workspace, scratch)
	if err != nil {
		return nil, err
	}
	s := &sandboxRun{}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	// Use the OS temporary directory, not a possibly workspace-local TMPDIR.
	// Never put private copies or trusted capture beneath a writable root.
	s.temp, err = os.MkdirTemp("/tmp", "arkex-execution-")
	if err != nil {
		return nil, err
	}
	s.temp, err = filepath.EvalSymlinks(s.temp)
	if err != nil {
		return nil, err
	}
	for _, root := range roots {
		if within(root, s.temp) {
			return nil, fmt.Errorf("private execution directory overlaps a write root; the system temporary directory cannot be a workspace or scratch root")
		}
	}
	for i, path := range roots {
		tree := &sandboxTree{path: path, execution: filepath.Join(s.temp, fmt.Sprint(i))}
		s.trees = append(s.trees, tree)
		tree.host, err = os.OpenRoot(path)
		if err != nil {
			return nil, err
		}
		tree.before, err = snapshotTree(ctx, tree.host, tree.execution, nil)
		if err != nil {
			return nil, fmt.Errorf("prepare private execution: %w", err)
		}
	}
	s.cmd, s.release, err = platformSandbox(ctx, s.trees, s.runtimePath(workdir), s.runtimePath(program), args)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Linux can mount copies at the logical paths. Seatbelt cannot remap paths:
// map structured tool paths/environment, but never rewrite opaque shell code.
func (s *sandboxRun) runtimePath(path string) string {
	if runtime.GOOS != "darwin" {
		return path
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	for _, tree := range s.trees {
		if within(tree.path, path) {
			rel, _ := filepath.Rel(tree.path, path)
			return filepath.Join(tree.execution, rel)
		}
	}
	return path
}

func (s *sandboxRun) environment(env []string) []string {
	env = append([]string(nil), env...)
	for i, value := range env {
		key, path, ok := strings.Cut(value, "=")
		if ok && filepath.IsAbs(path) {
			env[i] = key + "=" + s.runtimePath(path)
		}
	}
	return env
}

func (s *sandboxRun) Close() {
	if s.release != nil {
		s.release()
	}
	for _, tree := range s.trees {
		if tree.host != nil {
			_ = tree.host.Close()
		}
	}
	if s.temp != "" {
		// Commands can leave read-only directories. Restore search/write access
		// only inside our owned tree; os.Root blocks symlink escapes during cleanup.
		if root, err := os.OpenRoot(s.temp); err == nil {
			_ = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() {
					return root.Chmod(path, 0o700)
				}
				return nil
			})
			_ = root.Close()
		}
		_ = os.RemoveAll(s.temp)
	}
}

// CheckSandbox probes the real backend, including kernel support. Missing or
// disabled sandbox support must be reported before an agent starts using tools.
func CheckSandbox(ctx context.Context, workspace, scratch string) error {
	// Probe with empty private roots, not a copy of the user's entire project.
	if _, err := sandboxRoots(workspace, scratch); err != nil {
		return err
	}
	temp, err := os.MkdirTemp("/tmp", "arkex-sandbox-probe-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(temp) }()
	workspaceProbe := filepath.Join(temp, "workspace")
	if err := os.Mkdir(workspaceProbe, 0o700); err != nil {
		return err
	}
	source := filepath.Join(temp, "outside")
	if err := os.WriteFile(source, []byte("probe"), 0o600); err != nil {
		return err
	}
	// In particular, fail closed if an OS profile cannot enforce file-link
	// denial. Copies alone would not prevent introducing a new outside alias.
	probe := `test -x /bin/ln && test "$(cat "$1")" = probe || exit 40
printf probe > writable || exit 41
if /bin/ln "$1" outside-link; then exit 42; fi
if /bin/ln writable inside-link; then exit 43; fi`
	run, err := sandboxCommand(ctx, workspaceProbe, "", workspaceProbe, "/bin/sh", "-c", probe, "probe", source)
	if err != nil {
		return err
	}
	defer run.Close()
	if output, err := run.cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox unavailable (execution blocked): %w: %s", err, output)
	}
	return nil
}
