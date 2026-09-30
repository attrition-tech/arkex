package tools

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestSandboxSocketpairChild(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-3] != "arkex-ipc-probe" {
		return
	}
	kind, err := strconv.Atoi(os.Args[len(os.Args)-2])
	if err != nil {
		t.Fatal(err)
	}
	host := &unix.SockaddrUnix{Name: os.Args[len(os.Args)-1]}
	for _, flags := range []int{0, unix.SOCK_CLOEXEC, unix.SOCK_NONBLOCK, unix.SOCK_CLOEXEC | unix.SOCK_NONBLOCK} {
		fds, err := unix.Socketpair(unix.AF_UNIX, kind|flags, 0)
		if err != nil {
			t.Fatal("private connection-oriented pair blocked:", err)
		}
		if _, err := unix.Write(fds[0], []byte("ok")); err != nil {
			t.Fatal(err)
		}
		var data [2]byte
		if n, err := unix.Read(fds[1], data[:]); err != nil || n != 2 || string(data[:]) != "ok" {
			t.Fatalf("private pipe failed: %q %v", data, err)
		}
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	}
	for _, kind := range []int{unix.SOCK_STREAM, unix.SOCK_DGRAM, unix.SOCK_SEQPACKET, unix.SOCK_RAW} {
		fd, err := unix.Socket(unix.AF_UNIX, kind, 0)
		if !errors.Is(err, unix.EPERM) {
			_ = unix.Close(fd)
			t.Fatalf("host socket type %d not blocked by filter: %v", kind, err)
		}
	}
	for _, kind := range []int{unix.SOCK_DGRAM, unix.SOCK_RAW, unix.SOCK_STREAM | (1 << 20), unix.SOCK_SEQPACKET | (1 << 20)} {
		fds, err := unix.Socketpair(unix.AF_UNIX, kind|unix.SOCK_CLOEXEC, 0)
		if !errors.Is(err, unix.EPERM) {
			_ = unix.Close(fds[0])
			_ = unix.Close(fds[1])
			t.Fatalf("unsafe pair type %d not blocked by filter: %v", kind, err)
		}
	}
	for _, action := range []string{"connect", "disconnect", "shutdown", "peer-close"} {
		fds, err := unix.Socketpair(unix.AF_UNIX, kind|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		switch action {
		case "disconnect":
			addr := unix.RawSockaddr{Family: unix.AF_UNSPEC}
			_, _, errno := unix.Syscall(unix.SYS_CONNECT, uintptr(fds[0]), uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
			if errno == 0 {
				t.Fatal("connection-oriented pair disconnected")
			}
		case "shutdown":
			if err := unix.Shutdown(fds[0], unix.SHUT_RDWR); err != nil {
				t.Fatal(err)
			}
		case "peer-close":
			_ = unix.Close(fds[1])
		}
		if err := unix.Connect(fds[0], host); err == nil {
			t.Fatalf("pair type %d retargeted after %s", kind, action)
		}
		if err := unix.Sendto(fds[0], []byte("escaped"), unix.MSG_NOSIGNAL, host); err == nil {
			// Linux seqpacket ignores sendto's destination on a connected
			// socket. Prove delivery stayed at the private peer, rather than
			// mistaking a successful send for a successful host connection.
			var data [16]byte
			n, err := unix.Read(fds[1], data[:])
			if err != nil || string(data[:n]) != "escaped" {
				t.Fatalf("pair type %d delivered outside its peer after %s: %v", kind, action, err)
			}
		}
		_ = unix.Close(fds[0])
		if action != "peer-close" {
			_ = unix.Close(fds[1])
		}
	}
}

func TestSandboxSocketpairsCannotReachHost(t *testing.T) {
	for _, kind := range []int{unix.SOCK_STREAM, unix.SOCK_SEQPACKET} {
		for _, abstract := range []bool{false, true} {
			path := filepath.Join(t.TempDir(), "host.sock")
			if abstract {
				path = "@" + path
			}
			network := "unix"
			if kind == unix.SOCK_SEQPACKET {
				network = "unixpacket"
			}
			listener, err := net.ListenUnix(network, &net.UnixAddr{Name: path, Net: network})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := strconv.Quote(exe) + " -test.run=^TestSandboxSocketpairChild$ -- arkex-ipc-probe " + strconv.Itoa(kind) + " " + strconv.Quote(path)
			res, err := runBash(t, &Bash{Dir: t.TempDir(), Shell: "/bin/sh"}, map[string]any{"command": command, "timeout_ms": 10000})
			if err != nil {
				t.Fatalf("IPC probe failed (abstract=%v): %v: %s", abstract, err, res.Output)
			}
			if err := listener.SetDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			conn, err := listener.Accept()
			if err == nil {
				_ = conn.Close()
				t.Fatal("sandbox reached host Unix listener")
			}
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal("unexpected listener failure:", err)
			}
		}
	}
}
