// Package session persists conversations on disk so they can be resumed.
//
// Layout: <config dir>/sessions/<cwd hash>/<id>.json, one file per
// conversation, grouped by the directory arkex was started in. Files are
// mode 0600 because messages can carry file contents and command output.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/dantearo/arkex/internal/config"
	"github.com/dantearo/arkex/internal/fileutil"
)

// Session is one conversation and the state needed to continue it.
type Session struct {
	ID           string             `json:"id"`
	Cwd          string             `json:"cwd"`
	Title        string             `json:"title"`
	Model        string             `json:"model,omitempty"`
	Effort       *string            `json:"effort,omitempty"` // nil = legacy; empty string = provider default
	Mode         string             `json:"mode,omitempty"`
	State        string             `json:"state,omitempty"` // empty = active; archived or trash
	Created      time.Time          `json:"created"`
	Updated      time.Time          `json:"updated"`
	UsageIn      int64              `json:"usage_in,omitempty"`
	UsageOut     int64              `json:"usage_out,omitempty"`
	LastInput    int64              `json:"last_input,omitempty"` // latest request, not cumulative; zero means unknown
	Messages     []fantasy.Message  `json:"messages"`
	History      []fantasy.Message  `json:"history,omitempty"` // append-only context segments for prompt checkpoints
	ContextStart int                `json:"context_start,omitempty"`
	Prompts      []PromptCheckpoint `json:"prompts,omitempty"`
	ParentID     string             `json:"parent_id,omitempty"`
	savedHash    [sha256.Size]byte  // optimistic concurrency token; never serialized
	saved        bool
}

// Summary is what listings need: everything but the messages.
type Summary struct {
	ID       string
	Title    string
	Model    string
	Updated  time.Time
	Messages int
}

const maxTitle = 60

// Dir returns the directory holding cwd's sessions.
func Dir(cwd string) (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
	return filepath.Join(base, "sessions", hex.EncodeToString(sum[:6])), nil
}

// New returns an unsaved session for cwd. IDs sort by creation time.
func New(cwd string) *Session {
	now := time.Now()
	var r [3]byte
	_, _ = rand.Read(r[:])
	return &Session{
		ID:      now.Format("20060102-150405") + "-" + hex.EncodeToString(r[:]),
		Cwd:     cwd,
		Created: now,
		Updated: now,
	}
}

// Update records the conversation state and refreshes the title and time.
func (s *Session) Update(msgs []fantasy.Message, model, mode string, in, out int64) {
	s.syncContext(msgs)
	s.Messages = msgs
	s.Model, s.Mode = model, mode
	s.UsageIn, s.UsageOut = in, out
	s.Updated = time.Now()
	if s.Title == "" {
		s.Title = TitleFor(msgs)
	}
}

// TitleFor derives a title from the first line of the first user message.
func TitleFor(msgs []fantasy.Message) string {
	for _, m := range msgs {
		if m.Role != fantasy.MessageRoleUser {
			continue
		}
		text := UserText(m)
		if line, _, _ := strings.Cut(strings.TrimSpace(text), "\n"); line != "" {
			if len([]rune(line)) > maxTitle {
				return string([]rune(line)[:maxTitle-1]) + "…"
			}
			return line
		}
	}
	return "untitled"
}

// UserText returns a user message's text without attached @file blocks.
func UserText(m fantasy.Message) string {
	var sb strings.Builder
	for _, p := range m.Content {
		if t, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
			sb.WriteString(t.Text)
		}
	}
	text := sb.String()
	for _, tag := range []string{"\n\n<file path=", "\n\n<shell command="} {
		if i := strings.Index(text, tag); i >= 0 {
			text = text[:i]
		}
	}
	return text
}

// ErrConflict prevents a stale process from overwriting a changed/deleted session.
var ErrConflict = errors.New("session changed in another process; disk left unchanged and this version has not been saved")

// Save writes the session atomically. Empty branches are intentional and saved;
// simply opening arkex and quitting still leaves nothing behind.
func (s *Session) Save() error {
	if len(s.Messages) == 0 && s.ParentID == "" {
		return nil
	}
	dir, err := Dir(s.Cwd)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := fileutil.Lock(s.path(dir))
	if err != nil {
		return err
	}
	defer unlock()
	f, err := os.Open(s.path(dir))
	if err == nil {
		h := sha256.New()
		_, readErr := io.Copy(h, f)
		_ = f.Close()
		if readErr != nil {
			return readErr
		}
		if !s.saved || string(h.Sum(nil)) != string(s.savedHash[:]) {
			return ErrConflict
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if s.saved {
		return ErrConflict
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := fileutil.Write(s.path(dir), b); err != nil {
		return err
	}
	s.savedHash, s.saved = sha256.Sum256(b), true
	return nil
}

func (s *Session) path(dir string) string { return filepath.Join(dir, s.ID+".json") }

// Load reads one session of cwd by id. An id prefix is accepted when it is
// unambiguous, so "/resume 20260918-15" works.
func Load(cwd, id string) (*Session, error) {
	dir, err := Dir(cwd)
	if err != nil {
		return nil, err
	}
	id, err = resolveID(dir, id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("session %s is corrupt: %w", id, err)
	}
	s.savedHash, s.saved = sha256.Sum256(b), true
	return &s, nil
}

// Latest returns the most recently updated session of cwd, or nil when
// there is none.
func Latest(cwd string) (*Session, error) {
	list, err := List(cwd)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return Load(cwd, list[0].ID)
}

const (
	Archived = "archived"
	Trash    = "trash"
)

// List returns cwd's active sessions, newest first. Unreadable files are skipped.
func List(cwd string) ([]Summary, error) {
	return ListState(cwd, "")
}

// ListState includes only sessions in the requested state. Older files are active.
func ListState(cwd, state string) ([]Summary, error) {
	if state != "" && state != Archived && state != Trash {
		return nil, fmt.Errorf("invalid session state %q", state)
	}
	dir, err := Dir(cwd)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		s, savedState, err := readSummary(f)
		_ = f.Close()
		if err != nil || s.ID == "" || savedState != state {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// Delete removes one session file.
func Delete(cwd, id string) error {
	dir, err := Dir(cwd)
	if err != nil {
		return err
	}
	id, err = resolveID(dir, id)
	if err != nil {
		return err
	}
	unlock, err := fileutil.Lock(filepath.Join(dir, id+".json"))
	if err != nil {
		return err
	}
	defer unlock()
	return os.Remove(filepath.Join(dir, id+".json"))
}

// validID matches a generated id or a prefix of one: digits, a dash and
// hex. Anything else (separators, dots) could name a file outside dir.
var validID = regexp.MustCompile(`^[0-9a-f-]{1,22}$`)

func resolveID(dir, id string) (string, error) {
	if id == "" {
		return "", errors.New("session id required")
	}
	if !validID.MatchString(id) {
		return "", fmt.Errorf("invalid session id %q", id)
	}
	if _, err := os.Stat(filepath.Join(dir, id+".json")); err == nil {
		return id, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("no session %q", id)
	}
	var hits []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if strings.HasPrefix(name, id) && !strings.HasPrefix(name, ".") {
			hits = append(hits, name)
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no session %q", id)
	case 1:
		return hits[0], nil
	}
	return "", fmt.Errorf("%q matches %d sessions", id, len(hits))
}

// Age formats how long ago t was, for listings.
func Age(t time.Time, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("2006-01-02")
}
