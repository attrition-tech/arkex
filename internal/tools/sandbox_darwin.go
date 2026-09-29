package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func platformSandbox(ctx context.Context, trees []*sandboxTree, workdir, program string, argv []string) (*exec.Cmd, func(), error) {
	const seatbelt = "/usr/bin/sandbox-exec"
	if _, err := os.Stat(seatbelt); err != nil {
		return nil, nil, fmt.Errorf("macOS shell tools require %s (execution blocked): %w", seatbelt, err)
	}
	var profile strings.Builder
	profile.WriteString(`(version 1)
(deny default)
(allow file-read* file-map-executable process-exec process-fork sysctl-read)
(allow signal (target same-sandbox))
(allow process-info* (target same-sandbox))
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
	args := append([]string{"-p", profile.String(), program}, argv...)
	cmd := exec.CommandContext(ctx, seatbelt, args...)
	cmd.Dir = workdir
	return cmd, func() {}, nil
}
