package tools

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

func platformSandbox(ctx context.Context, trees []*sandboxTree, workdir, program string, argv []string) (*exec.Cmd, func(), error) {
	// Do not resolve the security boundary through a project-controlled PATH.
	const bwrap = "/usr/bin/bwrap"
	if _, err := os.Stat(bwrap); err != nil {
		return nil, nil, fmt.Errorf("shell tools on Linux require Bubblewrap at %s; install your distribution's bubblewrap package: %w", bwrap, err)
	}
	filter, err := sandboxFilter()
	if err != nil {
		return nil, nil, err
	}
	args := []string{"--die-with-parent", "--new-session", "--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup", "--cap-drop", "ALL", "--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev"}
	for _, tree := range trees {
		args = append(args, "--bind", tree.execution, tree.path)
	}
	args = append(args, "--chdir", workdir, "--seccomp", "3", "--", program)
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, bwrap, args...)
	cmd.ExtraFiles = []*os.File{filter}
	return cmd, func() { _ = filter.Close() }, nil
}

// A read-only bind does not prevent talking to a host Unix socket. Block
// those sockets and process/kernel escape interfaces as well. TCP/IP remains
// available; this is local filesystem confinement, not a network firewall.
func sandboxFilter() (*os.File, error) {
	arch := uint32(unix.AUDIT_ARCH_X86_64)
	if runtime.GOARCH == "arm64" {
		arch = unix.AUDIT_ARCH_AARCH64
	} else if runtime.GOARCH != "amd64" {
		return nil, fmt.Errorf("unsupported sandbox architecture %s", runtime.GOARCH)
	}
	load := func(offset uint32) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: offset}
	}
	equal := func(value uint32, skip uint8) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: value, Jt: skip}
	}
	ret := func(value uint32) unix.SockFilter { return unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: value} }
	deny := ret(unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM))
	filters := []unix.SockFilter{load(4), equal(arch, 1), ret(unix.SECCOMP_RET_KILL_PROCESS), load(0)}
	// Reject the x32 ABI, which shares the amd64 audit architecture.
	filters = append(filters, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x40000000, Jf: 1}, deny)
	calls := []uint32{unix.SYS_PTRACE, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PROCESS_VM_READV, unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_SETNS, unix.SYS_UNSHARE, unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_IO_URING_SETUP, unix.SYS_LINKAT}
	if runtime.GOARCH == "amd64" {
		// Legacy link(2) is syscall 86 on amd64; arm64 only has linkat.
		calls = append(calls, 86)
	}
	for _, call := range calls {
		filters = append(filters, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: call, Jf: 1}, deny)
	}
	// Stream pairs stay connected to their original peer and support libuv
	// child stdio. Datagram pairs can reconnect to host sockets: permit only
	// exact SOCK_STREAM (plus the two supported flags), never a bit test.
	filters = append(filters,
		equal(unix.SYS_SOCKET, 2),
		equal(unix.SYS_SOCKETPAIR, 1),
		ret(unix.SECCOMP_RET_ALLOW),
		load(16), // args[0]: family
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.AF_UNIX, Jf: 6},
		load(0),
		equal(unix.SYS_SOCKET, 3), // socket(AF_UNIX) always denied
		load(24),                  // args[1]: type
		unix.SockFilter{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: ^uint32(unix.SOCK_CLOEXEC | unix.SOCK_NONBLOCK)},
		equal(unix.SOCK_STREAM, 1),
		deny,
		ret(unix.SECCOMP_RET_ALLOW),
	)
	fd, err := unix.MemfdCreate("arkex-seccomp", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "arkex-seccomp")
	if err := binary.Write(f, binary.LittleEndian, filters); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
