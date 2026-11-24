package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestReplyLinksClickHoverAndKeyboardCopy(t *testing.T) {
	opened := linkBrowser(t)
	const first = "https://example.test/first?a=1&b=2"
	const second = "http://example.test/second"
	const source = "日本語 🧭 [essay](" + first + ") then prose.\n\n> [a longer link label ending wrappedword](" + second + ")\n\n| Link | Note |\n| --- | --- |\n| [tablelink](https://example.test/table) | 🧭 |\n\n```text\nhttps://codeonly.test\n```\n\nPlain http://bare.test and [again](" + first + ")."
	for _, width := range []int{28, 100} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("width%d/streaming%v", width, streaming), func(t *testing.T) {
				m, _ := testModel(t)
				m.width, m.height, m.running = width, 80, streaming
				b := newBlock(blockAssistant, source)
				m.blocks = []*block{b}
				if streaming {
					m.Update(m.streamMarkdown()())
					b.text.WriteString("\n\nLater text not yet painted.")
				}
				m.layout()
				if !strings.Contains(m.View().Content, "\x1b]8;") {
					t.Fatal("native terminal hyperlinks were removed")
				}
				for _, tc := range []struct{ label, target string }{{"essay", first}, {"wrappedword", second}, {"tablelink", "https://example.test/table"}, {"bare.test", "http://bare.test"}} {
					x, y := linkPosition(t, m, tc.label)
					m.Update(tea.MouseMotionMsg{X: x, Y: y})
					if hint := ansi.Strip(m.statusBar()); !strings.Contains(hint, "Click to open") || width == 100 && !strings.Contains(hint, tc.target) {
						t.Fatalf("missing destination feedback: %q", hint)
					}
					m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
					_, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
					if cmd == nil {
						t.Fatalf("click on %q produced no browser action", tc.label)
					}
					m.Update(cmd())
					opened(tc.target)
					m.flashText = ""
				}
				x, y := linkPosition(t, m, "codeonly")
				m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				if _, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft}); cmd != nil {
					t.Fatal("a URL inside a code sample must not open")
				}
				m.running = false
				m.refresh()
				typeKeys(m, "tab", "enter", "l", "i", "n", "k", "s", "enter")
				if m.pal == nil || len(m.pal.view) != 4 {
					t.Fatal("link menu must exclude code examples and deduplicate destinations")
				}
				typeKeys(m, "enter", "down")
				_, cmd := m.Update(key("enter"))
				if got := copiedText(t, cmd); got != first {
					t.Fatalf("keyboard copied %q; want exact URL %q", got, first)
				}
			})
		}
	}
}

func TestReplyLinkSelectionAndChangingContentNeverOpen(t *testing.T) {
	linkBrowser(t)
	for _, action := range []string{"drag", "drag back", "release elsewhere", "reflow", "scroll", "palette"} {
		t.Run(action, func(t *testing.T) {
			m, _ := testModel(t)
			m.blocks = []*block{newBlock(blockAssistant, "[selectable](https://example.test/first)\n\n[replacement](https://example.test/second)")}
			m.refresh()
			x, y := linkPosition(t, m, "selectable")
			m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			switch action {
			case "drag", "drag back":
				m.Update(tea.MouseMotionMsg{X: x + 5, Y: y, Button: tea.MouseLeft})
				if action == "drag back" {
					m.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
				} else {
					x += 5
				}
			case "release elsewhere":
				x, y = linkPosition(t, m, "replacement")
			case "reflow":
				m.blocks = []*block{newBlock(blockAssistant, "[selectable](https://example.test/changed)")}
				m.refresh()
			case "scroll":
				m.blocks = append(m.blocks, newBlock(blockSystem, strings.Repeat("filler\n", 80)))
				m.refresh()
			case "palette":
				m.openPalette()
			}
			_, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
			if action == "drag" {
				if m.selectedText() != "select" {
					t.Fatalf("drag lost selection: %q", m.selectedText())
				}
				if cmd == nil {
					t.Fatal("drag did not schedule auto-copy")
				}
				_, copyCmd := m.Update(cmd())
				if got := copiedText(t, copyCmd); got != "select" {
					t.Fatalf("auto-copy=%q", got)
				}
			} else if cmd != nil {
				t.Fatalf("%s unexpectedly produced a command", action)
			}
		})
	}
}

