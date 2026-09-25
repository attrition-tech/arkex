package tui

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/provider"
)

// ---- scroller ----

func TestScrollerViewIsExactlyHeightLines(t *testing.T) {
	s := &scroller{}
	s.SetWidth(10)
	s.SetHeight(4)

	// Fewer lines than the window: padded with blank rows, top-aligned.
	s.SetLines([]string{"a", "b"})
	if got := s.View(); got != "a\nb\n\n" {
		t.Fatalf("short view = %q", got)
	}
	// More lines than the window: GotoBottom shows the tail.
	s.SetLines([]string{"1", "2", "3", "4", "5", "6"})
	s.GotoBottom()
	if got := s.View(); got != "3\n4\n5\n6" || !s.AtBottom() || s.YOffset() != 2 {
		t.Fatalf("bottom view = %q off=%d atBottom=%v", got, s.YOffset(), s.AtBottom())
	}
	s.ScrollUp(10)
	if s.YOffset() != 0 || s.AtBottom() {
		t.Fatalf("scroll past top: off=%d", s.YOffset())
	}
	s.ScrollDown(1)
	if got := s.View(); got != "2\n3\n4\n5" {
		t.Fatalf("offset 1 view = %q", got)
	}
	// Content shrinks under a deep offset: the offset is clamped, not stale.
	s.GotoBottom()
	s.SetLines([]string{"x"})
	if s.YOffset() != 0 || s.View() != "x\n\n\n" {
		t.Fatalf("after shrink: off=%d view=%q", s.YOffset(), s.View())
	}
}

func TestScrollerClipsOnlyOverlongLines(t *testing.T) {
	s := &scroller{}
	s.SetWidth(6)
	s.SetHeight(3)
	styled := lipgloss.NewStyle().Bold(true).Render("abcdefgh") // wide: clipped to 6 cells
	wideBytes := "ééééé"                                        // 10 bytes, 5 cells: must not be clipped
	s.SetLines([]string{styled, wideBytes, "ok"})
	got := strings.Split(s.View(), "\n")
	if w := ansi.StringWidth(got[0]); w != 6 || ansi.Strip(got[0]) != "abcdef" {
		t.Fatalf("styled line: width %d text %q", w, ansi.Strip(got[0]))
	}
	if got[1] != wideBytes {
		t.Fatalf("multi-byte line altered: %q", got[1])
	}
}

// ---- incremental wrapping ----

// TestWrapStreamingMatchesFullWrap feeds text in random-sized chunks and
// checks that the incremental result equals a from-scratch wrap at every
// step (ignoring trailing blank lines, which the incremental path may show
// while a paragraph break is still arriving).
func TestWrapStreamingMatchesFullWrap(t *testing.T) {
	text := "\n\n  Leading blanks are dropped.\nA second line that is definitely long enough to wrap at least once at the width we use here.\n\n" +
		"Paragraph two has a `code span` and **bold** text, plus a very-long-token-without-spaces-that-must-be-hard-wrapped-somewhere.\n\n" +
		"- bullet one\n- bullet two\n\nfinal words   \n\n"
	const inner = 40
	wrap := lipgloss.NewStyle().Width(inner)
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 20; trial++ {
		b := &block{kind: blockAssistant}
		b.key = renderKey{gen: themeGen, width: inner, streaming: true}
		var got []string
		first := true
		for pos := 0; pos < len(text); {
			n := 1 + rng.Intn(12)
			if pos+n > len(text) {
				n = len(text) - pos
			}
			b.text.WriteString(text[pos : pos+n])
			pos += n
			got = wrapStreaming(b, inner, nil, first)
			first = false

			want := []string{}
			if s := strings.TrimSpace(b.text.String()); s != "" {
				want = strings.Split(wrap.Render(s), "\n")
			}
			if g, w := strings.Join(trimBlankTail(got), "\n"), strings.Join(trimBlankTail(want), "\n"); g != w {
				t.Fatalf("trial %d at %d bytes:\n got: %q\nwant: %q", trial, pos, g, w)
			}
		}
	}
}

