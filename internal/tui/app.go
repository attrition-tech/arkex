// Package tui is the interactive Bubble Tea front end. It consumes
// agent.Event values and never reaches into the agent's internals.
package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dantearo/arkex/internal/agent"
	"github.com/dantearo/arkex/internal/chatgpt"
	"github.com/dantearo/arkex/internal/config"
	"github.com/dantearo/arkex/internal/prompt"
	"github.com/dantearo/arkex/internal/sanitize"
	"github.com/dantearo/arkex/internal/session"
)

// Connection is a ready agent plus what the UI shows about it.
type Connection struct {
	Agent *agent.Agent
	// Name is the display name, e.g. "matrx/DeepSeek-V4.1-Flash".
	Name string
}

// contextWindow is the connected model's context window in tokens, 0 when
// unknown or nothing is connected. It lives on the agent's model so a
// window learned mid-run is reflected everywhere.
func (m *model) contextWindow() int {
	if m.sess.Agent == nil {
		return 0
	}
	return m.sess.Agent.ContextWindow()
}

// Options configures the TUI.
type Options struct {
	// Session may have a nil Agent: the TUI then opens on the Connections panel
	// so the user can connect something.
	Session Connection
	Cwd     string
	Version string
	// Asker, when set, is bound to the running program so the agent's
	// policy can prompt the user for tool approval.
	Asker *Asker
	// Mode is the shared mode switch (plan/build/auto). Required.
	Mode *agent.ModePolicy
	// Connect builds a fresh agent for a model selector. Used by /model and
	// the Connections panel.
	Connect func(ctx context.Context, selector string) (Connection, error)
	// Prepare binds conversation-owned resources before any model request.
	Prepare func(*agent.Agent, string) error
	// ConfigPath is the file the Connections panel edits (the global config).
	ConfigPath string
	// UserAgent is sent when probing endpoints.
	UserAgent string
	// StartupNote, when set, is shown in the Connections panel on first open
	// (for example why no model was connected).
	StartupNote string
	// Resume is a session id (or "latest") to load at startup.
	Resume string
	// ResumeModel is an explicit --model override for startup resume only.
	ResumeModel string
	// UI carries the config file's interface preferences.
	UI config.UI
	// Auth is where subscription sign-ins are kept; nil uses
	// ~/.arkex/auth.json.
	Auth *chatgpt.Store
	// Login points the ChatGPT sign-in at other servers (tests).
	Login chatgpt.Endpoints
}

var ErrRunStillActive = errors.New("agent did not stop; scratch files retained")

// Run starts the interactive session and blocks until the user quits.
func Run(ctx context.Context, o Options) (err error) {
	m := newModel(o)
	defer func() {
		if m.cancel != nil {
			m.cancel()
		}
		if m.runDone != nil {
			select {
			case <-m.runDone:
			case <-time.After(5 * time.Second):
				err = errors.Join(err, ErrRunStillActive)
			}
		}
	}()
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithFPS(targetFPS))
	m.program = p
	m.send = p.Send
	if o.Asker != nil {
		o.Asker.p = p
	}
	_, err = p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// messages sent from the agent goroutine into the Bubble Tea loop

type approvalMsg struct {
	call    agent.ToolCall
	reply   chan agent.Answer
	retry   bool
	partial bool
	err     error
}

type runDoneMsg struct{ err error }

// compactDoneMsg ends a manual /compact.
type compactDoneMsg struct {
	res agent.CompactResult
	err error
}

// Asker implements agent.Asker by round-tripping an approval prompt through
// the Bubble Tea program. Create it with NewAsker, hand it to the policy,
// and pass it in Options so Run can bind it.
type Asker struct{ p *tea.Program }

// NewAsker returns an unbound Asker.
func NewAsker() *Asker { return &Asker{} }

// Ask implements agent.Asker.
func (a *Asker) Ask(ctx context.Context, call agent.ToolCall) (agent.Answer, error) {
	if a.p == nil {
		return agent.Deny, errors.New("tui: approval requested before the program started")
	}
	reply := make(chan agent.Answer, 1)
	a.p.Send(approvalMsg{call: call, reply: reply})
	select {
	case ans := <-reply:
		return ans, nil
	case <-ctx.Done():
		return agent.Deny, ctx.Err()
	}
}

// ---- model ----

type blockKind int

const (
	blockUser blockKind = iota
	blockAssistant
	blockReasoning
	blockTool
	blockSystem
)

type block struct {
	prompt  int // one-based saved prompt checkpoint
	kind    blockKind
	text    strings.Builder
	name    string // tool name
	id      string
	status  string // tool: running | ok | error | denied
	summary string
	note    string         // "by you" when the user approved this call at a prompt
	args    map[string]any // tool input, for the card header
	output  string         // full tool output
	detail  string         // UI-only detail (diff)
	files   []string       // user: attached image names
	dur     time.Duration

	// rendered is a pre-drawn body for system blocks (the banner); when set
	// it is shown instead of text.
	rendered string

	// Render cache: lines is the block as last drawn, valid while key matches
	// the current inputs (theme, width, content length, state). Only the
	// block being streamed is redrawn per frame.
	lines        []string
	key          renderKey
	groupSummary toolGroupSummary
	// Incremental wrap state for the streaming block (see wrapStreaming).
	wrapDone  int
	wrapLines []string
	streamMD  *streamMarkdownMsg
}

// renderKey captures everything a block's rendering depends on. Content is
// keyed by length: blocks only ever grow (streaming) or are replaced.
type renderKey struct {
	gen, width int
	n          int    // content length (text, or tool output+detail+summary)
	state      string // tool status
	dur        time.Duration
	expanded   bool // reasoning shown / tool body expanded
	streaming  bool
	hover      bool // mouse over the card header
	grouped    bool // compact member row, without repeating the tool name
}

// inputKey captures everything the rendered input box depends on.
type inputKey struct {
	value                string
	line, rowOff, colOff int
	selStart, selEnd     textarea.Position
	sel, focused         bool
	width, height, gen   int
	offset               int
	chips                string // attachment names, NUL-separated
	hoverChip            int
	editing              bool
}