func TestReplyLinkInReopenedWork(t *testing.T) {
	opened := linkBrowser(t)
	m, _ := testModel(t)
	m.blocks = workTurn()
	head := newBlock(blockAssistant, "Checking [the docs](https://example.test/docs).")
	m.blocks[1] = head
	m.layout()
	for range 2 {
		clickWork(t, m, head)
		x, y := linkPosition(t, m, "the docs")
		m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		_, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
		if cmd == nil {
			t.Fatal("link in expanded work did not open")
		}
		m.Update(cmd())
		opened("https://example.test/docs")
		m.flashText = ""
		clickWork(t, m, head)
	}
}

func TestReplyLinkSafetyRemoteAndOpenFailure(t *testing.T) {
	linkBrowser(t)
	for _, target := range []string{"javascript:alert(1)", "file:///tmp/a", "mailto:a@example.test", "https://", "https://user:pass@example.test", "//example.test", "https://example.test/\u202eexe", "https://example.test\\evil"} {
		m, _ := testModel(t)
		m.blocks = []*block{newBlock(blockAssistant, "[bad](<"+target+">)")}
		m.refresh()
		x, y := linkPosition(t, m, "bad")
		m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		if _, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft}); cmd != nil {
			t.Fatalf("unsafe click admitted: %q", target)
		}
		typeKeys(m, "tab", "enter")
		if strings.Contains(ansi.Strip(m.View().Content), "Links") {
			t.Fatalf("unsafe target offered in link actions: %q", target)
		}
	}
	m, _ := testModel(t)
	m.blocks = []*block{newBlock(blockAssistant, "[essay](https://example.test/essay)")}
	m.refresh()
	x, y := linkPosition(t, m, "essay")
	t.Setenv("SSH_CONNECTION", "test remote connection")
	click(m, x, y)
	if !strings.Contains(m.flashText, "SSH:") || !strings.Contains(m.View().Content, "\x1b]8;") {
		t.Fatal("remote click must retain native links and explain local opening")
	}
	m.flashText = ""
	typeKeys(m, "tab", "enter", "l", "i", "n", "k", "s", "enter", "enter")
	_, cmd := m.Update(key("enter"))
	if cmd != nil || m.pal == nil || !strings.Contains(ansi.Strip(m.View().Content), "Over SSH") {
		t.Fatal("remote browser action was not disabled with an explanation")
	}
	typeKeys(m, "down")
	_, cmd = m.Update(key("enter"))
	if got := copiedText(t, cmd); got != "https://example.test/essay" {
		t.Fatalf("remote copy=%q", got)
	}
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("BROWSER", filepath.Join(t.TempDir(), "missing-browser"))
	m.flashText = ""
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	_, cmd = m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	if cmd == nil {
		t.Fatal("local link was not activated")
	}
	m.Update(cmd())
	for _, width := range []int{28, 100} {
		m.width = width
		m.layout()
		view := ansi.Strip(m.View().Content)
		if !strings.Contains(view, "browser") || !strings.Contains(view, "Copy URL") {
			t.Fatalf("browser start failure needs a visible Copy URL fallback at width %d", width)
		}
	}
}

func linkPosition(t *testing.T, m *model, label string) (int, int) {
	t.Helper()
	for y, row := range strings.Split(ansi.Strip(m.vp.View()), "\n") {
		if i := strings.Index(row, label); i >= 0 {
			return ansi.StringWidth(row[:i]), y
		}
	}
	t.Fatalf("visible link label %q missing:\n%s", label, transcript(m))
	return 0, 0
}

// Exercise the real browser-launch boundary without navigating or opening a GUI.
func linkBrowser(t *testing.T) func(string) {
	t.Helper()
	for _, name := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		t.Setenv(name, "")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "opened")
	browser := filepath.Join(dir, "browser")
	if err := os.WriteFile(browser, []byte("#!/bin/sh\nprintf '%s' \"$1\" > \"$ARKEX_TEST_LINK\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSER", browser)
	t.Setenv("ARKEX_TEST_LINK", path)
	return func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			got, _ := os.ReadFile(path)
			if string(got) == want {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("browser received %q; want %q", got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
