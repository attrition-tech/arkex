package tui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/chatgpt"
	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/setup"
	"github.com/attrition-tech/arkex/internal/tools"
)

// The Connections panel: what arkex can talk to, and the knobs to change
// it. It is a centered box over the transcript (like the palette) with a
// list of connections and their models; adding and editing use the form
// widget in form.go, model discovery uses a checkbox picker. It never
// blocks startup and everything works keyboard-only; the mouse is extra.

type panelMode int

const (
	pmList panelMode = iota
	pmConfirmDelete
	pmAdd
	pmEdit
	pmPick
	pmBusy
	pmLogin // browser sign-in in progress; shows the link
)

// row is one selectable line of the list: a connection header
// (model == nil) or one of its models.
type row struct {
	provID string
	prov   config.Connection
	model  *config.Model
}

func (r row) selector() string {
	if r.model == nil {
		return r.provID
	}
	return r.provID + "/" + r.model.ID
}

// panelGeom is where the last render put things, in coordinates relative
// to the inside of the box, so clicks can be mapped back.
type panelGeom struct {
	x, y, w, h int   // box on screen
	bodyY      int   // first body line
	rows       []int // list/pick: index per body line, -1 for none
	statusX    int   // list: where the status cell starts
	buttons    []rect
	buttonIDs  []string
	formY      int // add/edit: form origin
	formTop    int // add/edit: first form line shown (scroll)
}

type modelsPanel struct {
	mode  panelMode
	after panelMode // where pmBusy returns
	cfg   *config.Config
	rows  []row
	// cursor/top drive the list; delTarget is what pmConfirmDelete removes.
	cursor, top int
	delTarget   row

	err  string // red footer line
	note string // dim footer line

	hoverBtn string // button id under the mouse, "" when none
	busy     string // spinner label while pmBusy
	spin     spinner.Model
	// errs remembers the last connect error per connection so the list can
	// show "✕ error"; it is not persisted.
	errs     map[string]string
	busyConn string

	// add / edit
	form     *form
	preset   setup.Preset
	autoName string            // name the form filled in itself; replaced on preset change
	editID   string            // connection being edited; "" while adding
	editConn config.Connection // the saved connection being edited
	draft    config.Connection // connection being built; saved when models are picked
	draftID  string
	pickFrom panelMode // pmAdd or pmEdit: where the picker returns

	// subscriptions
	store     *chatgpt.Store            // saved sign-ins
	auth      map[string]chatgpt.Tokens // by connection id, for the list rows
	login     *chatgpt.Login            // in-flight browser sign-in
	loginCtx  context.Context
	loginStop context.CancelFunc
	draftTok  *chatgpt.Tokens          // sign-in made while adding; saved with the draft
	catalog   map[string]chatgpt.Model // last fetched subscription catalog, by id

	// picker
	filter  textinput.Model
	found   []string
	visible []string
	picked  map[string]bool
	pick    int // index into visible, or len(visible) = Confirm, +1 = Back
	// pickButtons are the Confirm/Back rects (inner coordinates) and the
	// body line they sit on.
	pickButtons    []rect
	pickButtonLine int

	box panelGeom
}

type modelsFetchedMsg struct {
	ids     []string
	catalog []chatgpt.Model // subscription fetches: names, context windows
	err     error
	owner   *modelsPanel
}

type loginDoneMsg struct {
	tokens chatgpt.Tokens
	err    error
	ctx    context.Context
}

type connectedMsg struct {
	gen      int
	sel      string
	conn     Connection
	name     string
	took     time.Duration
	err      error
	testOnly bool
}

const (
	panelWidth    = 76  // the panel is at least this wide when the terminal allows
	panelMaxWidth = 110 // and never wider than this: forms read badly stretched
	panelStatusW  = 12  // "◌ connecting"
)

func (m *model) openModels(note string) {
	fi := textinput.New()
	fi.Prompt = "  "
	fi.Placeholder = "type to filter"
	fi.SetVirtualCursor(false)
	fi.SetStyles(fieldStyles())
	p := &modelsPanel{
		filter: fi,
		spin:   spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(toolStyle)),
		picked: map[string]bool{},
		errs:   map[string]string{},
		note:   note,
		store:  m.o.Auth,
	}
	if m.panel != nil {
		p.errs = m.panel.errs
	}
	if p.store == nil {
		var err error
		if p.store, err = chatgpt.DefaultStore(); err != nil {
			p.err = err.Error()
		}
	}
	m.panel = p
	m.input.Blur()
	p.reload(m.o.Cwd, "")
	m.layout()
}

func (m *model) closeModels() {
	if m.panel != nil && m.panel.loginStop != nil {
		m.panel.loginStop()
	}
	m.panel = nil
	m.input.Focus()
	m.layout()
}

// reload re-reads the merged config and rebuilds rows, trying to keep the
// cursor on selector (or where it was).
func (p *modelsPanel) reload(cwd, selector string) {
	if selector == "" && p.cursor < len(p.rows) {
		selector = p.rows[p.cursor].selector()
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		p.err = err.Error()
		cfg = &config.Config{Connections: map[string]config.Connection{}}
	}
	p.cfg = cfg
	p.auth = map[string]chatgpt.Tokens{}
	if p.store != nil {
		for id, c := range cfg.Connections {
			if c.Kind != config.KindSubscription || (c.Subscription != "" && c.Subscription != "chatgpt") {
				continue
			}
			if tok, ok, err := p.store.Get(id); err != nil {
				p.err = err.Error()
			} else if ok {
				p.auth[id] = tok
			}
		}
	}
	p.rows = p.rows[:0]
	for _, id := range cfg.ConnectionIDs() {
		prov := cfg.Connections[id]
		p.rows = append(p.rows, row{provID: id, prov: prov})
		for i := range prov.Models {
			p.rows = append(p.rows, row{provID: id, prov: prov, model: &prov.Models[i]})
		}
	}
	prev := p.cursor
	p.cursor = -1
	for i, r := range p.rows {
		if r.selector() == selector {
			p.cursor = i
			break
		}
	}
	if p.cursor < 0 {
		p.cursor = max(0, min(prev, len(p.rows)-1))
	}
}

func (p *modelsPanel) current() (row, bool) {
	if p.cursor < len(p.rows) {
		return p.rows[p.cursor], true
	}
	return row{}, false
}

// ---- keys ----

// panelKey routes a key press while the panel is open.
func (m *model) panelKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	p := m.panel
	key := k.String()
	switch p.mode {
	case pmList:
		return m, m.listKey(key)
	case pmConfirmDelete:
		switch key {
		case "y", "Y": // deliberately not enter: too easy to hit by accident
			return m, m.panelAction("delete-yes")
		default:
			return m, m.panelAction("delete-no")
		}
	case pmAdd, pmEdit:
		return m, m.formKey(k)
	case pmPick:
		return m, m.pickKey(k)
	case pmLogin:
		switch key {
		case "esc", "ctrl+c":
			return m, m.loginAction("login-cancel")
		case "c":
			return m, m.loginAction("login-copy")
		case "o":
			return m, m.loginAction("login-open")
		}
	case pmBusy:
		if key == "esc" || key == "ctrl+c" {
			// The in-flight command finishes on its own; its result is
			// still applied, we just stop showing the spinner.
			p.mode, p.busy, p.busyConn = p.after, "", ""
		}
	}
	return m, nil
}

