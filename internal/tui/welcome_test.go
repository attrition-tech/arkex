package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/session"
)

func TestWelcomeContentAndStableGreeting(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	m := newModel(Options{Cwd: "/workspace/notes", Version: "v-test"})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if m.panel != nil || !m.input.Focused() || !m.onWelcome() {
		t.Fatal("startup must focus the welcome input, not open Connections")
	}
	text := ansi.Strip(strings.Join(frameLines(t, m, "welcome"), "\n"))
	for _, want := range []string{"arkex", "/workspace/notes", m.start.greeting, welcomePlaceholder, "Connect a model ▾"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	for _, removed := range []string{"v-test", "/connections", "Ctrl+P", "/resume", "Enter to send", "PICK UP", "/ Commands", "command palette", "new session"} {
		if strings.Contains(text, removed) {
			t.Fatalf("unwanted welcome label %q", removed)
		}
	}
	greeting := m.start.greeting
	typeText(m, "a draft")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.setTheme("light")
	t.Cleanup(func() { applyTheme(themes[0]) })
	m.setSession(Connection{Agent: &agent.Agent{}, Name: "other/current-model"})
	if got := ansi.Strip(m.View().Content); !strings.Contains(got, "other/current-model ▾") {
		t.Fatalf("selector must use current connection, not startup options: %s", got)
	}
	if m.start.greeting != greeting {
		t.Fatal("typing, resize, theme or model change rerolled the greeting")
	}
	// A sentinel avoids a probabilistic assertion about two random draws.
	m.start.greeting = "sentinel"
	m.o.Connect = nil
	m.newSession()
	if !m.onWelcome() || m.start.greeting == "" || m.start.greeting == "sentinel" {
		t.Fatal("a fresh session did not select a greeting")
	}
}

func TestWelcomeRecentWorkspaceSessionsAndResume(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	cwd := t.TempDir()
	save := func(cwd, title, state string, updated time.Time) *session.Session {
		s := session.New(cwd)
		s.Update([]fantasy.Message{fantasy.NewUserMessage(title)}, session.Usage{Model: "local/test"})
		s.Title, s.State, s.Updated = title, state, updated
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		return s
	}
	now := time.Now()
	var sessions []*session.Session
	for i := range 7 {
		sessions = append(sessions, save(cwd, fmt.Sprintf("Session %d", i), "", now.Add(-time.Duration(i+1)*time.Hour)))
	}
	save(cwd, "Hidden archive", session.Archived, now)
	save(cwd, "Hidden trash", session.Trash, now)
	save(t.TempDir(), "Other workspace", "", now)
	for _, method := range []string{"keyboard", "mouse"} {
		t.Run(method, func(t *testing.T) {
			m := newModel(Options{Cwd: cwd, Session: Connection{Agent: &agent.Agent{}, Name: "local/test"}})
			m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			text := ansi.Strip(m.View().Content)
			if len(m.start.recent) != 5 {
				t.Fatalf("recent count = %d", len(m.start.recent))
			}
			for i, s := range m.start.recent {
				if s.ID != sessions[i].ID || !strings.Contains(text, s.Title) {
					t.Fatalf("recent %d = %+v; not newest-first", i, s)
				}
			}
			for _, hidden := range []string{"Hidden", "Other workspace", "Session 5", "Session 6"} {
				if strings.Contains(text, hidden) {
					t.Fatalf("unwanted session %q in %s", hidden, text)
				}
			}
			if method == "keyboard" {
				typeKeys(m, "tab", "tab", "down") // model, newest, second newest
				if m.start.focus != 3 || m.View().Cursor != nil {
					t.Fatal("Tab/Down did not focus the second session")
				}
				typeKeys(m, "enter")
			} else {
				g := m.welcomeGeometry()
				before := m.View().Content
				motion(m, g.x+4, g.recent+1)
				if m.start.hover != 3 || m.View().Content == before || ansi.Strip(m.View().Content) != ansi.Strip(before) {
					t.Fatal("session hover should change style, not text or geometry")
				}
				click(m, g.x+4, g.recent+1)
			}
			if m.conv == nil || m.conv.ID != sessions[1].ID || m.onWelcome() {
				t.Fatal("did not resume the selected session")
			}
			frameLines(t, m, "resumed chat")
			if m.input.Width() != 96 || m.View().Cursor == nil || m.View().Cursor.X != 2 {
				t.Fatal("resume did not restore conversation input geometry")
			}
		})
	}
}

func TestWelcomeDraftConnectAndSubmit(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	m, _ := testModel(t)
	m.setInput("Keep this draft")
	m.attachments = []attachment{{name: "image.png", mediaType: "image/png", data: []byte("test")}}
	m.layout()
	greeting := m.start.greeting
	typeKeys(m, "enter")
	if m.panel == nil || m.input.Value() != "Keep this draft" || len(m.attachments) != 1 || m.conv != nil {
		t.Fatal("sending without a model must open Connections without consuming the draft")
	}
	m.closeModels()
	typeKeys(m, "tab", "enter")
	if m.panel == nil {
		t.Fatal("keyboard Connect a model did not open Connections")
	}
	m.closeModels()
	g := m.welcomeGeometry()
	click(m, m.width/2, g.model)
	if m.panel == nil {
		t.Fatal("mouse Connect a model did not open Connections")
	}
	m.closeModels()
	m.setSession(Connection{Agent: &agent.Agent{}, Name: "local/current"})
	typeKeys(m, "tab", "enter")
	if m.pal == nil || m.pal.level().title != "Switch model" {
		t.Fatal("connected selector did not open model picker")
	}
	m.closePalette()
	if m.start.greeting != greeting || m.input.Value() != "Keep this draft" {
		t.Fatal("connection dialogs changed the greeting or draft")
	}
	// Exercise the actual Enter path, but don't execute the returned network command.
	typeKeys(m, "enter")
	if m.onWelcome() || !m.running || m.conv == nil || m.input.Value() != "" || len(m.attachments) != 0 {
		t.Fatal("first send did not enter chat")
	}
	b := m.blocks[len(m.blocks)-1]
	if b.kind != blockUser || b.text.String() != "Keep this draft" || len(b.files) != 1 {
		t.Fatalf("sent draft damaged: %+v", b)
	}
	frameLines(t, m, "first send")
	if m.input.Width() != 96 || m.View().Cursor == nil || m.View().Cursor.X != 2 {
		t.Fatal("first send did not restore conversation composer")
	}
}

func TestWelcomeSmallScreensAndAttachments(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	for _, size := range [][2]int{{24, 8}, {40, 8}, {40, 12}, {80, 24}, {120, 34}} {
		for _, draft := range []string{"", strings.Repeat("日本語 draft\n", 12)} {
			m, _ := testModel(t)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.start.recent = make([]session.Summary, 5)
			for i := range m.start.recent {
				m.start.recent[i] = session.Summary{Title: strings.Repeat("Long 界 title ", 12), Updated: time.Now().Add(-24 * time.Hour)}
			}
			m.attachments = []attachment{{name: "a.png"}, {name: "b.jpg"}}
			m.setInput(draft)
			frameLines(t, m, fmt.Sprint(size, draft == ""))
			g := m.welcomeGeometry()
			c := m.View().Cursor
			if g.model >= m.height || c == nil || c.Y < g.input || c.Y >= g.rule || c.X < g.x+2 || c.X >= m.width {
				t.Fatalf("input/model inaccessible at %v: geometry=%+v cursor=%+v", size, g, c)
			}
			if m.input.Height() != min(inputMaxRows, m.inputRows) {
				t.Fatal("secondary content squeezed the input")
			}
			if len(m.chipHits) > 0 {
				x := g.x + 2 + m.chipHits[0].x0
				motion(m, x, g.chips)
				if m.hoverChip != 1 {
					t.Fatal("welcome chip hover missed")
				}
				click(m, x, g.chips)
				if len(m.attachments) != 1 || m.attachments[0].name != "b.jpg" {
					t.Fatal("welcome chip click removed wrong attachment")
				}
			}
			m.openPalette()
			frameLines(t, m, "welcome palette")
			m.closePalette()
			for i := range 20 {
				m.files = append(m.files, fmt.Sprintf("note-%02d.txt", i))
			}
			m.setInput("@n")
			m.updateCompletion()
			m.comp.sel = 19
			frameLines(t, m, "welcome file popup")
			if size[0] >= 40 && !strings.Contains(m.View().Content, "note-19.txt") {
				t.Fatal("popup hid the selected completion")
			}
			typeKeys(m, "tab")
			if m.start.focus != 0 || !strings.Contains(m.input.Value(), ".") {
				t.Fatal("welcome stole Tab from file completion")
			}
		}
	}
}
