package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/dantearo/arkex/internal/agent"
)

func TestRecalledPromptScrollAndCopy(t *testing.T) {
	m, _ := testModel(t)
	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	draft := strings.Join(lines, "\n")
	m.hist.add(draft)
	typeKeys(m, "up")
	v := m.View()
	if !strings.Contains(ansi.Strip(m.inputView()), "line 20") || m.input.ScrollYOffset() != 15 {
		t.Fatalf("recalled prompt not scrolled to end: offset=%d\n%s", m.input.ScrollYOffset(), m.inputView())
	}
	if v.Cursor == nil || v.Cursor.Y < m.inputY() || v.Cursor.Y >= m.inputY()+m.input.Height() {
		t.Fatalf("cursor outside input: %+v", v.Cursor)
	}
	y := m.inputY()
	m.handleMouse(tea.MouseClickMsg{X: 2, Y: y, Button: tea.MouseLeft})
	m.handleMouse(tea.MouseMotionMsg{X: 6, Y: y, Button: tea.MouseLeft})
	m.handleMouse(tea.MouseReleaseMsg{X: 6, Y: y, Button: tea.MouseLeft})
	if got := m.input.SelectedText(); got != "line" {
		t.Fatalf("prompt selection=%q", got)
	}
	_, cmd := m.Update(key("ctrl+c"))
	if cmd == nil || m.input.Value() != draft || !strings.Contains(m.flashText, "copied 4 chars") {
		t.Fatal("copy changed draft or failed to produce clipboard command")
	}
	typeKeys(m, "esc")
	if m.input.HasSelection() || m.input.Value() != draft {
		t.Fatal("escape must clear selection without changing draft")
	}
}

func TestStaleSelectionAcrossClear(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = []*block{newBlock(blockSystem, "first second")}
	m.refresh()
	m.selectPress(2, 0)
	m.selectDrag(6, 0)
	m.selectRelease()
	old := m.sel.gen
	m.clearSelection()
	m.selectPress(8, 0)
	m.selectDrag(13, 0)
	m.selectRelease()
	_, cmd := m.Update(copySelectionMsg{gen: old})
	if cmd != nil || m.flashText != "" {
		t.Fatal("old timer copied a new selection")
	}
}

func TestSmallTerminalDecisionsAndInput(t *testing.T) {
	for _, size := range [][2]int{{24, 8}, {40, 12}, {80, 10}, {80, 24}} {
		for _, state := range []string{"input", "approval", "retry"} {
			t.Run(fmt.Sprintf("%dx%d/%s", size[0], size[1], state), func(t *testing.T) {
				m, _ := testModel(t)
				m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				switch state {
				case "input":
					m.setInput(strings.Repeat("日本語 test\n", 20))
				case "approval":
					m.Update(approvalMsg{call: agent.ToolCall{Name: "write", Input: `{"path":"demo.txt","content":"one\ntwo\nthree"}`, Grantable: true}, reply: make(chan agent.Answer, 1)})
				case "retry":
					m.Update(approvalMsg{retry: true, err: agent.ErrMalformedToolResponse, reply: make(chan agent.Answer, 1)})
				}
				v := m.View()
				rows := strings.Split(v.Content, "\n")
				if len(rows) != size[1] {
					t.Fatalf("height=%d want=%d\n%s", len(rows), size[1], ansi.Strip(v.Content))
				}
				for _, row := range rows {
					if ansi.StringWidth(row) > size[0] {
						t.Fatalf("overwide row: %q", ansi.Strip(row))
					}
				}
				if m.pending != nil {
					for _, hit := range m.pending.hits {
						y := m.approvalButtonY() + hit.y
						if y >= size[1]-footerRows || hit.x1 > size[0]-1 {
							t.Fatalf("inaccessible action: %+v y=%d", hit, y)
						}
					}
				}
			})
		}
	}
}

func TestWheelTargetsPromptAndScrollbarPreservesText(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = []*block{newBlock(blockSystem, strings.Repeat("transcript\n", 80))}
	m.refresh()
	m.setInput(strings.Repeat("draft\n", 20))
	off := m.vp.YOffset()
	inputOff := m.input.ScrollYOffset()
	m.handleMouse(tea.MouseWheelMsg{X: 3, Y: m.inputY() + 1, Button: tea.MouseWheelUp})
	if m.vp.YOffset() != off || m.input.ScrollYOffset() != inputOff-1 {
		t.Fatal("input wheel scrolled transcript or failed to move input")
	}
	m.handleMouse(tea.MouseWheelMsg{X: 3, Y: 1, Button: tea.MouseWheelUp})
	if m.vp.YOffset() != off-1 {
		t.Fatal("transcript wheel must move exactly one line")
	}
	for _, offset := range []int{0, 45, 95} {
		got := ansi.Strip(scrollbar("abc  │\ndef  │\nghi  │\njkl  │\nmno  │", 5, 5, 100, offset))
		rows := strings.Split(got, "\n")
		for _, s := range rows {
			if ansi.StringWidth(s) != 6 || !strings.HasSuffix(s, "│") {
				t.Fatalf("scrollbar damaged border: %q", s)
			}
		}
		wantRow := offset * 4 / 95
		if !strings.Contains(rows[wantRow], "┃") {
			t.Fatalf("wrong thumb position: %s", got)
		}
	}
}

