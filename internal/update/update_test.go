package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEmbeddedKeys(t *testing.T) {
	keys, err := EmbeddedKeys()
	if err != nil || len(keys) == 0 {
		t.Fatalf("EmbeddedKeys: %v (%d keys)", err, len(keys))
	}
}

func TestParseKeysRejectsMismatchedID(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	line := PublicKeyLine(pub)
	bad := "0000000000000000" + line[16:]
	if _, err := ParseKeys(bad); err == nil {
		t.Fatal("expected key id mismatch error")
	}
	keys, err := ParseKeys("# comment\n\n" + line + "\n")
	if err != nil || len(keys) != 1 || !bytes.Equal(keys[0], pub) {
		t.Fatalf("ParseKeys round trip failed: %v", err)
	}
}

func TestVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	data := []byte("abc  arkex_0.1.0_linux_amd64.tar.gz\n")
	sig := []byte(SignatureLine(priv, data) + "\n")

	if err := Verify([]ed25519.PublicKey{pub}, data, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := Verify([]ed25519.PublicKey{pub}, []byte("tampered"), sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered data accepted: %v", err)
	}
	// Signed by a key we do not trust.
	untrusted := []byte(SignatureLine(otherPriv, data))
	if err := Verify([]ed25519.PublicKey{pub}, data, untrusted); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("untrusted key accepted: %v", err)
	}
	// Rotation: either trusted key may sign.
	if err := Verify([]ed25519.PublicKey{pub, otherPub}, data, untrusted); err != nil {
		t.Fatalf("second trusted key rejected: %v", err)
	}
	if err := Verify([]ed25519.PublicKey{pub}, data, []byte("\n# nothing\n")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("empty signature file accepted: %v", err)
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, candidate string
		want               bool
	}{
		{"0.1.0", "0.1.1", true},
		{"v0.1.0", "0.1.1", true},
		{"0.1.0", "v0.1.0", false},
		{"0.2.0", "0.1.9", false},
		{"0.9.0", "0.10.0", true}, // numeric, not lexical
		{"0.1.0", "0.2.0-rc.1", true},
		{"0.2.0-rc.1", "0.2.0", true},
		{"0.2.0", "0.2.0-rc.1", false},
		{"dev", "0.1.0", true},
		{"0.1.0", "garbage", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.current, c.candidate); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.current, c.candidate, got, c.want)
		}
	}
}

func TestChecksumFor(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	sums := []byte(digest + "  arkex_0.1.0_linux_amd64.tar.gz\n" + digest + " *arkex_0.1.0_windows_amd64.zip\n")
	for _, name := range []string{"arkex_0.1.0_linux_amd64.tar.gz", "arkex_0.1.0_windows_amd64.zip"} {
		if got, err := checksumFor(sums, name); err != nil || got != digest {
			t.Errorf("checksumFor(%s) = %q, %v", name, got, err)
		}
	}
	if _, err := checksumFor(sums, "arkex_0.1.0_darwin_arm64.tar.gz"); err == nil {
		t.Error("expected missing entry error")
	}
	// A prefix match must not count: "xarkex_…" is a different file.
	if _, err := checksumFor([]byte(digest+"  xarkex_0.1.0_linux_amd64.tar.gz\n"), "arkex_0.1.0_linux_amd64.tar.gz"); err == nil {
		t.Error("suffix match accepted")
	}
}

// release is an in-memory static host mimicking the goreleaser output layout.
type release struct {
	files map[string][]byte // path → body
}

func newRelease(t *testing.T, priv ed25519.PrivateKey, version, binContent string) *release {
	t.Helper()
	archive := tarGz(t, "arkex", []byte(binContent))
	name := fmt.Sprintf("arkex_%s_linux_amd64.tar.gz", version)
	sum := sha256.Sum256(archive)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	return &release{files: map[string][]byte{
		"/stable.json":                     []byte(fmt.Sprintf(`{"version":%q}`, version)),
		"/v" + version + "/" + name:        archive,
		"/v" + version + "/SHA256SUMS":     sums,
		"/v" + version + "/SHA256SUMS.sig": []byte(SignatureLine(priv, sums) + "\n"),
	}}
}

