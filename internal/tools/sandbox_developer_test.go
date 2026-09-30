package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These checks download real dependencies and resolve external names. Keep
// them explicit rather than silently turning the unit suite into network tests.
func TestSandboxDeveloperWorkflow(t *testing.T) {
	if os.Getenv("ARKEX_SANDBOX_INTEGRATION") != "1" {
		t.Skip("set ARKEX_SANDBOX_INTEGRATION=1 for native developer-tool checks")
	}
	for _, name := range []string{"node", "npm", "ruby"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatal(err)
		}
	}
	b := &Bash{Dir: t.TempDir(), TempDir: t.TempDir(), Shell: "/bin/sh"}
	run := func(t *testing.T, command string) string {
		t.Helper()
		res, err := runBash(t, b, map[string]any{"command": command, "timeout_ms": 600000})
		if err != nil {
			t.Fatalf("%v\n%s", err, res.Output)
		}
		return res.Output
	}
	t.Run("resolver and Ruby", func(t *testing.T) {
		output := run(t, `node -e 'require("dns").lookup("gitlab.com", {all:true}, (e,a) => {if(e) throw e; if(!a.length) throw Error("no addresses"); console.log("node-dns-ok")})' && ruby -rsocket -e 'abort "no addresses" if Addrinfo.getaddrinfo("gitlab.com", 443).empty?; File.write(File.join(ENV.fetch("TMPDIR"), "ruby-result"), "ruby-ok"); puts "ruby-dns-ok"'`)
		if !strings.Contains(output, "node-dns-ok") || !strings.Contains(output, "ruby-dns-ok") {
			t.Fatal(output)
		}
		sandboxContents(t, filepath.Join(b.TempDir, "ruby-result"), "ruby-ok")
	})
	t.Run("browser install and subsequent launch", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/download" {
				w.Header().Set("Content-Disposition", `attachment; filename="fixture.txt"`)
				_, _ = fmt.Fprint(w, "download-ok")
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, `<title>Sandbox browser</title><button onclick="this.textContent=42">Run</button><a href="/download">Download</a>`)
		}))
		defer server.Close()
		// Use the project's version in real work; pin this fixture for repeatability.
		run(t, `set -eu
npm install --prefix "$TMPDIR/pw" --ignore-scripts --no-audit --no-fund playwright@1.58.2
PLAYWRIGHT_BROWSERS_PATH="$TMPDIR/browsers" node "$TMPDIR/pw/node_modules/playwright/cli.js" install chromium --only-shell`)
		command := fmt.Sprintf(`PLAYWRIGHT_BROWSERS_PATH="$TMPDIR/browsers" node - <<'JS'
const { chromium } = require(process.env.TMPDIR + '/pw/node_modules/playwright');
(async () => {
  const browser = process.env.ARKEX_BROWSER_WS_ENDPOINT
    ? await chromium.connectOverCDP(process.env.ARKEX_BROWSER_WS_ENDPOINT, {isLocal: true})
    : await chromium.launch({headless: true});
  try {
    const page = await browser.newPage({viewport: {width: 640, height: 480}});
    await page.goto(%q);
    await page.click('button');
    if (await page.title() !== 'Sandbox browser' || await page.textContent('button') !== '42') throw Error('browser result mismatch');
    if (!process.env.ARKEX_SANDBOX_CONTROL) {
      await page.screenshot({path: 'browser.png'});
      const downloadReady = page.waitForEvent('download');
      await page.click('a');
      await (await downloadReady).saveAs('download.txt');
      require('fs').writeFileSync(process.env.TMPDIR + '/browser-result', 'scratch-browser-ok');
    }
    if (process.env.ARKEX_BROWSER_WS_ENDPOINT) console.log('endpoint=' + process.env.ARKEX_BROWSER_WS_ENDPOINT);
    console.log('browser-ok');
  } finally { await browser.close(); }
})().catch(e => {console.error(e); process.exitCode = 1;});
JS`, server.URL)
		// Fixed, trusted fixture only: a host control distinguishes browser/OS
		// incompatibility from a confinement failure. Production has no fallback.
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		control := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		control.Env = append(os.Environ(), "TMPDIR="+b.TempDir, "HOME="+t.TempDir(), "ARKEX_SANDBOX_CONTROL=1")
		if output, err := control.CombinedOutput(); err != nil || !strings.Contains(string(output), "browser-ok") {
			t.Fatalf("unsandboxed fixture failed: %v\n%s", err, output)
		}
		t.Log("unsandboxed browser control passed")
		// Separate call: catches scratch publication and stale macOS copy paths.
		input := map[string]any{"command": command, "timeout_ms": 600000}
		if runtime.GOOS == "darwin" {
			// Ordinary shell children must not gain a global Chromium IPC grant.
			res, err := runBash(t, b, input)
			if err == nil || !strings.Contains(res.Output, "MachPortRendezvousServer") || !strings.Contains(res.Output, "Permission denied") {
				t.Fatalf("expected ordinary browser IPC denial: %v\n%s", err, res.Output)
			}
			input["browser"] = "chromium"
		}
		res, err := runBash(t, b, input)
		if err != nil || !strings.Contains(res.Output, "browser-ok") {
			t.Fatalf("supported browser workflow: %v\n%s", err, res.Output)
		}
		sandboxContents(t, filepath.Join(b.Dir, "download.txt"), "download-ok")
		sandboxContents(t, filepath.Join(b.TempDir, "browser-result"), "scratch-browser-ok")
		image, err := os.ReadFile(filepath.Join(b.Dir, "browser.png"))
		if err != nil {
			t.Fatal(err)
		}
		config, err := png.DecodeConfig(bytes.NewReader(image))
		if err != nil || config.Width != 640 || config.Height != 480 {
			t.Fatalf("screenshot not published correctly: %+v %v", config, err)
		}
		if runtime.GOOS == "darwin" {
			assertBrowserStopped(t, res.Output)
			t.Run("cancel managed browser", func(t *testing.T) { checkBrowserCancellation(t, b) })
		}
	})
}

