package tui

import (
	"fmt"
	"image/color"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"

	"github.com/attrition-tech/arkex/internal/config"
)

// Theme is the set of colour roles every style in the TUI is built from.
// The default theme uses warm gray text and terminal ANSI accents;
// the others use their named palettes.
type Theme struct {
	Name string
	Desc string

	Accent   color.Color // headings, user bar, build mode, selection
	Muted    color.Color // dim text, borders, reasoning
	Text     color.Color // tool output body
	OnAccent color.Color // text drawn on Accent/Plan/Auto/Status backgrounds
	Tool     color.Color // tool names
	OK       color.Color // success, added lines
	Err      color.Color // errors, removed lines
	Warn     color.Color // auto mode badge
	Plan     color.Color // plan mode badge
	StatusBg color.Color // status bar background
	StatusFg color.Color // status bar text
	HoverBg  color.Color // fill under the row or control the mouse is over

	Markdown ansi.StyleConfig
}

func c(s string) color.Color { return lipgloss.Color(s) }

var themes = []Theme{
	{
		Name: "default", Desc: "warm gray text with your terminal's accent colours",
		Accent: c("12"), Muted: c("8"), Text: c("#b7b1a3"), OnAccent: c("0"), Tool: c("6"),
		OK: c("2"), Err: c("1"), Warn: c("11"), Plan: c("14"), StatusBg: c("4"), StatusFg: c("0"), HoverBg: c("236"),
		Markdown: styles.DarkStyleConfig,
	},
	{
		Name: "light", Desc: "terminal colours tuned for a light background",
		Accent: c("4"), Muted: c("8"), Text: c("0"), OnAccent: c("15"), Tool: c("6"),
		OK: c("2"), Err: c("1"), Warn: c("3"), Plan: c("6"), StatusBg: c("4"), StatusFg: c("15"), HoverBg: c("254"),
		Markdown: styles.LightStyleConfig,
	},
	{
		Name: "catppuccin", Desc: "Catppuccin Mocha",
		Accent: c("#89b4fa"), Muted: c("#6c7086"), Text: c("#cdd6f4"), OnAccent: c("#1e1e2e"), Tool: c("#94e2d5"),
		OK: c("#a6e3a1"), Err: c("#f38ba8"), Warn: c("#f9e2af"), Plan: c("#cba6f7"), StatusBg: c("#313244"), StatusFg: c("#cdd6f4"), HoverBg: c("#313244"),
		Markdown: styles.DarkStyleConfig,
	},
	{
		Name: "dracula", Desc: "Dracula",
		Accent: c("#bd93f9"), Muted: c("#6272a4"), Text: c("#f8f8f2"), OnAccent: c("#282a36"), Tool: c("#8be9fd"),
		OK: c("#50fa7b"), Err: c("#ff5555"), Warn: c("#f1fa8c"), Plan: c("#ff79c6"), StatusBg: c("#44475a"), StatusFg: c("#f8f8f2"), HoverBg: c("#44475a"),
		Markdown: styles.DraculaStyleConfig,
	},
	{
		Name: "gruvbox", Desc: "Gruvbox dark",
		Accent: c("#83a598"), Muted: c("#928374"), Text: c("#ebdbb2"), OnAccent: c("#282828"), Tool: c("#8ec07c"),
		OK: c("#b8bb26"), Err: c("#fb4934"), Warn: c("#fabd2f"), Plan: c("#d3869b"), StatusBg: c("#3c3836"), StatusFg: c("#ebdbb2"), HoverBg: c("#3c3836"),
		Markdown: styles.DarkStyleConfig,
	},
	{
		Name: "nord", Desc: "Nord",
		Accent: c("#88c0d0"), Muted: c("#4c566a"), Text: c("#d8dee9"), OnAccent: c("#2e3440"), Tool: c("#8fbcbb"),
		OK: c("#a3be8c"), Err: c("#bf616a"), Warn: c("#ebcb8b"), Plan: c("#b48ead"), StatusBg: c("#3b4252"), StatusFg: c("#d8dee9"),
		Markdown: styles.DarkStyleConfig,
	},
	{
		Name: "tokyo-night", Desc: "Tokyo Night",
		Accent: c("#7aa2f7"), Muted: c("#565f89"), Text: c("#c0caf5"), OnAccent: c("#1a1b26"), Tool: c("#7dcfff"),
		OK: c("#9ece6a"), Err: c("#f7768e"), Warn: c("#e0af68"), Plan: c("#bb9af7"), StatusBg: c("#292e42"), StatusFg: c("#c0caf5"),
		Markdown: styles.TokyoNightStyleConfig,
	},
}