type model struct {
	o       Options
	program *tea.Program
	send    func(tea.Msg) // delivers background events to the program (tests stub it)

	inputCache struct {
		key  inputKey
		view string
	}
	footerCache struct {
		key  footerKey
		view string
	}
	// Completed history is immutable during a run. Reuse its layout until
	// inspection, hover, width, theme, or the current turn changes.
	historyCache struct {
		first, turn       *block
		width, gen, count int
		lines             []string
		spans             []span
	}
	footerHits []footerHit  // toolbar pill positions from the last render
	hoverAct   footerAction // toolbar pill under the mouse, actNone when none
	hoverBlock *block       // tool or reasoning card under the mouse
	hoverGroup *block       // group header, identified by its first member
	hoverWork  *block       // completed-turn Work header

	vp     scroller
	input  textarea.Model
	width  int
	height int

	sess           Connection
	startupCmd     tea.Cmd
	connectionGen  int
	baseSystem     string
	md             markdown
	markdownBusy   bool
	markdownNext   time.Time
	openDetail     *block // at most one tool/reasoning detail is open
	openGroup      *block // container of the current inspection, not another detail
	openWork       *block // completed-turn container of the inspection
	lastInput      int64  // input tokens of the most recent model request
	comp           *completion
	dismissed      string   // completion word closed with esc
	files          []string // repository files for @ completion (nil until loaded)
	filesLoading   bool
	blocks         []*block
	running        bool
	compacting     bool
	paused         bool // the last run paused (stuck or step-capped); enter or /continue resumes it
	cancel         context.CancelFunc
	runDone        chan struct{}
	pending        *approval // tool call waiting for an answer; replaces the input box
	status         string
	usageIn        int64
	usageOut       int64
	startedAt      time.Time
	requestTimings []agent.RequestTiming
	requestStart   int
	retryUntil     time.Time
	retryAttempt   int
	liveCut        int
	rollFrom       int
	rollAt         time.Time
	rollPaused     time.Time
	rollScheduled  bool
	runDuration    time.Duration
	runGen         int // bumped per run; stale working ticks carry the old value
	frame          int // working strip animation frame
	step           int // current agent step while running
	ready          bool
	stickBottom    bool
	panel          *modelsPanel     // non-nil while the Connections panel is open
	pal            *palette         // non-nil while the command palette is open
	mouse          bool             // report mouse events (wheel, clicks)
	spans          []span           // block line ranges from the last renderBlocks
	lineBuf        []string         // transcript lines, reused between refreshes
	conv           *session.Session // current conversation on disk; nil until the first turn
	hist           *history         // prompt history for this directory
	shellNotes     []shellNote      // !cmd results waiting to ride along with the next prompt
	shellSeq       int
	welcome        *block       // banner block; the transcript is reset to it by /new
	attachments    []attachment // images queued for the next prompt (chips above the input)
	chipHits       []chipHit    // × positions from the last chip row draw
	hoverChip      int          // hovered remove button, attachment index + 1; zero means none
	inputRows      int          // wrapped rows the input text occupies (may exceed the box)
	sel            *selection   // mouse selection in the transcript, nil when none
	selectionGen   int
	inputDragging  bool
	flashText      string // transient transcript overlay, e.g. "✓ copied 3 lines"
	flashGen       int
	confirmKey     string
	confirmUntil   time.Time
	confirmFlash   int
	promptFocus    *block // keyboard-highlighted sent prompt; never copied into input
	editingPrompt  *promptEdit
	editAfterStop  int
	removeOnStop   int
	scrollHover    bool // pointer over the floating jump-to-latest control
}

func newModel(o Options) *model {
	ta := textarea.New()
	ta.Placeholder = "Ask anything…"
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.CharLimit = 0
	// The textarea grows with its wrapped content between 1 and inputMaxRows
	// rows and clamps its scroll offset when it shrinks again.
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = inputMaxRows
	// Real terminal cursor (positioned via tea.View.Cursor) instead of the
	// textarea's software-drawn blinking one: the terminal blinks it for
	// free, and the input view no longer changes twice a second.
	ta.SetVirtualCursor(false)
	ta.Focus()

	m := &model{
		o:           o,
		input:       ta,
		stickBottom: true,
		mouse:       o.UI.MouseOn(),
	}
	m.setSession(o.Session)
	t, knownTheme := themeByName(o.UI.Theme)
	if !knownTheme {
		t = themes[0]
	}
	applyTheme(t)
	m.applyInputStyles()
	m.welcome = newBlock(blockSystem, "")
	m.renderWelcome()
	m.blocks = append(m.blocks, m.welcome)
	if !knownTheme {
		m.appendSystem(fmt.Sprintf("unknown ui.theme %q in config; using default (/theme lists the themes)", o.UI.Theme))
	}
	m.hist = loadHistory(o.Cwd)
	if o.Resume != "" {
		m.startupCmd = m.resumeWithModel(o.Resume, o.ResumeModel)
	} else if o.Session.Agent == nil {
		m.openModels(o.StartupNote)
	}
	return m
}

// setSession swaps the active agent and remembers its base system prompt
// so mode notes can be appended per request.
func (m *model) setSession(c Connection) {
	m.sess = c
	if c.Agent == nil {
		m.sess.Name = "no model"
		m.baseSystem = ""
		return
	}
	m.baseSystem = c.Agent.System
}

func (m *model) mode() agent.Mode {
	if m.o.Mode == nil {
		return agent.ModeBuild
	}
	return m.o.Mode.Mode()
}

func (m *model) setMode(md agent.Mode) {
	if m.o.Mode != nil {
		m.o.Mode.SetMode(md)
	}
}

// newBlock allocates a block with initial text. strings.Builder must never
// be copied after use, so blocks are always built in place. Text is
// sanitized here because every block ends up on the terminal.
func newBlock(kind blockKind, text string) *block {
	b := &block{kind: kind}
	b.text.WriteString(sanitize.Terminal(text))
	return b
}

func (m *model) Init() tea.Cmd { return tea.Batch(m.startupCmd, m.flashTimer()) }

