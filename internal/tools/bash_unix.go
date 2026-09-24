//go:build unix

package tools

import (
	"errors"
	"os/exec"
	"syscall"
)

func resolveShell(override, environment string) (string, error) {
	if override != "" {
		return override, nil
	}
	if environment != "" {
		return environment, nil
	}
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		return "", errors.New("no POSIX shell found; set SHELL or configure Bash.Shell")
	}
	return "/bin/sh", nil
}

// setProcessGroup puts the child in its own process group and kills the whole
// group on cancellation so background children do not outlive the tool call.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
