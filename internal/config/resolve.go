package config

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// ResolveValue expands a config string value.
//
//	"!cmd args"   run the command, use trimmed stdout
//	"$NAME"       environment variable (also "${NAME}", inside literals)
//	"$$" / "$!"   literal "$" / "!"
//
// A referenced environment variable that is unset is an error, so a missing
// key fails loudly instead of sending an empty credential.
func ResolveValue(ctx context.Context, v string) (string, error) {
	if strings.HasPrefix(v, "$!") {
		return "!" + v[2:], nil
	}
	if strings.HasPrefix(v, "!") {
		return runCommand(ctx, v[1:])
	}
	var missing []string
	out := envRef.ReplaceAllStringFunc(v, func(m string) string {
		name := strings.Trim(m[1:], "{}")
		if val, ok := os.LookupEnv(name); ok {
			return val
		}
		missing = append(missing, name)
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable %s is not set", strings.Join(missing, ", "))
	}
	return strings.ReplaceAll(out, "$$", "$"), nil
}

func runCommand(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell(), "-c", command)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolving value via command failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func shell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}
