package tui

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dantearo/arkex/internal/config"
)

// panelModel returns a model whose global config lives in a temp ARKEX_HOME
// and whose Connections panel is open.
func panelModel(t *testing.T) (*model, *string) {
	home := t.TempDir()
	t.Setenv("ARKEX_HOME", home)
	m, sel := testModel(t)
	m.o.ConfigPath = home + "/config.json"
	m.layout()
	m.openModels("")
	return m, sel
}

func panelText(m *model) string {
	return ansi.Strip(m.View().Content)
}

// runCmd executes a command returned by Update (flattening batches) and
// returns the messages it produced.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	var out []tea.Msg
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			out = append(out, runCmd(c)...)
		}
	case nil:
	default:
		out = append(out, msg)
	}
	return out
}

func typeText(m *model, s string) {
	for _, r := range s {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// click renders the frame (so geometry is current) and sends a left click
// at a position inside the panel box, given in inner-box coordinates.
func clickInner(m *model, x, y int) {
	m.View()
	b := m.panel.box
	m.Update(tea.MouseClickMsg{X: b.x + 1 + x, Y: b.y + 1 + y, Button: tea.MouseLeft})
}

func mustLoad(t *testing.T, m *model) *config.Config {
	t.Helper()
	cfg, err := config.Load(m.o.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConnectionsAddFlowFormPickerSaveConnect(t *testing.T) {
	m, sel := panelModel(t)
	p := m.panel
	if len(p.rows) != 0 || !strings.Contains(panelText(m), "No connections yet") {
		t.Fatalf("empty panel not shown:\n%s", panelText(m))
	}

	typeKeys(m, "a")
	if p.mode != pmAdd || p.form.focused().id != "kind" {
		t.Fatalf("mode after a = %v focus=%v", p.mode, p.form.focused())
	}
	f := p.form
	// Kind → API key swaps the service list and prefills OpenAI.
	typeKeys(m, "right")
	if f.get("service").options[0].label != "OpenAI" || f.value("url") != "https://api.openai.com/v1" || f.value("name") != "openai" {
		t.Fatalf("api-key kind prefill: service=%v url=%q name=%q", f.get("service").options[0], f.value("url"), f.value("name"))
	}
	if !strings.Contains(panelText(m), "Runtime · soon") {
		t.Fatal("upcoming kinds not shown")
	}
	// Subscription swaps the URL/key fields for a sign-in button.
	typeKeys(m, "right")
	if f.get("kind").sel != 2 || !f.get("url").hidden || !f.get("key").hidden || f.get("login").hidden || !f.get("fetch").hidden {
		t.Fatalf("subscription fields: url hidden=%v key hidden=%v login hidden=%v fetch hidden=%v", f.get("url").hidden, f.get("key").hidden, f.get("login").hidden, f.get("fetch").hidden)
	}
	if !strings.Contains(panelText(m), "Sign in with ChatGPT") || strings.Contains(panelText(m), "Base URL") {
		t.Fatalf("subscription form:\n%s", panelText(m))
	}
	// Disabled kinds are skipped: right from Subscription wraps to LLM server.
	typeKeys(m, "right")
	if f.get("kind").sel != 0 || f.value("url") != "http://localhost:11434/v1" || f.value("name") != "ollama" || f.get("url").hidden || !f.get("login").hidden {
		t.Fatalf("wrap to llm-server: sel=%d url=%q name=%q", f.get("kind").sel, f.value("url"), f.value("name"))
	}
	// Service → Other clears the preset URL and auto name.
	typeKeys(m, "tab", "right", "right", "right", "right")
	if f.value("url") != "" || f.value("name") != "" || p.preset.ID != "other-server" {
		t.Fatalf("Other preset: url=%q name=%q preset=%s", f.value("url"), f.value("name"), p.preset.ID)
	}
	// Name, then URL by paste (bracketed paste arrives as a message).
	typeKeys(m, "tab")
	typeText(m, "bad name!")
	typeKeys(m, "enter") // enter moves to the next field
	if f.focused().id != "url" {
		t.Fatalf("enter did not advance: focus=%s", f.focused().id)
	}
	m.Update(tea.PasteMsg{Content: "http://localhost:11434/v1/"})
	if f.value("url") != "http://localhost:11434/v1/" {
		t.Fatalf("paste did not reach the field: %q", f.value("url"))
	}
	// Secret: masked on screen, show/hide button reveals it.
	typeKeys(m, "tab")
	typeText(m, "sk-secret-123")
	if strings.Contains(panelText(m), "sk-secret-123") {
		t.Fatal("API key echoed in the panel")
	}
	typeKeys(m, "tab", "enter")
	if !strings.Contains(panelText(m), "sk-secret-123") || f.get("reveal").label != "hide" {
		t.Fatal("show button did not reveal the key")
	}
	typeKeys(m, "enter")
	if strings.Contains(panelText(m), "sk-secret-123") {
		t.Fatal("hide button did not mask the key")
	}
	// Enabled checkbox, then Fetch: the bad name is refused inline.
	typeKeys(m, "tab", "tab", "enter")
	if p.mode != pmAdd || f.get("name").err == "" || f.focused().id != "name" {
		t.Fatalf("invalid id accepted: mode=%v err=%q focus=%s", p.mode, f.get("name").err, f.focused().id)
	}
	f.get("name").input.SetValue("ollama")
	_ = f.focusID("fetch")
	typeKeys(m, "enter")
	if p.mode != pmBusy || !strings.Contains(p.busy, "fetching http://localhost:11434/v1/models") {
		t.Fatalf("fetch not started: mode=%v busy=%q", p.mode, p.busy)
	}
	if p.draft.APIKey != "sk-secret-123" || p.draft.Kind != config.KindLLMServer || p.draftID != "ollama" {
		t.Fatalf("draft = %+v id=%q", p.draft, p.draftID)
	}
	// Fetch failure returns to the form with a manual Models field.
	m.Update(modelsFetchedMsg{err: errors.New("connection refused")})
	if p.mode != pmAdd || !strings.Contains(panelText(m), "connection refused") || f.get("models").hidden || f.focused().id != "models" {
		t.Fatalf("fetch error fallback: mode=%v text:\n%s", p.mode, panelText(m))
	}
	_ = f.focusID("fetch")
	typeKeys(m, "enter")
	m.Update(modelsFetchedMsg{ids: []string{"llama3.1", "qwen2.5-coder", "deepseek-r1"}})
	if p.mode != pmPick || len(p.visible) != 3 {
		t.Fatalf("pick mode: %v visible=%v", p.mode, p.visible)
	}
	// Typing filters; space and enter both tick; the cursor walks onto Confirm.
	typeText(m, "e")
	if strings.Join(p.visible, ",") != "qwen2.5-coder,deepseek-r1" {
		t.Fatalf("filter = %v", p.visible)
	}
	typeKeys(m, "space", "down", "enter")
	if p.countPicked() != 2 {
		t.Fatalf("picked = %d", p.countPicked())
	}
	typeKeys(m, "down")
	if p.pick != len(p.visible) || !strings.Contains(panelText(m), "›   Confirm  ") {
		t.Fatalf("cursor not on Confirm: pick=%d\n%s", p.pick, panelText(m))
	}
	_, cmd := m.Update(key("enter"))

	cfg := mustLoad(t, m)
	conn, ok := cfg.Connections["ollama"]
	if !ok || conn.Kind != config.KindLLMServer || conn.BaseURL != "http://localhost:11434/v1" || conn.APIKey != "sk-secret-123" || len(conn.Models) != 2 {
		t.Fatalf("saved connection = %+v", conn)
	}
	if conn.Models[0].ID != "qwen2.5-coder" || conn.Models[1].ID != "deepseek-r1" {
		t.Fatalf("models = %+v", conn.Models)
	}
	if cfg.Default != "ollama/qwen2.5-coder" {
		t.Fatalf("default = %q", cfg.Default)
	}
	if st, err := os.Stat(m.o.ConfigPath); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Fatalf("config perms = %v err=%v", st.Mode(), err)
	}
	if p.draft.APIKey != "" || p.form != nil {
		t.Fatal("panel kept the draft after saving")
	}
	// Nothing was connected, so the panel dials the first model.
	if p.mode != pmBusy || !strings.Contains(p.busy, "connecting to ollama/qwen2.5-coder") || p.busyConn != "ollama" {
		t.Fatalf("connect not started: mode=%v busy=%q", p.mode, p.busy)
	}
	if !strings.Contains(panelText(m), "◌ connecting") {
		t.Fatalf("connecting status not shown:\n%s", panelText(m))
	}
	var connected *connectedMsg
	for _, msg := range runCmd(cmd) {
		if c, ok := msg.(connectedMsg); ok {
			connected = &c
		}
	}
	if connected == nil || *sel != "ollama/qwen2.5-coder" {
		t.Fatalf("connect cmd did not dial: sel=%q", *sel)
	}
	m.Update(*connected)
	if p.mode != pmList || !strings.Contains(p.err, "test") || p.errs["ollama"] == "" {
		t.Fatalf("connect error not shown: mode=%v err=%q errs=%v", p.mode, p.err, p.errs)
	}
	text := panelText(m)
	for _, want := range []string{"ollama", "LLM server", "localhost:11434 · sk-…-123", "✕ error", "qwen2.5-coder", "★ default", "deepseek-r1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("list missing %q:\n%s", want, text)
		}
	}
	if len(p.rows) != 3 {
		t.Fatalf("rows = %d", len(p.rows))
	}
}

func TestConnectionsAddAPIKeyManualModels(t *testing.T) {
	m, _ := panelModel(t)
	p := m.panel
	typeKeys(m, "a", "right") // API key kind, OpenAI preset
	f := p.form
	// Vendors need a key: Fetch without one is refused on the key field.
	_ = f.focusID("fetch")
	typeKeys(m, "enter")
	if p.mode != pmAdd || f.get("key").err == "" || f.focused().id != "key" {
		t.Fatalf("missing key accepted: mode=%v err=%q", p.mode, f.get("key").err)
	}
	typeText(m, "$OPENAI_API_KEY")
	_ = f.focusID("fetch")
	typeKeys(m, "enter")
	m.Update(modelsFetchedMsg{err: errors.New("401 Unauthorized: check the API key")})
	if p.mode != pmAdd || f.get("models").hidden || f.get("save").hidden {
		t.Fatalf("manual fallback not offered: mode=%v", p.mode)
	}
	// Empty list is refused; then two ids save without a fetch.
	_ = f.focusID("save")
	typeKeys(m, "enter")
	if p.mode != pmAdd || f.get("models").err == "" {
		t.Fatal("empty model list accepted")
	}
	typeText(m, "gpt-5.6-sol, o3 ,")
	_ = f.focusID("save")
	typeKeys(m, "enter")
	cfg := mustLoad(t, m)
	conn := cfg.Connections["openai"]
	if conn.Kind != config.KindAPIKey || conn.APIKey != "$OPENAI_API_KEY" || conn.Compat.Thinking != config.ThinkingReasoningEffort {
		t.Fatalf("saved = %+v", conn)
	}
	if len(conn.Models) != 2 || conn.Models[0].ID != "gpt-5.6-sol" || conn.Models[1].ID != "o3" || !conn.Models[0].Reasoning {
		t.Fatalf("models = %+v", conn.Models)
	}
	if p.mode != pmBusy { // connects because nothing else is configured
		t.Fatalf("mode = %v", p.mode)
	}
	typeKeys(m, "esc")
	text := panelText(m)
	if !strings.Contains(text, "API key") || !strings.Contains(text, "$OPENAI_API_KEY") || strings.Contains(text, "api.openai.com") {
		t.Fatalf("api-key row should show the env name, not the host:\n%s", text)
	}
}

func TestConnectionsListToggleDeleteAndClicks(t *testing.T) {
	m, _ := panelModel(t)
	conn := config.Connection{Kind: config.KindLLMServer, API: config.APIOpenAICompat, BaseURL: "http://h/v1", Models: []config.Model{{ID: "a"}, {ID: "b"}}}
	if err := config.SaveConnection(m.o.ConfigPath, "srv", conn, "srv/a"); err != nil {
		t.Fatal(err)
	}
	p := m.panel
	typeKeys(m, "r")
	if len(p.rows) != 3 || p.rows[1].selector() != "srv/a" {
		t.Fatalf("rows after reload = %d", len(p.rows))
	}

	// Disable model b (row 2) and re-enable it.
	typeKeys(m, "down", "down", "space")
	cfg := mustLoad(t, m)
	if !cfg.Connections["srv"].Models[1].Disabled || cfg.Connections["srv"].Models[0].Disabled {
		t.Fatalf("disable wrote wrong model: %+v", cfg.Connections["srv"].Models)
	}
	if p.cursor != 2 || p.note != "disabled srv/b" {
		t.Fatalf("cursor=%d note=%q", p.cursor, p.note)
	}
	typeKeys(m, "enter")
	if !strings.Contains(p.err, "disabled") {
		t.Fatalf("disabled model selectable: err=%q", p.err)
	}
	typeKeys(m, "x")
	if mustLoad(t, m).Connections["srv"].Models[1].Disabled {
		t.Fatal("re-enable failed")
	}

	// Delete needs y; anything else keeps.
	typeKeys(m, "d")
	if p.mode != pmConfirmDelete || !strings.Contains(p.note, "delete srv/b?") {
		t.Fatalf("confirm: mode=%v note=%q", p.mode, p.note)
	}
	typeKeys(m, "enter")
	if p.mode != pmList || len(mustLoad(t, m).Connections["srv"].Models) != 2 {
		t.Fatal("enter must not confirm a delete")
	}
	typeKeys(m, "d", "y")
	cfg = mustLoad(t, m)
	if len(cfg.Connections["srv"].Models) != 1 || cfg.Connections["srv"].Models[0].ID != "a" {
		t.Fatalf("delete model: %+v", cfg.Connections["srv"].Models)
	}
	if len(p.rows) != 2 || p.cursor != 1 {
		t.Fatalf("rows=%d cursor=%d after delete", len(p.rows), p.cursor)
	}

	// Connection delete via the confirm buttons: Keep, then Delete.
	typeKeys(m, "up", "d")
	if !strings.Contains(p.note, "delete connection srv and its 1 model(s)?") {
		t.Fatalf("confirm note = %q", p.note)
	}
	m.View()
	if len(p.box.buttonIDs) != 2 || p.box.buttonIDs[1] != "delete-no" {
		t.Fatalf("confirm buttons = %v", p.box.buttonIDs)
	}
	keep := p.box.buttons[1]
	clickInner(m, keep.x+1, keep.y)
	if p.mode != pmList || len(mustLoad(t, m).Connections) != 1 {
		t.Fatal("Keep button did not cancel")
	}
	// Status cell click toggles the connection on/off.
	m.View()
	clickInner(m, p.box.statusX+2, p.box.bodyY)
	if !mustLoad(t, m).Connections["srv"].Disabled || !strings.Contains(panelText(m), "○ disabled") {
		t.Fatalf("status click did not disable:\n%s", panelText(m))
	}
	// Clicking a model row selects it; a second click acts like enter.
	clickInner(m, 6, p.box.bodyY+1)
	if p.cursor != 1 {
		t.Fatalf("row click cursor = %d", p.cursor)
	}
	clickInner(m, 6, p.box.bodyY+1)
	if !strings.Contains(p.err, "disabled") {
		t.Fatalf("second click did not act: err=%q", p.err)
	}
	// [ Delete ] button acts on the cursor row: move to the connection first.
	typeKeys(m, "up")
	m.View()
	del := p.box.buttons[2]
	clickInner(m, del.x+1, del.y)
	typeKeys(m, "y")
	if _, still := mustLoad(t, m).Connections["srv"]; still || len(p.rows) != 0 {
		t.Fatalf("connection delete: rows=%d", len(p.rows))
	}

	// A click outside the box closes the panel; esc does too.
	m.View()
	m.Update(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	if m.panel != nil || !m.input.Focused() {
		t.Fatal("outside click did not close the panel")
	}
	m.openModels("")
	typeKeys(m, "esc")
	if m.panel != nil || !m.input.Focused() {
		t.Fatal("panel did not close")
	}
}

func TestConnectionsEditFormSaveRenameAndRefetch(t *testing.T) {
	m, _ := panelModel(t)
	conn := config.Connection{Kind: config.KindLLMServer, API: config.APIOpenAICompat, BaseURL: "http://h/v1", APIKey: "k",
		Headers: map[string]string{"X-Team": "a"},
		Models:  []config.Model{{ID: "a", Reasoning: true}, {ID: "b"}}}
	if err := config.SaveConnection(m.o.ConfigPath, "srv", conn, "srv/a"); err != nil {
		t.Fatal(err)
	}
	p := m.panel
	typeKeys(m, "r", "enter") // enter on the connection row opens the editor
	if p.mode != pmEdit || p.editID != "srv" {
		t.Fatalf("edit not opened: mode=%v", p.mode)
	}
	f := p.form
	if f.value("name") != "srv" || f.value("url") != "http://h/v1" || !f.get("enabled").on || f.get("model:b") == nil {
		t.Fatalf("edit form not prefilled: name=%q url=%q", f.value("name"), f.value("url"))
	}
	if !strings.Contains(panelText(m), "★ default") {
		t.Fatal("default marker missing in edit form")
	}
	// Untick model b, rename, save.
	_ = f.focusID("model:b")
	typeKeys(m, "space")
	f.get("name").input.SetValue("srv2")
	_ = f.focusID("save")
	typeKeys(m, "enter")
	cfg := mustLoad(t, m)
	if _, old := cfg.Connections["srv"]; old {
		t.Fatal("old id kept after rename")
	}
	got := cfg.Connections["srv2"]
	if got.Headers["X-Team"] != "a" || got.APIKey != "k" || len(got.Models) != 2 || !got.Models[1].Disabled || got.Models[0].Disabled || !got.Models[0].Reasoning {
		t.Fatalf("edited connection = %+v", got)
	}
	if cfg.Default != "srv2/a" {
		t.Fatalf("default = %q", cfg.Default)
	}
	if p.mode != pmList || p.note != "saved srv2" {
		t.Fatalf("after save: mode=%v note=%q", p.mode, p.note)
	}

	// Refetch keeps the entries of models that still exist and pre-ticks them.
	typeKeys(m, "e")
	f = p.form
	_ = f.focusID("refetch")
	typeKeys(m, "enter")
	if p.mode != pmBusy || p.pickFrom != pmEdit {
		t.Fatalf("refetch: mode=%v from=%v", p.mode, p.pickFrom)
	}
	m.Update(modelsFetchedMsg{ids: []string{"a", "c"}})
	if p.mode != pmPick || !p.picked["a"] || p.picked["c"] {
		t.Fatalf("picker preticks = %v", p.picked)
	}
	// esc from the picker returns to the form intact.
	typeKeys(m, "esc")
	if p.mode != pmEdit || p.form != f {
		t.Fatalf("esc did not return to the edit form: mode=%v", p.mode)
	}
	_ = f.focusID("refetch")
	typeKeys(m, "enter")
	m.Update(modelsFetchedMsg{ids: []string{"a", "c"}})
	typeKeys(m, "down", "space", "down", "enter") // tick c, Confirm
	cfg = mustLoad(t, m)
	got = cfg.Connections["srv2"]
	if len(got.Models) != 2 || got.Models[0].ID != "a" || !got.Models[0].Reasoning || got.Models[1].ID != "c" {
		t.Fatalf("models after refetch = %+v", got.Models)
	}
	if p.mode != pmList {
		t.Fatalf("mode after confirm = %v (editing must not auto-connect)", p.mode)
	}

	// Delete from the editor goes through the same confirmation.
	typeKeys(m, "e")
	_ = p.form.focusID("delete")
	typeKeys(m, "enter")
	if p.mode != pmConfirmDelete || p.delTarget.provID != "srv2" || p.delTarget.model != nil {
		t.Fatalf("delete from editor: mode=%v target=%+v", p.mode, p.delTarget)
	}
	typeKeys(m, "n")
	if p.mode != pmList {
		t.Fatal("n did not keep")
	}
}

func TestConnectionsPanelFramesStayInBounds(t *testing.T) {
	m, _ := panelModel(t)
	conn := config.Connection{Kind: config.KindLLMServer, API: config.APIOpenAICompat, BaseURL: "http://h/v1", Models: []config.Model{{ID: "a"}}}
	if err := config.SaveConnection(m.o.ConfigPath, "srv", conn, "srv/a"); err != nil {
		t.Fatal(err)
	}
	typeKeys(m, "r")
	for _, size := range [][2]int{{100, 30}, {50, 14}, {36, 10}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		frameLines(t, m, "list")
		typeKeys(m, "a")
		lines := frameLines(t, m, "add")
		if size[0] < formNarrow+4 && !m.panel.form.narrow {
			t.Errorf("width %d: form not in narrow layout", size[0])
		}
		if v := m.View(); v.Cursor != nil && (v.Cursor.Y < 0 || v.Cursor.Y >= len(lines)) {
			t.Errorf("cursor off screen: %+v", v.Cursor)
		}
		_ = m.panel.form.focusID("name")
		if v := m.View(); v.Cursor == nil {
			t.Errorf("width %d: no terminal cursor for the focused text field", size[0])
		}
		typeKeys(m, "esc")
		if m.panel.mode != pmList {
			t.Fatal("esc did not cancel the form")
		}
	}
}