// trimBlankTail drops trailing blank lines and each line's trailing padding
// (lipgloss pads a block to its widest line, which differs when a hard-wrapped
// token is wider than the width; the renderer clears the row anyway).
func trimBlankTail(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, strings.TrimRight(l, " "))
	}
	for len(out) > 0 && strings.TrimSpace(ansi.Strip(out[len(out)-1])) == "" {
		out = out[:len(out)-1]
	}
	return out
}

// TestWrapStreamingCommitsSoftBreaks is the worst case for a naive wrapper: one
// long paragraph with no newlines, arriving a few bytes at a time. The
// incremental result must still match a from-scratch wrap and most of the
// text must be committed (not re-wrapped every frame).
func TestWrapStreamingCommitsSoftBreaks(t *testing.T) {
	words := []string{"alpha", "be", "gamma-delta", "epsilonzetaetathetaiotakappalambdamuxiomicronpirhosigma", "tau", "u", "phi"}
	var sb strings.Builder
	rng := rand.New(rand.NewSource(3))
	for sb.Len() < 3000 {
		sb.WriteString(words[rng.Intn(len(words))])
		sb.WriteString(strings.Repeat(" ", 1+rng.Intn(2)))
	}
	text := sb.String()
	const inner = 37
	wrap := lipgloss.NewStyle().Width(inner)
	b := &block{kind: blockAssistant}
	first := true
	var got []string
	for pos := 0; pos < len(text); {
		n := 1 + rng.Intn(3)
		if pos+n > len(text) {
			n = len(text) - pos
		}
		b.text.WriteString(text[pos : pos+n])
		pos += n
		got = wrapStreaming(b, inner, nil, first)
		first = false
		want := strings.Split(wrap.Render(strings.TrimSpace(b.text.String())), "\n")
		if g, w := strings.Join(trimBlankTail(got), "\n"), strings.Join(trimBlankTail(want), "\n"); g != w {
			t.Fatalf("at %d bytes:\n got: %q\nwant: %q", pos, g, w)
		}
	}
	if b.wrapDone < len(text)*9/10 || len(b.wrapLines) < len(got)-2 {
		t.Fatalf("soft breaks not committed: wrapDone=%d of %d, committed %d of %d lines", b.wrapDone, len(text), len(b.wrapLines), len(got))
	}
}

func TestWrapStreamingRestartsOnWidthChange(t *testing.T) {
	b := &block{kind: blockAssistant}
	b.text.WriteString("one two three four five six seven eight nine ten\nsecond line here\ntail")
	at40 := wrapStreaming(b, 40, nil, true)
	at20 := wrapStreaming(b, 20, nil, true) // restart: prefix must be re-wrapped, not reused
	if len(at20) <= len(at40) {
		t.Fatalf("narrower width should yield more lines: %d vs %d", len(at20), len(at40))
	}
	for _, l := range at20 {
		if w := ansi.StringWidth(l); w > 20 {
			t.Fatalf("line wider than 20 after restart: %q (%d)", l, w)
		}
	}
}

// ---- block cache ----