func (r *release) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, ok := r.files[req.URL.Path]
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(body)
}

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
	}{{"LICENSE", []byte("MIT")}, {name, content}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestClient(t *testing.T, srv *httptest.Server, pub ed25519.PublicKey, exe string) *Client {
	t.Helper()
	var selfTested string
	c := &Client{
		BaseURL:        srv.URL,
		CurrentVersion: "0.1.0",
		Keys:           []ed25519.PublicKey{pub},
		GOOS:           "linux",
		GOARCH:         "amd64",
		Executable:     exe,
		SelfTest: func(_ context.Context, bin string) error {
			selfTested = bin
			return nil
		},
	}
	t.Cleanup(func() {
		stagedDir, _ := filepath.EvalSymlinks(filepath.Dir(selfTested))
		exeDir, _ := filepath.EvalSymlinks(filepath.Dir(exe))
		if selfTested != "" && (stagedDir != exeDir || !strings.HasPrefix(filepath.Base(selfTested), "."+filepath.Base(exe)+".new-")) {
			t.Errorf("self-test ran on %q, want a staged file beside %q", selfTested, exe)
		}
	})
	return c
}

func TestGetRequiresHTTPSOffLoopback(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://get.arkex.dev/stable.json": true,
		"http://127.0.0.1:8765/stable.json": true,
		"http://localhost/stable.json":      true,
		"http://get.arkex.dev/stable.json":  false,
		"ftp://get.arkex.dev/stable.json":   false,
	} {
		if err := allowedURL(raw); (err == nil) != ok {
			t.Errorf("allowedURL(%s) = %v, want ok=%v", raw, err, ok)
		}
	}
}

func TestApplyReplacesExecutable(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r := newRelease(t, priv, "0.2.0", "NEW BINARY")
	srv := httptest.NewServer(r)
	defer srv.Close()

	exe := filepath.Join(t.TempDir(), "arkex")
	if err := os.WriteFile(exe, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, srv, pub, exe)
	var progress [][2]int64
	c.Progress = func(n, total int64) { progress = append(progress, [2]int64{n, total}) }

	m, newer, err := c.Check(context.Background())
	if err != nil || !newer || m.Version != "0.2.0" {
		t.Fatalf("Check = %+v, %v, %v", m, newer, err)
	}
	if len(progress) != 0 {
		t.Fatal("manifest must not emit archive progress")
	}
	res, err := c.Apply(context.Background(), m)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	size := int64(len(r.files["/v0.2.0/arkex_0.2.0_linux_amd64.tar.gz"]))
	if len(progress) < 2 || progress[0] != [2]int64{0, size} || progress[len(progress)-1] != [2]int64{size, size} {
		t.Fatalf("archive progress = %v, size %d", progress, size)
	}
	for i := 1; i < len(progress); i++ {
		if progress[i][0] <= progress[i-1][0] || progress[i][1] != size {
			t.Fatalf("non-archive or non-monotonic progress: %v", progress)
		}
	}
	resultExe, _ := filepath.EvalSymlinks(res.Executable)
	wantExe, _ := filepath.EvalSymlinks(exe)
	if res.To != "0.2.0" || resultExe != wantExe {
		t.Errorf("Result = %+v", res)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "NEW BINARY" {
		t.Fatalf("executable content = %q", got)
	}
	if fi, err := os.Stat(exe); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0) {
		t.Fatalf("executable bit lost: %v %v", fi, err)
	}
	if _, err := os.Stat(exe + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staged file left behind: %v", err)
	}
}

