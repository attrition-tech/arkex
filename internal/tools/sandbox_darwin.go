package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func platformSandbox(ctx context.Context, trees []*sandboxTree, workdir, program string, argv []string) (*exec.Cmd, func(), error) {
	const seatbelt = "/usr/bin/sandbox-exec"
	if _, err := os.Stat(seatbelt); err != nil {
		return nil, nil, fmt.Errorf("macOS shell tools require %s (execution blocked): %w", seatbelt, err)
	}
	args := append([]string{"-p", darwinProfile(trees), program}, argv...)
	cmd := exec.CommandContext(ctx, seatbelt, args...)
	cmd.Dir = workdir
	return cmd, func() {}, nil
}

func darwinProfile(trees []*sandboxTree) string {
	var profile strings.Builder
	profile.WriteString(`(version 1)
(deny default)
(allow file-read* file-map-executable process-exec process-fork sysctl-read)
(allow signal (target same-sandbox))
(allow process-info* (target same-sandbox))
; Power notifications used during Chromium startup. Older Chromium releases
; dereference a null notification port when this specific client is denied.
(allow iokit-open (iokit-user-client-class "RootDomainUserClient"))
(allow mach-lookup
  (global-name "com.apple.system.opendirectoryd.libinfo")
  (global-name "com.apple.system.opendirectoryd.membership")
  (global-name "com.apple.SystemConfiguration.DNSConfiguration")
  (global-name "com.apple.SystemConfiguration.configd")
  (global-name "com.apple.networkd")
  (global-name "com.apple.SecurityServer")
  (global-name "com.apple.ocspd")
  (global-name "com.apple.trustd.agent")
  (global-name "com.apple.TrustEvaluationAgent"))
(allow system-socket (require-all (socket-domain AF_SYSTEM) (socket-protocol 2)))
; DNS-SD/libinfo queries use this Unix socket, not just Mach configuration
; services. Socket creation alone grants no access to other host endpoints.
(allow system-socket (socket-domain AF_UNIX))
(allow network-outbound
  (remote unix-socket (literal "/private/var/run/mDNSResponder"))
  (remote unix-socket (literal "/var/run/mDNSResponder")))
(allow network-outbound (remote ip "*:*"))
(allow network-bind network-inbound (local ip "*:*"))
(deny system-fcntl (fcntl-command 80 110))
(deny file-link)
(allow file-write-data (literal "/dev/null") (literal "/dev/zero"))
`)
	for _, tree := range trees {
		fmt.Fprintf(&profile, "(allow file-write* (subpath %s))\n", strconv.Quote(tree.execution))
		// Keep the root itself in place: replacing it with a symlink must
		// not redirect a later tool call's writable-root resolution.
		fmt.Fprintf(&profile, "(deny file-write-unlink (literal %s))\n", strconv.Quote(tree.execution))
	}
	return profile.String()
}

func executeShellSandbox(ctx context.Context, run *sandboxRun, scratch, requestedBrowser string) error {
	if requestedBrowser != "" {
		browser, err := startBrowser(ctx, run, scratch)
		if err != nil {
			return fmt.Errorf("managed browser startup failed; command not run and unpublished changes discarded: %w", err)
		}
		// Stop before capture/publication; both processes share the private trees.
		defer browser.Close()
		run.cmd.Env = append(run.cmd.Env, "ARKEX_BROWSER_WS_ENDPOINT="+browser.endpoint)
	}
	return run.cmd.Run()
}

func startBrowser(ctx context.Context, run *sandboxRun, scratch string) (*managedBrowser, error) {
	stock := filepath.Join(run.temp, "browser-stock")
	program, err := prepareChromium(ctx, run.runtimePath(scratch), stock, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	data := filepath.Join(run.temp, "browser-data")
	if err := os.Mkdir(data, 0o700); err != nil {
		return nil, err
	}
	trees := append(append([]*sandboxTree(nil), run.trees...), &sandboxTree{execution: data})
	args := []string{"--headless", "--no-sandbox", "--disable-gpu", "--disable-breakpad", "--no-first-run", "--disable-background-networking", "--remote-debugging-port=0", "--user-data-dir=" + filepath.Join(data, "profile")}
	cmd := darwinBrowserCommand(ctx, darwinBrowserProfile(trees, stock), program, args...)
	cmd.Dir = data
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + data, "TMPDIR=" + data, "TMP=" + data, "TEMP=" + data}
	return launchManagedBrowser(ctx, cmd)
}

func darwinBrowserProfile(trees []*sandboxTree, stock string) string {
	profile := darwinProfile(trees) + `
(allow mach-register mach-lookup
  (global-name (string-append "org.chromium.Chromium.MachPortRendezvousServer." (param "ARKEX_BROWSER_PID"))))
`
	// Only this verified stock distribution may execute under the extra grant.
	// Neither model-selected programs nor loader/environment overrides enter it.
	return profile + fmt.Sprintf("(deny process-exec (require-not (subpath %s)))\n", strconv.Quote(stock))
}

// The fixed bootstrap and sandbox-exec both exec, preserving the browser PID.
// No user command is interpolated or run before applying the profile. Ordinary
// shells never receive this grant, including children of the model's command.
func darwinBrowserCommand(ctx context.Context, profile, program string, args ...string) *exec.Cmd {
	bootstrap := `profile=$1; shift; exec /usr/bin/sandbox-exec -D "ARKEX_BROWSER_PID=$$" -p "$profile" "$@"`
	argv := append([]string{"-c", bootstrap, "arkex-browser", profile, program}, args...)
	return exec.CommandContext(ctx, "/bin/sh", argv...)
}