func (m *model) listKey(key string) tea.Cmd {
	p := m.panel
	r, ok := p.current()
	switch key {
	case "esc", "q", "ctrl+c":
		m.closeModels()
	case "up", "k", "ctrl+p":
		p.cursor = max(0, p.cursor-1)
	case "down", "j", "ctrl+n":
		p.cursor = min(max(0, len(p.rows)-1), p.cursor+1)
	case "a", "+":
		return m.openAdd()
	case "e":
		if ok {
			return m.openEdit(r.provID)
		}
	case "r":
		p.reload(m.o.Cwd, "")
		p.err, p.note = "", "reloaded"
	case "d":
		if ok {
			m.confirmDelete(r)
		}
	case "x", " ", "space":
		if ok {
			m.toggleRow(r)
		}
	case "enter":
		if !ok {
			return nil
		}
		if r.model == nil {
			return m.openEdit(r.provID)
		}
		return m.useRow(r, false)
	case "t":
		if ok && r.model != nil {
			return m.useRow(r, true)
		}
	}
	return nil
}

// toggleRow flips the enabled flag of a connection or model.
func (m *model) toggleRow(r row) {
	disabled, modelID := r.prov.Disabled, ""
	if r.model != nil {
		disabled, modelID = r.model.Disabled, r.model.ID
	}
	verb := "disabled"
	if disabled {
		verb = "enabled"
	}
	m.afterEdit(config.SetDisabled(m.o.ConfigPath, r.provID, modelID, !disabled), verb+" "+r.selector(), r.selector())
}

// useRow connects to (or tests) the model on row r.
func (m *model) useRow(r row, testOnly bool) tea.Cmd {
	p := m.panel
	if r.prov.Disabled || r.model.Disabled {
		p.err = r.selector() + " is disabled (space to enable)"
		return nil
	}
	if m.running {
		p.err = "a run is in progress; press esc twice in the chat to cancel it first"
		return nil
	}
	return m.connect(r.selector(), testOnly)
}

func (m *model) confirmDelete(r row) {
	p := m.panel
	p.mode, p.delTarget, p.err = pmConfirmDelete, r, ""
	if r.model == nil {
		p.note = fmt.Sprintf("delete connection %s and its %d model(s)?", r.provID, len(r.prov.Models))
	} else {
		p.note = fmt.Sprintf("delete %s?", r.selector())
	}
}

// panelAction runs a named button of the list or confirm screens.
func (m *model) panelAction(id string) tea.Cmd {
	p := m.panel
	switch id {
	case "add":
		return m.openAdd()
	case "edit":
		if r, ok := p.current(); ok {
			return m.openEdit(r.provID)
		}
	case "delete":
		if r, ok := p.current(); ok {
			m.confirmDelete(r)
		}
	case "delete-yes":
		r := p.delTarget
		p.mode = pmList
		var err error
		if r.model == nil {
			err = config.RemoveConnection(m.o.ConfigPath, r.provID)
			if err == nil && r.prov.Kind == config.KindSubscription && (r.prov.Subscription == "" || r.prov.Subscription == "chatgpt") && p.store != nil {
				err = p.store.Remove(r.provID) // the sign-in goes with the connection
			}
		} else {
			err = config.RemoveModel(m.o.ConfigPath, r.provID, r.model.ID)
		}
		m.afterEdit(err, "removed "+r.selector(), "")
	case "delete-no":
		p.mode, p.note = pmList, ""
	}
	return nil
}

// afterEdit reloads after a config write and reports the outcome. A
// connection that survives its own deletion lives in the project config,
// which the panel does not edit.
func (m *model) afterEdit(err error, done, keep string) {
	p := m.panel
	p.err, p.note = "", ""
	if err != nil {
		p.err = err.Error()
		return
	}
	before := len(p.rows)
	p.reload(m.o.Cwd, keep)
	p.note = done
	if strings.HasPrefix(done, "removed") && len(p.rows) == before {
		p.note = done + " from " + m.o.ConfigPath + ", but it is also defined in " + config.ProjectPath(m.o.Cwd)
	}
	if p.cursor >= len(p.rows) {
		p.cursor = max(0, len(p.rows)-1)
	}
}

// ---- add / edit forms ----

var kindOptions = []choiceOpt{
	{label: config.KindLLMServer.Label()},
	{label: config.KindAPIKey.Label()},
	{label: config.KindSubscription.Label()},
	{label: config.KindRuntime.Label(), disabled: true},
}

var formKinds = []config.Kind{config.KindLLMServer, config.KindAPIKey, config.KindSubscription}

// kindHelp is the line under the Kind pills for the selected kind.
var kindHelp = map[config.Kind]string{
	config.KindLLMServer:    "a server that speaks the OpenAI API — Ollama, llama.cpp, LM Studio, vLLM…",
	config.KindAPIKey:       "a hosted vendor you pay per token — OpenAI, Anthropic, OpenRouter, DeepSeek…",
	config.KindSubscription: "your ChatGPT plan, signed in through the browser; no key to paste",
}

const subscriptionNote = "Uses your ChatGPT plan through the Codex backend. Your browser opens to sign in; " +
	"arkex keeps only the sign-in tokens, in auth.json next to config.json (mode 0600)."

func (m *model) openAdd() tea.Cmd {
	p := m.panel
	p.mode, p.err, p.note = pmAdd, "", ""
	p.editID, p.editConn = "", config.Connection{}
	p.draftTok = nil
	f := newForm()
	f.add(choiceField("kind", "Kind", kindOptions, 0))
	f.add(choiceField("service", "Service", nil, 0)).gap = true
	f.add(noteField(subscriptionNote)).id = "subnote"
	f.add(textField("name", "Name", "", "short id, used as name/model")).gap = true
	f.add(textField("url", "Base URL", "", "https://your-server.example/v1")).gap = true
	key := f.add(secretField("key", "API key", "", ""))
	key.gap, key.help = true, "$MY_VAR reads it from the environment"
	f.add(buttonField("reveal", "show")).inline = true
	enabled := f.add(checkField("enabled", "Enabled", true))
	enabled.gap, enabled.help = true, "disabled connections stay in the list but are hidden from the model picker"
	models := f.add(textField("models", "Models", "", "model ids, comma separated"))
	models.hidden, models.gap = true, true
	fetch := f.add(buttonField("fetch", "Fetch models →"))
	fetch.gap, fetch.primary = true, true
	login := f.add(buttonField("login", "Sign in with ChatGPT →"))
	login.gap, login.primary = true, true
	save := f.add(buttonField("save", "Save"))
	save.inline, save.hidden, save.primary = true, true, true
	f.add(buttonField("cancel", "Cancel")).inline = true
	p.form = f
	p.setKind(0)
	return f.focusID("kind")
}

// formKind is the kind of the connection the open form describes.
func (p *modelsPanel) formKind() config.Kind {
	if p.editID != "" {
		return p.editConn.Kind
	}
	if k := p.form.get("kind"); k != nil {
		return formKinds[min(k.sel, len(formKinds)-1)]
	}
	return config.KindLLMServer
}

// setKind fills the Service options for the kind at index i, applies its
// first preset and shows the fields that kind needs: servers and vendors
// take a URL and key and fetch /models; a subscription signs in instead.
func (p *modelsPanel) setKind(i int) {
	kind := formKinds[min(i, len(formKinds)-1)]
	var opts []choiceOpt
	for _, ps := range setup.PresetsFor(kind) {
		opts = append(opts, choiceOpt{label: ps.Label})
	}
	f := p.form
	f.get("kind").help = kindHelp[kind]
	svc := f.get("service")
	svc.options, svc.sel = opts, 0
	p.applyPreset()
	sub := kind == config.KindSubscription
	for _, id := range []string{"url", "key", "reveal", "fetch"} {
		f.get(id).hidden = sub
	}
	f.get("login").hidden = !sub
	f.get("subnote").hidden = !sub
	if sub {
		// Sign-in replaces the hand-typed model list.
		f.get("models").hidden, f.get("save").hidden = true, true
	}
}

