package tools

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// This runs as a real child of the confined shell, not as a mock policy.
func TestSandboxChild(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-3] != "arkex-sandbox-probe" {
		return
	}
	op, path := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	var err error
	switch op {
	case "write":
		err = os.WriteFile(path, []byte("child"), 0o600)
	case "socket":
		var conn net.Conn
		conn, err = net.Dial("unix", path)
		if err == nil {
			_ = conn.Close()
		}
	}
	if err != nil {
		os.Exit(7)
	}
	os.Exit(0)
}

func TestSandboxRealWriteBoundary(t *testing.T) {
	root, scratch, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := CheckSandbox(t.Context(), root, scratch); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "fixture"), []byte("outside-readable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	b := &Bash{Dir: root, TempDir: scratch, Shell: "/bin/sh"}
	res, err := runBash(t, b, map[string]any{"command": "cat fixture", "workdir": outside})
	if err != nil || res.Output != "outside-readable\n" {
		t.Fatalf("outside read: %s %v", res.Output, err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := strconv.Quote(exe) + " -test.run=^TestSandboxChild$ -- arkex-sandbox-probe write "
	for _, tc := range []struct {
		name, command, path string
		allowed             bool
	}{
		{"workspace child", child + `"$PWD/child"`, filepath.Join(root, "child"), true},
		{"scratch child", child + `"$TMPDIR/child"`, filepath.Join(scratch, "child"), true},
		{"absolute workspace", child + strconv.Quote(filepath.Join(root, "absolute-child")), filepath.Join(root, "absolute-child"), runtime.GOOS == "linux"},
		{"absolute scratch", child + strconv.Quote(filepath.Join(scratch, "absolute-child")), filepath.Join(scratch, "absolute-child"), runtime.GOOS == "linux"},
		{"outside child", child + strconv.Quote(filepath.Join(outside, "child")), filepath.Join(outside, "child"), false},
		{"symlink", "echo escaped > escape/new", filepath.Join(outside, "new"), false},
		{"computed path", "dest=" + strconv.Quote(outside) + "; echo escaped > \"$dest/computed\"", filepath.Join(outside, "computed"), false},
		{"parent of scratch", "echo escaped > \"$TMPDIR/../arkex-scratch-escape\"", filepath.Join(scratch, "../arkex-scratch-escape"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := runBash(t, b, map[string]any{"command": tc.command})
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v: %v %s", tc.allowed, err, res.Output)
			}
			data, statErr := os.ReadFile(tc.path)
			if tc.allowed && (statErr != nil || string(data) != "child") {
				t.Fatalf("wrong child write: %s %v", data, statErr)
			}
			if !tc.allowed && !os.IsNotExist(statErr) {
				t.Fatalf("outside write happened: %s %v", data, statErr)
			}
		})
	}
	// Replacing the active scratch must revoke the former scratch tree.
	b.TempDir = outside
	res, err = runBash(t, b, map[string]any{"command": "echo changed > " + strconv.Quote(filepath.Join(scratch, "child"))})
	if err == nil {
		t.Fatalf("old scratch writable: %s", res.Output)
	}
	data, _ := os.ReadFile(filepath.Join(scratch, "child"))
	if string(data) != "child" {
		t.Fatal("previous scratch was changed")
	}
	// Starting outside does not grant write access there.
	if res, err := runBash(t, b, map[string]any{"workdir": filepath.Dir(root), "command": "echo escaped > forbidden"}); err == nil {
		t.Fatalf("workdir expanded writes: %s", res.Output)
	}
}

func TestSandboxBlocksHostUnixSocket(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "host.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	exe, _ := os.Executable()
	res, err := runBash(t, &Bash{Dir: root, Shell: "/bin/sh"}, map[string]any{"command": strconv.Quote(exe) + " -test.run=^TestSandboxChild$ -- arkex-sandbox-probe socket " + strconv.Quote(path)})
	if err == nil {
		t.Fatalf("host IPC reachable: %s", res.Output)
	}
}

func TestSandboxSetupFailureNeverExecutes(t *testing.T) {
	root := t.TempDir()
	res, err := runBash(t, &Bash{Dir: root, TempDir: filepath.Join(root, "missing"), Shell: "/bin/sh"}, map[string]any{"command": "touch must-not-run"})
	if err == nil {
		t.Fatalf("invalid sandbox accepted: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(root, "must-not-run")); !os.IsNotExist(err) {
		t.Fatal("unrestricted fallback executed")
	}
}

func TestSandboxWriteRootsCannotBeReplaced(t *testing.T) {
	workspace, scratch := t.TempDir(), t.TempDir()
	b := &Bash{Dir: workspace, TempDir: scratch, Shell: "/bin/sh"}
	for _, root := range []string{workspace, scratch} {
		res, err := runBash(t, b, map[string]any{"command": "rmdir " + strconv.Quote(root)})
		if err == nil {
			t.Fatalf("write root can be removed and replaced: %s", res.Output)
		}
		if st, err := os.Lstat(root); err != nil || !st.IsDir() {
			t.Fatalf("write root changed: %s: %v", root, err)
		}
	}
}

func TestSandboxNodeChildPipes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is not installed")
	}
	b := &Bash{Dir: t.TempDir(), TempDir: t.TempDir(), Shell: "/bin/sh"}
	command := strconv.Quote(node) + ` -e 'process.stdout.write(require("child_process").execFileSync("/bin/sh", ["-c", "printf node-child-ok"]))'`
	res, err := runBash(t, b, map[string]any{"command": command})
	if err != nil || strings.TrimSpace(res.Output) != "node-child-ok" {
		t.Fatalf("Node child stdio failed: %v: %s", err, res.Output)
	}
}

func TestFileWriteBoundary(t *testing.T) {
	root, scratch, outside := t.TempDir(), t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "keep")
	if err := os.WriteFile(target, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []Tool{&Write{Root: root, TempDir: scratch}, &Edit{Root: root, TempDir: scratch}} {
		for _, path := range []string{target, filepath.Join(root, "escape/keep"), root + "-sibling/file"} {
			args := map[string]string{"path": path, "content": "after"}
			if tool.Name() == "edit" {
				args = map[string]string{"path": path, "old_string": "before", "new_string": "after"}
			}
			raw, _ := json.Marshal(args)
			if _, err := tool.Run(t.Context(), raw); err == nil {
				t.Fatalf("%s escaped: %s", tool.Name(), path)
			}
		}
	}
	data, _ := os.ReadFile(target)
	if string(data) != "before" {
		t.Fatal("outside file changed")
	}
	// An inside hard link must be replaced, not truncate an outside inode.
	if err := os.Link(target, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Edit{Root: root}).Run(t.Context(), json.RawMessage(`{"path":"hardlink","old_string":"before","new_string":"after"}`)); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(target)
	if string(data) != "before" {
		t.Fatal("hardlink changed outside inode")
	}
	read, err := (&Read{Root: root}).Run(t.Context(), json.RawMessage(`{"path":`+strconv.Quote(target)+`}`))
	if err != nil || !strings.Contains(read.Output, "before") {
		t.Fatal("outside read blocked", err)
	}
}
