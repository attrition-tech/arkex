package scratch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConversationOwnershipAndCleanup(t *testing.T) {
	var s, other Store
	t.Cleanup(func() { _ = s.Close(); _ = other.Close() })
	a, err := s.Dir("../../untrusted-id")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Dir("second")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Dir("../../untrusted-id")
	if err != nil || again != a || a == b || filepath.Dir(a) != s.root {
		t.Fatal("incorrect conversation ownership")
	}
	outside, err := other.Dir("second")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(outside, "keep")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(a, "link")); err != nil {
		t.Skip(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Fatal("scratch not removed")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "keep" {
		t.Fatal("cleanup touched another run")
	}
	fresh, err := s.Dir("../../untrusted-id")
	if err != nil || fresh == a {
		t.Fatal("restart must use fresh scratch")
	}
}

func TestReplacedScratchIsNotTrusted(t *testing.T) {
	var s Store
	t.Cleanup(func() { _ = s.Close() })
	dir, err := s.Dir("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Skip(err)
	}
	if _, err := s.Dir("a"); err == nil {
		t.Fatal("adopted an external symlink as scratch")
	}
}
