package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Installed versions are package-manager records, not guesses about which
// executable a formula provides. Runtime suitability is checked in the sandbox.
func formulaInventory(output string) map[string][]string {
	installed := make(map[string][]string)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			installed[strings.TrimPrefix(fields[0], "homebrew/core/")] = fields[1:]
		}
	}
	return installed
}

func installFormula(ctx context.Context, brew, name, version string, run packageRunner) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	formula := "homebrew/core/" + name
	out, err := run(brew, "list", "--formula", "--versions")
	if err != nil {
		return Result{Output: out}, fmt.Errorf("cannot inspect installed formulas: %w", err)
	}
	installed := formulaInventory(out)
	if versions := installed[name]; len(versions) > 0 {
		if version != "" && !slices.Contains(versions, version) {
			return Result{}, fmt.Errorf("%s already has versions %v; requested %s; automatic upgrades/downgrades are not permitted", name, versions, version)
		}
		return Result{Summary: name + " already installed", Output: fmt.Sprintf("Homebrew reports %s versions %v. No installation performed. Verify project compatibility and executable availability through bash.", name, versions)}, nil
	}
	out, err = run(brew, "info", "--json=v2", "--formula", formula)
	if err != nil {
		return Result{Output: out}, fmt.Errorf("cannot resolve formula: %w", err)
	}
	var info struct {
		Formulae []struct {
			FullName string `json:"full_name"`
			Tap      string `json:"tap"`
			Revision int    `json:"revision"`
			Versions struct {
				Stable string `json:"stable"`
			} `json:"versions"`
		} `json:"formulae"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil || len(info.Formulae) != 1 {
		return Result{}, errors.New("invalid formula metadata; no installation performed")
	}
	f := info.Formulae[0]
	if f.FullName != name || f.Tap != "homebrew/core" || f.Versions.Stable == "" || f.Revision < 0 {
		return Result{}, errors.New("installation requires a canonical official core formula with a stable version; use info to resolve the package name")
	}
	resolved := f.Versions.Stable
	if f.Revision > 0 {
		resolved += "_" + strconv.Itoa(f.Revision)
	}
	if version != "" && version != resolved {
		return Result{}, fmt.Errorf("requested version %s is not the available stable package version %s; no installation performed", version, resolved)
	}
	out, err = run(brew, "deps", "--formula", "--topological", "--full-name", formula)
	if err != nil {
		return Result{Output: out}, fmt.Errorf("cannot resolve official dependencies: %w", err)
	}
	formulas := []string{}
	seen := map[string]bool{name: true}
	for _, dependency := range strings.Fields(out) {
		dependency = strings.TrimPrefix(dependency, "homebrew/core/")
		if !packageName.MatchString(dependency) {
			return Result{}, fmt.Errorf("unsupported dependency %q; no installation performed", dependency)
		}
		if len(installed[dependency]) == 0 && !seen[dependency] {
			formulas = append(formulas, "homebrew/core/"+dependency)
			seen[dependency] = true
		}
	}
	formulas = append(formulas, formula)
	// --force-bottle does not propagate into Homebrew dependency installers.
	// Install the complete missing closure in order with recursion disabled,
	// so every actual installation is explicitly bottle-only and fail-fast.
	for _, selected := range formulas {
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("installation interrupted; earlier dependencies may remain installed: %w", err)
		}
		out, err = run(brew, "install", "--formula", "--force-bottle", "--ignore-dependencies", "--no-ask", selected)
		if err != nil {
			return Result{Output: out}, fmt.Errorf("installation failed for %s; earlier dependencies may remain installed; no unrestricted fallback: %w", selected, err)
		}
	}
	out, err = run(brew, "list", "--formula", "--versions", formula)
	if err != nil || !slices.Contains(formulaInventory(out)[name], resolved) {
		return Result{Output: out}, fmt.Errorf("installation verification failed for %s %s (packages may remain installed): %v", name, resolved, err)
	}
	return Result{Summary: name + " installation verified", Output: fmt.Sprintf("Homebrew reports %s %s installed. Verify executable availability and project compatibility through bash. Ordinary tools remain confined to the workspace and active scratch.", name, resolved)}, nil
}