// applyPreset prefills URL, name and key hint from the selected service.
func (p *modelsPanel) applyPreset() {
	f := p.form
	p.draftTok, p.catalog = nil, nil
	kind := formKinds[min(f.get("kind").sel, len(formKinds)-1)]
	presets := setup.PresetsFor(kind)
	p.preset = presets[min(f.get("service").sel, len(presets)-1)]
	if kind == config.KindSubscription {
		f.get("login").label = "Sign in with " + p.preset.Label + " →"
		f.get("subnote").text = subscriptionNote
	}

	urlF := f.get("url")
	if p.preset.BaseURL != "" || setup.DetectPreset(urlF.input.Value()).BaseURL != "" {
		urlF.input.SetValue(p.preset.BaseURL)
		urlF.input.CursorEnd()
	}
	urlF.help = p.preset.Hint

	nameF := f.get("name")
	if v := strings.TrimSpace(nameF.input.Value()); v == "" || v == p.autoName {
		name := ""
		if p.preset.BaseURL != "" {
			name = p.preset.ID
			if _, taken := p.cfg.Connections[name]; taken {
				name += "2"
			}
		}
		nameF.input.SetValue(name)
		nameF.input.CursorEnd()
		p.autoName = name
	}

	keyF := f.get("key")
	if p.preset.NeedsKey {
		keyF.input.Placeholder = "required"
		keyF.help = "stored in config.json (mode 0600) · type $MY_VAR to read it from the environment instead"
	} else {
		keyF.input.Placeholder = "optional for local servers"
		keyF.help = "$MY_VAR reads it from the environment"
	}
}

func (m *model) openEdit(id string) tea.Cmd {
	p := m.panel
	c, ok := p.cfg.Connections[id]
	if !ok {
		p.err = "connection " + id + " not found"
		return nil
	}
	if c.Kind == config.KindSubscription && c.Subscription != "" && c.Subscription != "chatgpt" {
		p.err = "unsupported subscription — delete this connection and add a supported one"
		return nil
	}
	p.mode, p.err, p.note = pmEdit, "", ""
	p.editID, p.editConn = id, c
	p.preset = setup.DetectPreset(c.BaseURL)
	if c.Kind == config.KindSubscription {
		service := c.Subscription
		if service == "" {
			service = "chatgpt"
		}
		p.preset = setup.FindPreset(service)
	}
	sub := c.Kind == config.KindSubscription
	f := newForm()
	f.add(noteField(c.Kind.Label() + " · " + p.preset.Label))
	f.add(textField("name", "Name", id, "")).gap = true
	if sub {
		who := "not signed in"
		if t, ok := p.auth[id]; ok {
			who = "signed in as " + t.Summary()
		}
		f.add(noteField(who)).id = "who"
	} else {
		f.add(textField("url", "Base URL", c.BaseURL, "")).gap = true
		key := f.add(secretField("key", "API key", c.APIKey, "none"))
		key.gap, key.help = true, "$MY_VAR reads it from the environment"
		f.add(buttonField("reveal", "show")).inline = true
	}
	f.add(checkField("enabled", "Enabled", !c.Disabled)).gap = true
	if len(c.Models) > 0 {
		f.add(noteField("Models · space ticks the ones you want to see")).gap = true
	}
	for _, md := range c.Models {
		label := md.ID
		if id+"/"+md.ID == p.cfg.Default {
			label += "  " + selStyle.Render("★ default")
		}
		f.add(checkField("model:"+md.ID, label, !md.Disabled))
	}
	if sub {
		f.add(buttonField("login", "Sign in again")).gap = true
		f.add(buttonField("signout", "Sign out")).inline = true
	}
	save := f.add(buttonField("save", "Save"))
	save.gap, save.primary = true, true
	f.add(buttonField("refetch", "Refetch models")).inline = true
	f.add(buttonField("test", "Test")).inline = true
	f.add(buttonField("delete", "Delete")).inline = true
	f.add(buttonField("cancel", "Cancel")).inline = true
	p.form = f
	return f.focusID("name")
}

// formKey drives the add/edit form and runs its actions.
func (m *model) formKey(k tea.KeyPressMsg) tea.Cmd {
	p := m.panel
	switch k.String() {
	case "esc", "ctrl+c":
		p.closeForm()
		return nil
	}
	action, cmd := p.form.key(k)
	// Typing into the name field ends auto-naming.
	if fl := p.form.focused(); p.mode == pmAdd && fl != nil && fl.id == "name" && strings.TrimSpace(fl.input.Value()) != p.autoName {
		p.autoName = "\x00"
	}
	return tea.Batch(cmd, m.formAction(action))
}

func (p *modelsPanel) closeForm() {
	p.mode, p.form, p.err = pmList, nil, ""
	p.draft, p.draftID, p.draftTok = config.Connection{}, "", nil
}

// formAction reacts to a form action id (button or "changed:<field>").
func (m *model) formAction(action string) tea.Cmd {
	p := m.panel
	f := p.form
	switch action {
	case "":
		return nil
	case "changed:kind":
		p.setKind(f.get("kind").sel)
	case "changed:service":
		p.applyPreset()
	case "reveal":
		key, btn := f.get("key"), f.get("reveal")
		if key.input.EchoMode == textinput.EchoPassword {
			key.input.EchoMode, btn.label = textinput.EchoNormal, "hide"
		} else {
			key.input.EchoMode, btn.label = textinput.EchoPassword, "show"
		}
	case "cancel":
		p.closeForm()
	case "login":
		if !m.buildDraft() {
			return nil
		}
		return m.startLogin()
	case "signout":
		if p.store == nil {
			return nil
		}
		if err := p.store.Remove(p.editID); err != nil {
			p.err = err.Error()
			return nil
		}
		delete(p.auth, p.editID)
		if who := f.get("who"); who != nil {
			who.text = "not signed in"
		}
		p.note = "signed out of " + p.editID + " · its models stay until you delete the connection"
	case "fetch", "refetch":
		if !m.buildDraft() {
			return nil
		}
		p.pickFrom = p.mode
		if p.formKind() == config.KindSubscription {
			tok, ok := p.auth[p.editID]
			if !ok {
				p.err = "not signed in; use Sign in again first"
				return nil
			}
			return m.fetchCatalog(tok)
		}
		return m.fetchModels()
	case "save":
		if !m.buildDraft() {
			return nil
		}
		if p.mode == pmAdd {
			var ids []string
			for _, s := range strings.Split(f.value("models"), ",") {
				if s = strings.TrimSpace(s); s != "" {
					ids = append(ids, s)
				}
			}
			if len(ids) == 0 {
				f.get("models").err = "type at least one model id"
				return f.focusID("models")
			}
			return m.saveDraft(ids)
		}
		return m.saveDraft(nil)
	case "test":
		sel := ""
		for _, md := range p.editConn.EnabledModels() {
			sel = p.editID + "/" + md.ID
			if sel == p.cfg.Default {
				break
			}
		}
		if sel == "" {
			p.err = "no enabled model to test; save first"
			return nil
		}
		p.note = "testing the saved connection"
		return m.connect(sel, true)
	case "delete":
		if r, ok := p.rowFor(p.editID); ok {
			p.form = nil
			m.confirmDelete(r)
		}
	}
	return nil
}

