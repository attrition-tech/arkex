// Package scratch owns disposable conversation directories for one Arkex run.
package scratch

import (
	"fmt"
	"os"
	"path/filepath"
)

type Store struct {
	root string
	dirs map[string]string
}

// Dir creates lazily. Session IDs are map keys, never filesystem paths.
func (s *Store) Dir(id string) (string, error) {
	if dir := s.dirs[id]; dir != "" {
		if err := unchangedDirectory(dir); err != nil {
			return "", err
		}
		return dir, nil
	}
	if s.root == "" {
		root, err := os.MkdirTemp("", "arkex-")
		if err != nil {
			return "", err
		}
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			_ = os.Remove(root)
			return "", err
		}
		s.root = real
		s.dirs = make(map[string]string)
	}
	if err := unchangedDirectory(s.root); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(s.root, "session-")
	if err != nil {
		return "", err
	}
	s.dirs[id] = dir
	return dir, nil
}

func unchangedDirectory(dir string) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if real != dir || !info.IsDir() {
		return fmt.Errorf("scratch directory replaced: %s", dir)
	}
	return nil
}

// Close removes only this process's randomly created tree, not other runs.
// Call only after tools have stopped. Crashes intentionally leave leftovers.
func (s *Store) Close() error {
	if s.root == "" {
		return nil
	}
	err := os.RemoveAll(s.root)
	if err == nil {
		s.root, s.dirs = "", nil
	}
	return err
}
