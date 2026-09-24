package tui

import (
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestNoticeUsesRoundedBorderWithoutBackground(t *testing.T) {
	old := theme
	applyTheme(themes[0])
	t.Cleanup(func() { applyTheme(old) })
	m, _ := testModel(t)
	background := regexp.MustCompile(`\x1b\[(?:[0-9]+;)*(?:4[0-8]|10[0-7])(?:;[0-9]+)*m`)
	for _, text := range []string{"Session resumed", strings.Repeat("A longer notice ", 8)} {
		m.flash(text)
		box, _, _ := m.noticeBox()
		if background.MatchString(box) {
			t.Fatalf("notice sets a background: %q", box)
		}
		plain := ansi.Strip(box)
		if !strings.HasPrefix(plain, "╭") || !strings.HasSuffix(plain, "╯") || !strings.Contains(plain, "╮\n") || !strings.Contains(plain, "\n╰") {
			t.Fatalf("notice lacks rounded corners: %q", plain)
		}
	}
}

func TestAssumedContextHover(t *testing.T) {
	m := footerModel(t)
	m.sess.Agent.SetContextWindow(0)
	m.setInput("draft stays here")
	before := m.View().Content
	count, offset := len(m.blocks), m.vp.YOffset()
	motion(m, hitCenter(t, m, actContext), m.toolbarY())
	box, _, _ := m.noticeBox()
	plain := strings.Join(strings.Fields(ansi.Strip(box)), " ")
	for _, part := range []string{"Assumed context limit: 256k", "80%", "contextWindow", "server-reported"} {
		if !strings.Contains(plain, part) {
			t.Fatalf("tooltip missing %q: %s", part, plain)
		}
	}
	if m.flashText != "" || len(m.blocks) != count || m.vp.YOffset() != offset || m.input.Value() != "draft stays here" {
		t.Fatal("hover must not mutate notices, transcript or draft")
	}
	motion(m, 0, m.statusY())
	if m.View().Content != before {
		t.Fatal("leaving gauge must restore original view")
	}
	motion(m, hitCenter(t, m, actContext), m.toolbarY())
	m.flash("Press Esc again to stop")
	if box, _, _ := m.noticeBox(); strings.Contains(box, "Assumed") || !strings.Contains(box, "Press Esc") {
		t.Fatal("tooltip must yield to notices")
	}
	m.flashText = ""
	m.sess.Agent.SetContextWindow(262144)
	if box, _, _ := m.noticeBox(); box != "" || strings.Contains(ansi.Strip(m.footer()), "ctx*") {
		t.Fatal("known window must not show estimated marker or tooltip")
	}
	m.sess.Agent.SetContextWindow(0)
	for _, size := range [][2]int{{120, 30}, {60, 20}, {24, 8}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.hoverAct = actContext
		box, x, y := m.noticeBox()
		if box != "" && (x < 0 || y < 0 || x+lipgloss.Width(box) > m.width || y+lipgloss.Height(box) > m.vp.Height()) {
			t.Fatalf("tooltip outside viewport at %v", size)
		}
	}
}

func TestNoticeReplacesExpiresAndRestoresTranscript(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := testModel(t)
		m.blocks = []*block{newBlock(blockAssistant, strings.Repeat("Underlying conversation text.\n", 50))}
		m.refresh()
		m.setInput("unfinished draft")
		before := m.View()
		offset, height, count := m.vp.YOffset(), m.vp.Height(), len(m.blocks)
		_, first := m.Update(connectedMsg{testOnly: true, sel: "local/first"})
		firstResult := make(chan tea.Msg, 1)
		go func() { firstResult <- first() }()
		time.Sleep(4 * time.Second)
		_, second := m.Update(connectedMsg{testOnly: true, sel: "local/second"})
		secondResult := make(chan tea.Msg, 1)
		go func() { secondResult <- second() }()
		shown := m.View()
		if plain := ansi.Strip(shown.Content); !strings.Contains(plain, "local/second answered") || strings.Contains(plain, "local/first answered") {
			t.Fatal("notice was not replaced")
		}
		if len(m.blocks) != count || m.vp.YOffset() != offset || m.vp.Height() != height || m.input.Value() != "unfinished draft" || !m.input.Focused() || *shown.Cursor != *before.Cursor {
			t.Fatal("notice moved the transcript or input focus/cursor")
		}
		time.Sleep(time.Second) // the first notice expires, but the second stays
		m.Update(<-firstResult)
		if m.flashText == "" {
			t.Fatal("old expiry cleared a newer notice")
		}
		time.Sleep(3999 * time.Millisecond)
		select {
		case <-secondResult:
			t.Fatal("notice expired before five seconds")
		default:
		}
		time.Sleep(time.Millisecond)
		if _, cmd := m.Update(<-secondResult); cmd != nil {
			t.Fatal("expiry must not start a recurring timer")
		}
		if m.flashText != "" || m.View().Content != before.Content {
			t.Fatal("expiry did not restore the exact underlying view")
		}
	})
}