func (p *modelsPanel) rowFor(id string) (row, bool) {
	for i, r := range p.rows {
		if r.provID == id && r.model == nil {
			p.cursor = i
			return r, true
		}
	}
	return row{}, false
}

// buildDraft validates the form and fills p.draft/p.draftID. Field errors
// are shown inline and focus moves to the first bad field.
func (m *model) buildDraft() bool {
	p := m.panel
	f := p.form
	for _, fl := range f.fields {
		fl.err = ""
	}
	p.err = ""
	var bad *field

	sub := p.formKind() == config.KindSubscription
	var u string
	if sub {
		// No URL field: the backend is fixed, kept from an edited
		// connection so a hand-edited baseUrl survives.
		u = p.editConn.BaseURL
		if u == "" {
			u = p.preset.BaseURL
		}
	} else {
		var err error
		if u, err = setup.NormalizeBaseURL(f.value("url")); err != nil {
			f.get("url").err = err.Error()
			bad = f.get("url")
		}
	}
	name := f.value("name")
	if name == "" && u != "" {
		name = setup.SuggestName(u)
		if sub {
			name = p.preset.ID
		}
		f.get("name").input.SetValue(name)
	}
	switch _, taken := p.cfg.Connections[name]; {
	case !setup.ValidID(name):
		f.get("name").err = "use letters, digits, - or _"
	case taken && name != p.editID:
		f.get("name").err = fmt.Sprintf("%q already exists; pick another name or delete it first", name)
	}
	if f.get("name").err != "" && bad == nil {
		bad = f.get("name")
	}
	key := f.value("key")
	if p.preset.NeedsKey && key == "" && p.mode == pmAdd {
		f.get("key").err = "this service needs an API key"
		if bad == nil {
			bad = f.get("key")
		}
	}
	if bad != nil {
		f.focusID(bad.id)
		return false
	}
	if setup.IsPlainHTTP(u) {
		p.note = "warning: plain http to a remote host — your API key and code travel unencrypted; prefer https"
	}

	d := p.editConn // keeps Headers, Compat, Models, Name of an edited connection
	d.Kind, d.API, d.BaseURL, d.APIKey = p.preset.Kind, config.APIOpenAICompat, u, key
	if sub {
		d.API = config.APIOpenAI // Responses API on the Codex backend
		d.Subscription = p.preset.ID
	}
	d.Disabled = !f.get("enabled").on
	if p.mode == pmAdd {
		d.Compat = p.preset.Compat
	} else {
		d.Kind = p.editConn.Kind
		for i := range d.Models {
			if fl := f.get("model:" + d.Models[i].ID); fl != nil {
				d.Models[i].Disabled = !fl.on
			}
		}
	}
	p.draft, p.draftID = d, name
	return true
}

func (m *model) fetchModels() tea.Cmd {
	p := m.panel
	p.after, p.mode, p.err = p.pickFrom, pmBusy, ""
	p.busy = "fetching " + p.draft.BaseURL + "/models"
	url, key, ua := p.draft.BaseURL, p.draft.APIKey, m.o.UserAgent
	return tea.Batch(p.spin.Tick, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resolved, err := config.ResolveValue(ctx, key)
		if err != nil {
			return modelsFetchedMsg{err: err}
		}
		ids, err := setup.ListModels(ctx, nil, url, resolved, ua)
		return modelsFetchedMsg{ids: ids, err: err}
	})
}

// startLogin binds the callback port, opens the browser and switches to
// the sign-in screen. The result arrives as loginDoneMsg.
func (m *model) startLogin() tea.Cmd {
	p := m.panel
	if p.loginStop != nil {
		p.loginStop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.loginCtx = ctx
	l, err := chatgpt.StartLogin(ctx, nil, m.o.Login)
	if err != nil {
		cancel()
		p.err = err.Error()
		return nil
	}
	p.login, p.loginStop = l, cancel
	p.after, p.mode, p.err, p.note = p.mode, pmLogin, "", ""
	return tea.Batch(p.spin.Tick, func() tea.Msg {
		t, err := l.Wait(ctx)
		return loginDoneMsg{tokens: t, err: err, ctx: ctx}
	})
}

// loginAction runs a button of the sign-in screen.
func (m *model) loginAction(id string) tea.Cmd {
	p := m.panel
	if p.login == nil {
		return nil
	}
	switch id {
	case "login-cancel":
		p.loginStop() // Wait returns context.Canceled; loginDoneMsg cleans up
	case "login-copy":
		if p.login.URL == "" {
			return nil
		}
		p.note = "link copied"
		return tea.SetClipboard(p.login.URL)
	case "login-open":
		if p.login.URL == "" {
			return nil
		}
		if err := chatgpt.OpenBrowser(p.login.URL); err != nil {
			p.err = "could not open a browser: " + err.Error()
		} else {
			p.note = "opened in your browser"
		}
	}
	return nil
}

// loginDone applies the outcome of a browser sign-in: while editing, the
// tokens are saved right away; while adding, they wait in the draft and
// the model catalog is fetched.
func (m *model) loginDone(msg loginDoneMsg) tea.Cmd {
	p := m.panel
	if msg.ctx != nil && msg.ctx.Err() != nil {
		msg.err = msg.ctx.Err()
	}
	if p.loginStop != nil {
		p.loginStop()
	}
	p.login, p.loginStop = nil, nil
	p.loginCtx = nil
	if p.mode == pmLogin {
		p.mode = p.after
	}
	if p.form == nil {
		return nil
	}
	switch {
	case errors.Is(msg.err, context.Canceled):
		p.note = "sign-in cancelled"
		return nil
	case errors.Is(msg.err, context.DeadlineExceeded):
		p.err = "sign-in code expired; sign in again"
		return nil
	case msg.err != nil:
		p.err = msg.err.Error()
		return nil
	}
	tok := msg.tokens
	if p.editID != "" {
		if p.store != nil {
			if err := p.store.Put(p.editID, tok); err != nil {
				p.err = err.Error()
				return nil
			}
		}
		if p.auth == nil {
			p.auth = map[string]chatgpt.Tokens{}
		}
		p.auth[p.editID] = tok
		if who := p.form.get("who"); who != nil {
			who.text = "signed in as " + tok.Summary()
		}
		p.note = "signed in as " + tok.Summary()
		return nil
	}
	p.draftTok = &tok
	p.pickFrom = pmAdd
	return m.fetchCatalog(tok)
}

// fetchCatalog lists the models a subscription can use.
func (m *model) fetchCatalog(tok chatgpt.Tokens) tea.Cmd {
	p := m.panel
	p.after, p.mode, p.err = p.pickFrom, pmBusy, ""
	p.busy = "fetching the models your plan includes"
	ep := m.o.Login
	return tea.Batch(p.spin.Tick, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ms, err := chatgpt.ListModels(ctx, nil, ep, tok)
		if err != nil {
			return modelsFetchedMsg{err: err, owner: p}
		}
		ids := make([]string, 0, len(ms))
		for _, cm := range ms {
			ids = append(ids, cm.ID)
		}
		return modelsFetchedMsg{ids: ids, catalog: ms, owner: p}
	})
}

