package tools

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(rel)
}

// os.Root resolves every component relative to an open directory handle, so
// a concurrent symlink substitution cannot turn a checked path into an escape.
func writeRoot(workspace, scratch, path string) (*os.Root, string, error) {
	for _, dir := range []string{workspace, scratch} {
		if dir == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, "", err
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, "", err
		}
		base := abs
		if !within(base, path) {
			base = real
		}
		if !within(base, path) || real == "/" {
			continue
		}
		rel, _ := filepath.Rel(base, path)
		root, err := os.OpenRoot(real)
		return root, rel, err
	}
	return nil, "", fmt.Errorf("write blocked: %s is outside the workspace and active scratch directory", path)
}

// Replace rather than truncate: an existing hard link must not change its
// outside inode. Refuse symlink destinations; reads through symlinks remain OK.
func replaceFile(root *os.Root, name string, data []byte) error {
	mode := os.FileMode(0o644)
	if st, err := root.Lstat(name); err == nil {
		if !st.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular file %s", name)
		}
		mode = st.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp := filepath.Join(filepath.Dir(name), ".arkex-write-"+rand.Text())
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(temp) }()
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return root.Rename(temp, name)
}
