package tools

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Clone from the pinned source FD into a new private inode. Never precreate or
// overwrite the destination. A nil file requests the ordinary-copy fallback.
func cloneSandboxFile(source *os.File, dest *os.Root, name string) (*os.File, error) {
	var info unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &info); err != nil {
		return nil, err
	}
	// Cloning preserves BSD flags, including immutable/append-only flags.
	// Ordinary copying keeps staging editable and cleanable without touching
	// the original flags or requiring administrator privileges.
	if info.Flags != 0 {
		return nil, nil
	}
	parent, err := filesystemParent(dest, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	err = unix.Fclonefileat(int(source.Fd()), int(parent.Fd()), filepath.Base(name), unix.CLONE_NOOWNERCOPY)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOSYS) {
		return nil, nil
	}
	if err != nil {
		return nil, err // EEXIST, permissions, I/O and space failures are not fallbacks.
	}
	// Hash the private clone, not the possibly changing source. A read-only
	// descriptor also permits staging source files whose mode is read-only.
	return dest.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}
