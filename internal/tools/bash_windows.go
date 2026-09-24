//go:build windows

package tools

import (
	"errors"
	"os/exec"
)

func resolveShell(override, environment string) (string, error) {
	if override != "" {
		return override, nil
	}
	if environment != "" {
		if shell, err := exec.LookPath(environment); err == nil {
			return shell, nil
		}
	}
	for _, name := range []string{"bash.exe", "sh.exe"} {
		if shell, err := exec.LookPath(name); err == nil {
			return shell, nil
		}
	}
	return "", errors.New("no POSIX shell found on PATH; install Git Bash, set SHELL, or configure Bash.Shell")
}

// setProcessGroup is a no-op on Windows; exec.CommandContext kills the
// direct child on cancellation.
func setProcessGroup(*exec.Cmd) {}
