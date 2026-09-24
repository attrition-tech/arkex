package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestBackdropRemovesHighlightsWithoutChangingText(t *testing.T) {
	s := "\x1b[1;31;44mbright 界\x1b[0m\n  second line  "
	got := muteBackdrop(s)
	if ansi.Strip(got) != ansi.Strip(s) || strings.Contains(got, "31;44") || !strings.Contains(got, "\x1b[") {
		t.Fatalf("backdrop lost geometry or kept highlights: %q", got)
	}
}

func TestPaletteBackdropRestoresFrame(t *testing.T) {
	for _, themeChoice := range []Theme{themes[0], themes[1]} {
		m := footerModel(t)
		old := theme
		applyTheme(themeChoice)
		t.Cleanup(func() { applyTheme(old) })
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
		m.blocks = append(m.blocks, newBlock(blockAssistant, strings.Repeat("Background conversation\n", 40)))
		m.refresh()
		m.setInput("draft remains")
		before := m.View().Content
		offset := m.vp.YOffset()
		m.openPalette()
		shown := m.View().Content
		if shown == before || !strings.HasSuffix(shown, muteBackdrop(m.inputView())+"\n"+muteBackdrop(m.footer())) {
			t.Fatal("palette did not mute composer and footer")
		}
		frameLines(t, m, "muted palette")
		m.closePalette()
		if m.View().Content != before || m.vp.YOffset() != offset || m.input.Value() != "draft remains" {
			t.Fatal("closing palette changed underlying content, offset, or draft")
		}
	}
}
