package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"

	"github.com/dantearo/arkex/internal/sanitize"
	"github.com/dantearo/arkex/internal/tui"
)

// updateView is a single in-place download row, not a full-screen program.
// It draws only on byte arrivals (at most 8 fps); there is no animation loop.
type updateView struct {
	out             io.Writer
	file            *os.File
	terminal        bool
	width           int
	accent          lipgloss.Style
	lastDraw        time.Time
	received, total int64
	active          bool
}

func newUpdateView(out io.Writer) *updateView {
	v := &updateView{out: out, width: 80}
	if f, ok := out.(*os.File); ok && term.IsTerminal(f.Fd()) && os.Getenv("TERM") != "dumb" {
		v.terminal, v.file = true, f
		if os.Getenv("NO_COLOR") == "" {
			v.accent = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
		}
	}
	return v
}

func (v *updateView) start() {
	if v.terminal {
		_, _ = fmt.Fprintln(v.out, v.accent.Render(strings.Trim(tui.Banner, "\n")))
		v.logf("checking for updates…")
	}
}

func (v *updateView) logf(format string, args ...any) {
	v.finish()
	line := sanitize.Terminal(fmt.Sprintf(format, args...))
	if v.terminal {
		prefix := "  · "
		if line == "signature and checksum verified" {
			prefix = "  ✓ "
		}
		line = prefix + line
	}
	_, _ = fmt.Fprintln(v.out, line)
}

func (v *updateView) progress(received, total int64) {
	if !v.terminal {
		return
	}
	first := !v.active
	v.active, v.received, v.total = true, received, total
	if !first && time.Since(v.lastDraw) < time.Second/8 && (total <= 0 || received < total) {
		return
	}
	v.draw()
}

func (v *updateView) draw() {
	if v.file != nil {
		if width, _, err := term.GetSize(v.file.Fd()); err == nil {
			v.width = width
		}
	}
	label := fmt.Sprintf("  Download  %s received", updateBytes(v.received))
	if v.total > 0 {
		fraction := min(1.0, float64(v.received)/float64(v.total))
		tail := fmt.Sprintf(" %3d%%  %s / %s", int(fraction*100), updateBytes(v.received), updateBytes(v.total))
		cells := max(4, min(24, v.width-14-ansi.StringWidth(tail)))
		filled := int(fraction * float64(cells))
		bar := v.accent.Render(strings.Repeat("━", filled)) + strings.Repeat("─", cells-filled)
		label = "  Download  " + bar + tail
	}
	_, _ = fmt.Fprint(v.out, "\r\x1b[2K", ansi.Truncate(label, max(1, v.width-1), "…"))
	v.lastDraw = time.Now()
}

func (v *updateView) finish() {
	if v.active {
		v.draw() // flush the last bytes even for unknown lengths or failures
		_, _ = fmt.Fprintln(v.out)
		v.active = false
	}
}

func updateBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
}
