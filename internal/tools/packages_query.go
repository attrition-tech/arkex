package tools

import (
	"errors"
	"fmt"
	"regexp"
)

func queryPackage(m packageManager, in packageInput, run packageRunner) (Result, error) {
	var args []string
	switch m.Name {
	case "brew":
		if in.Action == "search" {
			args = []string{"search", "--formula", in.Package}
		} else {
			// Includes canonical name, stable/installed versions and dependencies.
			args = []string{"info", "--json=v2", "--formula", "homebrew/core/" + in.Package}
		}
	case "apt":
		// Do not refresh or write APT's on-disk caches. policy includes the
		// installed version, candidates, priorities and configured sources.
		args = []string{"-o", "Dir::Cache::pkgcache=", "-o", "Dir::Cache::srcpkgcache="}
		if in.Action == "search" {
			args = append(args, "search", "--names-only", regexp.QuoteMeta(in.Package))
		} else {
			args = append(args, "policy", in.Package)
		}
	case "dnf":
		// The runner supplies cache-only, disabled plugins and scratch logs.
		args = []string{in.Action, in.Package}
	default:
		return Result{}, errors.New("unsupported package manager")
	}
	out, err := run(m.Executable, args...)
	if err != nil {
		return Result{Output: out}, fmt.Errorf("%s %s failed; cached metadata may be missing; no unrestricted fallback: %w", m.Name, in.Action, err)
	}
	if out == "" {
		out = "No matching package metadata found. This does not prove the executable is absent; inspect existing installations through bash."
	}
	return Result{Summary: m.Name + " " + in.Action + " " + in.Package, Output: out}, nil
}