func (m *model) Update(msg tea.Msg) (next tea.Model, cmd tea.Cmd) {
	gen := m.flashGen
	defer func() {
		// Actions can replace the notice without threading a timer through
		// every command handler. Only a new notice schedules a one-shot tick.
		if gen != m.flashGen {
			cmd = tea.Batch(cmd, m.flashTimer())
		}
		if !m.rollAt.IsZero() && m.rollPaused.IsZero() && !m.rollScheduled {
			m.rollScheduled = true
			cmd = tea.Batch(cmd, m.rollTick())
		}
	}()
	switch msg := msg.(type) {
	case rollMsg:
		if msg.gen != m.runGen {
			return m, nil
		}
		m.rollScheduled = false
		if !m.stickBottom || m.openWork != nil || m.openDetail != nil || m.openGroup != nil || m.sel != nil {
			if m.rollPaused.IsZero() {
				m.rollPaused = time.Now()
			}
			return m, nil
		}
		if time.Since(m.rollAt) >= rollDuration || !m.running {
			m.rollAt = time.Time{}
		}
		m.refresh()
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeInput()
		m.ready = true
		return m, nil

	case tea.KeyPressMsg:
		if msg.String() != m.confirmKey {
			m.disarmConfirmation()
		}
		if m.panel != nil && m.pending == nil {
			return m.panelKey(msg)
		}
		return m.handleKey(msg)

	case tea.MouseMsg:
		if _, click := msg.(tea.MouseClickMsg); click {
			if m.promptFocus != nil {
				m.promptFocus = nil
				m.refresh()
			}
			onNotice := m.noticeHit(msg.Mouse().X, msg.Mouse().Y)
			m.disarmConfirmation()
			if onNotice {
				return m, nil
			}
		}
		return m.handleMouse(msg)

	case approvalMsg:
		m.disarmConfirmation()
		m.promptFocus = nil
		m.pending = newApproval(msg.call, msg.reply)
		if msg.retry {
			m.pending.retry = true
			m.closePalette()
			m.closeModels()
			m.pending.title = "Connection still unavailable · 3 retries exhausted"
			if msg.partial {
				m.pending.title = "Response interrupted · retry restarts this response"
			}
			if errors.Is(msg.err, agent.ErrIncompleteResponse) {
				m.pending.title = "Incomplete response · 2 retries exhausted"
			} else if errors.Is(msg.err, agent.ErrMalformedToolResponse) {
				m.pending.title = "Malformed tool response · 2 retries exhausted"
			}
			m.pending.buttons = []approveButton{{label: "Try again", keys: "enter", answer: agent.AllowOnce}, {label: "Cancel", keys: "esc ×2", answer: agent.Deny}}
		}
		m.status = ""
		m.layout() // the box replaces the input and may be taller
		return m, nil

	case eventsMsg:
		for _, e := range msg.events {
			m.applyEvent(e)
		}
		m.refresh()
		return m, m.streamMarkdown()

	case streamMarkdownMsg:
		m.finishStreamMarkdown(msg)
		return m, m.streamMarkdown()

	case filesMsg:
		m.files = msg.files
		if m.files == nil {
			m.files = []string{}
		}
		m.filesLoading = false
		return m, m.updateCompletion()

	case shellDoneMsg:
		m.finishShell(msg)
		return m, nil

	case runDoneMsg:
		m.compacting = false
		m.disarmConfirmation()
		m.pending = nil
		m.rollAt = time.Time{}
		m.rollPaused = time.Time{}
		m.retryUntil = time.Time{}
		m.runDuration = time.Since(m.startedAt)
		m.running = false
		m.cancel = nil
		m.step = 0
		m.saveConv()
		var paused *agent.PausedError
		if errors.As(msg.err, &paused) {
			m.paused = true
			m.appendSystem(fmt.Sprintf("paused after %d tool steps: %s. Press enter (or /continue) to let it keep going, or type a new instruction.", paused.Steps, paused.Reason))
		} else if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
			m.appendSystem("error: " + msg.err.Error())
			if agent.IsContextOverflow(msg.err) {
				m.appendSystem("the conversation still does not fit the model's context window — /compact summarises it again, /new starts over")
			}
		} else if errors.Is(msg.err, context.Canceled) {
			m.appendSystem("cancelled")
		}
		m.status = ""
		m.refresh()
		return m, m.finishPromptAction()
	case compactDoneMsg:
		return m, m.compactDone(msg)
	case sessionTitleMsg:
		return m, m.sessionTitleDone(msg)
	case sessionConnectedMsg:
		return m, m.sessionConnected(msg)
	case workingMsg:
		return m, m.workingFrame(msg)
	case copySelectionMsg:
		if m.sel != nil && !m.sel.dragging && m.sel.gen == msg.gen {
			return m, m.copySelection()
		}
		return m, nil
	case flashClearMsg:
		if msg.gen == m.flashGen {
			m.flashText = ""
			m.disarmConfirmation()
		}
		return m, nil
	}

	if cmd, ok := m.panelMsg(msg); ok {
		return m, cmd
	}
	if m.pal != nil {
		// Paste and cursor messages belong to the palette's search field.
		if m.pal.busy {
			return m, nil
		}
		var cmd tea.Cmd
		before := m.pal.input.Value()
		m.pal.input, cmd = m.pal.input.Update(msg)
		if m.pal.input.Value() != before {
			m.pal.sel = 0
			m.pal.filter()
		}
		return m, cmd
	}

	if p, ok := msg.(tea.PasteMsg); ok {
		m.disarmConfirmation()
		m.promptFocus = nil
		if m.pending != nil || m.panel != nil {
			return m, nil
		}
		// A dropped image file arrives as its path: make it a chip.
		if name, ok := pastedImagePath(m.o.Cwd, p.Content); ok {
			if err := m.attach(name); err != nil {
				m.appendSystem(err.Error())
			}
			m.layout()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		m.resizeInput()
		return m, cmd
	}

	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) handleKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.pal == nil && m.pending == nil && m.promptFocus != nil && k.String() == "delete" {
		return m, m.confirmPromptRemove(m.promptFocus.prompt)
	}
	if m.pal == nil && m.pending == nil && m.promptFocus != nil && (k.String() == "e" || k.String() == "enter") {
		return m, m.requestPromptEdit(m.promptFocus.prompt)
	}
	if m.promptFocus != nil && k.String() != "tab" && k.String() != "shift+tab" {
		m.promptFocus = nil
		m.refresh()
		if k.String() == "esc" {
			return m, nil
		}
	}
	// Approval prompt swallows keys until answered.
	if m.pending != nil {
		switch key := k.String(); key {
		case "left", "shift+tab", "up":
			m.pending.move(-1)
		case "right", "tab", "down":
			m.pending.move(1)
		case "pgup":
			m.pending.scroll(-m.pending.page)
		case "pgdown":
			m.pending.scroll(m.pending.page)
		case "ctrl+home":
			m.pending.scroll(-len(m.pending.lines))
		case "ctrl+end":
			m.pending.scroll(len(m.pending.lines))
		case "ctrl+c":
			if m.confirmDanger(key) {
				m.answerPending(agent.Deny)
				m.cancelRun()
			}
		default:
			if key == "esc" && m.pending.retry && !m.confirmDanger(key) {
				return m, nil
			}
			if ans, ok := m.pending.byKey(key); ok {
				m.answerPending(ans)
			}
		}
		return m, nil
	}

	if m.pal != nil {
		return m, m.paletteKey(k)
	}
	if m.editingPrompt != nil && k.String() == "esc" {
		m.cancelPromptEdit()
		return m, nil
	}
	if cmd, ok := m.compKey(k); ok {
		return m, cmd
	}

	switch k.String() {
	case "ctrl+p", "super+p":
		return m, m.openPalette()
	case "/":
		if m.input.Value() == "" && m.editingPrompt == nil {
			return m, m.openPalette()
		}
	case "ctrl+c":
		if m.input.HasSelection() || (m.sel != nil && !m.sel.empty()) {
			m.disarmConfirmation()
			return m, m.copyActiveSelection()
		}
		if !m.confirmDanger(k.String()) {
			return m, nil
		}
		if m.running {
			m.cancelRun()
			return m, nil
		}
		return m, tea.Quit
	case "esc":
		if m.input.HasSelection() {
			m.disarmConfirmation()
			m.input.ClearSelection()
			return m, nil
		}
		if m.sel != nil {
			m.disarmConfirmation()
			m.clearSelection()
			return m, nil
		}
		if m.running && m.confirmDanger(k.String()) {
			m.cancelRun()
		}
		return m, nil
	case "ctrl+shift+c", "super+c":
		return m, m.copyActiveSelection()
	case "tab", "shift+tab":
		if m.input.Value() == "" && m.editingPrompt == nil {
			m.focusPrompt(k.String() == "tab")
			return m, nil
		}
		if k.String() == "shift+tab" {
			return m, nil
		}
	case "alt+enter", "ctrl+j", "shift+enter":
		m.input.InsertString("\n")
		m.resizeInput()
		return m, nil
	case "backspace":
		if m.input.Value() == "" && len(m.attachments) > 0 {
			m.removeAttachment(len(m.attachments) - 1)
			return m, nil
		}
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if m.editingPrompt != nil {
			if text != "" || len(m.attachments) != 0 {
				return m, m.confirmPromptResend()
			}
			return m, nil
		}
		if text == "" && len(m.attachments) == 0 {
			if m.paused && !m.running {
				return m, m.continueRun()
			}
			return m, nil
		}
		if strings.HasPrefix(text, "/") {
			m.input.Reset()
			m.comp = nil
			m.resizeInput()
			m.hist.add(text)
			return m.command(text)
		}
		if m.running {
			m.appendSystem("still working; wait or press esc twice to cancel")
			m.refresh()
			return m, nil
		}
		m.input.Reset()
		m.comp = nil
		if text != "" {
			m.hist.add(text)
		}
		if strings.HasPrefix(text, "!") && len(text) > 1 {
			m.resizeInput()
			return m, m.runShell(strings.TrimSpace(text[1:]))
		}
		text = m.takeImageMentions(text)
		files := m.fileParts()
		m.attachments = nil
		m.resizeInput()
		return m, m.submit(text, files)
	case "up":
		if m.input.Line() == 0 {
			if text, ok := m.hist.prev(m.input.Value()); ok {
				m.setInput(text)
			}
			return m, nil
		}
	case "down":
		if m.input.Line() == m.input.LineCount()-1 {
			if text, ok := m.hist.next(); ok {
				m.setInput(text)
			}
			return m, nil
		}
	case "ctrl+l":
		m.replaceTranscript(nil)
		note := "screen cleared"
		if m.conv != nil {
			note += " · the conversation continues (/new starts over)"
		}
		m.appendSystem(note)
		m.refresh()
		return m, nil
	case "pgup":
		m.vp.ScrollUp(m.vp.Height() / 2)
		m.stickBottom = m.vp.AtBottom()
		return m, nil
	case "pgdown":
		m.vp.ScrollDown(m.vp.Height() / 2)
		m.stickBottom = m.vp.AtBottom()
		return m, nil
	case "ctrl+up":
		m.vp.ScrollUp(1)
		m.stickBottom = m.vp.AtBottom()
		return m, nil
	case "ctrl+down":
		m.vp.ScrollDown(1)
		m.stickBottom = m.vp.AtBottom()
		return m, nil
	case "ctrl+home":
		m.vp.ScrollUp(m.vp.YOffset())
		m.stickBottom = m.vp.AtBottom()
		return m, nil
	case "ctrl+end":
		m.vp.GotoBottom()
		m.stickBottom = true
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	m.resizeInput()
	return m, tea.Batch(cmd, m.updateCompletion())
}

