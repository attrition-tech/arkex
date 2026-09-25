// Package update implements arkex's self-update against a static file host.
//
// Layout served by the host (any HTTP server; no API required):
//
//	<base>/<channel>.json                                   Manifest
//	<base>/v<version>/arkex_<version>_<os>_<arch>.tar.gz    (zip on windows)
//	<base>/v<version>/SHA256SUMS
//	<base>/v<version>/SHA256SUMS.sig                        ed25519, see sign.go
//
// Trust chain: the release public keys are compiled into the binary
// (keys.txt) → they verify SHA256SUMS.sig → SHA256SUMS pins the archive.
// The manifest is not signed: a hostile host can withhold an update but
// cannot install anything the release key did not sign, and the client
// never moves to a lower version than the one it runs.
package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// DefaultBaseURL is the release host baked into official builds via
// -ldflags "-X github.com/attrition-tech/arkex/internal/update.DefaultBaseURL=https://…".
// Empty means "no update source configured".
var DefaultBaseURL = ""

// EnvBaseURL overrides DefaultBaseURL at runtime (useful for mirrors/tests).
const EnvBaseURL = "ARKEX_UPDATE_URL"

// DefaultChannel is the manifest consulted when none is requested.
const DefaultChannel = "stable"

// Manifest is the JSON document at <base>/<channel>.json.
type Manifest struct {
	Version     string `json:"version"`                // without leading "v"
	PublishedAt string `json:"published_at,omitempty"` // RFC 3339
	Notes       string `json:"notes,omitempty"`
}

// Tag is the directory name of the release assets, e.g. "v0.1.0".
func (m Manifest) Tag() string { return "v" + strings.TrimPrefix(m.Version, "v") }

// Client performs update checks and installs. Zero-value fields fall back to
// the running process (executable path, OS/arch) and sensible defaults.
type Client struct {
	BaseURL        string
	Channel        string
	CurrentVersion string
	Keys           []ed25519.PublicKey
	HTTP           *http.Client
	UserAgent      string

	// GOOS/GOARCH/Executable default to the running process.
	GOOS, GOARCH string
	Executable   string

	// SelfTest runs the freshly extracted binary before it replaces the
	// current one. nil uses `<bin> --version`.
	SelfTest func(ctx context.Context, bin string) error

	// Log receives progress lines; nil discards them.
	Log func(format string, args ...any)
	// Progress reports archive bytes received and Content-Length (-1 if
	// unknown). Called synchronously; completion does not imply verification.
	Progress func(received, total int64)
}

// Result describes a completed update.
type Result struct {
	From, To   string
	Executable string
}

// ResolveBaseURL resolves the configured host: explicit field, then env, then build default.
func ResolveBaseURL(explicit string) string {
	for _, u := range []string{explicit, os.Getenv(EnvBaseURL), DefaultBaseURL} {
		if u = strings.TrimSpace(u); u != "" {
			return strings.TrimRight(u, "/")
		}
	}
	return ""
}

// IsNewer reports whether candidate is a higher release than current. A
// current version that is not valid semver (e.g. "dev") is treated as older
// than any release so development builds can move onto the release train.
func IsNewer(current, candidate string) bool {
	cand := "v" + strings.TrimPrefix(candidate, "v")
	if !semver.IsValid(cand) {
		return false
	}
	cur := "v" + strings.TrimPrefix(current, "v")
	if !semver.IsValid(cur) {
		return true
	}
	return semver.Compare(cand, cur) > 0
}