func TestCopyCodeSource(t *testing.T) {
	input := "Prose `inline`.\n\n~~~python\nprint('```')\n~~~\n\n    indented\n"
	if got := codeBlocks(input); !reflect.DeepEqual(got, []string{"print('```')\n", "indented\n"}) {
		t.Fatalf("code=%q", got)
	}
}

func TestCopyPaletteRetainsSelection(t *testing.T) {
	m, _ := testModel(t)
	m.setInput("alpha beta")
	m.input.BeginSelection(0, 0)
	m.input.ExtendSelection(5, 0)
	m.input.EndSelection()
	m.openPalette()
	// Clicking a palette row clears the visual selection, but not the value
	// captured when opening the copy menu.
	m.input.ClearSelection()
	for _, item := range m.pal.level().items {
		if item.title == "Copy selection" {
			item.action(m)
			if m.pal != nil || !strings.Contains(m.flashText, "copied 5 chars") || m.input.Value() != "alpha beta" {
				t.Fatal("palette copy lost selection or changed draft")
			}
			return
		}
	}
	t.Fatal("copy action missing")
}

func TestTranscriptResetReleasesHiddenReferences(t *testing.T) {
	m, _ := testModel(t)
	m.o.Connect = nil // exercise the supplied-agent new-session path
	for i := 0; i < 25; i++ {
		m.blocks = append(m.blocks, workTurn()...)
	}
	m.refresh()
	m.hoverBlock, m.openDetail = m.blocks[2], m.blocks[2]
	m.newSession()
	if m.hoverBlock != nil || m.openDetail != nil || m.historyCache.first != nil {
		t.Fatal("new session retained old inspection/cache")
	}
	for _, s := range m.spans[len(m.spans):cap(m.spans)] {
		if s.b != nil {
			t.Fatal("span backing array retains hidden block")
		}
	}
	for _, line := range m.lineBuf[len(m.lineBuf):cap(m.lineBuf)] {
		if line != "" {
			t.Fatal("line backing array retains hidden text")
		}
	}
}

func BenchmarkLongHistoryFrame(b *testing.B) {
	for _, action := range []string{"stream", "typing"} {
		b.Run(action, func(b *testing.B) {
			m, _ := testModel(b)
			for i := 0; i < 2000; i++ {
				m.blocks = append(m.blocks, workTurn()...)
			}
			m.blocks = append(m.blocks, newBlock(blockUser, "live"), newBlock(blockAssistant, "response"))
			m.running = true
			m.refresh()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if action == "stream" {
					m.Update(eventsMsg{events: []agent.Event{agent.TextDelta{Text: "word "}}})
				} else {
					m.Update(key("x"))
					if i%80 == 79 {
						m.setInput("")
					}
				}
				m.View()
			}
		})
	}
}

func TestHistoryCacheMatchesFullRender(t *testing.T) {
	m, _ := testModel(t)
	for i := 0; i < 20; i++ {
		m.blocks = append(m.blocks, workTurn()...)
	}
	m.blocks = append(m.blocks, newBlock(blockUser, "live"), newBlock(blockReasoning, "current thought"))
	m.running = true
	m.refresh()
	for _, change := range []string{"delta", "resize", "hover", "inspect", "close", "finish", "clear"} {
		switch change {
		case "delta":
			m.blocks[len(m.blocks)-1].text.WriteString(" more")
		case "resize":
			m.width = 65
		case "hover":
			m.hoverBlock = m.blocks[2]
		case "inspect":
			m.hoverBlock = nil
			m.openDetail = m.blocks[2]
		case "close":
			m.openDetail, m.openWork, m.openGroup = nil, nil, nil
		case "finish":
			m.running = false
		case "clear":
			m.blocks = []*block{newBlock(blockUser, "new"), newBlock(blockAssistant, "fresh")}
		}
		m.refresh()
		got := strings.Join(m.vp.lines, "\n")
		spans := append([]span(nil), m.spans...)
		m.historyCache.turn = nil
		m.refresh()
		if got != strings.Join(m.vp.lines, "\n") || !reflect.DeepEqual(spans, m.spans) {
			t.Fatalf("cache differs after %s", change)
		}
	}
}