func (m *model) command(text string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(text)
	switch fields[0] {
	case "/quit", "/exit", "/q":
		return m, m.confirmQuit()
	case "/clear", "/new":
		if m.running {
			m.appendSystem("cannot clear while working; press esc twice to cancel first")
		} else {
			return m, m.newSession()
		}
		m.refresh()
	case "/resume", "/sessions":
		if len(fields) < 2 {
			items := sessionItems(m)
			if len(items) == 0 {
				m.appendSystem("no saved sessions in " + m.o.Cwd + " yet — they are saved after each turn")
				m.refresh()
				return m, nil
			}
			return m, m.openPaletteSub("Resume session", items)
		}
		cmd := m.resume(fields[1])
		m.refresh()
		return m, cmd
	case "/rename":
		if m.conv == nil {
			return m, m.sessionError(errors.New("no saved session to rename"))
		}
		if len(fields) < 2 {
			return m, m.editSessionTitle(m.conv.ID, m.conv.Title)
		}
		return m, m.renameSession(m.conv.ID, strings.TrimSpace(strings.TrimPrefix(text, "/rename")))
	case "/mode":
		if len(fields) < 2 {
			m.setMode(m.mode().Next())
		} else if md, err := agent.ParseMode(fields[1]); err != nil {
			m.appendSystem(err.Error())
		} else {
			m.setMode(md)
		}
		m.flash("mode: " + string(m.mode()) + " — " + modeHelp(m.mode()))
		m.refresh()
	case "/compact":
		return m, m.compact()
	case "/timing":
		return m, m.openPaletteSub("Run timing (current session)", timingItems(m))
	case "/continue":
		if !m.paused {
			m.appendSystem("nothing to continue — the last run finished on its own")
			m.refresh()
			return m, nil
		}
		return m, m.continueRun()
	case "/effort":
		if len(fields) < 2 {
			return m, m.openPaletteSub("Reasoning effort", effortItems(m))
		}
		m.setEffort(fields[1])
	case "/theme":
		if len(fields) < 2 {
			return m, m.openPaletteSub("Theme", themeItems(m))
		}
		m.setTheme(fields[1])
	case "/mouse":
		m.mouse = !m.mouse
		if m.mouse {
			m.flash("mouse on — wheel scrolls, click toggles tool cards and picks rows")
		} else {
			m.flash("mouse off — the terminal handles selection and scrolling again")
		}
		if m.o.ConfigPath != "" {
			if err := config.SetUI(m.o.ConfigPath, "mouse", m.mouse); err != nil {
				m.appendSystem("could not save ui.mouse: " + err.Error())
			}
		}
		m.refresh()
	case "/models", "/connections":
		m.openModels("")
	case "/model":
		if len(fields) < 2 {
			m.openModels("")
			return m, nil
		}
		if m.running {
			m.appendSystem("cannot switch models while working; press esc twice to cancel first")
			m.refresh()
			return m, nil
		}
		return m, m.connect(fields[1], false)
	case "/help":
		m.appendSystem(helpText())
		m.refresh()
	default:
		m.appendSystem("unknown command " + text)
		m.refresh()
	}
	return m, nil
}

// submit sends text (plus any image parts) as the next user turn.
func (m *model) submit(text string, files []fantasy.FilePart) tea.Cmd {
	if m.sess.Agent == nil {
		m.appendSystem("no model connected — /models to add one")
		m.refresh()
		return nil
	}
	if m.conv == nil {
		m.conv = session.New(m.o.Cwd)
	}
	send := expandMentions(m.o.Cwd, text) + m.takeShellNotes()
	if send == "" {
		send = "(see the attached image)"
	}
	msg := fantasy.NewUserMessage(send, files...)
	b := userBlock(text, attachmentNames(files))
	b.prompt = m.conv.Checkpoint(m.sess.Agent.Messages(), text, msg, m.sess.Name)
	m.blocks = append(m.blocks, b)
	return m.startRun(func(ctx context.Context, ag *agent.Agent, emit func(agent.Event)) error {
		return ag.RunMessage(ctx, msg, emit)
	})
}

// userBlock builds the transcript block for a prompt, listing attached
// images under the text.
func userBlock(text string, images []string) *block {
	b := newBlock(blockUser, text)
	b.files = images
	return b
}

// continueRun resumes a paused run.
func (m *model) continueRun() tea.Cmd {
	if m.sess.Agent == nil || !m.paused {
		return nil
	}
	m.paused = false
	m.appendSystem("continuing…")
	return m.startRun(func(ctx context.Context, ag *agent.Agent, emit func(agent.Event)) error {
		return ag.Continue(ctx, emit)
	})
}

func (m *model) prepareAgent() error {
	if m.conv == nil {
		m.conv = session.New(m.o.Cwd)
	}
	ag := m.sess.Agent
	ag.System = m.baseSystem
	if m.mode() == agent.ModePlan {
		ag.System += prompt.PlanNote
	}
	if m.o.Prepare != nil {
		return m.o.Prepare(ag, m.conv.ID)
	}
	return nil
}

// startRun marks the model busy and runs fn in the background, folding its
// events per frame. runDoneMsg carries the result.
func (m *model) startRun(fn func(context.Context, *agent.Agent, func(agent.Event)) error) tea.Cmd {
	if err := m.prepareAgent(); err != nil {
		m.appendSystem(err.Error())
		m.refresh()
		return nil
	}
	m.disarmConfirmation()
	m.liveCut, m.rollFrom = 0, 0
	m.rollAt, m.rollScheduled = time.Time{}, false
	m.rollPaused = time.Time{}
	m.running = true
	m.paused = false
	m.startedAt = time.Now()
	m.requestTimings, m.runDuration = nil, 0
	m.runGen++
	m.frame, m.step = 0, 0
	m.status = ""
	m.stickBottom = true
	m.refresh()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	send := m.send
	if send == nil {
		send = func(tea.Msg) {} // no program (tests): events are dropped
	}
	ag := m.sess.Agent
	done := make(chan struct{})
	m.runDone = done
	run := func() tea.Msg {
		defer close(done)
		// Events are folded per frame; the final flush runs before
		// runDoneMsg is returned, so the program sees them in order.
		co := newCoalescer(send)
		ag.Retry = func(ctx context.Context, err error, partial bool) bool {
			co.flush()
			reply := make(chan agent.Answer, 1)
			send(approvalMsg{call: agent.ToolCall{Reason: "Completed tools are preserved. " + err.Error()}, reply: reply, retry: true, partial: partial, err: err})
			select {
			case <-ctx.Done():
				return false
			case answer := <-reply:
				return answer == agent.AllowOnce
			}
		}
		defer func() { ag.Retry = nil }()
		err := fn(ctx, ag, co.push)
		co.flush()
		return runDoneMsg{err: err}
	}
	return tea.Batch(run, m.workingTick())
}

