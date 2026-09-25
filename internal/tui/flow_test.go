package tui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/agent"
)

func transcript(m *model) string {
	return ansi.Strip(m.vp.View())
}

func events(m *model, es ...agent.Event) {
	m.Update(eventsMsg{events: es})
}

func TestToolCardLifecycle(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	m.running = true

	events(m,
		agent.TurnStart{Step: 1},
		agent.TextDelta{Text: "Looking."},
		agent.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"go test ./..."}`},
	)
	if m.step != 1 || m.activity() != "" {
		t.Fatalf("step=%d activity=%q", m.step, m.activity())
	}
	tr := transcript(m)
	if !strings.Contains(tr, "go test ./...") || !strings.Contains(tr, "bash") {
		t.Fatalf("running card missing command:\n%s", tr)
	}
	card := m.tool("c1")
	if card == nil || card.status != "running" {
		t.Fatalf("card = %+v", card)
	}

	// Text after a tool call must start a new assistant block, not be
	// appended to the one before the card.
	events(m, agent.TextDelta{Text: "Then more."})
	if n := len(m.blocks); m.blocks[n-1].kind != blockAssistant || m.blocks[n-1].text.String() != "Then more." || m.blocks[1].text.String() != "Looking." {
		t.Fatalf("text ordering broken: %q / %q", m.blocks[1].text.String(), m.blocks[n-1].text.String())
	}

	events(m, agent.ToolResult{ID: "c1", Name: "bash", Output: "ok\tpkg\n", Summary: "go test ./...", Duration: 1500 * time.Millisecond})
	if card.status != "ok" || card.dur != 1500*time.Millisecond || card.summary != "go test ./..." {
		t.Fatalf("card after result = %+v", card)
	}
	tr = transcript(m)
	if !strings.Contains(tr, "✓") || !strings.Contains(tr, "1.5s") {
		t.Fatalf("ok card not drawn with check and duration:\n%s", tr)
	}

	// Denied: decision first, then the result must not flip it to ok/error.
	events(m,
		agent.ToolCall{ID: "c2", Name: "edit", Input: `{"path":"a.go"}`},
		agent.ToolDecision{ID: "c2", Name: "edit", Allowed: false, Reason: "plan mode: read-only"},
		agent.ToolResult{ID: "c2", Name: "edit", Output: "denied", IsError: true},
	)
	if c := m.tool("c2"); c.status != "denied" || c.summary != "plan mode: read-only" {
		t.Fatalf("denied card = %+v", c)
	}
	// Error result.
	events(m,
		agent.ToolCall{ID: "c3", Name: "read", Input: `{"path":"missing"}`},
		agent.ToolResult{ID: "c3", Name: "read", Output: "open missing: no such file", IsError: true},
	)
	if c := m.tool("c3"); c.status != "error" {
		t.Fatalf("error card = %+v", c)
	}
	if !strings.Contains(transcript(m), "✗") {
		t.Fatalf("error card has no cross:\n%s", transcript(m))
	}

	// Usage accumulates across turns; lastInput is the latest request only.
	events(m,
		agent.TurnEnd{Usage: fantasy.Usage{InputTokens: 100, OutputTokens: 10}},
		agent.TurnEnd{Usage: fantasy.Usage{InputTokens: 150, OutputTokens: 5}},
	)
	if m.usageIn != 250 || m.usageOut != 15 || m.lastInput != 150 {
		t.Fatalf("usage = %d/%d last=%d", m.usageIn, m.usageOut, m.lastInput)
	}

	// Run end: error is surfaced as a system line, status resets.
	m.Update(runDoneMsg{err: errors.New("model exploded")})
	if m.running || m.status != "" {
		t.Fatalf("running=%v status=%q after done", m.running, m.status)
	}
	if !strings.Contains(transcript(m), "error: model exploded") {
		t.Fatalf("run error not shown:\n%s", transcript(m))
	}
	// Cancellation is reported as such, not as an error.
	m.running = true
	m.Update(runDoneMsg{err: context.Canceled})
	tr = transcript(m)
	if !strings.Contains(tr, "cancelled") || strings.Contains(tr, "error: context canceled") {
		t.Fatalf("cancel not reported:\n%s", tr)
	}
}

func TestApprovalPromptSwallowsKeysUntilAnswered(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	m.running = true
	baseVP := m.vp.Height()

	ask := func(call agent.ToolCall) chan agent.Answer {
		reply := make(chan agent.Answer, 1)
		m.Update(approvalMsg{call: call, reply: reply})
		return reply
	}
	bash := agent.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"echo hi"}`, Grantable: true}

	reply := ask(bash)
	if m.pending == nil {
		t.Fatal("no pending prompt")
	}
	if v := m.View(); v.Cursor != nil {
		t.Fatal("cursor shown while the approval prompt is up")
	}
	// The box replaces the input and shows what the tool wants to do.
	plain := ansi.Strip(m.View().Content)
	for _, want := range []string{"bash needs your permission", "$ echo hi", "Allow", "Allow this session", "Deny", "enter confirm"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("prompt missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "Ask anything") {
		t.Fatal("input box still drawn under the prompt")
	}
	if m.vp.Height() >= baseVP {
		t.Fatalf("transcript must shrink for the taller box: %d >= %d", m.vp.Height(), baseVP)
	}
	// Ordinary keys must not leak into the input box.
	typeKeys(m, "x", "z")
	if m.input.Value() != "" || len(reply) != 0 {
		t.Fatalf("keys leaked: input=%q replies=%d", m.input.Value(), len(reply))
	}
	// Enter takes the marked answer; right moves the mark.
	typeKeys(m, "right", "enter")
	if got := <-reply; got != agent.AllowSession || m.pending != nil || m.status != "" {
		t.Fatalf("right+enter: got=%v pending=%v status=%q", got, m.pending != nil, m.status)
	}
	if m.vp.Height() != baseVP {
		t.Fatalf("layout not restored: %d != %d", m.vp.Height(), baseVP)
	}

	reply = ask(bash)
	typeKeys(m, "enter")
	if got := <-reply; got != agent.AllowOnce {
		t.Fatalf("enter defaults to allow once, got %v", got)
	}
	reply = ask(bash)
	typeKeys(m, "esc")
	if got := <-reply; got != agent.Deny || m.pending != nil {
		t.Fatalf("esc: got=%v pending=%v", got, m.pending != nil)
	}
	reply = ask(bash)
	typeKeys(m, "a")
	if got := <-reply; got != agent.AllowSession {
		t.Fatalf("a: got=%v", got)
	}

	// A boundary prompt is not grantable: no session button, "a" is inert,
	// and the reason is shown.
	outside := agent.ToolCall{ID: "c2", Name: "write", Input: `{"path":"/home/x/notes.md"}`, Reason: "writes outside the workspace: ~/notes.md"}
	reply = ask(outside)
	plain = ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "writes outside the workspace: ~/notes.md") || strings.Contains(plain, "this session") {
		t.Fatalf("boundary prompt wrong:\n%s", plain)
	}
	typeKeys(m, "a")
	if len(reply) != 0 {
		t.Fatal("a answered a non-grantable prompt")
	}
	typeKeys(m, "n")
	if got := <-reply; got != agent.Deny {
		t.Fatalf("n: got=%v", got)
	}

	reply = ask(bash)
	cancelled := false
	m.cancel = func() { cancelled = true }
	typeKeys(m, "ctrl+c", "ctrl+c")
	if got := <-reply; got != agent.Deny || !cancelled || m.status != "cancelling…" {
		t.Fatalf("ctrl+c: got=%v cancelled=%v status=%q", got, cancelled, m.status)
	}
}

