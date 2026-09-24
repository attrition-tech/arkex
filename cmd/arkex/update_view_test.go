package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestUpdateViewPlain(t *testing.T) {
	var out bytes.Buffer
	v := newUpdateView(&out)
	v.start()
	v.progress(512, 1024)
	v.logf("testing new binary")
	v.finish()
	if out.String() != "testing new binary\n" {
		t.Fatalf("piped output = %q", out.String())
	}
}

func TestUpdateViewProgress(t *testing.T) {
	var out bytes.Buffer
	v := newUpdateView(&out)
	v.terminal = true
	v.progress(0, 4096)
	n := out.Len()
	v.progress(1536, 4096)
	if out.Len() != n {
		t.Fatal("unthrottled intermediate draw")
	}
	v.lastDraw = time.Time{}
	v.progress(4095, 4096)
	if !strings.Contains(out.String(), "99%") || strings.Contains(out.String(), "100%") {
		t.Fatalf("premature completion: %q", out.String())
	}
	v.progress(4096, 4096)
	v.logf("signature and checksum verified")
	if !strings.Contains(out.String(), "100%") || !strings.Contains(out.String(), "\n  ✓ signature and checksum verified\n") {
		t.Fatalf("completion: %q", out.String())
	}
	if v.active {
		t.Fatal("progress row still active")
	}
}

func TestUpdateViewFlushAndWidth(t *testing.T) {
	for _, total := range []int64{-1, 4096} {
		var out bytes.Buffer
		v := newUpdateView(&out)
		v.terminal, v.width = true, 40
		v.progress(0, total)
		v.progress(1024, total)
		v.finish() // includes failure: never invent completion
		got := ansi.Strip(out.String())
		if !strings.Contains(got, "1.0 KiB") || strings.Contains(got, "100%") {
			t.Fatalf("flush total %d: %q", total, got)
		}
		if total < 0 && strings.Contains(got, "%") {
			t.Fatalf("unknown length has percentage: %q", got)
		}
		for _, line := range strings.Split(strings.TrimSpace(got), "\r") {
			if ansi.StringWidth(line) >= v.width {
				t.Fatalf("row wraps: %q", line)
			}
		}
	}
}