func (m *model) cancelRun() {
	if m.cancel != nil {
		m.cancel()
	}
	m.status = "cancelling…"
}

func (m *model) applyEvent(e agent.Event) {
	switch e := e.(type) {
	case agent.RetryWait:
		m.retryUntil, m.retryAttempt = e.Until, e.Attempt
	case agent.RequestRestart:
		m.retryUntil = time.Time{}
		clear(m.blocks[min(m.requestStart, len(m.blocks)):])
		m.blocks = m.blocks[:min(m.requestStart, len(m.blocks))]
		m.openDetail, m.openGroup = nil, nil
	case agent.RequestTiming:
		m.requestTimings = append(m.requestTimings, e)
	case agent.ReasoningTime:
		for i := len(m.blocks) - 1; i >= 0; i-- {
			b := m.blocks[i]
			if b.kind == blockUser {
				break
			}
			if b.kind == blockReasoning && b.id == e.ID {
				b.dur += e.Duration
				break
			}
		}
	case agent.TurnStart:
		m.step = e.Step
		m.requestStart = len(m.blocks)
	case agent.TextDelta:
		if len(m.blocks) == m.requestStart {
			m.blocks = append(m.blocks, newBlock(blockAssistant, ""))
		}
		m.last(blockAssistant, "").text.WriteString(sanitize.Terminal(e.Text))
	case agent.ReasoningDelta:
		// Some compatible servers leak a stray whitespace "reasoning" token
		// in the middle of the answer; opening a block for it would cut the
		// assistant text in two.
		if strings.TrimSpace(e.Text) == "" {
			if n := len(m.blocks); n == 0 || m.blocks[n-1].kind != blockReasoning {
				return
			}
		}
		if len(m.blocks) == m.requestStart {
			m.blocks = append(m.blocks, newBlock(blockReasoning, ""))
		}
		b := m.last(blockReasoning, "")
		b.id = e.ID
		b.text.WriteString(sanitize.Terminal(e.Text))
	case agent.ToolCall:
		m.blocks = append(m.blocks, &block{kind: blockTool, id: e.ID, name: e.Name, status: "running", args: toolArgs(e.Input)})
	case agent.ToolDecision:
		if b := m.tool(e.ID); b != nil {
			switch {
			case !e.Allowed:
				b.status = "denied"
				b.summary = sanitize.Terminal(e.Reason)
			case strings.HasPrefix(e.Reason, "approved by user"):
				b.note = "by you"
				if strings.HasSuffix(e.Reason, "for this session") {
					b.note = "by you · session"
				}
			}
		}
	case agent.ToolResult:
		if b := m.tool(e.ID); b != nil {
			b.dur = e.Duration
			b.output = sanitize.Terminal(e.Output)
			b.detail = sanitize.Terminal(e.Detail)
			if b.status != "denied" {
				if e.IsError {
					b.status = "error"
				} else {
					b.status = "ok"
					if e.Summary != "" {
						b.summary = sanitize.Terminal(e.Summary)
					}
				}
			}
		}
	case agent.TurnEnd:
		m.usageIn += e.Usage.InputTokens
		m.usageOut += e.Usage.OutputTokens
		m.lastInput = agent.ContextInput(e.Usage)
	case agent.Compacting:
		m.compacting = e.Active
	case agent.Compacted:
		m.usageIn += e.Usage.InputTokens
		m.usageOut += e.Usage.OutputTokens
		m.lastInput = 0
		m.blocks = append(m.blocks, compactBlock(e.Summary, e.Dropped))
		if e.Trimmed > 0 {
			m.appendSystem(fmt.Sprintf("the %d oldest messages no longer fit and were dropped unsummarised", e.Trimmed))
		}
	case agent.CompactFailed:
		m.appendSystem("compaction failed, trying the request anyway: " + e.Err.Error())
	case agent.ContextWindowLearned:
		m.learnedContextWindow(e.Tokens)
	case agent.RunEnd:
		// runDoneMsg handles final state.
	}
}

// last returns the trailing block of kind, or appends a new one. Text
// arriving after a tool call starts a fresh assistant block so ordering is
// preserved.
func (m *model) last(kind blockKind, name string) *block {
	if n := len(m.blocks); n > 0 && m.blocks[n-1].kind == kind {
		return m.blocks[n-1]
	}
	b := &block{kind: kind, name: name}
	m.blocks = append(m.blocks, b)
	return b
}

func (m *model) tool(id string) *block {
	for i := len(m.blocks) - 1; i >= 0; i-- {
		if m.blocks[i].kind == blockTool && m.blocks[i].id == id {
			return m.blocks[i]
		}
	}
	return nil
}

// renderWelcome (re)draws workspace info and recent sessions with the current theme.
func (m *model) renderWelcome() {
	m.historyCache.turn = nil
	o := m.o
	r := dimStyle.Render(fmt.Sprintf("  %s · %s", o.Version, o.Cwd))
	if o.Session.Agent == nil {
		r += "\n" + dimStyle.Render("  No model connected yet. /connections lists what is set up and lets you add, disable or remove LLM servers and API keys.")
	} else {
		r += "\n" + dimStyle.Render("  "+o.Session.Name+" · /help for commands · /mode switches plan/build/auto")
	}
	for _, line := range m.recentLines() {
		r += "\n" + line
	}
	m.welcome.rendered = r
	m.welcome.lines = nil // drop the cached lines even if the length is unchanged
}

// setInput replaces the input text and puts the cursor at its end.
func (m *model) setInput(text string) {
	m.input.SetValue(text)
	// SetValue does not populate the textarea viewport. Synchronize it before
	// MoveToEnd, otherwise its scroll offset is clamped against stale content.
	m.input, _ = m.input.Update(nil)
	m.input.MoveToEnd()
	m.resizeInput()
	m.comp = nil
}

func (m *model) appendSystem(s string) {
	m.blocks = append(m.blocks, newBlock(blockSystem, s))
}

func (m *model) replaceTranscript(blocks []*block) {
	m.blocks = blocks
	m.promptFocus = nil
	m.openDetail, m.openGroup, m.openWork = nil, nil, nil
	m.hoverBlock, m.hoverGroup, m.hoverWork = nil, nil, nil
	m.clearSelection()
	m.historyCache.first, m.historyCache.turn = nil, nil
	m.historyCache.lines, m.historyCache.spans = nil, nil
}

// ---- layout & view ----

// resizeInput re-lays the screen out after the input changed: the textarea
// sizes itself (DynamicHeight) and inputRows records the text's real wrapped
// height so the status bar can say how much is scrolled out of view.
func (m *model) resizeInput() {
	if m.width > 0 {
		m.input.SetWidth(m.width - 4)
	}
	m.inputRows = inputRows(m.input.Value(), m.input.Width())
	if m.vp.Width() == m.width && m.vp.Height() == max(0, m.height-m.boxRows()-footerRows) {
		return // editing within the same input height cannot change the transcript
	}
	m.layout()
}

