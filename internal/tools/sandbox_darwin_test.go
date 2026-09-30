package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDarwinBrowserExactIPC(t *testing.T) {
	// Compile a fixed native probe, never code supplied through the tool. It
	// exercises the actual bootstrap APIs and the production browser profile.
	stock, writable, outside := t.TempDir(), t.TempDir(), t.TempDir()
	source := filepath.Join(stock, "probe.c")
	program := filepath.Join(stock, "probe")
	code := `#include <servers/bootstrap.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>

int main(int argc, char **argv) {
  char own[256];
  snprintf(own, sizeof(own), "org.chromium.Chromium.MachPortRendezvousServer.%d", getpid());
  mach_port_t port = MACH_PORT_NULL;
  kern_return_t kr = bootstrap_check_in(bootstrap_port, own, &port);
  if (kr != KERN_SUCCESS) { fprintf(stderr, "own check-in: %d pid=%d\n", kr, getpid()); return 10; }
  if (argc == 1) { puts(own); fflush(stdout); sleep(60); return 0; }
  kr = bootstrap_look_up(bootstrap_port, own, &port);
  if (kr != KERN_SUCCESS) { fprintf(stderr, "own lookup: %d\n", kr); return 11; }
  const char *blocked[] = {"org.chromium.Chromium.MachPortRendezvousServer.1", argv[1]};
  for (int i = 0; i < 2; i++) {
    kr = bootstrap_check_in(bootstrap_port, blocked[i], &port);
    if (kr != BOOTSTRAP_NOT_PRIVILEGED) { fprintf(stderr, "foreign check-in: %d\n", kr); return 12; }
    kr = bootstrap_look_up(bootstrap_port, blocked[i], &port);
    if (kr != BOOTSTRAP_NOT_PRIVILEGED) { fprintf(stderr, "foreign lookup: %d\n", kr); return 13; }
  }
  FILE *f = fopen(argv[2], "w");
  if (!f) return 14;
  fputs("private-ok", f); fclose(f);
  f = fopen(argv[3], "w");
  if (f) { fclose(f); return 15; }
  execl("/bin/sh", "sh", "-c", "exit 99", NULL);
  if (errno != EPERM && errno != EACCES) return 16;
  puts("exact-ipc-ok");
  return 0;
}`
	if err := os.WriteFile(source, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "/usr/bin/clang", source, "-o", program).CombinedOutput(); err != nil {
		t.Fatalf("compile native IPC probe: %v\n%s", err, output)
	}
	// A real host registration distinguishes denied lookup from absent service.
	host := exec.CommandContext(ctx, program)
	setProcessGroup(host)
	stdout, err := host.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Cancel(); _ = host.Wait() }()
	name, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	// Canonical paths matter: macOS exposes /var through /private/var.
	stock, err = filepath.EvalSymlinks(stock)
	if err != nil {
		t.Fatal(err)
	}
	writable, err = filepath.EvalSymlinks(writable)
	if err != nil {
		t.Fatal(err)
	}
	program = filepath.Join(stock, "probe")
	privateFile, outsideFile := filepath.Join(writable, "result"), filepath.Join(outside, "unchanged")
	if err := os.WriteFile(outsideFile, []byte("outside-ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := darwinBrowserProfile([]*sandboxTree{{execution: writable}}, stock)
	cmd := darwinBrowserCommand(ctx, profile, program, strings.TrimSpace(name), privateFile, outsideFile)
	if output, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(output), "exact-ipc-ok") {
		t.Fatalf("PID preservation / IPC boundary: %v\n%s", err, output)
	}
	sandboxContents(t, privateFile, "private-ok")
	sandboxContents(t, outsideFile, "outside-ok")
	// The ordinary profile must reject even the process's own registration.
	ordinary := darwinBrowserCommand(ctx, darwinProfile(nil), program)
	output, err := ordinary.CombinedOutput()
	if err == nil || !strings.Contains(string(output), fmt.Sprintf("own check-in: %d", 1100)) {
		t.Fatalf("ordinary shell gained registration: %v\n%s", err, output)
	}
}
