package tools

import (
	"os"

	"golang.org/x/sys/unix"
)

// Never emulate no-replace with check-then-rename: concurrent WIP must survive.
func filesystemRenameExclusive(oldDir *os.File, oldName string, newDir *os.File, newName string) error {
	return unix.Renameat2(int(oldDir.Fd()), oldName, int(newDir.Fd()), newName, unix.RENAME_NOREPLACE)
}