func TestApprovalPromptMouse(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	m.running = true
	reply := make(chan agent.Answer, 1)
	m.Update(approvalMsg{call: agent.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"ls"}`, Grantable: true}, reply: reply})
	m.View()
	y := m.approvalButtonY()
	hits := m.pending.hits
	if len(hits) != 3 {
		t.Fatalf("hits = %+v", hits)
	}
	// Hover lights the row's button; nothing else changes.
	m.handleMouse(tea.MouseMotionMsg{X: hits[2].x0, Y: y, Button: tea.MouseNone})
	if m.pending.hover != 2 || len(reply) != 0 {
		t.Fatalf("hover=%d replies=%d", m.pending.hover, len(reply))
	}
	m.handleMouse(tea.MouseMotionMsg{X: hits[2].x0, Y: y - 1, Button: tea.MouseNone})
	if m.pending.hover != -1 {
		t.Fatalf("hover off the row = %d", m.pending.hover)
	}
	// A click elsewhere is swallowed; a click on Deny answers.
	click(m, 0, 0)
	if len(reply) != 0 || m.pending == nil {
		t.Fatal("stray click answered the prompt")
	}
	click(m, hits[2].x0+1, y)
	if got := <-reply; got != agent.Deny || m.pending != nil {
		t.Fatalf("click deny: got=%v pending=%v", got, m.pending != nil)
	}
}

func TestApprovalPreview(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	input, _ := json.Marshal(map[string]any{
		"path": "missing.go", "old_string": "before\nold", "new_string": "after\nnew\nextra", "replace_all": true,
	})
	reply := make(chan agent.Answer, 1)
	m.Update(approvalMsg{call: agent.ToolCall{Name: "edit", Input: string(input), Grantable: true}, reply: reply})
	plain := ansi.Strip(m.View().Content)
	for _, want := range []string{"Proposed replacement · all matches", "- before", "- old", "+ after", "+ extra"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("missing %q:\n%s", want, plain)
		}
	}
	frameLines(t, m, "edit preview")
	typeKeys(m, "n")
	if <-reply != agent.Deny {
		t.Fatal("preview changed denial semantics")
	}

	input, _ = json.Marshal(map[string]any{"path": "new.txt", "content": strings.Repeat("line\n", 30) + "last\x1b[31m"})
	m.Update(approvalMsg{call: agent.ToolCall{Name: "write", Input: string(input)}, reply: reply})
	a := m.pending
	if strings.Contains(a.preview, "\x1b") {
		t.Fatal("preview contains terminal controls")
	}
	plain = ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "replaces entire file") || !strings.Contains(plain, "of 31") || strings.Contains(plain, "+ last") {
		t.Fatalf("write preview:\n%s", plain)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if a.offset != a.page || len(reply) != 0 {
		t.Fatalf("page down offset=%d page=%d replies=%d", a.offset, a.page, len(reply))
	}
	m.wheel(-1)
	if a.offset != max(0, a.page-wheelLines) {
		t.Fatal("wheel did not scroll preview")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	if !strings.Contains(ansi.Strip(m.View().Content), "+ last") {
		t.Fatal("last preview line inaccessible")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	if a.offset != 0 {
		t.Fatal("home did not restore start")
	}
	frameLines(t, m, "write preview")
	typeKeys(m, "n")
	<-reply

	empty := newApproval(agent.ToolCall{Name: "write", Input: `{"path":"existing","content":""}`}, reply)
	if empty.preview != "(empty file)" || !strings.Contains(empty.caption, "replaces entire file") {
		t.Fatal("empty write must still warn about replacement")
	}
}

func TestScopedBashApprovalDetails(t *testing.T) {
	m, _ := testModel(t)
	m.running = true
	raw, _ := json.Marshal(map[string]any{"command": strings.Repeat("echo line\n", 30) + "echo LAST_COMMAND"})
	call := agent.ToolCall{Name: "bash", Input: string(raw), Grantable: true,
		TrustDirectory: "/outside/shared", TrustAccess: "read, changes and shell path checks",
		Reason:  "command names a path outside the workspace: /outside/shared/file",
		Workdir: "/workspace/falak"}
	for _, size := range [][2]int{{120, 36}, {60, 24}, {24, 8}, {120, 36}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		reply := make(chan agent.Answer, 1)
		m.Update(approvalMsg{call: call, reply: reply})
		frameLines(t, m, "scoped approval")
		if size[0] == 120 {
			plain := ansi.Strip(m.View().Content)
			for _, want := range []string{"Trust directory", "/outside/shared", "until arkex exits", "/workspace/falak", "not a sandbox"} {
				if !strings.Contains(plain, want) {
					t.Fatalf("missing %q:\n%s", want, plain)
				}
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
			if !strings.Contains(ansi.Strip(m.View().Content), "LAST_COMMAND") {
				t.Fatal("command tail unavailable")
			}
		}
		if a, ok := m.pending.byKey("enter"); !ok || a != agent.AllowOnce {
			t.Fatal("default must remain Allow once")
		}
		typeKeys(m, "a")
		if <-reply != agent.AllowSession {
			t.Fatal("trust shortcut broken")
		}
	}
}

func TestApprovalCompactGeometryAndHits(t *testing.T) {
	m, _ := testModel(t)
	m.running = true
	input, _ := json.Marshal(map[string]any{"path": "src/long-name.go", "old_string": strings.Repeat("removed ", 40), "new_string": "replacement\nlast"})
	for _, size := range [][2]int{{120, 34}, {60, 20}, {40, 20}, {80, 24}, {60, 20}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for selected := 0; selected < 3; selected++ {
			reply := make(chan agent.Answer, 1)
			m.Update(approvalMsg{call: agent.ToolCall{Name: "edit", Input: string(input), Grantable: true}, reply: reply})
			m.pending.sel = selected
			lines := frameLines(t, m, "compact approval")
			if m.vp.Height() < 4 || !strings.Contains(ansi.Strip(lines[len(lines)-2]), "BUILD") {
				t.Fatal("approval displaced transcript or footer")
			}
			for _, h := range m.pending.hits {
				y := m.approvalButtonY() + h.y
				label := ansi.Cut(ansi.Strip(lines[y]), h.x0, h.x1)
				if !strings.Contains(label, m.pending.buttons[h.idx].label) {
					t.Fatalf("%v hit %+v covers %q", size, h, label)
				}
			}
			hit := m.pending.hits[selected]
			y := m.approvalButtonY() + hit.y
			motion(m, hit.x0+1, y)
			if m.pending.hover != selected {
				t.Fatal("hover missed wrapped action")
			}
			want := m.pending.buttons[selected].answer
			click(m, hit.x0+1, y)
			if got := <-reply; got != want {
				t.Fatalf("clicked answer %v, want %v", got, want)
			}
			frameLines(t, m, "after approval")
		}
	}
	// Resize a live, scrolled preview, not only newly created prompts.
	m.Update(approvalMsg{call: agent.ToolCall{Name: "edit", Input: string(input), Reason: "writes outside the workspace: /outside/long-name.go"}, reply: make(chan agent.Answer, 1)})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 34})
	frameLines(t, m, "resized preview")
	if !strings.Contains(ansi.Strip(m.View().Content), "+ last") {
		t.Fatal("resize lost the end of the preview")
	}
}

func TestApprovedCardSaysByYou(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	m.applyEvent(agent.ToolCall{ID: "t1", Name: "bash", Input: `{"command":"ls"}`})
	m.applyEvent(agent.ToolDecision{ID: "t1", Name: "bash", Allowed: true, Reason: "approved by user for this session"})
	m.applyEvent(agent.ToolResult{ID: "t1", Name: "bash", Output: "a\n"})
	m.applyEvent(agent.ToolCall{ID: "t2", Name: "bash", Input: `{"command":"pwd"}`})
	m.applyEvent(agent.ToolDecision{ID: "t2", Name: "bash", Allowed: true, Reason: "auto mode"})
	m.applyEvent(agent.ToolResult{ID: "t2", Name: "bash", Output: "/x\n"})
	m.refresh()
	lines := m.renderBlocks(100)
	plain := ansi.Strip(strings.Join(lines, "\n"))
	if !strings.Contains(plain, "by you · session") {
		t.Fatalf("approved card lacks the note:\n%s", plain)
	}
	if strings.Count(plain, "by you") != 1 {
		t.Fatalf("policy-allowed card must not say by you:\n%s", plain)
	}
}

// frameLines splits a View into lines and checks the frame is exactly the
// terminal size: every line fits and there are exactly height of them.
func frameLines(t *testing.T, m *model, what string) []string {
	t.Helper()
	lines := strings.Split(m.View().Content, "\n")
	if len(lines) != m.height {
		t.Fatalf("%s: frame has %d lines, want %d:\n%s", what, len(lines), m.height, m.View().Content)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > m.width {
			t.Fatalf("%s: line %d is %d wide (max %d): %q", what, i, w, m.width, ansi.Strip(l))
		}
	}
	return lines
}

func TestViewFrameGeometryAndCursor(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	for i := 0; i < 40; i++ {
		m.blocks = append(m.blocks, newBlock(blockAssistant, strings.Repeat("word ", 40)))
	}
	m.refresh()

	lines := frameLines(t, m, "default")
	v := m.View()
	// 30 rows: 25 transcript, top border, input text, bottom border,
	// toolbar, hints. The cursor sits after the border and padding.
	if v.Cursor == nil || v.Cursor.Y != 26 || v.Cursor.X != 2 {
		t.Fatalf("cursor = %+v", v.Cursor)
	}
	if !strings.Contains(ansi.Strip(lines[28]), "BUILD") {
		t.Fatalf("toolbar not on row 28: %q", ansi.Strip(lines[28]))
	}
	if !strings.HasSuffix(ansi.Strip(lines[29]), "/ or cmd+p for command palette") {
		t.Fatalf("hints not on row 29: %q", ansi.Strip(lines[29]))
	}
	if !v.AltScreen {
		t.Fatal("alt screen off")
	}

	// Two input rows: the transcript shrinks by one and the cursor follows.
	typeKeys(m, "a", "shift+enter", "b")
	frameLines(t, m, "two-line input")
	if v := m.View(); v.Cursor == nil || v.Cursor.Y != 26 || v.Cursor.X != 3 {
		t.Fatalf("two-line cursor = %+v", v.Cursor)
	}
	if m.vp.Height() != 24 {
		t.Fatalf("viewport height = %d", m.vp.Height())
	}
	typeKeys(m, "ctrl+u", "backspace", "ctrl+u")

	// Palette overlay stays inside the frame and owns the cursor.
	typeKeys(m, "ctrl+p")
	lines = frameLines(t, m, "palette")
	if m.pal == nil || !strings.Contains(ansi.Strip(strings.Join(lines, "\n")), "/connections") {
		t.Fatal("palette not drawn")
	}
	if v := m.View(); v.Cursor == nil || v.Cursor.Y >= 26 {
		t.Fatalf("palette cursor = %+v", v.Cursor)
	}
	typeKeys(m, "esc")

	// Slash popup at the bottom of the transcript.
	typeKeys(m, "/", "t", "h")
	lines = frameLines(t, m, "popup")
	if !strings.Contains(ansi.Strip(strings.Join(lines[:26], "\n")), "/theme") {
		t.Fatal("slash popup not drawn above the bar")
	}
	typeKeys(m, "esc", "ctrl+u")

	// Connections panel overlays the transcript; input box is one line.
	m.openModels("")
	lines = frameLines(t, m, "panel")
	if !strings.Contains(ansi.Strip(strings.Join(lines, "\n")), "Connections") {
		t.Fatalf("panel not drawn:\n%s", ansi.Strip(strings.Join(lines, "\n")))
	}
	m.closeModels()

	// Narrow terminal: still exactly sized.
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	frameLines(t, m, "narrow")
}

func TestSlashCommandsHelpNewAndCompact(t *testing.T) {
	m, _ := testModel(t)
	m.o.Connect = nil
	m.layout()
	m.blocks = append(m.blocks, newBlock(blockUser, "old question"))
	m.refresh()

	m.command("/help")
	if !strings.Contains(transcript(m), "/models") {
		t.Fatalf("help missing:\n%s", transcript(m))
	}
	m.command("/new")
	if strings.Contains(transcript(m), "old question") || len(m.blocks) != 1 {
		t.Fatalf("/new kept the conversation: %d blocks", len(m.blocks))
	}
	m.command("/nope")
	if !strings.Contains(transcript(m), "unknown command") {
		t.Fatalf("unknown command not reported:\n%s", transcript(m))
	}
}

// TestUntrustedTextCannotDriveTheTerminal feeds escape sequences through
// every ingestion point (model text, reasoning, tool output, tool title,
// denial reason, system notes) and checks the rendered frame contains no
// escape other than the styling we emit ourselves.
func TestUntrustedTextCannotDriveTheTerminal(t *testing.T) {
	m, _ := testModel(t)
	m.layout()
	m.running = true
	osc52 := "\x1b]52;c;SGVsbG8=\x07"
	events(m,
		agent.TextDelta{Text: "hello " + osc52 + "world\x1b[2J!"},
		agent.ReasoningDelta{Text: "thinking" + osc52},
		agent.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"echo \u001b]52;c;SGVsbG8=\u0007hi"}`},
		agent.ToolDecision{ID: "c1", Name: "bash", Allowed: false, Reason: "nope" + osc52},
		agent.ToolResult{ID: "c1", Name: "bash", Output: "out" + osc52 + "\x1b[Hput", IsError: true},
	)
	m.appendSystem("error: " + osc52 + "boom")
	var tr string
	for _, b := range m.blocks {
		m.openDetail = b
		m.refresh()
		tr += transcript(m)
		frame := m.View().Content
		for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\x1b[H"} {
			if strings.Contains(frame, bad) {
				t.Fatalf("frame contains %q:\n%q", bad, frame)
			}
		}
	}
	for _, want := range []string{"hello world!", "$ echo hi", "nope", "boom"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("visible text %q missing from:\n%s", want, tr)
		}
	}
}