// saveDraft writes the draft with the given model ids (nil keeps the
// draft's models) and, when nothing is connected yet, connects to the
// first one.
func (m *model) saveDraft(ids []string) tea.Cmd {
	p := m.panel
	d := p.draft
	if ids != nil {
		existing := map[string]config.Model{}
		for _, md := range d.Models {
			existing[md.ID] = md
		}
		d.Models = nil
		for _, id := range ids {
			md, exists := existing[id]
			if !exists {
				md = setup.GuessModel(id, p.preset)
			}
			if cm, ok := p.catalog[id]; ok && d.Kind == config.KindSubscription {
				if !exists {
					md.Name, md.ContextWindow, md.Reasoning = cm.Name, cm.ContextWindow, true
				}
				if len(md.ReasoningLevels) == 0 && len(cm.ReasoningLevels) > 0 {
					md.ReasoningLevels = append([]string(nil), cm.ReasoningLevels...)
					md.ReasoningDefault = cm.Reasoning
				}
			}
			d.Models = append(d.Models, md)
		}
	}
	first := ""
	if en := d.EnabledModels(); len(en) > 0 {
		first = p.draftID + "/" + en[0].ID
	}
	defSel := ""
	adding := p.editID == ""
	if adding && first != "" && (p.cfg.Default == "" || m.sess.Agent == nil) {
		defSel = first
	}
	done := fmt.Sprintf("saved %s", p.draftID)
	if adding {
		done = fmt.Sprintf("added %s with %d model(s)", p.draftID, len(d.Models))
	}
	err := config.UpdateConnection(m.o.ConfigPath, p.editID, p.draftID, d, defSel)
	if err == nil && d.Kind == config.KindSubscription && p.store != nil {
		// The sign-in follows the connection: saved under the new id, or
		// moved when the connection was renamed.
		switch {
		case p.draftTok != nil:
			err = p.store.Put(p.draftID, *p.draftTok)
		case !adding && p.editID != p.draftID:
			err = p.store.Rename(p.editID, p.draftID)
		}
	}
	p.form = nil
	p.mode = pmList
	m.afterEdit(err, done, p.draftID)
	p.draft, p.draftID, p.editID, p.draftTok = config.Connection{}, "", "", nil // do not keep the secret around
	if err != nil || !adding || m.sess.Agent != nil || first == "" || d.Disabled {
		return nil
	}
	return m.connect(first, false)
}

// ---- picker ----

func (m *model) openPick(ids []string) tea.Cmd {
	p := m.panel
	p.mode, p.err = pmPick, ""
	p.found, p.pick = ids, 0
	p.picked = map[string]bool{}
	for _, md := range p.draft.Models {
		p.picked[md.ID] = true
	}
	p.filter.Reset()
	p.applyFilter()
	return p.filter.Focus()
}

func (p *modelsPanel) applyFilter() {
	q := strings.ToLower(strings.TrimSpace(p.filter.Value()))
	p.visible = p.visible[:0]
	for _, id := range p.found {
		if q == "" || strings.Contains(strings.ToLower(id), q) {
			p.visible = append(p.visible, id)
		}
	}
	if p.pick > len(p.visible)+1 {
		p.pick = len(p.visible) + 1
	}
}

func (m *model) pickKey(k tea.KeyPressMsg) tea.Cmd {
	p := m.panel
	n := len(p.visible)
	switch k.String() {
	case "esc", "ctrl+c":
		p.filter.Blur()
		p.mode = p.pickFrom
		return nil
	case "up", "ctrl+p", "shift+tab":
		p.pick = max(0, p.pick-1)
		return nil
	case "down", "ctrl+n", "tab":
		p.pick = min(n+1, p.pick+1)
		return nil
	case "ctrl+a":
		all := true
		for _, id := range p.visible {
			if !p.picked[id] {
				all = false
			}
		}
		for _, id := range p.visible {
			p.picked[id] = !all
		}
		return nil
	case "space", " ", "enter":
		switch {
		case p.pick < n:
			id := p.visible[p.pick]
			p.picked[id] = !p.picked[id]
		case p.pick == n:
			return m.pickAction("confirm")
		default:
			return m.pickAction("back")
		}
		return nil
	}
	var cmd tea.Cmd
	p.filter, cmd = p.filter.Update(k)
	p.applyFilter()
	return cmd
}

func (m *model) pickAction(id string) tea.Cmd {
	p := m.panel
	switch id {
	case "back":
		p.filter.Blur()
		p.mode = p.pickFrom
	case "confirm":
		var ids []string
		for _, id := range p.found {
			if p.picked[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			p.err = "tick at least one model (space)"
			return nil
		}
		p.filter.Blur()
		return m.saveDraft(ids)
	}
	return nil
}

func (p *modelsPanel) countPicked() int {
	n := 0
	for _, v := range p.picked {
		if v {
			n++
		}
	}
	return n
}

// ---- connecting ----

// connect opens selector in the background. With testOnly it only sends a
// ping and reports; otherwise the agent is swapped in and the default is
// updated.
func (m *model) connect(sel string, testOnly bool) tea.Cmd {
	m.connectionGen++
	gen := m.connectionGen
	if m.o.Connect == nil {
		return func() tea.Msg { return connectedMsg{gen: gen, sel: sel, err: errors.New("no connector available")} }
	}
	var tick tea.Cmd
	if p := m.panel; p != nil {
		if p.mode != pmBusy {
			p.after = p.mode
		}
		p.mode, p.err = pmBusy, ""
		p.busyConn = providerOf(sel)
		p.busy = "connecting to " + sel
		if testOnly {
			p.busy = "testing " + sel
		}
		tick = p.spin.Tick
	} else {
		m.status = "connecting to " + sel + "…"
	}
	connect := m.o.Connect
	return tea.Batch(tick, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		c, err := connect(ctx, sel)
		if err != nil {
			return connectedMsg{gen: gen, sel: sel, err: err, testOnly: testOnly}
		}
		if !testOnly {
			return connectedMsg{gen: gen, sel: sel, conn: c, name: c.Name}
		}
		// Throwaway copy: no tools, one step, its own history.
		probe := &agent.Agent{Model: c.Agent.Model, Tools: tools.NewRegistry(), Policy: agent.AllowAll{}, MaxSteps: 1}
		start := time.Now()
		if err := probe.Run(ctx, "Reply with the single word: pong", func(agent.Event) {}); err != nil {
			return connectedMsg{gen: gen, sel: sel, err: err, testOnly: true}
		}
		return connectedMsg{gen: gen, sel: sel, name: c.Name, took: time.Since(start), testOnly: true}
	})
}