// inputBoxRows is the height of the input box's content: the textarea plus
// the chip row when images are attached.
func (m *model) inputBoxRows() int {
	h := m.input.Height()
	if len(m.attachments) > 0 {
		h++
	}
	return h
}

func (m *model) layout() {
	if m.width == 0 {
		return
	}
	m.input.MaxHeight = max(1, min(inputMaxRows, m.height-footerRows-3))
	if len(m.attachments) > 0 {
		m.input.MaxHeight = max(1, m.input.MaxHeight-1)
	}
	m.input.SetWidth(max(1, m.width-4)) // borders plus one-cell margins
	m.input.SetHeight(min(m.inputRows, m.input.MaxHeight))
	vpH := m.height - m.boxRows() - footerRows
	vpH = max(0, vpH)
	m.vp.SetWidth(m.width)
	m.vp.SetHeight(vpH)
	m.refresh()
}

func (m *model) refresh() {
	if m.width == 0 {
		return
	}
	m.advanceRoll()
	m.vp.SetLines(m.renderBlocks(m.width))
	if m.stickBottom {
		m.vp.GotoBottom()
	}
}

// renderBlocks returns the transcript as display lines, one blank line
// between blocks, and records each block's line span for mouse hits. Blocks
// are drawn through a per-block cache, so a streamed token costs one block,
// not the whole conversation.
func (m *model) renderBlocks(width int) []string {
	inner := max(1, width-4)
	oldSpans := m.spans
	m.spans = m.spans[:0]
	lines := m.lineBuf[:0]
	groupVisible := false
	liveStart, latestProgress, latestThinking := -1, -1, -1
	var earlierThinking []*block
	if m.running {
		for i := len(m.blocks) - 1; i >= 0; i-- {
			b := m.blocks[i]
			if b.kind == blockUser {
				liveStart = i
				break
			}
			if latestProgress < 0 && b.kind == blockAssistant {
				latestProgress = i
			}
			if b.kind == blockReasoning && (latestThinking < 0 || (m.openDetail == b && (m.openWork == nil || m.openWork.kind != blockUser))) {
				latestThinking = i
			}
		}
		if liveStart >= 0 {
			for i := liveStart + 1; i < len(m.blocks); i++ {
				if m.blocks[i].kind == blockReasoning && i != latestThinking {
					earlierThinking = append(earlierThinking, m.blocks[i])
				}
			}
		}
	}
	start := 0
	cache := &m.historyCache
	cacheable := liveStart > 0 && m.openWork == nil && m.openGroup == nil && m.openDetail == nil &&
		m.hoverWork == nil && m.hoverGroup == nil && m.hoverBlock == nil && m.promptFocus == nil
	if cacheable && cache.turn == m.blocks[liveStart] && cache.first == m.blocks[0] &&
		cache.count == liveStart && cache.width == width && cache.gen == themeGen {
		start = liveStart
		lines = append(lines, cache.lines...)
		m.spans = append(m.spans, cache.spans...)
	} else {
		cache.first, cache.turn = nil, nil
		cache.lines, cache.spans = nil, nil
	}
	sections := m.workSectionsFrom(start)
	// Keep an inspection open when its live turn finishes and becomes Work.
	for _, section := range sections {
		for i := section.start; i < section.end; i++ {
			if !section.contains(m, i) {
				continue
			}
			b := m.blocks[i]
			if m.running && b.kind == blockReasoning && section.start > liveStart && liveStart >= 0 {
				continue
			}
			if b == m.openDetail || b == m.openGroup {
				m.openWork = section.head
			}
		}
	}
	workExists := len(earlierThinking) > 0 && m.openWork == m.blocks[liveStart]
	for _, section := range sections {
		workExists = workExists || section.head == m.openWork
	}
	if !workExists {
		m.openWork = nil
	}
	var work *block
	var activeWork *workSection
	workEnd, sectionIndex := -1, 0
	if m.openDetail != nil {
		// A failure or prompt can split a group. Follow the inspected member,
		// rather than opening both the old prefix and its new container.
		m.openGroup = nil
	}
	for i := start; i < len(m.blocks); i++ {
		if i == liveStart && cacheable && cache.turn == nil {
			cache.first, cache.turn = m.blocks[0], m.blocks[i]
			cache.width, cache.gen, cache.count = width, themeGen, liveStart
			cache.lines = append([]string(nil), lines...)
			cache.spans = append([]span(nil), m.spans...)
		}
		b := m.blocks[i]
		if i == liveStart+1 && len(earlierThinking) > 0 {
			head := m.blocks[liveStart]
			lines = append(lines, "")
			top := len(lines)
			lines = append(lines, m.workHeader(workSection{head: head, label: fmt.Sprintf("Earlier thinking · %d sections", len(earlierThinking))}, inner))
			m.spans = append(m.spans, span{top: top, bottom: top + 1, b: head, work: head, workHeader: true})
			if m.openWork == head {
				for _, old := range earlierThinking {
					top = len(lines)
					for _, line := range m.blockLines(old, max(1, inner-2), false) {
						lines = append(lines, "  "+line)
					}
					m.spans = append(m.spans, span{top: top, bottom: len(lines), b: old, work: head})
				}
			}
		}
		if i >= workEnd {
			activeWork = nil
		}
		if sectionIndex < len(sections) && sections[sectionIndex].start == i {
			section := sections[sectionIndex]
			activeWork, workEnd = &sections[sectionIndex], section.end
			sectionIndex++
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			top := len(lines)
			lines = append(lines, m.workHeader(section, inner))
			m.spans = append(m.spans, span{top: top, bottom: top + 1, b: section.head, work: section.head, workHeader: true})
			if m.openWork != section.head {
				if !m.running || section.end <= liveStart {
					// Folded history keeps source content for copy/resume, but
					// need not retain rendered tool bodies or streaming snapshots.
					for _, hidden := range m.blocks[section.start:section.end] {
						hidden.lines, hidden.wrapLines, hidden.streamMD = nil, nil, nil
						hidden.wrapDone = 0
					}
				}
				lines = append(lines, m.rollingTail(section, inner)...)
				if !section.live {
					i = section.end - 1
					continue
				}
			}
		}
		work = nil
		if activeWork != nil && activeWork.contains(m, i) {
			if m.openWork == activeWork.head {
				work = activeWork.head
			} else if i != latestThinking {
				continue
			}
		}
		if liveStart >= 0 && i > liveStart && b.kind == blockAssistant && i != latestProgress {
			continue
		}
		if liveStart >= 0 && i > liveStart && b.kind == blockReasoning && i != latestThinking {
			continue
		}
		contentWidth := inner
		if work != nil {
			contentWidth = max(1, inner-2)
		}
		end := m.toolGroupEnd(i)
		if sectionIndex < len(sections) {
			end = min(end, sections[sectionIndex].start)
		}
		if work != nil {
			end = min(end, workEnd)
		}
		if m.running && m.liveCut > 0 && i >= m.liveCut {
			end = i + 1
		}
		if end-i >= 2 {
			members := m.blocks[i:end]
			// Preserve an already-open card if another call joins its run.
			for _, member := range members {
				if m.openDetail == member {
					m.openGroup = b
				}
			}
			if len(lines) > 0 && work == nil {
				lines = append(lines, "")
			}
			top := len(lines)
			margin := "  "
			if work != nil {
				margin += "  "
			}
			lines = append(lines, margin+m.toolGroupHeader(members, contentWidth))
			m.spans = append(m.spans, span{top: top, bottom: top + 1, b: b, group: b, header: true, work: work})
			if m.openGroup == b {
				groupVisible = true
				for _, member := range members {
					top = len(lines)
					for _, line := range m.renderBlockLines(member, max(1, contentWidth-2), false, true) {
						lines = append(lines, margin+line)
					}
					m.spans = append(m.spans, span{top: top, bottom: len(lines), b: member, group: b, work: work})
				}
			}
			i = end - 1
			continue
		}
		streaming := m.running && i == len(m.blocks)-1 && (b.kind == blockAssistant || b.kind == blockReasoning)
		blockWidth := contentWidth
		if b.kind == blockUser {
			blockWidth = max(1, width-2)
		}
		bl := m.blockLines(b, blockWidth, streaming)
		if len(bl) == 0 {
			continue
		}
		if len(lines) > 0 && (work == nil || b.kind == blockAssistant) {
			lines = append(lines, "")
		}
		top := len(lines)
		for _, line := range bl {
			if work != nil {
				line = "  " + line
			}
			lines = append(lines, line)
		}
		m.spans = append(m.spans, span{top: top, bottom: top + len(bl), b: b, work: work})
	}
	if !groupVisible {
		m.openGroup = nil
	}
	if m.running && !m.compacting && m.activity() != "" {
		lines = append(lines, "", "  "+m.workingStrip(inner))
	}
	// Shorter transcripts must release hidden backing-array references too.
	if len(lines) < len(m.lineBuf) {
		clear(m.lineBuf[len(lines):])
	}
	if len(m.spans) < len(oldSpans) {
		clear(oldSpans[len(m.spans):])
	}
	m.lineBuf = lines
	return lines
}