// themeByName looks a theme up case-insensitively; "" is the default.
func themeByName(name string) (Theme, bool) {
	if name == "" {
		name = "default"
	}
	for _, t := range themes {
		if strings.EqualFold(t.Name, name) {
			return t, true
		}
	}
	return Theme{}, false
}

// themeNames lists the available themes, default first, rest sorted.
func themeNames() []string {
	names := make([]string, 0, len(themes))
	for _, t := range themes {
		if t.Name != "default" {
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	return append([]string{"default"}, names...)
}

// Current theme state. themeGen changes on every applyTheme so cached
// renders (markdown, banner) know to redo themselves.
var (
	theme    Theme
	themeGen int
)

// applyTheme rebuilds every package-level style from t.
func applyTheme(t Theme) {
	if t.HoverBg == nil {
		t.HoverBg = t.StatusBg
	}
	if t.Name == "default" {
		text := "#b7b1a3"
		t.Markdown.Document.Color = &text
	}
	code := "6"
	if t.Name != "default" && t.Name != "light" {
		r, g, b, _ := t.Tool.RGBA()
		code = fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
	}
	t.Markdown.Code.Color = &code
	t.Markdown.Code.BackgroundColor = nil
	t.Markdown.Code.Prefix, t.Markdown.Code.Suffix = "", ""
	theme = t
	themeGen++
	base := lipgloss.NewStyle().Foreground(t.Text)

	// render.go
	textStyle = base
	userBarStyle = base.Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(t.Accent).PaddingLeft(1)
	reasoningStyle = base.Foreground(t.Muted).Italic(true)
	toolStyle = base.Foreground(t.Tool)
	okStyle = base.Foreground(t.OK)
	errStyle = base.Foreground(t.Err)
	dimStyle = base.Foreground(t.Muted)
	addStyle = base.Foreground(t.OK)
	delStyle = base.Foreground(t.Err)
	modeBuildStyle = base.Foreground(t.OnAccent).Background(t.Accent).Bold(true).Padding(0, 1)
	modePlanStyle = base.Foreground(t.OnAccent).Background(t.Plan).Bold(true).Padding(0, 1)
	modeAutoStyle = base.Foreground(t.OnAccent).Background(t.Warn).Bold(true).Padding(0, 1)
	borderStyle = base.Border(lipgloss.RoundedBorder()).BorderForeground(t.Muted).Padding(0, 1)
	pillStyle = base.Foreground(t.Text).Background(t.HoverBg).Padding(0, 1)
	pillHoverStyle = base.Foreground(t.OnAccent).Background(t.Text).Padding(0, 1)
	gaugeStyle = base.Foreground(t.Accent)
	gaugeWarnStyle = base.Foreground(t.Warn)
	hintsStyle = base.Foreground(t.Muted)
	hoverStyle = base.Background(t.HoverBg)
	toolBodyStyle = base.Foreground(t.Text).PaddingLeft(2)
	toolBodyDimmed = base.Foreground(t.Muted).PaddingLeft(2)

	// select.go
	markStyle = base.Foreground(t.OnAccent).Background(t.Accent)

	// attach.go
	chipStyle = base.Foreground(t.Text).Background(t.HoverBg)
	chipXStyle = base.Foreground(t.Muted).Background(t.HoverBg)
	chipXHoverStyle = base.Foreground(t.OnAccent).Background(t.Text)

	// complete.go
	popupBorder = base.Border(lipgloss.RoundedBorder()).BorderForeground(t.Muted)
	popupSel = base.Foreground(t.OnAccent).Background(t.Accent).Bold(true)
	popupDesc = base.Foreground(t.Muted)
	popupSelDsc = base.Foreground(t.OnAccent).Background(t.Accent)

	// palette.go
	paletteTitle = base.Bold(true)
	paletteGroup = base.Foreground(t.Muted).Bold(true)
	paletteRule = base.Foreground(t.Muted)
	paletteBorder = base.Border(lipgloss.RoundedBorder()).BorderForeground(t.Accent)

	// models.go
	titleStyle = base.Bold(true).Foreground(t.Accent)
	selStyle = base.Foreground(t.Accent)
	disabledStyle = base.Foreground(t.Muted).Strikethrough(true)
	keyStyle = base.Foreground(t.Tool)

	// form.go
	formLabel = base.Foreground(t.Muted)
	formField = base.Underline(true)
	formFieldFoc = base.Underline(true).Foreground(t.Accent)
	formButton = base.Foreground(t.Accent).Background(t.HoverBg)
	formButtonHv = base.Foreground(t.OnAccent).Background(t.Text)
	formButtonFc = base.Foreground(t.OnAccent).Background(t.Accent).Bold(true)
	formChoice = base.Foreground(t.Text).Background(t.HoverBg)
	formChoiceOn = base.Foreground(t.OnAccent).Background(t.Accent).Bold(true)
	formChoiceHv = base.Foreground(t.OnAccent).Background(t.Text)

	// fillRow re-arms this after every reset inside a row it lights.
	hoverSeq = strings.TrimSuffix(strings.TrimSuffix(hoverStyle.Render("x"), "\x1b[m"), "x")
}

// setTheme switches the theme, persists it under ui.theme, and redraws.
func (m *model) setTheme(name string) {
	t, ok := themeByName(name)
	if !ok {
		m.appendSystem("unknown theme " + name + " — available: " + strings.Join(themeNames(), ", "))
		m.refresh()
		return
	}
	applyTheme(t)
	m.applyInputStyles()
	m.renderWelcome()
	if m.o.ConfigPath != "" {
		var v any = t.Name
		if t.Name == "default" {
			v = nil
		}
		if err := config.SetUI(m.o.ConfigPath, "theme", v); err != nil {
			m.appendSystem("could not save ui.theme: " + err.Error())
		}
	}
	m.flash("theme: " + t.Name + " · " + t.Desc)
	m.refresh()
}

// applyInputStyles colours the textarea prompt and placeholder from the
// current theme. The textarea keeps its own style set, so applyTheme cannot
// reach it.
func (m *model) applyInputStyles() {
	s := textarea.DefaultDarkStyles()
	if theme.Name == "light" {
		s = textarea.DefaultLightStyles()
	}
	for _, st := range []*textarea.StyleState{&s.Focused, &s.Blurred} {
		st.Base = st.Base.UnsetBackground()
		st.CursorLine = lipgloss.NewStyle() // no shaded current line, including the placeholder
		st.Placeholder = lipgloss.NewStyle().Foreground(theme.Muted)
		st.Prompt = lipgloss.NewStyle() // empty prompt must not reset the text foreground
		st.Text = lipgloss.NewStyle().Foreground(theme.Text)
	}
	m.input.SetStyles(s)
}

// themeItems is the palette sub-list for /theme.
func themeItems(*model) []paletteItem {
	var items []paletteItem
	for _, name := range themeNames() {
		t, _ := themeByName(name)
		hint := t.Desc
		if t.Name == theme.Name {
			hint = "current · " + hint
		}
		items = append(items, paletteItem{group: "Themes", title: t.Name, hint: hint, cmd: "/theme " + t.Name})
	}
	return items
}

func init() {
	t, _ := themeByName("default")
	applyTheme(t)
}