// panelMsg handles async results and spinner ticks. It reports whether the
// message was consumed. connectedMsg is handled even with the panel closed
// because /model uses the same path.
func (m *model) panelMsg(msg tea.Msg) (tea.Cmd, bool) {
	p := m.panel
	switch msg := msg.(type) {
	case spinner.TickMsg:
		if p == nil || (p.mode != pmBusy && p.mode != pmLogin) {
			return nil, true
		}
		var cmd tea.Cmd
		p.spin, cmd = p.spin.Update(msg)
		return cmd, true

	case loginDoneMsg:
		if p == nil || (msg.ctx != nil && p.loginCtx != msg.ctx) {
			return nil, true
		}
		return m.loginDone(msg), true

	case modelsFetchedMsg:
		if p == nil || p.form == nil || (msg.owner != nil && msg.owner != p) {
			return nil, true
		}
		p.busy, p.busyConn = "", ""
		if msg.catalog != nil {
			p.catalog = make(map[string]chatgpt.Model, len(msg.catalog))
			for _, cm := range msg.catalog {
				p.catalog[cm.ID] = cm
			}
		}
		if msg.err != nil {
			p.mode, p.err = p.pickFrom, msg.err.Error()
			if p.mode == pmAdd {
				// Fall back to typing model ids by hand.
				p.form.get("models").hidden = false
				p.form.get("save").hidden = false
				return p.form.focusID("models"), true
			}
			return nil, true
		}
		return m.openPick(msg.ids), true

	case connectedMsg:
		if msg.gen != m.connectionGen {
			return nil, true
		}
		if p != nil && p.mode == pmBusy {
			p.mode, p.busy, p.busyConn = p.after, "", ""
		}
		id := providerOf(msg.sel)
		switch {
		case msg.err != nil:
			text := fmt.Sprintf("%s: %v", msg.sel, msg.err)
			if p != nil {
				p.err = text
				p.errs[id] = msg.err.Error()
			} else {
				m.status = ""
				m.appendSystem("error: " + text)
				m.refresh()
			}
		case msg.testOnly:
			text := fmt.Sprintf("%s answered in %s", msg.sel, msg.took.Round(time.Millisecond))
			if p != nil {
				p.note = text
				delete(p.errs, id)
			} else {
				m.flash(text)
				m.refresh()
			}
		default:
			old := m.sess
			m.setSession(msg.conn)
			m.carryConv(old, msg.conn) // the conversation continues on the new model
			m.saveConv()               // remember the model even if no further prompt is sent
			text := "using " + msg.name
			if old.Agent != nil && len(old.Agent.Messages()) > 0 {
				text += " · conversation carried over"
				if providerOf(old.Name) != providerOf(msg.name) {
					text += " (reasoning dropped: different provider)"
				}
			}
			if err := config.SetDefault(m.o.ConfigPath, msg.sel); err != nil {
				m.appendSystem("could not save as default: " + err.Error())
				m.refresh()
			}
			if p != nil {
				delete(p.errs, id)
				p.reload(m.o.Cwd, msg.sel)
				p.note = text + " · esc to chat"
			} else {
				m.status = ""
				m.flash(text)
				m.refresh()
			}
		}
		return nil, true
	}

	// Anything else (bracketed paste, clipboard results, cursor blink)
	// belongs to the focused text field while a form or the picker is
	// open; otherwise it would land in the hidden chat box.
	if p != nil {
		switch p.mode {
		case pmAdd, pmEdit:
			return p.form.update(msg), true
		case pmPick:
			var cmd tea.Cmd
			p.filter, cmd = p.filter.Update(msg)
			p.applyFilter()
			return cmd, true
		}
	}
	return nil, false
}

// ---- mouse ----

// panelClick maps a click in the transcript area. Outside the box closes
// the list (forms keep what was typed); inside, rows select, the status
// cell toggles, buttons act, and form fields take focus.
func (m *model) panelClick(x, y int) tea.Cmd {
	p := m.panel
	b := p.box
	if b.w == 0 {
		return nil
	}
	if x < b.x || x >= b.x+b.w || y < b.y || y >= b.y+b.h {
		if p.mode == pmList {
			m.closeModels()
		}
		return nil
	}
	ix, iy := x-b.x-1, y-b.y-1 // inside the border
	for i, r := range b.buttons {
		if r.contains(ix, iy) {
			switch p.mode {
			case pmList, pmConfirmDelete:
				return m.panelAction(b.buttonIDs[i])
			case pmPick:
				return m.pickAction(b.buttonIDs[i])
			case pmLogin:
				return m.loginAction(b.buttonIDs[i])
			}
		}
	}
	switch p.mode {
	case pmList:
		li := iy - b.bodyY
		if li < 0 || li >= len(b.rows) || b.rows[li] < 0 {
			return nil
		}
		idx := b.rows[li]
		r := p.rows[idx]
		if r.model == nil && ix >= b.statusX {
			p.cursor = idx
			m.toggleRow(r)
			return nil
		}
		if idx == p.cursor {
			return m.listKey("enter")
		}
		p.cursor = idx
	case pmPick:
		li := iy - b.bodyY
		if li < 0 || li >= len(b.rows) || b.rows[li] < 0 {
			return nil
		}
		p.pick = b.rows[li]
		id := p.visible[p.pick]
		p.picked[id] = !p.picked[id]
	case pmAdd, pmEdit:
		action, cmd := p.form.click(ix, iy-b.formY+b.formTop)
		return tea.Batch(cmd, m.formAction(action))
	}
	return nil
}

// panelWheel moves the list or picker cursor.
func (p *modelsPanel) wheel(dir int) {
	switch p.mode {
	case pmList:
		p.cursor = min(max(0, len(p.rows)-1), max(0, p.cursor+dir))
	case pmPick:
		p.pick = min(len(p.visible)+1, max(0, p.pick+dir))
	}
}

// ---- view ----

var (
	titleStyle    lipgloss.Style
	selStyle      lipgloss.Style
	disabledStyle lipgloss.Style
	keyStyle      lipgloss.Style
)