// blockLines returns b drawn at the given inner width, reusing the cached
// lines when nothing it depends on has changed.
func (m *model) blockLines(b *block, inner int, streaming bool) (lines []string) {
	return m.renderBlockLines(b, inner, streaming, false)
}

func (m *model) renderBlockLines(b *block, inner int, streaming, grouped bool) (lines []string) {
	key := renderKey{gen: themeGen, width: inner, streaming: streaming, grouped: grouped}
	switch b.kind {
	case blockAssistant:
		if streaming {
			if b.streamMD != nil {
				key.n = b.streamMD.n
			}
		} else {
			key.n = b.text.Len()
		}
	case blockTool:
		key.n = len(b.output) + len(b.detail) + len(b.summary) + len(b.note)
		key.state = b.status
		key.dur = b.dur
		if m.running && b.status == "running" {
			key.state += loader(m.frame)
			key.dur = time.Since(m.startedAt).Round(time.Second)
		}
		key.expanded = m.openDetail == b
		key.hover = b == m.hoverBlock
	case blockReasoning:
		key.n = b.text.Len()
		key.dur = b.dur
		key.expanded = m.openDetail == b
		key.hover = b == m.hoverBlock && !key.expanded
	case blockSystem:
		key.n = b.text.Len() + len(b.rendered)
	case blockUser:
		key.n = b.text.Len()
		key.hover = b == m.promptFocus
	default:
		key.n = b.text.Len()
	}
	if b.lines != nil && b.key == key {
		return b.lines
	}
	// Cache the screen-space margin with the block, not once per frame.
	defer func() {
		margin := "  "
		if b.kind == blockUser {
			margin = " "
		}
		padded := make([]string, len(lines))
		for i, line := range lines {
			padded[i] = margin + ansi.Truncate(line, inner, "")
		}
		b.lines, lines = padded, padded
	}()
	prev := b.key
	b.key = key
	// The incremental wrap prefix survives only while width, theme and the
	// streaming state are unchanged.
	restart := prev.width != inner || prev.gen != themeGen || !prev.streaming || prev.expanded != key.expanded

	wrap := lipgloss.NewStyle().Width(inner)
	var out string
	switch b.kind {
	case blockUser:
		body := b.text.String()
		for _, f := range b.files {
			body += "\n" + dimStyle.Render("▣ "+f)
		}
		style := userBarStyle.Width(inner)
		if key.hover {
			style = style.Background(theme.HoverBg).BorderBackground(theme.HoverBg)
		}
		out = style.Render(strings.TrimPrefix(body, "\n"))
	case blockAssistant:
		if streaming {
			if b.streamMD == nil || b.streamMD.width != inner || b.streamMD.gen != themeGen {
				return nil
			}
			return b.streamMD.lines
		}
		b.streamMD = nil
		b.wrapLines = nil
		out = m.md.render(strings.TrimSpace(b.text.String()), inner)
	case blockReasoning:
		if !key.expanded {
			label := "Thinking"
			if b.name != "" {
				label = b.name
			}
			if b.dur > 0 {
				label += " · " + timingDuration(b.dur)
			}
			out = dimStyle.Render("▸ " + label)
			if key.hover {
				out = fillRow(out, inner)
			}
			break
		}
		if streaming {
			b.lines = wrapStreaming(b, inner, &reasoningStyle, restart)
			return b.lines
		}
		b.wrapLines = nil
		out = reasoningStyle.Render(wrap.Render(strings.TrimSpace(b.text.String())))
	case blockTool:
		mark, elapsed := "", ""
		if m.running && b.status == "running" {
			mark, elapsed = loader(m.frame), key.dur.String()
		}
		out = renderToolRow(b, m.o.Cwd, inner, key.expanded, key.hover, !key.grouped, mark, elapsed)
	case blockSystem:
		if b.rendered != "" {
			out = b.rendered
		} else {
			out = dimStyle.Render(wrap.Render(b.text.String()))
		}
	}
	if out == "" {
		return nil
	}
	b.lines = strings.Split(out, "\n")
	return b.lines
}

// wrapStreaming wraps a block that is still receiving text and returns its
// display lines. Greedy wrapping is prefix-stable: appending text never moves
// a break that is already committed, so everything but the last visual line
// is wrapped once and kept in b.wrapLines (b.wrapDone is the byte offset in
// b.text where the uncommitted remainder starts). A token therefore costs
// O(last line) rather than O(block). Lines are not padded to the width; the
// renderer clears the rest of the row. style, when set, is applied per line.
func wrapStreaming(b *block, inner int, style *lipgloss.Style, restart bool) []string {
	text := b.text.String()
	if restart || b.wrapLines == nil {
		b.wrapLines = []string{}
		b.wrapDone = 0
	}
	if len(b.wrapLines) == 0 {
		// Nothing committed yet: skip leading whitespace, like TrimSpace
		// does on the final render.
		b.wrapDone = len(text) - len(strings.TrimLeft(text, " \t\r\n"))
	}
	styled := func(s string) string {
		if style != nil {
			return style.Render(s)
		}
		return s
	}
	commit := func(wrapped string) {
		for _, l := range strings.Split(wrapped, "\n") {
			b.wrapLines = append(b.wrapLines, styled(l))
		}
	}

	tail := text[b.wrapDone:]
	// Source newlines: everything before the last one is final.
	if i := strings.LastIndexByte(tail, '\n'); i >= 0 {
		commit(ansi.Wrap(tail[:i], inner, ""))
		b.wrapDone += i + 1
		tail = tail[i+1:]
	}
	t := strings.TrimRight(tail, " \t\r\n")
	if t == "" {
		return b.wrapLines
	}
	wrapped := ansi.Wrap(t, inner, "")
	// Soft breaks: commit every visual line but the last, provided we can
	// locate the last line in the source (Wrap only replaces break
	// whitespace with newlines, so it is a suffix of the source).
	if i := strings.LastIndexByte(wrapped, '\n'); i >= 0 {
		if last := wrapped[i+1:]; last != "" && strings.HasSuffix(t, last) {
			commit(wrapped[:i])
			b.wrapDone += len(t) - len(last)
			wrapped = last
		}
	}
	lines := append(make([]string, 0, len(b.wrapLines)+2), b.wrapLines...)
	for _, l := range strings.Split(wrapped, "\n") {
		lines = append(lines, styled(l))
	}
	return lines
}