func TestNoticeGeometryAndCoveredMouseHits(t *testing.T) {
	m, _ := testModel(t)
	for _, width := range []int{100, 60, 20} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		m.flash(strings.Repeat("界 long-session-name ", 40))
		box, x, y := m.noticeBox()
		if x < 0 || x+lipgloss.Width(box) != width-2 || y < 0 || y+lipgloss.Height(box) != m.vp.Height() || lipgloss.Height(box) > 4 {
			t.Fatalf("notice exceeds transcript bounds at width %d", width)
		}
	}
	_, x, y := m.noticeBox()
	tool := &block{kind: blockTool, name: "read", status: "ok"}
	m.spans = []span{{top: m.vp.YOffset() + y, bottom: m.vp.YOffset() + y + 1, b: tool}}
	click(m, x, y)
	if m.openDetail != nil || m.sel != nil {
		t.Fatal("click passed through notice")
	}
	m.Update(flashClearMsg{gen: m.flashGen})
	click(m, x, y)
	if m.openDetail != tool {
		t.Fatal("underlying card was not clickable after expiry")
	}
	m.flash("hidden by menu")
	m.openPalette()
	if box, _, _ := m.noticeBox(); box != "" {
		t.Fatal("notice must not cover the command palette")
	}
}

func TestSuccessfulSettingsDoNotAddTranscriptBlocks(t *testing.T) {
	m := effortModel(t)
	n := len(m.blocks)
	for _, command := range []string{"/effort high", "/mode plan", "/mouse"} {
		m.command(command)
		if len(m.blocks) != n || m.flashText == "" {
			t.Fatalf("%s did not produce a transient notice", command)
		}
	}
	m.command("/effort not-an-effort")
	if len(m.blocks) != n+1 {
		t.Fatal("actionable errors must remain in the transcript")
	}
}

func TestScrollOverlayClickAndGeometry(t *testing.T) {
	m, _ := testModel(t)
	for i := 0; i < 70; i++ {
		m.blocks = append(m.blocks, newBlock(blockSystem, "Earlier conversation"))
	}
	m.refresh()
	m.wheel(-1)
	m.vp.ScrollUp(35)
	m.setInput("keep this draft")
	offset, height, count := m.vp.YOffset(), m.vp.Height(), len(m.blocks)
	box, x, y := m.scrollBox()
	if !strings.Contains(box, "36 lines below") || x != (m.width-lipgloss.Width(box))/2 || y+lipgloss.Height(box) != height {
		t.Fatalf("wrong scroll overlay: %q at %d,%d", box, x, y)
	}
	before := m.View().Content
	m.handleMouse(tea.MouseMotionMsg{X: x + 2, Y: y + 1})
	if !m.scrollHover || m.View().Content == before {
		t.Fatal("missing hover feedback")
	}
	if m.vp.YOffset() != offset || m.vp.Height() != height || len(m.blocks) != count {
		t.Fatal("overlay reflowed the transcript")
	}
	tool := groupTool("read", "covered.go")
	m.spans = []span{{top: offset + y, bottom: offset + y + 3, b: tool}}
	click(m, x+2, y+1)
	if !m.vp.AtBottom() || !m.stickBottom || m.openDetail != nil || m.sel != nil || m.input.Value() != "keep this draft" {
		t.Fatal("jump failed or click passed through overlay")
	}
	if box, _, _ := m.scrollBox(); box != "" {
		t.Fatal("overlay remained at bottom")
	}
	m.blocks = append(m.blocks, newBlock(blockSystem, "new output"))
	m.refresh()
	if !m.vp.AtBottom() {
		t.Fatal("jump did not resume following output")
	}
	for _, width := range []int{100, 60, 20} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		m.wheel(-1)
		m.flash("Settings saved")
		box, x, y = m.scrollBox()
		notice, nx, ny := m.noticeBox()
		if box == "" || x < 0 || y < 0 || x+lipgloss.Width(box) > width || y+lipgloss.Height(box) > m.vp.Height() {
			t.Fatalf("overlay outside viewport at width %d", width)
		}
		if x < nx+lipgloss.Width(notice) && x+lipgloss.Width(box) > nx && y+lipgloss.Height(box) > ny {
			t.Fatal("scroll control overlaps notice")
		}
	}
	m.openPalette()
	if box, _, _ := m.scrollBox(); box != "" {
		t.Fatal("overlay covered menu")
	}
}
