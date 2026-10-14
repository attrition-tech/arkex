package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/chatgpt"
	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/provider"
	"github.com/attrition-tech/arkex/internal/session"
	"github.com/charmbracelet/x/ansi"
)

func effortModel(t *testing.T) *model {
	m, _ := testModel(t)
	m.o.Connect = nil // a standalone agent; connection transitions have separate tests
	t.Setenv("ARKEX_HOME", t.TempDir())
	ref := config.ModelRef{ConnID: "custom", Conn: config.Connection{Kind: config.KindAPIKey, API: config.APIOpenAICompat, BaseURL: "https://custom.example/v1", Compat: config.Compat{Thinking: config.ThinkingReasoningEffort}}, Model: config.Model{ID: "custom-model", Reasoning: true, ReasoningLevels: []string{"low", "medium", "high", "xhigh"}}}
	lm, err := provider.Open(t.Context(), ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.sess = Connection{Name: ref.String(), Agent: &agent.Agent{Model: lm}}
	m.layout()
	return m
}

func TestEffortSelectionAndResume(t *testing.T) {
	m := effortModel(t)
	m.sess.Agent.SetMessages([]fantasy.Message{fantasy.NewUserMessage("keep this conversation")})
	m.conv = session.New(m.o.Cwd)
	m.setEffort("xhigh")
	if m.sess.Agent.Model.Ref.Thinking != "xhigh" || len(m.sess.Agent.Messages()) != 1 {
		t.Fatal("selection reset conversation")
	}
	saved, err := session.Load(m.o.Cwd, m.conv.ID)
	if err != nil || saved.Effort == nil || *saved.Effort != "xhigh" {
		t.Fatalf("selection not saved: %v", err)
	}
	m.running = true
	m.setEffort("low")
	if m.sess.Agent.Model.Ref.Thinking != "xhigh" {
		t.Fatal("changed in-flight options")
	}
	m.running = false
	m.setEffort("off")
	if m.sess.Agent.Model.Ref.Thinking != "xhigh" {
		t.Fatal("unsupported off accepted")
	}
	if err := m.sess.Agent.Model.SetThinking("low"); err != nil {
		t.Fatal(err)
	}
	m.resume(saved.ID)
	if m.sess.Agent.Model.Ref.Thinking != "xhigh" {
		t.Fatal("resume lost effort")
	}
	m.setEffort("default")
	saved, err = session.Load(m.o.Cwd, m.conv.ID)
	if err != nil || saved.Effort == nil || *saved.Effort != "" {
		t.Fatal("Default not persisted distinctly from legacy")
	}
	if err := m.sess.Agent.Model.SetThinking("high"); err != nil {
		t.Fatal(err)
	}
	m.resume(saved.ID)
	if m.sess.Agent.Model.Ref.Thinking != "" {
		t.Fatal("Default did not restore")
	}
}

func TestEffortResumeModelBoundaries(t *testing.T) {
	m := effortModel(t)
	m.sess.Agent.Model.Ref.Model.ID = "custom:model"
	m.sess.Agent.Model.Ref.Model.ReasoningLevels = []string{"low", "high"}
	m.conv = session.New(m.o.Cwd)
	m.sess.Agent.SetMessages([]fantasy.Message{fantasy.NewUserMessage("saved history")})
	m.setEffort("high")
	id := m.conv.ID
	if err := m.sess.Agent.Model.SetThinking("low"); err != nil {
		t.Fatal(err)
	}
	m.resume(id)
	if m.sess.Agent.Model.Ref.Thinking != "high" {
		t.Fatal("colon in model ID prevented restore")
	}
	m.sess.Agent.Model.Ref.Model.ID = "different:model"
	if err := m.sess.Agent.Model.SetThinking("low"); err != nil {
		t.Fatal(err)
	}
	m.resume(id)
	if m.sess.Agent.Model.Ref.Thinking != "low" {
		t.Fatal("restored effort onto a different model")
	}
	m.sess.Agent.Model.Ref.Model.ID = "custom:model"
	m.sess.Agent.Model.Ref.Model.ReasoningLevels = []string{"low"}
	m.resume(id)
	if m.sess.Agent.Model.Ref.Thinking != "low" || m.pal == nil {
		t.Fatal("unavailable saved effort must ask rather than silently reset")
	}
}

func TestEffortMenuAndVisibilityAreSeparate(t *testing.T) {
	m := effortModel(t)
	items := effortItems(m)
	if titles(items) != "Default|Low|Medium|High|Xhigh|Check reasoning…" || !strings.Contains(items[0].detail, "always on") {
		t.Fatalf("menu: %+v", items)
	}
	m.openPaletteSub("Reasoning effort", items)
	typeKeys(m, "down", "enter")
	if m.sess.Agent.Model.Ref.Thinking != "low" || m.pal != nil {
		t.Fatal("keyboard selection failed")
	}
	reasoning := newBlock(blockReasoning, "Consider the alternatives.")
	m.blocks = append(m.blocks, reasoning)
	m.toggleDetail(reasoning)
	if m.openDetail != reasoning || m.sess.Agent.Model.Ref.Thinking != "low" {
		t.Fatal("display toggle changed inference")
	}
	m.sess.Agent.Model.Ref.Conn.BaseURL = "http://localhost:1/v1"
	m.sess.Agent.Model.Ref.Model.ReasoningLevels = nil
	m.sess.Agent.Model.Ref.Conn.Compat.Thinking = config.ThinkingQwen
	if got := titles(effortItems(m)); got != "Default|Off|On|Check reasoning…" {
		t.Fatal(got)
	}
	m.sess.Agent.Model.Ref.Conn.Compat.Thinking = ""
	items = effortItems(m)
	if len(items) != 2 || !strings.Contains(items[0].detail, "not advertised") {
		t.Fatal("unknown model offered fake controls")
	}
}

func TestDiscoveredReasoningIsSaved(t *testing.T) {
	m, _, _, _ := subscriptionModel(t)
	typeKeys(m, "a", "right", "right")
	if !m.buildDraft() {
		t.Fatal("draft")
	}
	p := m.panel
	p.catalog = map[string]chatgpt.Model{"discovered": {ID: "discovered", Reasoning: "low", ReasoningLevels: []string{"off", "low", "high"}}}
	m.saveDraft([]string{"discovered"})
	cfg, err := config.Load(m.o.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	md := cfg.Connections["chatgpt"].Models[0]
	if strings.Join(md.ReasoningLevels, ",") != "off,low,high" || md.ReasoningDefault != "low" {
		t.Fatalf("discovery discarded: %+v", md)
	}
	// Fetching again must not overwrite an existing manual capability list.
	m.openEdit("chatgpt")
	if !m.buildDraft() {
		t.Fatal("edit draft")
	}
	p.catalog = map[string]chatgpt.Model{"discovered": {ID: "discovered", Reasoning: "medium", ReasoningLevels: []string{"medium"}}}
	m.saveDraft([]string{"discovered"})
	cfg, err = config.Load(m.o.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Connections["chatgpt"].Models[0].ReasoningLevels, ",") != "off,low,high" {
		t.Fatal("overwrote configured controls")
	}
}

func TestReasoningCheckPreviewApplyAndCancellation(t *testing.T) {
	m := effortModel(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if effort, ok := body["reasoning_effort"]; ok && effort != "low" && effort != "max" {
			w.WriteHeader(400)
			_, _ = fmt.Fprint(w, `{"error":{"message":"Invalid reasoning_effort","param":"reasoning_effort"}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	mdl := m.sess.Agent.Model
	mdl.Ref.Conn.BaseURL = server.URL + "/v1"
	mdl.Ref.Conn.Models = []config.Model{mdl.Ref.Model, {ID: "sibling", ReasoningLevels: []string{"high"}}}
	m.o.ConfigPath, _ = config.GlobalPath()
	if err := config.SaveConnection(m.o.ConfigPath, "custom", mdl.Ref.Conn, "custom/sibling"); err != nil {
		t.Fatal(err)
	}
	m.sess.Agent.SetMessages([]fantasy.Message{fantasy.NewUserMessage("PRIVATE CONVERSATION")})
	m.conv = session.New(m.o.Cwd)
	m.setEffort("high")
	beforeHistory, beforeRef := m.sess.Agent.Messages(), mdl.Ref
	beforeConfig, _ := os.ReadFile(m.o.ConfigPath)
	h := &harness{model: m}
	m.command("/effort")
	typeKeys(m, "c", "h", "e", "c", "k", "enter")
	if requests.Load() != 0 || m.pal == nil || m.pal.view[m.pal.sel].title != "Cancel" {
		t.Fatal("opening check sent a request or failed to default to Cancel")
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	confirmation := ansi.Strip(strings.Join(frameLines(t, m, "reasoning check confirmation"), "\n"))
	for _, warning := range []string{"35 synthetic requests", "64 output tokens requested each", "15 s per request", "No history, attachments or tools", "not reasoning proof"} {
		if !strings.Contains(confirmation, warning) {
			t.Fatalf("narrow confirmation hid %q: %s", warning, confirmation)
		}
	}
	typeKeys(m, "down")
	_, check := m.Update(key("enter"))
	h.drive(check)
	if requests.Load() == 0 || m.pal == nil || !strings.Contains(titles(m.pal.view), "Accepted by server") {
		t.Fatal("missing check preview")
	}
	unchanged, _ := os.ReadFile(m.o.ConfigPath)
	if !slices.Equal(beforeConfig, unchanged) || !reflect.DeepEqual(mdl.Ref, beforeRef) || !reflect.DeepEqual(beforeHistory, m.sess.Agent.Messages()) {
		t.Fatal("preview changed config, current effort or history")
	}
	for _, width := range []int{120, 60, 40} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		frameLines(t, m, "checked reasoning preview")
	}
	// Filter to the explicit apply action, leaving informational rows alone.
	typeKeys(m, "u", "s", "e", "enter")
	cfg, err := config.Load(m.o.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Connections["custom"].Models[0]
	if !got.ReasoningChecked || !slices.Equal(got.ReasoningLevels, []string{"low", "max"}) || cfg.Default != "custom/sibling" || !reflect.DeepEqual(cfg.Connections["custom"].Models[1], beforeRef.Conn.Models[1]) {
		t.Fatalf("wrong saved capabilities or sibling/default changed: %+v", cfg)
	}
	if mdl.Ref.Thinking != "" || !reflect.DeepEqual(beforeHistory, m.sess.Agent.Messages()) || !strings.Contains(effortItems(m)[0].detail, "accepted by server") {
		t.Fatal("apply changed history or misrepresented capabilities")
	}
	typeKeys(m, "down", "enter")
	if mdl.Ref.Thinking != "low" {
		t.Fatal("checked level could not be selected")
	}
	// Cancel before the queued command executes; no request/result may escape.
	count := requests.Load()
	cmd := m.startReasoningCheck()
	typeKeys(m, "esc")
	h.drive(cmd)
	if m.pal != nil || requests.Load() != count || mdl.Ref.Thinking != "low" {
		t.Fatal("dismissed check sent a request or changed the model")
	}
}
