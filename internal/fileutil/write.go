// Package fileutil provides private atomic writes and cross-process file locks.
package fileutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// Lock serializes cooperating writers until unlock is called. The sidecar must
// remain in place after unlocking; removing it could split competing locks.
func Lock(path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := flock.New(path+".lock", flock.SetPermissions(0o600))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ok, err := l.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil || !ok {
		_ = l.Close()
		if err == nil {
			err = fmt.Errorf("file is busy: %s", path)
		}
		return nil, err
	}
	return func() { _ = l.Close() }, nil
}

// Write replaces path using a unique, owner-only staging file in its directory.
// Callers doing read-modify-write must hold Lock across the entire operation.
func Write(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".arkex-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
