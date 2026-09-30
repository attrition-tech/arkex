package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, `<title>Sandbox browser</title><button onclick="this.textContent=42">Run</button>`)
		}))
		defer server.Close()
		// Use the project's version in real work; pin this fixture for repeatability.
		run(t, `set -eu
npm install --prefix "$TMPDIR/pw" --ignore-scripts --no-audit --no-fund playwright@1.58.2
PLAYWRIGHT_BROWSERS_PATH="$TMPDIR/browsers" node "$TMPDIR/pw/node_modules/playwright/cli.js" install chromium --only-shell`)
		command := fmt.Sprintf(`PLAYWRIGHT_BROWSERS_PATH="$TMPDIR/browsers" node - <<'JS'
const { chromium } = require(process.env.TMPDIR + '/pw/node_modules/playwright');
(async () => {
  const browser = await chromium.launch({headless: true});
  try {
    const page = await browser.newPage();
    await page.goto(%q);
    await page.click('button');
    if (await page.title() !== 'Sandbox browser' || await page.textContent('button') !== '42') throw Error('browser result mismatch');
    console.log('browser-ok');
  } finally { await browser.close(); }
})().catch(e => {console.error(e); process.exitCode = 1;});
JS`, server.URL)
		// Fixed, trusted fixture only: a host control distinguishes browser/OS
		// incompatibility from a confinement failure. Production has no fallback.
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		control := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		control.Env = append(os.Environ(), "TMPDIR="+b.TempDir, "HOME="+t.TempDir())
		if output, err := control.CombinedOutput(); err != nil || !strings.Contains(string(output), "browser-ok") {
			t.Fatalf("unsandboxed fixture failed: %v\n%s", err, output)
		}
		t.Log("unsandboxed browser control passed")
		// Separate call: catches scratch publication and stale macOS copy paths.
		output := run(t, command)
		if !strings.Contains(output, "browser-ok") {
			t.Fatal(output)
		}
	})
}
