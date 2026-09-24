package session

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func TestConcurrentSavesRejectStaleAndDeletedSessions(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	s := New(t.TempDir())
	s.Update([]fantasy.Message{fantasy.NewUserMessage("original")}, "m", "build", 11, 7)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	a, err := Load(s.Cwd, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(s.Cwd, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.Title, b.Title = "client A", "client B"
	results := make(chan error, 2)
	go func() { results <- a.Save() }()
	go func() { results <- b.Save() }()
	first, second := <-results, <-results
	if (first != nil || !errors.Is(second, ErrConflict)) && (second != nil || !errors.Is(first, ErrConflict)) {
		t.Fatalf("expected one save and one conflict: %v / %v", first, second)
	}
	saved, err := Load(s.Cwd, s.ID)
	if err != nil || (saved.Title != a.Title && saved.Title != b.Title) || saved.UsageIn != 11 {
		t.Fatalf("saved data damaged: %+v %v", saved, err)
	}
	if err := Delete(s.Cwd, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := saved.Save(); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleted session recreated: %v", err)
	}
	if _, err := Load(s.Cwd, s.ID); err == nil {
		t.Fatal("deleted session resolved through a lock sidecar")
	}
}

func TestSaveListLoad(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	cwd := t.TempDir()

	empty := New(cwd)
	if err := empty.Save(); err != nil {
		t.Fatal(err)
	}
	if list, _ := List(cwd); len(list) != 0 {
		t.Fatalf("empty sessions must not be written, got %+v", list)
	}

	a := New(cwd)
	a.Update([]fantasy.Message{
		fantasy.NewUserMessage("fix the build\n\n<file path=\"go.mod\">\nmodule x\n</file>"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "done"}}},
	}, "local/m", "build", 10, 5)
	if a.Title != "fix the build" {
		t.Fatalf("title = %q (attached files must not leak into it)", a.Title)
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)
	b := New(cwd)
	b.Update([]fantasy.Message{fantasy.NewUserMessage(strings.Repeat("x", 100))}, "local/m", "plan", 1, 1)
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(b.Title)); n != maxTitle {
		t.Fatalf("long title must be cut to %d runes, got %d", maxTitle, n)
	}

	// Another directory must not see these sessions.
	if list, _ := List(t.TempDir()); len(list) != 0 {
		t.Fatalf("sessions leaked across directories: %+v", list)
	}

	list, err := List(cwd)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v err = %v", list, err)
	}
	if list[0].ID != b.ID || list[1].ID != a.ID || list[1].Messages != 2 {
		t.Fatalf("newest first expected: %+v", list)
	}

	got, err := Load(cwd, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "build" || got.UsageIn != 10 || len(got.Messages) != 2 || UserText(got.Messages[0]) != "fix the build" {
		t.Fatalf("round trip lost data: %+v", got)
	}
	// Prefix resolution: the date part is shared, the random suffix is not.
	if _, err := Load(cwd, a.ID[:9]); err == nil {
		t.Fatal("ambiguous prefix must fail")
	}
	if s, err := Load(cwd, a.ID[:len(a.ID)-1]); err != nil || s.ID != a.ID {
		t.Fatalf("unique prefix must resolve: %v", err)
	}
	if latest, _ := Latest(cwd); latest == nil || latest.ID != b.ID {
		t.Fatalf("latest = %+v", latest)
	}

	dir, _ := Dir(cwd)
	if st, _ := os.Stat(filepath.Join(dir, a.ID+".json")); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("session files must be 0600, got %v", st.Mode().Perm())
	}
	// A corrupt file is skipped by List but reported by Load.
	if err := os.WriteFile(filepath.Join(dir, "20200101-000000-bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if list, _ := List(cwd); len(list) != 2 {
		t.Fatalf("corrupt file must be skipped, got %d", len(list))
	}
	if _, err := Load(cwd, "20200101-000000-bad"); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected corrupt error, got %v", err)
	}

	if err := Delete(cwd, a.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := List(cwd); len(list) != 1 {
		t.Fatalf("delete failed: %+v", list)
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cases := map[time.Duration]string{
		30 * time.Second:    "just now",
		5 * time.Minute:     "5m ago",
		3 * time.Hour:       "3h ago",
		2 * 24 * time.Hour:  "2d ago",
		30 * 24 * time.Hour: "2026-08-19",
	}
	for d, want := range cases {
		if got := Age(now.Add(-d), now); got != want {
			t.Errorf("Age(-%v) = %q want %q", d, got, want)
		}
	}
}

func TestIDsWithPathSeparatorsAreRejected(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("ARKEX_HOME", t.TempDir())
	for _, id := range []string{"../x", "a/b", "..", ".", "20260919-224832-1157af.json", "x\\y"} {
		if _, err := Load(cwd, id); err == nil || !strings.Contains(err.Error(), "invalid session id") {
			t.Errorf("Load(%q) err = %v, want invalid session id", id, err)
		}
		if err := Delete(cwd, id); err == nil || !strings.Contains(err.Error(), "invalid session id") {
			t.Errorf("Delete(%q) err = %v, want invalid session id", id, err)
		}
	}
}