// Check fetches the channel manifest and reports whether it is newer than
// the running version.
func (c *Client) Check(ctx context.Context) (Manifest, bool, error) {
	base := ResolveBaseURL(c.BaseURL)
	if base == "" {
		return Manifest{}, false, errors.New("no update source configured: set " + EnvBaseURL)
	}
	channel := c.Channel
	if channel == "" {
		channel = DefaultChannel
	}
	body, err := c.get(ctx, base+"/"+channel+".json", 64<<10, nil)
	if err != nil {
		return Manifest{}, false, fmt.Errorf("fetch manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return Manifest{}, false, fmt.Errorf("parse manifest: %w", err)
	}
	if !semver.IsValid(m.Tag()) {
		return Manifest{}, false, fmt.Errorf("manifest has invalid version %q", m.Version)
	}
	return m, IsNewer(c.CurrentVersion, m.Version), nil
}

// Apply downloads, verifies, self-tests and installs the release described by m.
func (c *Client) Apply(ctx context.Context, m Manifest) (Result, error) {
	base := ResolveBaseURL(c.BaseURL)
	if base == "" {
		return Result{}, errors.New("no update source configured: set " + EnvBaseURL)
	}
	keys := c.Keys
	if keys == nil {
		var err error
		if keys, err = EmbeddedKeys(); err != nil {
			return Result{}, err
		}
	}
	goos, goarch := c.GOOS, c.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	exe, err := c.executable()
	if err != nil {
		return Result{}, err
	}

	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	version := strings.TrimPrefix(m.Version, "v")
	archiveName := fmt.Sprintf("arkex_%s_%s_%s%s", version, goos, goarch, ext)
	dir := base + "/" + m.Tag()

	c.logf("downloading %s", archiveName)
	sums, err := c.get(ctx, dir+"/SHA256SUMS", 1<<20, nil)
	if err != nil {
		return Result{}, fmt.Errorf("fetch SHA256SUMS: %w", err)
	}
	sig, err := c.get(ctx, dir+"/SHA256SUMS.sig", 64<<10, nil)
	if err != nil {
		return Result{}, fmt.Errorf("fetch SHA256SUMS.sig: %w", err)
	}
	if err := Verify(keys, sums, sig); err != nil {
		return Result{}, fmt.Errorf("release signature check failed for SHA256SUMS: %w", err)
	}
	want, err := checksumFor(sums, archiveName)
	if err != nil {
		return Result{}, err
	}
	archive, err := c.get(ctx, dir+"/"+archiveName, 256<<20, c.Progress)
	if err != nil {
		return Result{}, fmt.Errorf("fetch archive: %w", err)
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return Result{}, fmt.Errorf("checksum mismatch for %s", archiveName)
	}
	c.logf("signature and checksum verified")

	binName := "arkex"
	if goos == "windows" {
		binName += ".exe"
	}
	bin, err := extractBinary(archive, ext, binName)
	if err != nil {
		return Result{}, err
	}

	// Stage next to the target so the final rename is atomic (same
	// filesystem). A fresh unpredictable name means nothing can plant a
	// file or symlink there first.
	staged, err := stage(exe, bin)
	if err != nil {
		return Result{}, fmt.Errorf("stage new binary: %w (is %s writable?)", err, filepath.Dir(exe))
	}
	defer func() { _ = os.Remove(staged) }() // no-op after a successful rename

	selfTest := c.SelfTest
	if selfTest == nil {
		selfTest = defaultSelfTest
	}
	c.logf("testing new binary")
	if err := selfTest(ctx, staged); err != nil {
		return Result{}, fmt.Errorf("new binary failed self-test: %w", err)
	}
	c.logf("installing update")
	if err := replaceExecutable(exe, staged, goos); err != nil {
		return Result{}, err
	}
	return Result{From: c.CurrentVersion, To: version, Executable: exe}, nil
}

func (c *Client) executable() (string, error) {
	exe := c.Executable
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return "", fmt.Errorf("locate executable: %w", err)
		}
	}
	// Follow symlinks so we replace the real file, not the link.
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe, nil
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}

// stage writes bin to a new exclusive file beside exe and returns its path.
func stage(exe string, bin []byte) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(exe), "."+filepath.Base(exe)+".new-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(bin); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Chmod(0o755); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// allowedURL reports whether url may be fetched: HTTPS anywhere, plain HTTP
// only on loopback (tests and local mirrors). Signatures already stop a
// forged binary; this stops an on-path observer from seeing or stalling
// update checks.
func allowedURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return fmt.Errorf("update URL %s is not https; only loopback may use http", raw)
	}
	return fmt.Errorf("update URL %s must use https", raw)
}

func (c *Client) get(ctx context.Context, url string, limit int64, progress func(int64, int64)) ([]byte, error) {
	if err := allowedURL(url); err != nil {
		return nil, err
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var reader = io.LimitReader(resp.Body, limit+1)
	if progress != nil {
		progress(0, resp.ContentLength)
		reader = &progressReader{Reader: reader, total: resp.ContentLength, notify: progress}
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("GET %s: response larger than %d bytes", url, limit)
	}
	return body, nil
}

type progressReader struct {
	io.Reader
	received, total int64
	notify          func(int64, int64)
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.received += int64(n)
		r.notify(r.received, r.total)
	}
	return n, err
}

// checksumFor finds the sha256 for name in a `sha256sum`-style listing.
func checksumFor(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			if len(fields[0]) != sha256.Size*2 {
				return "", fmt.Errorf("SHA256SUMS: malformed digest for %s", name)
			}
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS has no entry for %s (no build for this platform?)", name)
}

// extractBinary pulls the single file named binName out of a tar.gz or zip.
func extractBinary(archive []byte, ext, binName string) ([]byte, error) {
	const maxBin = 256 << 20
	switch ext {
	case ".tar.gz":
		gz, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			return nil, fmt.Errorf("open archive: %w", err)
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("read archive: %w", err)
			}
			if h.Typeflag == tar.TypeReg && path.Base(h.Name) == binName {
				return io.ReadAll(io.LimitReader(tr, maxBin))
			}
		}
	case ".zip":
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, fmt.Errorf("open archive: %w", err)
		}
		for _, f := range zr.File {
			if path.Base(f.Name) == binName && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer func() { _ = rc.Close() }()
				return io.ReadAll(io.LimitReader(rc, maxBin))
			}
		}
	default:
		return nil, fmt.Errorf("unsupported archive type %q", ext)
	}
	return nil, fmt.Errorf("archive does not contain %s", binName)
}

func defaultSelfTest(ctx context.Context, bin string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// replaceExecutable moves staged over exe. On Windows the running image is
// locked against deletion but not rename, so the old file is moved aside.
func replaceExecutable(exe, staged, goos string) error {
	if goos != "windows" {
		if err := os.Rename(staged, exe); err != nil {
			return fmt.Errorf("replace %s: %w", exe, err)
		}
		return nil
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("move aside %s: %w", exe, err)
	}
	if err := os.Rename(staged, exe); err != nil {
		_ = os.Rename(old, exe) // best-effort rollback
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	return nil
}
