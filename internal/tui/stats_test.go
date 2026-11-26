package tui

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/attrition-tech/arkex/internal/session"
	"github.com/charmbracelet/x/ansi"
)

func TestStatsHoverDelayAndDismissal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := footerModel(t)
		m.setInput("draft stays here")
		before := m.View()
		offset, count := m.vp.YOffset(), len(m.blocks)
		move := func(x, y int) <-chan tea.Msg {
			_, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: y})
			ch := make(chan tea.Msg, 1)
			if cmd != nil {
				go func() { ch <- cmd() }()
			}
			return ch
		}
		shown := func() bool { return strings.Contains(ansi.Strip(m.View().Content), "Session stats") }
		enter := move(hitCenter(t, m, actModel), m.toolbarY())
		time.Sleep(349 * time.Millisecond)
		select {
		case <-enter:
			t.Fatal("hover timer fired too early")
		default:
		}
		if shown() {
			t.Fatal("stats opened before hover delay")
		}
		time.Sleep(time.Millisecond)
		m.Update(<-enter)
		if !shown() || m.input.Value() != "draft stays here" || m.vp.YOffset() != offset || len(m.blocks) != count || *m.View().Cursor != *before.Cursor {
			t.Fatal("hover failed or changed the draft/transcript/cursor")
		}
		leave := move(0, m.toolbarY())
		move(3, m.vp.Height()-2) // reach the card before the leave timer fires
		time.Sleep(350 * time.Millisecond)
		m.Update(<-leave)
		if !shown() {
			t.Fatal("stale leave timer closed a reentered card")
		}
		leave = move(0, m.toolbarY())
		time.Sleep(350 * time.Millisecond)
		m.Update(<-leave)
		if shown() {
			t.Fatal("stats stayed open after leaving")
		}
		enter = move(hitCenter(t, m, actModel), m.toolbarY())
		typeKeys(m, "!")
		time.Sleep(350 * time.Millisecond)
		m.Update(<-enter)
		if shown() || m.input.Value() != "draft stays here!" {
			t.Fatal("a delayed hover interrupted typing")
		}
	})
}

func TestStatsKeyboardUsageAndPriority(t *testing.T) {
	m := footerModel(t)
	m.mouse = false
	m.session = session.New(m.o.Cwd)
	m.session.Title = "Named in terminal"
	m.session.TitleUsageIn, m.session.TitleUsageOut = 11, 3
	typeKeys(m, "/", "s", "t", "a", "t", "s", "enter")
	plain := strings.Join(strings.Fields(ansi.Strip(m.View().Content)), " ")
	for _, want := range []string{"Session stats", "local/deepseek-v4-flash", "450 / 1000 · 45%", "Session input ↑ 53", "Session output ↓ 10", "First answer —"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("missing %q: %s", want, plain)
		}
	}
	if !strings.Contains(m.View().WindowTitle, "Named in terminal") || strings.Contains(plain, "Named in terminal") {
		t.Fatal("session name must stay in the window title, not the footer")
	}
	m.running = true
	cancelled := false
	m.cancel = func() { cancelled = true }
	typeKeys(m, "esc")
	if strings.Contains(m.View().Content, "Session stats") || cancelled || m.confirmKey != "" {
		t.Fatal("closing stats must not arm or trigger run cancellation")
	}
	m.command("/stats")
	typeKeys(m, "ctrl+p")
	if m.pal == nil || strings.Contains(m.View().Content, "Session stats") {
		t.Fatal("palette must take priority over stats")
	}
	m.closePalette()
	m.command("/stats")
	m.Update(approvalMsg{retry: true, reply: make(chan bool, 1)})
	if m.pending == nil || strings.Contains(m.View().Content, "Session stats") {
		t.Fatal("approval must take priority over stats")
	}
	m.pending = nil
	m.command("/stats")
	m.flash("Steering added to context")
	if strings.Contains(m.View().Content, "Session stats") || !strings.Contains(ansi.Strip(m.View().Content), "Steering added to context") {
		t.Fatal("actionable notices must take priority over stats")
	}
}

func TestStatsShortScreenScrollAndCoveredLinks(t *testing.T) {
	m := footerModel(t)
	m.blocks = []*block{newBlock(blockAssistant, strings.Repeat("[covered](https://example.test/hidden)\n\n", 30))}
	m.layout()
	m.command("/stats")
	// The bottom link is covered; neither destination feedback nor click-through
	// may reveal or activate the underlying link.
	x, y := 3, m.vp.Height()-3
	if !m.statsHit(x, y) {
		t.Fatal("fixture missed the overlay")
	}
	m.Update(tea.MouseMotionMsg{X: x, Y: y})
	if strings.Contains(m.statusBar(), "example.test") {
		t.Fatal("hidden link destination leaked through the panel")
	}
	click(m, x, y)
	if m.sel != nil || m.pal != nil {
		t.Fatal("stats click reached the transcript")
	}
	for _, size := range [][2]int{{60, 20}, {24, 8}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		frameLines(t, m, "short stats")
		seen := ""
		for range 20 {
			seen += ansi.Strip(m.View().Content)
			m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
		}
		if !strings.Contains(seen, "Run elapsed") || !strings.Contains(seen, "/timing") || !strings.Contains(seen, "details") {
			t.Fatalf("stats unreachable at %v: %s", size, seen)
		}
		for range 20 {
			m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
		}
		if !strings.Contains(ansi.Strip(m.View().Content), "local/deepseek") {
			t.Fatal("page up did not return to the first detail")
		}
	}
}