// panelBox renders the panel and returns it with the offset at which it
// sits inside a width×height area, plus the terminal cursor for the
// focused text field (relative to the same area).
func (m *model) panelBox(width, height int) (box string, x, y int, cursor *tea.Cursor) {
	p := m.panel
	p.box = panelGeom{}
	inner := max(20, min(width-4, max(panelWidth, min(panelMaxWidth, width*85/100))))
	maxLines := max(3, height-2) // inside the border
	// List-like screens fill most of the terminal rather than hugging
	// their rows; forms grow with their fields.
	minBody := max(6, height*7/10-6)

	title, right := " Connections", "esc"
	switch p.mode {
	case pmAdd:
		title = " Add connection"
	case pmEdit:
		title = " Edit " + p.editID
	case pmPick:
		title = fmt.Sprintf(" Pick models · %d found · %d ticked", len(p.found), p.countPicked())
		right = "esc back"
	case pmConfirmDelete:
		title = " Delete?"
		right = "y / n"
	case pmBusy:
		right = "esc"
	case pmLogin:
		title = " Sign in with ChatGPT"
		right = "esc cancel"
	}
	head := " " + titleStyle.Render(ansi.Truncate(title, inner-lipgloss.Width(right)-3, "…"))
	head += strings.Repeat(" ", max(1, inner-lipgloss.Width(head)-lipgloss.Width(right)-1)) + popupDesc.Render(right)
	rule := paletteRule.Render(strings.Repeat("─", inner))

	// Footer: spinner / error / note, always one line.
	var footer string
	switch {
	case p.mode == pmBusy:
		footer = " " + p.spin.View() + " " + p.busy
	case p.mode == pmLogin && p.err == "" && p.note == "":
		footer = " " + p.spin.View() + " waiting for the browser…"
	case p.err != "":
		footer = " " + errStyle.Render(p.err)
	case p.note != "":
		footer = " " + dimStyle.Render(p.note)
	}
	footer = ansi.Truncate(footer, inner, "…")

	// Buttons row for list-like screens.
	var buttons string
	var buttonIDs []string
	var buttonRects []rect
	addButtons := func(items ...[2]string) {
		x := 1
		var parts []string
		for _, it := range items {
			txt := "  " + it[1] + "  "
			w := lipgloss.Width(txt)
			buttonRects = append(buttonRects, rect{x: x, y: 0, w: w, h: 1})
			buttonIDs = append(buttonIDs, it[0])
			st := formButton
			if p.hoverBtn == it[0] {
				st = formButtonHv
			}
			parts = append(parts, st.Render(txt))
			x += w + 2
		}
		buttons = " " + strings.Join(parts, "  ")
	}
	switch p.mode {
	case pmList, pmBusy:
		if p.after == pmEdit && p.mode == pmBusy {
			break
		}
		addButtons([2]string{"add", "+ Add"}, [2]string{"edit", "Edit"}, [2]string{"delete", "Delete"})
	case pmConfirmDelete:
		addButtons([2]string{"delete-yes", "Delete"}, [2]string{"delete-no", "Keep"})
	case pmLogin:
		addButtons([2]string{"login-copy", "Copy link"}, [2]string{"login-open", "Open browser"}, [2]string{"login-cancel", "Cancel"})
	case pmPick:
		// Rendered as part of the body so the cursor can reach them.
	}

	fixed := 2 + 2 // head, rule, rule, footer
	if buttons != "" {
		fixed++
	}
	// The box grows with its content up to the available height.
	bodyH := max(1, maxLines-fixed)
	lines := []string{head, rule}
	p.box.bodyY = len(lines)
	switch p.mode {
	case pmAdd, pmEdit:
		lines = append(lines, m.formLines(inner, bodyH)...)
	case pmPick:
		lines = append(lines, p.pickLines(inner, min(bodyH, max(minBody, len(p.visible)+2)))...)
	case pmLogin:
		lines = append(lines, p.loginLines(inner, bodyH)...)
	default:
		want := max(minBody, p.listHeight())
		if len(p.rows) == 0 {
			want = max(minBody, 7)
		}
		lines = append(lines, p.listLines(m, inner, min(bodyH, want))...)
	}
	lines = append(lines, rule, footer)
	if buttons != "" {
		for i := range buttonRects {
			buttonRects[i].y = len(lines)
		}
		lines = append(lines, buttons)
		p.box.buttons, p.box.buttonIDs = buttonRects, buttonIDs
	}
	if p.mode == pmPick {
		// Picker buttons live on the last body line; record their rects.
		for i := range p.pickButtons {
			p.pickButtons[i].y = p.box.bodyY + p.pickButtonLine
		}
		p.box.buttons, p.box.buttonIDs = p.pickButtons, []string{"confirm", "back"}
	}

	if len(lines) > maxLines { // tiny terminal: keep the top of the box
		lines = lines[:maxLines]
	}
	box = paletteBorder.Width(inner + 2).Render(strings.Join(lines, "\n"))
	x = (width - inner - 2) / 2
	y = max(0, (height-lipgloss.Height(box))/2)
	p.box.x, p.box.y, p.box.w, p.box.h = x, y, inner+2, lipgloss.Height(box)

	switch p.mode {
	case pmAdd, pmEdit:
		if c := p.form.cursor(); c != nil {
			c.Y -= p.box.formTop
			if c.Y >= 0 && c.Y < bodyH {
				c.X += x + 1
				c.Y += y + 1 + p.box.formY
				cursor = c
			}
		}
	case pmPick:
		if c := p.filter.Cursor(); c != nil {
			c.X += x + 1
			c.Y += y + 1 + p.box.bodyY
			cursor = c
		}
	}
	return box, x, y, cursor
}

// formLines renders the form into at most height lines, scrolling so the
// focused field stays visible.
func (m *model) formLines(inner, height int) []string {
	p := m.panel
	all := p.form.render(inner)
	p.box.formY = p.box.bodyY
	top := 0
	if len(all) > height {
		if fl := p.form.focused(); fl != nil && len(fl.rects) > 0 {
			fy := fl.rects[0].y
			if fy >= height {
				top = fy - height + 1
			}
		}
		top = min(top, len(all)-height)
		all = all[top : top+height]
	}
	p.box.formTop = top
	return all
}

// loginLines renders the sign-in screen: what is happening and the link,
// wrapped so it can be selected and copied when no browser opened.
func (p *modelsPanel) loginLines(inner, height int) []string {
	p.box.rows = p.box.rows[:0]
	out := []string{
		"",
		"  Finish signing in to ChatGPT in the browser tab that just opened.",
		"  Come back here when it says you are signed in.",
		"",
		"  " + dimStyle.Render("No browser? Press ") + keyStyle.Render("c") + dimStyle.Render(" to copy the link, or open it yourself:"),
	}
	if p.login != nil {
		w := max(10, inner-4)
		for u := p.login.URL; u != ""; {
			n := min(len(u), w)
			out = append(out, "  "+dimStyle.Render(u[:n]))
			u = u[n:]
		}
	}
	if len(out) > height {
		out = out[:height]
	}
	for len(out) < height {
		out = append(out, "")
	}
	for i := range out {
		out[i] = ansi.Truncate(out[i], inner, "…")
		p.box.rows = append(p.box.rows, -1)
	}
	return out
}

// listLines renders the connection/model rows into exactly height lines.
func (p *modelsPanel) listLines(m *model, inner, height int) []string {
	p.box.rows = p.box.rows[:0]
	if len(p.rows) == 0 {
		out := []string{
			"",
			"  No connections yet.",
			"",
			"  Press " + keyStyle.Render("a") + " (or click " + formButton.Render("  + Add  ") + ") to connect a server, an API key or a ChatGPT plan.",
			"  " + dimStyle.Render("Ollama, llama.cpp, LM Studio, vLLM, OpenAI, Anthropic, OpenRouter, DeepSeek… anything serving /v1."),
			"",
			"  " + dimStyle.Render("Or edit "+m.o.ConfigPath+" by hand and press r."),
		}
		for len(out) < height {
			out = append(out, "")
		}
		for i := range out {
			out[i] = ansi.Truncate(out[i], inner, "…")
			p.box.rows = append(p.box.rows, -1)
		}
		return out[:height]
	}
	// Display lines: each row, with a blank line before every connection
	// after the first. Scrolling works in display lines so the cursor row
	// is always inside the window.
	entries := p.listEntries()
	curLine := 0
	for i, e := range entries {
		if e == p.cursor {
			curLine = i
		}
	}
	if curLine < p.top {
		p.top = curLine
	}
	if curLine >= p.top+height {
		p.top = curLine - height + 1
	}
	p.top = max(0, min(p.top, max(0, len(entries)-height)))
	p.box.statusX = inner - panelStatusW
	nameW := 12
	for _, r := range p.rows {
		if r.model == nil {
			nameW = max(nameW, lipgloss.Width(r.provID))
		}
	}
	nameW = min(nameW, 20)
	current := currentSelector(m.sess.Name)

	var out []string
	for li := p.top; li < len(entries) && len(out) < height; li++ {
		i := entries[li]
		if i < 0 {
			out = append(out, "")
			p.box.rows = append(p.box.rows, -1)
			continue
		}
		r := p.rows[i]
		cur := i == p.cursor && (p.mode == pmList || p.mode == pmBusy)
		prefix := "  "
		if cur {
			prefix = selStyle.Render("› ")
		}
		var line string
		if r.model == nil {
			name := padRight(ansi.Truncate(r.provID, nameW, "…"), nameW)
			if r.prov.Disabled {
				name = disabledStyle.Render(name)
			} else {
				name = lipgloss.NewStyle().Bold(true).Render(name)
			}
			left := prefix + name + "  " + dimStyle.Render(padRight(r.prov.Kind.Label(), 12)) + " " + dimStyle.Render(p.authSummary(r.provID, r.prov))
			left = ansi.Truncate(left, p.box.statusX-1, "…")
			line = padRightANSI(left, p.box.statusX) + p.status(r.provID, r.prov)
		} else {
			sel := r.selector()
			mark := dimStyle.Render("○")
			if sel == current {
				mark = okStyle.Render("●")
			}
			id := r.model.ID
			off := r.prov.Disabled || r.model.Disabled
			if off {
				id = disabledStyle.Render(id)
			}
			line = prefix + "    " + mark + " " + id
			if sel == p.cfg.Default {
				line += "  " + selStyle.Render("★ default")
			}
			if sel == current {
				line += "  " + okStyle.Render("connected")
			}
			if off {
				line += "  " + dimStyle.Render("disabled")
			}
			line += "  " + dimStyle.Render(setup.Describe(*r.model, setup.Preset{Compat: r.prov.Compat}))
			line = ansi.Truncate(line, inner, "…")
		}
		if cur {
			line = fillRow(line, inner)
		}
		out = append(out, line)
		p.box.rows = append(p.box.rows, i)
	}
	for len(out) < height {
		out = append(out, "")
		p.box.rows = append(p.box.rows, -1)
	}
	return out
}

