package main

import (
	"os"
	"runtime/pprof"
)

// startProfile writes a CPU profile to path until the returned func is called.
// It is a development aid (ARKEX_CPUPROFILE=/tmp/cpu.out arkex); nil path or
// an unwritable file disables it silently.
func startProfile(path string) func() {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		return nil
	}
	return func() {
		pprof.StopCPUProfile()
		_ = f.Close()
	}
}
