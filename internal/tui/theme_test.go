package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"

	"github.com/attrition-tech/arkex/internal/config"
)

func TestInputHasNoPromptOrBackgroundInAnyTheme(t *testing.T) {
	m, _ := testModel(t)
	t.Cleanup(func() { applyTheme(themes[0]) })
	for _, theme := range themes {
		applyTheme(theme)
		m.applyInputStyles()
		if m.input.Prompt != "" {
			t.Fatalf("%s: prompt %q", theme.Name, m.input.Prompt)
		}
		s := m.input.Styles()
		for _, state := range []textarea.StyleState{s.Focused, s.Blurred} {
			for _, style := range []lipgloss.Style{state.Base, state.CursorLine, state.Text, state.Placeholder} {
				if _, ok := style.GetBackground().(lipgloss.NoColor); !ok {
					t.Fatalf("%s: input background is set", theme.Name)
				}
			}
		}
	}
}

func TestDefaultTextIsWarmInStreamingAndMarkdown(t *testing.T) {
	m, _ := testModel(t)
	applyTheme(themes[0])
	const warm = "\x1b[38;2;183;177;163m"
	for _, streaming := range []bool{true, false} {
		b := newBlock(blockAssistant, "Readable reply")
		m.blocks = []*block{b}
		m.running = streaming
		if streaming {
			m.Update(m.streamMarkdown()())
		}
		got := strings.Join(m.blockLines(b, m.width-4, streaming), "\n")
		if !strings.Contains(got, warm) {
			t.Fatalf("streaming=%v: missing warm text: %q", streaming, got)
		}
	}
	if got := m.paletteRow(paletteItem{title: "Session"}, 60, false); !strings.Contains(got, warm) {
		t.Fatalf("palette inherited terminal white: %q", got)
	}
	m.applyInputStyles()
	m.input.SetValue("Readable input")
	if got := m.inputView(); !strings.Contains(got, warm+"Readable input") {
		t.Fatalf("empty prompt reset input foreground: %q", got)
	}
}

func TestControlsUseDistinctBackgroundHover(t *testing.T) {
	t.Cleanup(func() { applyTheme(themes[0]) })
	for _, th := range themes {
		applyTheme(th)
		for _, pair := range [][2]lipgloss.Style{{pillStyle, pillHoverStyle}, {formButton, formButtonHv}, {formChoice, formChoiceHv}, {formButtonFc, formButtonHv}} {
			if pair[1].GetUnderline() || pair[0].GetBackground() == pair[1].GetBackground() || pair[0].GetBackground() == nil {
				t.Fatalf("%s: missing distinct background-only hover", th.Name)
			}
			if lipgloss.Width(pair[0].Render("button")) != lipgloss.Width(pair[1].Render("button")) {
				t.Fatal("hover changes hit target width")
			}
		}
	}
}

func TestThemeSwitchPersistsAndRerenders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ARKEX_HOME", home)
	m, _ := testModel(t)
	m.o.ConfigPath = home + "/config.json"
	t.Cleanup(func() { applyTheme(themes[0]) })

	// Cached markdown must be redone after a theme change.
	b := newBlock(blockAssistant, "# hi")
	m.blocks = append(m.blocks, b)
	m.refresh()
	key := b.key
	before := m.welcome.rendered

	m.command("/theme dracula")
	if theme.Name != "dracula" {
		t.Fatalf("theme = %q", theme.Name)
	}
	cfg, err := config.Load("")
	if err != nil || cfg.UI.Theme != "dracula" {
		t.Fatalf("saved theme = %q err = %v", cfg.UI.Theme, err)
	}
	m.refresh()
	if b.key == key {
		t.Fatal("markdown cache not invalidated")
	}
	if m.welcome.rendered == before {
		t.Fatal("banner not redrawn")
	}
	if !strings.Contains(m.flashText, "theme: dracula") {
		t.Fatalf("notice = %q", m.flashText)
	}

	// Back to default removes the key instead of writing "default".
	m.command("/theme default")
	cfg, _ = config.Load("")
	if cfg.UI.Theme != "" {
		t.Fatalf("ui.theme after default = %q", cfg.UI.Theme)
	}

	m.command("/theme solarized")
	if note := m.blocks[len(m.blocks)-1].text.String(); !strings.Contains(note, "unknown theme solarized") || !strings.Contains(note, "tokyo-night") {
		t.Fatalf("note = %q", note)
	}
	if theme.Name != "default" {
		t.Fatal("unknown theme must not change the current one")
	}
}

func TestThemePaletteAndStartup(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	m, _ := testModel(t)
	t.Cleanup(func() { applyTheme(themes[0]) })
	m.command("/theme")
	if m.pal == nil || m.pal.level().title != "Theme" {
		t.Fatal("/theme without a name must open the palette sub-list")
	}
	got := titles(m.pal.level().items)
	if !strings.HasPrefix(got, "default") || !strings.Contains(got, "catppuccin") || len(m.pal.level().items) != len(themes) {
		t.Fatalf("items = %q", got)
	}

	// Startup with an unknown theme falls back and says so.
	m2 := newModel(Options{Cwd: t.TempDir(), UI: config.UI{Theme: "nope"}})
	if theme.Name != "default" || !strings.Contains(m2.blocks[len(m2.blocks)-1].text.String(), `unknown ui.theme "nope"`) {
		t.Fatalf("startup fallback: theme=%q blocks=%d", theme.Name, len(m2.blocks))
	}
	m3 := newModel(Options{Cwd: t.TempDir(), UI: config.UI{Theme: "Nord"}})
	if theme.Name != "nord" || len(m3.blocks) != 1 {
		t.Fatalf("startup theme = %q", theme.Name)
	}
}
