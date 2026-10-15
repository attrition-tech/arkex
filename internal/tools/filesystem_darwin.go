package tools

import (
	"os"

	"golang.org/x/sys/unix"
)

// Use libSystem through x/sys (also with cgo disabled), not numeric syscalls.
func filesystemRenameExclusive(oldDir *os.File, oldName string, newDir *os.File, newName string) error {
	return unix.RenameatxNp(int(oldDir.Fd()), oldName, int(newDir.Fd()), newName, unix.RENAME_EXCL)
}