func TestBlockCacheReusesUnchangedBlocks(t *testing.T) {
	m, _ := testModel(t)
	user := newBlock(blockUser, "hello")
	tool := &block{kind: blockTool, id: "t1", name: "bash", status: "running", args: map[string]any{"command": "ls"}}
	m.blocks = append(m.blocks, user, tool)
	m.refresh()
	userLines, toolLines := user.lines, tool.lines
	if len(userLines) == 0 || len(toolLines) == 0 {
		t.Fatal("blocks not rendered")
	}

	// A second refresh with nothing changed must hand back the same slices.
	m.refresh()
	if &user.lines[0] != &userLines[0] || &tool.lines[0] != &toolLines[0] {
		t.Fatal("unchanged blocks were re-rendered")
	}

	// State changes that alter the drawing must invalidate: status, output,
	// duration, expand toggle, theme, width.
	tool.status, tool.output, tool.dur = "ok", "file\n", 3*time.Millisecond
	m.refresh()
	if &tool.lines[0] == &toolLines[0] || !strings.Contains(strings.Join(tool.lines, "\n"), "✓") {
		t.Fatalf("tool result not redrawn: %q", tool.lines)
	}
	prev := tool.lines
	m.openDetail = tool
	m.refresh()
	if &tool.lines[0] == &prev[0] {
		t.Fatal("expand toggle did not redraw the tool card")
	}
	prev = user.lines
	m.width = 60
	m.refresh()
	if &user.lines[0] == &prev[0] {
		t.Fatal("width change did not redraw")
	}
	prev = user.lines
	themeGen++
	t.Cleanup(func() { applyTheme(themes[0]) })
	m.refresh()
	if &user.lines[0] == &prev[0] {
		t.Fatal("theme change did not redraw")
	}
}

func TestRenderBlocksSpansMatchLines(t *testing.T) {
	m, _ := testModel(t)
	m.blocks = append(m.blocks, newBlock(blockUser, "q"), newBlock(blockAssistant, "# title\n\nbody"), newBlock(blockSystem, "note"))
	lines := m.renderBlocks(m.width)
	if len(m.spans) != len(m.blocks) {
		t.Fatalf("spans = %d, blocks = %d", len(m.spans), len(m.blocks))
	}
	for i, sp := range m.spans {
		if sp.b != m.blocks[i] || sp.top < 0 || sp.bottom > len(lines) || sp.bottom-sp.top != len(sp.b.lines) {
			t.Fatalf("span %d = %+v (lines %d)", i, sp, len(sp.b.lines))
		}
		if i > 0 && lines[sp.top-1] != "" {
			t.Fatalf("block %d not separated by a blank line: %q", i, lines[sp.top-1])
		}
	}
}

// Streaming snapshots render off the UI path; completion renders the final text.
func TestStreamingBlockRendersMarkdownBeforeDone(t *testing.T) {
	m, _ := testModel(t)
	m.running = true
	b := newBlock(blockAssistant, "# Heading")
	m.blocks = append(m.blocks, b)
	cmd := m.streamMarkdown()
	if cmd == nil {
		t.Fatal("missing streaming render")
	}
	m.Update(cmd())
	m.refresh()
	if !b.key.streaming || !strings.Contains(ansi.Strip(strings.Join(b.lines, "")), "Heading") || strings.Contains(ansi.Strip(strings.Join(b.lines, "")), "# Heading") {
		t.Fatalf("streaming block should be Markdown: %q", b.lines)
	}
	m.running = false
	m.refresh()
	if b.key.streaming || strings.Contains(ansi.Strip(strings.Join(b.lines, "")), "# Heading") {
		t.Fatalf("finished block should be Markdown (no literal #): %q", b.lines)
	}
	done := b.lines
	m.refresh()
	if &b.lines[0] != &done[0] {
		t.Fatal("finished block re-rendered without a change")
	}
}

func TestDropEmptyStyles(t *testing.T) {
	cases := map[string]string{
		// Glamour padding leftovers after truncation.
		"\x1b[38;5;252mhello\x1b[m\x1b[38;5;252m\x1b[m\x1b[38;5;252m\x1b[m": "\x1b[38;5;252mhello\x1b[m",
		// Several sets then a reset: only the reset matters.
		"\x1b[1m\x1b[38;5;252m\x1b[m normal": "\x1b[m normal",
		// A reset must survive even when doubled; it ends the bold.
		"\x1b[1mbold\x1b[m\x1b[m x": "\x1b[1mbold\x1b[m x",
		// Reset then set: nothing to drop.
		"\x1b[m\x1b[1mX": "\x1b[m\x1b[1mX",
		// Set after the last reset in a run is kept.
		"\x1b[31m\x1b[m\x1b[1mX\x1b[m": "\x1b[m\x1b[1mX\x1b[m",
		// Non-SGR CSI passes through untouched.
		"\x1b[2K\x1b[31mred\x1b[m": "\x1b[2K\x1b[31mred\x1b[m",
		"plain":                    "plain",
	}
	for in, want := range cases {
		if got := dropEmptyStyles(in); got != want {
			t.Errorf("dropEmptyStyles(%q) = %q, want %q", in, got, want)
		}
		if ansi.Strip(dropEmptyStyles(in)) != ansi.Strip(in) {
			t.Errorf("visible text changed for %q", in)
		}
	}
	// The real thing: a Markdown paragraph must come out without per-cell junk.
	md := &markdown{}
	out := md.render("hello world", 100)
	if n := strings.Count(out, "\x1b["); n > 6 {
		t.Fatalf("rendered paragraph carries %d escape sequences: %q", n, out)
	}
}