func assertBrowserStopped(t *testing.T, output string) {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if endpoint, ok := strings.CutPrefix(line, "endpoint="); ok {
			u, err := url.Parse(endpoint)
			if err != nil || u.Hostname() != "127.0.0.1" {
				t.Fatalf("invalid endpoint: %s %v", endpoint, err)
			}
			conn, err := net.DialTimeout("tcp", u.Host, time.Second)
			if err == nil {
				_ = conn.Close()
				t.Fatal("browser still listening after tool completion")
			}
			return
		}
	}
	t.Fatal("browser endpoint missing from fixture output")
}

func checkBrowserCancellation(t *testing.T, b *Bash) {
	t.Helper()
	ready := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case ready <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	command := fmt.Sprintf(`node - <<'JS'
const {chromium} = require(process.env.TMPDIR + '/pw/node_modules/playwright');
(async () => {
  const browser = await chromium.connectOverCDP(process.env.ARKEX_BROWSER_WS_ENDPOINT, {isLocal: true});
  const page = await browser.newPage();
  require('fs').writeFileSync('canceled-result', 'must-not-publish');
  console.log('endpoint=' + process.env.ARKEX_BROWSER_WS_ENDPOINT);
  await page.goto(%q);
  await new Promise(() => {});
})().catch(e => {console.error(e); process.exitCode = 1;});
JS`, server.URL)
	input, err := json.Marshal(map[string]any{"browser": "chromium", "command": command, "timeout_ms": 60000})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	type completion struct {
		res Result
		err error
	}
	done := make(chan completion, 1)
	go func() { res, err := b.Run(ctx, input); done <- completion{res, err} }()
	select {
	case <-ready:
	case result := <-done:
		t.Fatalf("browser never became ready: %v\n%s", result.err, result.res.Output)
	case <-ctx.Done():
		t.Fatal("timed out waiting for browser interaction")
	}
	cancel()
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) || !strings.Contains(result.res.Output, "unpublished command changes discarded") {
			t.Fatalf("cancellation not reported: %v\n%s", result.err, result.res.Output)
		}
		assertBrowserStopped(t, result.res.Output)
	case <-time.After(10 * time.Second):
		t.Fatal("browser cancellation did not complete")
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "canceled-result")); !os.IsNotExist(err) {
		t.Fatal("canceled browser work was published")
	}
}