// listEntries is the list as display lines: row indexes, with -1 for the
// blank separator drawn above every connection but the first.
func (p *modelsPanel) listEntries() []int {
	var out []int
	for i, r := range p.rows {
		if r.model == nil && i > 0 {
			out = append(out, -1)
		}
		out = append(out, i)
	}
	return out
}

// listHeight is how many display lines the whole list takes.
func (p *modelsPanel) listHeight() int { return len(p.listEntries()) }

// status renders the right-aligned state cell of a connection row.
func (p *modelsPanel) status(id string, c config.Connection) string {
	switch {
	case p.mode == pmBusy && p.busyConn == id:
		return toolStyle.Render(padRight("◌ connecting", panelStatusW))
	case p.errs[id] != "":
		return errStyle.Render(padRight("✕ error", panelStatusW))
	case c.Disabled:
		return dimStyle.Render(padRight("○ disabled", panelStatusW))
	}
	return okStyle.Render(padRight("● enabled", panelStatusW))
}

// authSummary is the short "where/how" text of a connection row: the host
// for servers, a masked key or its $ENV name for vendors, the account for
// subscriptions.
func (p *modelsPanel) authSummary(id string, c config.Connection) string {
	if c.Kind == config.KindSubscription {
		if t, ok := p.auth[id]; ok {
			return t.Summary()
		}
		return "not signed in"
	}
	host := c.BaseURL
	if u, err := url.Parse(c.BaseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	key := ""
	switch {
	case c.APIKey == "":
	case strings.HasPrefix(c.APIKey, "$"):
		key = c.APIKey
	case len(c.APIKey) > 8:
		key = c.APIKey[:3] + "…" + c.APIKey[len(c.APIKey)-4:]
	default:
		key = "key set"
	}
	if c.Kind == config.KindAPIKey && key != "" {
		return key
	}
	if key != "" {
		return host + " · " + key
	}
	return host
}

// pickLines renders the filter, the checkbox rows and the Confirm/Back
// buttons into exactly height lines.
func (p *modelsPanel) pickLines(inner, height int) []string {
	p.box.rows = p.box.rows[:0]
	p.filter.SetWidth(inner - 4)
	out := []string{ansi.Truncate(p.filter.View(), inner, "")}
	p.box.rows = append(p.box.rows, -1)

	n := len(p.visible)
	listH := max(1, height-2) // filter + buttons
	top := 0
	if p.pick < n && p.pick >= listH {
		top = p.pick - listH + 1
	}
	if p.pick >= n && n > listH {
		top = n - listH
	}
	for i := top; i < n && i < top+listH; i++ {
		id := p.visible[i]
		box := "[ ]"
		if p.picked[id] {
			box = okStyle.Render("[x]")
		}
		prefix := "  "
		if i == p.pick {
			prefix = selStyle.Render("› ")
		}
		desc := setup.Describe(setup.GuessModel(id, p.preset), p.preset)
		line := ansi.Truncate(fmt.Sprintf("%s%s %s  %s", prefix, box, id, dimStyle.Render(desc)), inner, "…")
		if i == p.pick {
			line = fillRow(line, inner)
		}
		out = append(out, line)
		p.box.rows = append(p.box.rows, i)
	}
	if n == 0 {
		out = append(out, dimStyle.Render("  no model matches the filter"))
		p.box.rows = append(p.box.rows, -1)
	}
	for len(out) < height-1 {
		out = append(out, "")
		p.box.rows = append(p.box.rows, -1)
	}
	out = out[:height-1]
	p.box.rows = p.box.rows[:height-1]

	// Buttons: focused one inverted when the cursor is on it.
	x := 2
	p.pickButtons = p.pickButtons[:0]
	var parts []string
	for i, label := range []string{"Confirm", "Back"} {
		txt := "  " + label + "  "
		w := lipgloss.Width(txt)
		p.pickButtons = append(p.pickButtons, rect{x: x, w: w, h: 1})
		if p.hoverBtn == []string{"confirm", "back"}[i] {
			parts = append(parts, formButtonHv.Bold(p.pick == n+i).Render(txt))
		} else if p.pick == n+i {
			parts = append(parts, formButtonFc.Render(txt))
		} else {
			parts = append(parts, formButton.Render(txt))
		}
		x += w + 2
	}
	prefix := "  "
	if p.pick >= n {
		prefix = selStyle.Render("› ")
	}
	p.pickButtonLine = len(out)
	out = append(out, prefix+strings.Join(parts, "  "))
	p.box.rows = append(p.box.rows, -1)
	return out
}

// panelBottom renders the key legend shown in the box under the status bar.
func (m *model) panelBottom() string {
	p := m.panel
	switch p.mode {
	case pmConfirmDelete:
		return keyStyle.Render("y") + " delete  " + keyStyle.Render("n") + " keep"
	case pmAdd, pmEdit:
		return legend([][2]string{{"tab/↑↓", "move"}, {"enter", "next / press"}, {"←→", "choose"}, {"space", "tick"}, {"esc", "cancel"}})
	case pmPick:
		return legend([][2]string{{"type", "filter"}, {"space/enter", "tick"}, {"ctrl+a", "all"}, {"↓", "to Confirm"}, {"esc", "back"}})
	case pmBusy:
		return dimStyle.Render("esc back")
	case pmLogin:
		return legend([][2]string{{"c", "copy link"}, {"o", "open browser again"}, {"esc", "cancel"}})
	}
	if len(p.rows) == 0 {
		return legend([][2]string{{"a", "add"}, {"r", "reload"}, {"esc", "close"}})
	}
	return legend([][2]string{{"enter", "use / edit"}, {"space", "on/off"}, {"a", "add"}, {"d", "delete"}, {"t", "test"}, {"esc", "close"}})
}

func legend(items [][2]string) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, keyStyle.Render(it[0])+" "+it[1])
	}
	return strings.Join(parts, dimStyle.Render("  ·  "))
}

// currentSelector strips a ":thinking" suffix from the connected model
// name so it matches row selectors.
func currentSelector(name string) string {
	if i := strings.LastIndex(name, ":"); i > 0 && !strings.Contains(name[i:], "/") {
		return name[:i]
	}
	return name
}

func padRightANSI(s string, w int) string {
	if n := w - ansi.StringWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