// ---- coalescer ----

func TestCoalescerBatchesInOrderAndFlushesTail(t *testing.T) {
	var got []tea.Msg
	c := newCoalescer(func(msg tea.Msg) { got = append(got, msg) })
	for i := 0; i < 50; i++ {
		c.push(agent.TextDelta{Text: fmt.Sprint(i)})
	}
	time.Sleep(4 * frameInterval)
	c.push(agent.TextDelta{Text: "late"})
	c.flush() // what Run's goroutine does before returning runDoneMsg

	if len(got) < 2 || len(got) > 4 {
		t.Fatalf("expected a few batches, got %d", len(got))
	}
	var texts []string
	for _, msg := range got {
		for _, e := range msg.(eventsMsg).events {
			texts = append(texts, e.(agent.TextDelta).Text)
		}
	}
	if len(texts) != 51 || texts[0] != "0" || texts[49] != "49" || texts[50] != "late" {
		t.Fatalf("events lost or reordered: %d %v", len(texts), texts[:min(5, len(texts))])
	}
	// The first batch holds most of the burst: that is the whole point.
	if n := len(got[0].(eventsMsg).events); n < 40 {
		t.Fatalf("first batch only had %d events", n)
	}
	before := len(got)
	c.flush()
	if len(got) != before {
		t.Fatal("empty flush sent a message")
	}
}