func modeHelp(md agent.Mode) string {
	switch md {
	case agent.ModePlan:
		return "read-only tools; the model writes a plan"
	case agent.ModeAuto:
		return "trusted scope runs quietly; config denies apply"
	}
	return "edits and commands ask for approval per config"
}

func (m *model) mouseMode() tea.MouseMode {
	if m.mouse {
		// All-motion reporting drives hover; the terminal only sends a
		// motion event when the pointer crosses into another cell.
		return tea.MouseModeAllMotion
	}
	return tea.MouseModeNone
}

// modeBadge is the mode pill; hovered it gains the ▾ hint of a picker.
func (m *model) modeBadge(hover bool) string {
	st, label := modeBuildStyle, "BUILD"
	switch m.mode() {
	case agent.ModePlan:
		st, label = modePlanStyle, "PLAN"
	case agent.ModeAuto:
		st, label = modeAutoStyle, "AUTO"
	}
	if hover {
		st = pillHoverStyle.Bold(true)
	}
	return st.Render(label + " ▾")
}

// inputView renders the bordered input box, reusing the previous render
// while the textarea's visible state is unchanged. The textarea rebuilds and
// wraps its whole content on every View call, so this is the largest
// per-frame cost once the transcript is cached.
func (m *model) inputView() string {
	li := m.input.LineInfo()
	key := inputKey{
		value: m.input.Value(), line: m.input.Line(),
		rowOff: li.RowOffset, colOff: li.ColumnOffset,
		focused: m.input.Focused(),
		width:   m.width, height: m.input.Height(), gen: themeGen,
		offset: m.input.ScrollYOffset(),
	}
	key.selStart, key.selEnd, key.sel = m.input.Selection()
	key.hoverChip = m.hoverChip
	key.editing = m.editingPrompt != nil
	for _, a := range m.attachments {
		key.chips += a.name + "\x00"
	}
	if c := &m.inputCache; c.view != "" && c.key == key {
		return c.view
	}
	body := m.input.View()
	if chips := m.chipRow(m.width - 4); chips != "" {
		body = chips + "\n" + body
	}
	view := borderStyle.Width(m.width).Render(body)
	if key.editing && m.width > 6 {
		label := ansi.Truncate(" Editing prompt · Enter resend · Esc cancel ", m.width-4, "…")
		view = overlay(view, titleStyle.Render(label), 2, 0)
	}
	if m.inputRows > m.input.Height() {
		rows := strings.Split(view, "\n")
		start := 1
		if len(m.attachments) > 0 {
			start++
		}
		end := min(len(rows), start+m.input.Height())
		marked := scrollbar(strings.Join(rows[start:end], "\n"), m.width-1, end-start, m.inputRows, m.input.ScrollYOffset())
		copy(rows[start:end], strings.Split(marked, "\n"))
		view = strings.Join(rows, "\n")
	}
	m.inputCache.key, m.inputCache.view = key, view
	return view
}

// windowTitle is independent of terminal width: tabs have their own layout.
func (m *model) windowTitle() string {
	name := strings.Join(strings.Fields(sanitize.Terminal(m.convTitle())), " ")
	if name == "" {
		name = "New session"
	}
	dir := strings.Join(strings.Fields(sanitize.Terminal(filepath.Base(filepath.Clean(m.o.Cwd)))), " ")
	suffix := " - " + ansi.Truncate(dir, 20, "…") + " - arkex"
	prefix := ""
	if m.running {
		prefix = loader(m.frame) + " "
	}
	return prefix + ansi.Truncate(name, 64-ansi.StringWidth(prefix+suffix), "…") + suffix
}

func (m *model) View() (v tea.View) {
	defer func() { v.WindowTitle = m.windowTitle() }()
	if !m.ready {
		return tea.NewView("")
	}
	footer := m.footer()

	if m.panel != nil {
		transcript := muteBackdrop(m.vp.View())
		box, x, y, cursor := m.panelBox(m.width, m.vp.Height())
		transcript = overlay(transcript, box, x, y)
		v = tea.NewView(transcript + "\n" + muteBackdrop(footer))
		v.AltScreen = true
		v.MouseMode = m.mouseMode()
		if m.pending == nil {
			v.Cursor = cursor
		}
		return v
	}

	var inputView string
	if m.pending != nil {
		inputView = m.approvalView()
	} else {
		inputView = m.inputView()
	}
	transcript := scrollbar(m.highlight(m.vp.View()), m.width, m.vp.Height(), len(m.vp.lines), m.vp.YOffset())
	if m.pending != nil {
		transcript, footer = muteBackdrop(transcript), muteBackdrop(footer)
	}
	var paletteCursor *tea.Cursor
	if m.pal != nil {
		if box, x, y, c := m.paletteBox(m.width, m.vp.Height()); box != "" {
			transcript = overlay(muteBackdrop(transcript), box, x, y)
			inputView, footer = muteBackdrop(inputView), muteBackdrop(footer)
			paletteCursor = c
		}
	} else if popup := m.renderPopup(m.width); popup != "" {
		// Overlay the popup on the bottom of the transcript, above the box.
		tl := strings.Split(transcript, "\n")
		pl := strings.Split(popup, "\n")
		if len(pl) < len(tl) {
			tl = strings.Split(muteBackdrop(transcript), "\n")
			footer = muteBackdrop(footer)
			copy(tl[len(tl)-len(pl):], pl)
			transcript = strings.Join(tl, "\n")
		}
	}
	if box, x, y := m.noticeBox(); box != "" {
		transcript = overlay(transcript, box, x, y)
	}
	if box, x, y := m.scrollBox(); box != "" {
		transcript = overlay(transcript, box, x, y)
	}
	// Plain concatenation: every part is already full-width or the renderer
	// clears the rest of the row, so no per-line padding (JoinVertical
	// measures every line of the frame).
	content := inputView + "\n" + footer
	if m.vp.Height() > 0 {
		content = transcript + "\n" + content
	}

	v = tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = m.mouseMode()
	switch {
	case m.pending != nil:
	case m.pal != nil:
		v.Cursor = paletteCursor
	case m.promptFocus != nil:
	default:
		if c := m.input.Cursor(); c != nil {
			c.X += 2                 // left border and padding
			c.Y += m.vp.Height() + 1 // top border
			if len(m.attachments) > 0 {
				c.Y++ // chip row
			}
			if c.Y >= m.vp.Height()+1 && c.Y < m.vp.Height()+1+m.inputBoxRows() && c.X < m.width-1 {
				v.Cursor = c
			}
		}
	}
	return v
}

// answerPending resolves the approval prompt and gives the input box back.
func (m *model) answerPending(ans agent.Answer) {
	if m.pending == nil {
		return
	}
	m.disarmConfirmation()
	m.pending.answer(ans)
	m.pending = nil
	m.status = ""
	m.layout()
}