func TestApplyRejectsTampering(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)

	cases := map[string]func(r *release){
		"archive swapped after signing": func(r *release) {
			r.files["/v0.2.0/arkex_0.2.0_linux_amd64.tar.gz"] = tarGz(t, "arkex", []byte("EVIL"))
		},
		"SHA256SUMS edited": func(r *release) {
			evil := tarGz(t, "arkex", []byte("EVIL"))
			sum := sha256.Sum256(evil)
			r.files["/v0.2.0/arkex_0.2.0_linux_amd64.tar.gz"] = evil
			r.files["/v0.2.0/SHA256SUMS"] = []byte(hex.EncodeToString(sum[:]) + "  arkex_0.2.0_linux_amd64.tar.gz\n")
		},
		"signed by untrusted key": func(r *release) {
			r.files["/v0.2.0/SHA256SUMS.sig"] = []byte(SignatureLine(otherPriv, r.files["/v0.2.0/SHA256SUMS"]))
		},
		"signature missing": func(r *release) {
			delete(r.files, "/v0.2.0/SHA256SUMS.sig")
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRelease(t, priv, "0.2.0", "NEW BINARY")
			tamper(r)
			srv := httptest.NewServer(r)
			defer srv.Close()

			exe := filepath.Join(t.TempDir(), "arkex")
			if err := os.WriteFile(exe, []byte("OLD BINARY"), 0o755); err != nil {
				t.Fatal(err)
			}
			c := newTestClient(t, srv, pub, exe)
			c.Log = func(format string, args ...any) {
				line := fmt.Sprintf(format, args...)
				if strings.Contains(line, "verified") || strings.Contains(line, "installing") {
					t.Errorf("premature success log: %s", line)
				}
			}
			c.SelfTest = func(context.Context, string) error {
				t.Fatal("self-test must not run on unverified binary")
				return nil
			}
			m, _, err := c.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Apply(context.Background(), m); err == nil {
				t.Fatal("Apply succeeded on tampered release")
			}
			got, _ := os.ReadFile(exe)
			if string(got) != "OLD BINARY" {
				t.Fatalf("executable modified: %q", got)
			}
			if _, err := os.Stat(exe + ".new"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("staged file left behind: %v", err)
			}
		})
	}
}

func TestApplyKeepsOldBinaryWhenSelfTestFails(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	srv := httptest.NewServer(newRelease(t, priv, "0.2.0", "BROKEN"))
	defer srv.Close()

	exe := filepath.Join(t.TempDir(), "arkex")
	_ = os.WriteFile(exe, []byte("OLD BINARY"), 0o755)
	c := newTestClient(t, srv, pub, exe)
	c.SelfTest = func(context.Context, string) error { return errors.New("segfault") }

	m, _, _ := c.Check(context.Background())
	if _, err := c.Apply(context.Background(), m); err == nil || !strings.Contains(err.Error(), "self-test") {
		t.Fatalf("Apply = %v, want self-test failure", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "OLD BINARY" {
		t.Fatalf("executable modified: %q", got)
	}
}

func TestCheckNotNewer(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	srv := httptest.NewServer(newRelease(t, priv, "0.1.0", "SAME"))
	defer srv.Close()
	c := newTestClient(t, srv, pub, filepath.Join(t.TempDir(), "arkex"))
	if _, newer, err := c.Check(context.Background()); err != nil || newer {
		t.Fatalf("Check = newer %v, err %v; want not newer", newer, err)
	}
}

func TestResolveBaseURL(t *testing.T) {
	t.Setenv(EnvBaseURL, "")
	old := DefaultBaseURL
	DefaultBaseURL = "https://built.in/"
	defer func() { DefaultBaseURL = old }()
	if got := ResolveBaseURL(""); got != "https://built.in" {
		t.Errorf("default: %q", got)
	}
	t.Setenv(EnvBaseURL, "https://env.example/")
	if got := ResolveBaseURL(""); got != "https://env.example" {
		t.Errorf("env: %q", got)
	}
	if got := ResolveBaseURL("https://flag.example"); got != "https://flag.example" {
		t.Errorf("explicit: %q", got)
	}
}