func TestEventsMsgAppliesAllThenRefreshesOnce(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	m.running = true
	m.Update(eventsMsg{events: []agent.Event{
		agent.TextDelta{Text: "hello "},
		agent.TextDelta{Text: "world"},
		agent.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"ls"}`},
	}})
	if n := len(m.blocks); n != 3 { // banner, assistant, tool
		t.Fatalf("blocks = %d", n)
	}
	if got := m.blocks[1].text.String(); got != "hello world" {
		t.Fatalf("assistant text = %q", got)
	}
	if !strings.Contains(ansi.Strip(m.vp.View()), "hello world") {
		t.Fatal("transcript not refreshed after the batch")
	}
}

// ---- input / status caches ----

// checkCached returns the possibly-cached render and the render with the
// cache dropped; they must agree, and the render must differ from prev so
// the mutation under test actually reaches the screen.
func checkCached(t *testing.T, what string, prev string, render func() string, drop func()) string {
	t.Helper()
	got := render()
	drop()
	want := render()
	if got != want {
		t.Fatalf("%s: stale cache:\n got %q\nwant %q", what, ansi.Strip(got), ansi.Strip(want))
	}
	if got == prev {
		t.Fatalf("%s: mutation did not change the render: %q", what, ansi.Strip(got))
	}
	return got
}

func TestInputViewCacheTracksEveryVisibleChange(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	drop := func() { m.inputCache.view = "" }
	render := m.inputView

	first := render()
	if render() != first || m.inputCache.view != first {
		t.Fatal("unchanged input was not served from the cache")
	}
	if !strings.Contains(ansi.Strip(first), "Ask anything") {
		t.Fatalf("placeholder missing: %q", ansi.Strip(first))
	}

	typeKeys(m, "h", "i")
	v := checkCached(t, "typing", first, render, drop)
	typeKeys(m, "shift+enter", "x") // second row, taller box
	m.layout()
	v = checkCached(t, "newline", v, render, drop)
	typeKeys(m, "up") // cursor row changes; the cursor-line style moves
	v = checkCached(t, "cursor row", v, render, drop)
	m.width = 60
	m.layout()
	v = checkCached(t, "width", v, render, drop)
	m.input.Blur()
	// With a native cursor and no current-line fill, focus does not
	// change the textarea's text render.
	if got := render(); got != v {
		t.Fatal("blur changed the transparent input")
	}
	drop()
	if render() != v {
		t.Fatal("blurred cached and uncached input differ")
	}
	m.input.Focus()
	t.Cleanup(func() { applyTheme(themes[0]) })
	dracula, _ := themeByName("dracula")
	applyTheme(dracula)
	m.applyInputStyles()
	checkCached(t, "theme", v, render, drop)
}

func TestFooterCacheTracksEveryVisibleChange(t *testing.T) {
	m, _ := testModel(t)
	m.sess.Name = "local/model"
	m.sess.Agent = &agent.Agent{Model: &provider.Model{}} // gives the ctx gauge a window to read
	drop := func() { m.footerCache.view = "" }
	render := m.footer

	first := render()
	if render() != first || !strings.Contains(ansi.Strip(first), "/ or cmd+p for command palette") {
		t.Fatalf("initial footer not cached or wrong: %q", ansi.Strip(first))
	}
	m.setInput("typing hides the hints")
	v := checkCached(t, "typing", first, render, drop)
	if strings.Contains(ansi.Strip(v), "/ or cmd+p for command palette") {
		t.Fatalf("hints must hide while typing: %q", ansi.Strip(v))
	}
	m.setInput("")
	m.hoverAct = actModel
	v = checkCached(t, "hover", v, render, drop)
	m.hoverAct = actNone
	m.status = "thinking"
	v = checkCached(t, "status", v, render, drop)
	m.running, m.startedAt = true, time.Now().Add(-2100*time.Millisecond)
	v = checkCached(t, "running", v, render, drop)
	if !strings.Contains(ansi.Strip(v), "2s") {
		t.Fatalf("elapsed missing: %q", ansi.Strip(v))
	}
	m.startedAt = m.startedAt.Add(-time.Second) // now 3s: a new second must redraw
	v = checkCached(t, "elapsed", v, render, drop)
	m.usageIn, m.usageOut = 10, 20
	v = checkCached(t, "usage", v, render, drop)
	m.sess.Agent.SetContextWindow(1000)
	m.lastInput = 250
	v = checkCached(t, "ctx", v, render, drop)
	if !strings.Contains(ansi.Strip(v), "25%") {
		t.Fatalf("ctx gauge missing: %q", ansi.Strip(v))
	}
	m.o.Mode.SetMode(agent.ModePlan)
	v = checkCached(t, "mode", v, render, drop)
	m.width = 80
	v = checkCached(t, "width", v, render, drop)
	t.Cleanup(func() { applyTheme(themes[0]) })
	dracula, _ := themeByName("dracula")
	applyTheme(dracula)
	checkCached(t, "theme", v, render, drop)
}

// ---- benchmarks: go test -bench Stream -run xxx ./internal/tui ----

// BenchmarkStreamToken measures one streamed token against a long transcript:
// Update (apply + refresh) plus View, which is what Bubble Tea does per
// message.
func BenchmarkStreamToken(b *testing.B) {
	m, _ := testModel(b)
	for i := 0; i < 200; i++ {
		m.blocks = append(m.blocks, newBlock(blockUser, "question"), newBlock(blockAssistant, "## Answer\n\nSome **markdown** with `code` and a list:\n\n- one\n- two\n"))
	}
	m.running = true
	m.blocks = append(m.blocks, newBlock(blockAssistant, strings.Repeat("streamed words go here. ", 200)))
	m.refresh()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Update(eventsMsg{events: []agent.Event{agent.TextDelta{Text: "tok "}}})
		_ = m.View()
	}
}
