package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/attrition-tech/arkex/internal/fileutil"
)

func TestConfigResetScopesAndConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		args        []string
		reset, all  bool
		wantErr     bool
	}{
		{name: "preview", args: []string{"reset", "--all", "--dry-run"}},
		{name: "EOF refuses", args: []string{"reset"}, wantErr: true},
		{name: "no refuses", input: "no\n", args: []string{"reset", "--all"}, wantErr: true},
		{name: "config only", input: "reset\n", args: []string{"reset"}, reset: true},
		{name: "flush alias", args: []string{"flush", "--yes"}, reset: true},
		{name: "all", args: []string{"reset", "--all", "--yes"}, reset: true, all: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, workspace := t.TempDir(), t.TempDir()
			t.Setenv("ARKEX_HOME", home)
			t.Chdir(workspace)
			files := []string{"config.json", "config.json.bak", "auth.json", "sessions/work/id.json", "bin/arkex", "unrelated.txt", ".reset-backup-old/keep"}
			for _, name := range files {
				path := filepath.Join(home, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				// Deliberately invalid configuration: reset must work even when
				// normal startup cannot parse the user's broken settings.
				if err := os.WriteFile(path, []byte("private original: "+name), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(workspace, ".arkex"), 0o700); err != nil {
				t.Fatal(err)
			}
			project := filepath.Join(workspace, ".arkex", "config.json")
			if err := os.WriteFile(project, []byte("project settings"), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			cmd := newRoot()
			cmd.SetArgs(append([]string{"config"}, tc.args...))
			cmd.SetIn(strings.NewReader(tc.input))
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err := cmd.ExecuteContext(t.Context())
			if (err != nil) != tc.wantErr || strings.Contains(out.String(), "private original:") {
				t.Fatal("wrong result or leaked credentials", err, out.String())
			}
			backups, err := filepath.Glob(filepath.Join(home, ".reset-backup-*"))
			wantBackups := 1
			if tc.reset {
				wantBackups++
			}
			if err != nil || len(backups) != wantBackups {
				t.Fatal("unexpected backups", backups, err)
			}
			backup := ""
			for _, path := range backups {
				if filepath.Base(path) != ".reset-backup-old" {
					backup = path
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != 0o700 || !strings.Contains(out.String(), path) {
						t.Fatal("backup must be private and discoverable", err)
					}
				}
			}
			for _, name := range files {
				moved := tc.reset && (strings.HasPrefix(name, "config.json") || tc.all && (name == "auth.json" || strings.HasPrefix(name, "sessions/")))
				path := filepath.Join(home, name)
				if moved {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatal("reset entry is still active", name, err)
					}
					path = filepath.Join(backup, name)
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "private original: "+name {
					t.Fatal("original lost or unrelated file changed", name, err)
				}
			}
			data, err := os.ReadFile(project)
			if err != nil || string(data) != "project settings" {
				t.Fatal("reset changed project settings", err)
			}
		})
	}
}

func TestConfigResetDoesNotFollowSymlinkOrWaitThroughCancellation(t *testing.T) {
	home, external := t.TempDir(), t.TempDir()
	t.Setenv("ARKEX_HOME", home)
	target := filepath.Join(external, "config.json")
	if err := os.WriteFile(target, []byte("external secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	unlock, err := fileutil.Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	cmd := newConfigResetCmd()
	cmd.SetArgs([]string{"--yes"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err = cmd.ExecuteContext(ctx)
	unlock()
	if err == nil {
		t.Fatal("reset ignored a busy writer and cancellation")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("cancelled reset moved the entry", err)
	}
	cmd = newConfigResetCmd()
	cmd.SetArgs([]string{"--yes"})
	cmd.SetOut(&out)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "external secret" {
		t.Fatal("reset followed symlink", err)
	}
}

func TestReinstallAttemptsSameVersionButNeverDowngradesOrResetsOnFailure(t *testing.T) {
	oldVersion := version
	version = "1.2.3"
	t.Cleanup(func() { version = oldVersion })
	for _, tc := range []struct {
		name, candidate string
		args            []string
		wantErr         bool
		wantDownloads   int
	}{
		{"normal up to date", "1.2.3", nil, false, 0},
		{"same reinstall", "1.2.3", []string{"--reinstall"}, true, 1},
		{"older refused", "1.2.2", []string{"--reinstall"}, true, 0},
		{"fresh download fails", "1.2.3", []string{"--fresh", "--yes"}, true, 1},
		{"fresh confirmation refuses", "1.2.3", []string{"--fresh"}, true, 0},
		{"check fresh conflict", "1.2.3", []string{"--fresh", "--check"}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("ARKEX_HOME", home)
			path := filepath.Join(home, "config.json")
			if err := os.WriteFile(path, []byte("keep my credentials"), 0o600); err != nil {
				t.Fatal(err)
			}
			var downloads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/stable.json" {
					_, _ = fmt.Fprintf(w, `{"version":%q}`, tc.candidate)
					return
				}
				downloads.Add(1)
				http.NotFound(w, r)
			}))
			defer server.Close()
			var out bytes.Buffer
			cmd := newUpdateCmd()
			cmd.SetArgs(append(tc.args, "--url", server.URL))
			cmd.SetIn(strings.NewReader("no\n"))
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err := cmd.ExecuteContext(t.Context())
			if (err != nil) != tc.wantErr || int(downloads.Load()) != tc.wantDownloads {
				t.Fatal("wrong update behavior", downloads.Load(), err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "keep my credentials" {
				t.Fatal("failed download or unapproved reset touched configuration", err)
			}
		})
	}
}
