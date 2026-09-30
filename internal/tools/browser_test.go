package tools

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestChromiumArchiveValidation(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
		mode        os.FileMode
		tamper      bool
		wantError   string
	}{
		{"valid", "stock/bin/browser", 0o755, false, ""},
		{"tampered", "stock/bin/browser", 0o755, true, "SHA-256 mismatch"},
		{"traversal", "../outside", 0o644, false, "invalid archive path"},
		{"symlink", "stock/link", os.ModeSymlink | 0o777, false, "unsupported browser archive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "archive.zip")
			file, err := os.Create(archive)
			if err != nil {
				t.Fatal(err)
			}
			writer := zip.NewWriter(file)
			header := &zip.FileHeader{Name: tc.entry}
			header.SetMode(tc.mode)
			entry, err := writer.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = entry.Write([]byte("stock-content"))
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(data))
			if tc.tamper {
				data[len(data)/2] ^= 1
				if err := os.WriteFile(archive, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			file, err = os.Open(archive)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			destination := filepath.Join(t.TempDir(), "unpacked")
			err = unpackChromium(t.Context(), file, digest, destination)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("wanted %q, got %v", tc.wantError, err)
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(destination), "outside")); !os.IsNotExist(err) {
					t.Fatal("archive escaped destination")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			sandboxContents(t, filepath.Join(destination, tc.entry), "stock-content")
			info, err := os.Stat(filepath.Join(destination, tc.entry))
			if err != nil || info.Mode().Perm() != 0o555 {
				t.Fatalf("stock executable permissions: %v %v", info, err)
			}
		})
	}
}

func TestChromiumRejectsModifiedCache(t *testing.T) {
	scratch := t.TempDir()
	archive := filepath.Join(scratch, "arkex-chromium-"+managedChromiumVersion+"-mac-arm64.zip")
	if err := os.WriteFile(archive, []byte("not the stock browser"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "stock")
	if _, err := prepareChromium(t.Context(), scratch, destination, "arm64"); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("modified cache accepted: %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("unverified archive was extracted")
	}
}

func TestChromiumDownloadFailureIsNotCached(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := downloadChromium(t.Context(), server.URL, filepath.Join(dir, "archive.zip")); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("download error lost: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed download persisted: %v %v", entries, err)
	}
}

func TestBashBrowserFailureDoesNotRunCommand(t *testing.T) {
	b := &Bash{Dir: t.TempDir(), TempDir: t.TempDir(), Shell: "/bin/sh"}
	platform, _, err := chromiumArchive(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(b.TempDir, "arkex-chromium-"+managedChromiumVersion+"-"+platform+".zip")
	if err := os.WriteFile(archive, []byte("bad-archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = runBash(t, b, map[string]any{"browser": "chromium", "command": "touch must-not-run"})
	want := "macOS-only"
	if runtime.GOOS == "darwin" {
		want = "SHA-256 mismatch"
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("browser failure hidden: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "must-not-run")); !os.IsNotExist(err) {
		t.Fatal("command ran after browser failure")
	}
	sandboxContents(t, archive, "bad-archive")
}

func TestBrowserReadinessParsing(t *testing.T) {
	b := &browserOutput{limitedOutput: limitedOutput{limit: 50}, ready: make(chan string, 1)}
	for _, endpoint := range []string{"ws://example.com:123/devtools/browser/a", "ws://127.0.0.1:123/wrong", "ws://user@127.0.0.1:123/devtools/browser/a"} {
		_, _ = b.Write([]byte("DevTools listening on " + endpoint + "\n"))
	}
	_, _ = b.Write([]byte(strings.Repeat("x", 9000)))
	_, _ = b.Write([]byte("\nDevTools listening on ws://127.0.0.1:54321/devtools/"))
	select {
	case endpoint := <-b.ready:
		t.Fatalf("invalid or incomplete readiness: %s", endpoint)
	default:
	}
	_, _ = b.Write([]byte("browser/fixture\n"))
	select {
	case endpoint := <-b.ready:
		if endpoint != "ws://127.0.0.1:54321/devtools/browser/fixture" {
			t.Fatal(endpoint)
		}
	default:
		t.Fatal("split readiness line lost")
	}
	if b.buf.Len() > 51 || len(b.line) > 8192 {
		t.Fatal("unbounded browser output")
	}
}

func TestManagedBrowserLifecycle(t *testing.T) {
	t.Run("already exited", func(t *testing.T) {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reader.Close(); _ = writer.Close() }()
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", `printf 'DevTools listening on ws://127.0.0.1:54321/devtools/browser/fixture\n'; read done`)
		cmd.Stdin = reader
		browser, err := launchManagedBrowser(t.Context(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.WriteString("exit\n"); err != nil {
			t.Fatal(err)
		}
		<-browser.done
		cmd.Cancel = func() error { t.Error("attempted to signal a reaped browser PID"); return nil }
		browser.Close()
	})
	t.Run("owned shutdown", func(t *testing.T) {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", `printf 'DevTools listening on ws://127.0.0.1:54321/devtools/browser/fixture\n'; exec sleep 60`)
		browser, err := launchManagedBrowser(t.Context(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		if browser.endpoint != "ws://127.0.0.1:54321/devtools/browser/fixture" {
			t.Fatalf("wrong browser endpoint: %s", browser.endpoint)
		}
		browser.Close()
		browser.Close() // idempotent cleanup is used by the caller and defer
		if cmd.ProcessState == nil || cmd.ProcessState.Success() {
			t.Fatal("browser not stopped and reaped")
		}
	})
	t.Run("canceled startup", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "exec sleep 60")
		browser, err := launchManagedBrowser(ctx, cmd)
		if browser != nil || !errors.Is(err, context.DeadlineExceeded) || cmd.ProcessState == nil {
			t.Fatalf("canceled browser not reaped: %v %v", browser, err)
		}
	})
	t.Run("early failure", func(t *testing.T) {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "echo startup-failure; exit 19")
		browser, err := launchManagedBrowser(t.Context(), cmd)
		if browser != nil || err == nil || !strings.Contains(err.Error(), "startup-failure") || !strings.Contains(err.Error(), "19") {
			t.Fatalf("startup failure hidden: %v %v", browser, err)
		}
	})
}
