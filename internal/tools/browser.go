package tools

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The managed browser is trusted stock code, never a model-selected executable.
// Keep the complete distribution outside both writable trees. The downloadable
// archive may be cached in scratch, but its digest is checked before every use.
const managedChromiumVersion = "145.0.7632.6"

func chromiumArchive(arch string) (platform, digest string, err error) {
	switch arch {
	case "arm64":
		return "mac-arm64", "8510b9b1575538aa6a092ed16d981738b2ebf52c5e41ea4c54a7ce16756cdcf5", nil
	case "amd64":
		return "mac-x64", "4316f7f98213a173e2356406203359cd4ab5b2b2db36cb219b9d99edec746819", nil
	default:
		return "", "", fmt.Errorf("managed Chromium is unsupported on %s", arch)
	}
}

func prepareChromium(ctx context.Context, scratch, destination, arch string) (string, error) {
	platform, digest, err := chromiumArchive(arch)
	if err != nil {
		return "", err
	}
	archive := filepath.Join(scratch, "arkex-chromium-"+managedChromiumVersion+"-"+platform+".zip")
	file, err := os.Open(archive)
	if errors.Is(err, os.ErrNotExist) {
		endpoint := "https://storage.googleapis.com/chrome-for-testing-public/" + managedChromiumVersion + "/" + platform + "/chrome-headless-shell-" + platform + ".zip"
		err = downloadChromium(ctx, endpoint, archive)
		if err == nil {
			file, err = os.Open(archive)
		}
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	if err := unpackChromium(ctx, file, digest, destination); err != nil {
		return "", fmt.Errorf("managed browser archive rejected (remove %s to download it again): %w", archive, err)
	}
	return filepath.Join(destination, "chrome-headless-shell-"+platform, "chrome-headless-shell"), nil
}

func downloadChromium(ctx context.Context, endpoint, archive string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("managed browser download: HTTP %d", response.StatusCode)
	}
	file, err := os.CreateTemp(filepath.Dir(archive), ".arkex-browser-download-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	n, copyErr := io.Copy(file, io.LimitReader(response.Body, (512<<20)+1))
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if n > 512<<20 {
		return errors.New("managed browser archive exceeds size limit")
	}
	return os.Rename(file.Name(), archive)
}

func unpackChromium(ctx context.Context, file *os.File, digest, destination string) error {
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, (512<<20)+1)); err != nil {
		return err
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != digest {
		return errors.New("SHA-256 mismatch")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	reader, err := zip.NewReader(file, info.Size())
	if err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var size uint64
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if !filepath.IsLocal(name) || path.Clean(name) != name || strings.Contains(name, "\\") {
			return fmt.Errorf("invalid archive path %q", entry.Name)
		}
		if entry.Mode().IsDir() {
			if err := root.MkdirAll(name, 0o700); err != nil {
				return err
			}
			continue
		}
		if !entry.Mode().IsRegular() || entry.UncompressedSize64 > (1<<30)-size {
			return errors.New("unsupported browser archive entry or expanded size")
		}
		size += entry.UncompressedSize64
		if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
			return err
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		target, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444|entry.Mode().Perm()&0o111)
		if err != nil {
			_ = source.Close()
			return err
		}
		_, copyErr := io.Copy(target, source)
		err = errors.Join(copyErr, source.Close(), target.Close())
		if err != nil {
			return err
		}
	}
	return nil
}

type managedBrowser struct {
	cmd      *exec.Cmd
	endpoint string
	done     chan struct{}
	err      error // read only after done closes
	stop     sync.Once
}

func (b *managedBrowser) Close() {
	b.stop.Do(func() {
		_ = b.cmd.Cancel()
		<-b.done
	})
}

// Browser stdout/stderr are drained with a bounded capture. exec.Cmd serializes
// writes to a shared writer; callers read the capture only after Wait completes.
type browserOutput struct {
	limitedOutput
	line  string
	ready chan string
}

func (b *browserOutput) Write(p []byte) (int, error) {
	_, _ = b.limitedOutput.Write(p)
	b.line += string(p)
	for {
		line, rest, ok := strings.Cut(b.line, "\n")
		if !ok {
			break
		}
		b.line = rest
		if endpoint, ok := strings.CutPrefix(strings.TrimSpace(line), "DevTools listening on "); ok {
			u, err := url.Parse(endpoint)
			if err == nil && u.Scheme == "ws" && u.Hostname() == "127.0.0.1" && u.Port() != "" && u.User == nil && strings.HasPrefix(u.Path, "/devtools/browser/") {
				select {
				case b.ready <- endpoint:
				default:
				}
			}
		}
	}
	if len(b.line) > 8192 {
		b.line = ""
	}
	return len(p), nil
}

func launchManagedBrowser(ctx context.Context, cmd *exec.Cmd) (*managedBrowser, error) {
	output := &browserOutput{limitedOutput: limitedOutput{limit: maxBashOutput}, ready: make(chan string, 1)}
	cmd.Stdout, cmd.Stderr = output, output
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b := &managedBrowser{cmd: cmd, done: make(chan struct{})}
	go func() { b.err = cmd.Wait(); close(b.done) }()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	var err error
	select {
	case b.endpoint = <-output.ready:
		return b, nil
	case <-b.done:
		err = fmt.Errorf("browser exited before readiness: %v", b.err)
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = errors.New("browser startup timed out")
	}
	b.Close()
	return nil, fmt.Errorf("%w\n%s", err, output.String())
}
