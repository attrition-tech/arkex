package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/attrition-tech/arkex/internal/config"
)

// Store is the token file: ~/.arkex/auth.json, mode 0600, one entry per
// connection id. It lives apart from config.json so the config can be
// shared or checked in without the sign-in going with it.
type Store struct {
	Path string
}

type authFile struct {
	Version int               `json:"version"`
	ChatGPT map[string]Tokens `json:"chatgpt"`
}

// lock coordinates edits and rotating refresh tokens across processes and store instances.
func (s *Store) lock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return nil, err
	}
	l := flock.New(s.Path+".lock", flock.SetPermissions(0o600))
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ok, err := l.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	if !ok {
		_ = l.Close()
		return nil, fmt.Errorf("sign-in store is busy")
	}
	return func() { _ = l.Close() }, nil
}

// Update holds the file lock across refresh and persistence, rereading the
// latest token pair before calling fn. A signed-out account is never recreated.
func (s *Store) Update(ctx context.Context, id string, fn func(Tokens) (Tokens, error)) (Tokens, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return Tokens{}, err
	}
	defer unlock()
	f, err := s.read()
	if err != nil {
		return Tokens{}, err
	}
	accounts := f.ChatGPT
	t, ok := accounts[id]
	if !ok {
		return Tokens{}, fmt.Errorf("not signed in; open /connections and sign in again")
	}
	previous := t
	t, err = fn(t)
	if err != nil {
		return Tokens{}, err
	}
	if t == previous {
		return t, nil
	}
	accounts[id] = t
	return t, s.write(f)
}

// DefaultStore returns the store in the arkex config directory.
func DefaultStore() (*Store, error) {
	d, err := config.Dir()
	if err != nil {
		return nil, err
	}
	return &Store{Path: filepath.Join(d, "auth.json")}, nil
}

func (s *Store) read() (authFile, error) {
	f := authFile{Version: 1, ChatGPT: map[string]Tokens{}}
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("%s: %w", s.Path, err)
	}
	if f.ChatGPT == nil {
		f.ChatGPT = map[string]Tokens{}
	}
	return f, nil
}

func (s *Store) write(f authFile) error {
	f.Version = 1
	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".auth-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

// Get returns the tokens saved for connection id.
func (s *Store) Get(id string) (Tokens, bool, error) {
	unlock, err := s.lock(context.Background())
	if err != nil {
		return Tokens{}, false, err
	}
	defer unlock()
	f, err := s.read()
	if err != nil {
		return Tokens{}, false, err
	}
	t, ok := f.ChatGPT[id]
	return t, ok, nil
}

// All returns every saved sign-in by connection id.
func (s *Store) All() (map[string]Tokens, error) {
	unlock, err := s.lock(context.Background())
	if err != nil {
		return nil, err
	}
	defer unlock()
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	return f.ChatGPT, nil
}

// Put saves tokens for connection id.
func (s *Store) Put(id string, t Tokens) error {
	unlock, err := s.lock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	f.ChatGPT[id] = t
	return s.write(f)
}

// Remove forgets the sign-in of connection id. Missing ids are fine.
func (s *Store) Remove(id string) error {
	unlock, err := s.lock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	accounts := f.ChatGPT
	if _, ok := accounts[id]; !ok {
		return nil
	}
	delete(accounts, id)
	return s.write(f)
}

// Rename moves the sign-in of oldID to newID, so renaming a connection
// keeps its account.
func (s *Store) Rename(oldID, newID string) error {
	if oldID == newID {
		return nil
	}
	unlock, err := s.lock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	accounts := f.ChatGPT
	t, ok := accounts[oldID]
	if !ok {
		return nil
	}
	delete(accounts, oldID)
	accounts[newID] = t
	return s.write(f)
}
