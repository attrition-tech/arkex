package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/dantearo/arkex/internal/agent"
	"github.com/dantearo/arkex/internal/config"
	"github.com/dantearo/arkex/internal/prompt"
	"github.com/dantearo/arkex/internal/provider"
	"github.com/dantearo/arkex/internal/scratch"
	"github.com/dantearo/arkex/internal/session"
)

func sessionModelHarness(t *testing.T) (*harness, *session.Session) {
	t.Helper()
	t.Setenv("ARKEX_HOME", t.TempDir())
	m, _ := testModel(t)
	m.o.ConfigPath, _ = config.GlobalPath()
	c := config.Connection{API: config.APIOpenAICompat, BaseURL: "http://localhost:1/v1",
		Compat: config.Compat{Thinking: config.ThinkingReasoningEffort},
		Models: []config.Model{{ID: "a", ReasoningLevels: []string{"low", "high"}}, {ID: "b", ReasoningLevels: []string{"low", "high"}}}}
	if err := config.SaveConnection(m.o.ConfigPath, "local", c, "local/b:low"); err != nil {
		t.Fatal(err)
	}
	m.o.Connect = func(ctx context.Context, selector string) (Connection, error) {
		cfg, err := config.Load(m.o.Cwd)
		if err != nil {
			return Connection{}, err
		}
		ref, err := cfg.Resolve(selector)
		if err != nil {
			return Connection{}, err
		}
		lm, err := provider.Open(ctx, ref, nil)
		if err != nil {
			return Connection{}, err
		}
		name := ref.String()
		if ref.Thinking != "" {
			name += ":" + ref.Thinking
		}
		return Connection{Agent: &agent.Agent{Model: lm}, Name: name}, nil
	}
	curr, err := m.o.Connect(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	m.setSession(curr)
	m.sess.Agent.SetMessages([]fantasy.Message{fantasy.NewUserMessage("current conversation")})
	s := session.New(m.o.Cwd)
	s.Update(sampleMessages(), "local/a:high", "plan", 71, 19)
	s.LastInput = 33
	high := "high"
	s.Effort = &high
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	return &harness{model: m}, s
}

func TestScratchFollowsConversationNotModel(t *testing.T) {
	h, saved := sessionModelHarness(t)
	var store scratch.Store
	t.Cleanup(func() { _ = store.Close() })
	var active string
	h.o.Prepare = func(ag *agent.Agent, id string) error {
		dir, err := store.Dir(id)
		if err == nil {
			active = dir
			ag.System += prompt.ScratchNote(dir)
		}
		return err
	}
	prepare := func() string {
		t.Helper()
		if err := h.prepareAgent(); err != nil {
			t.Fatal(err)
		}
		if strings.Count(h.sess.Agent.System, "# Temporary files") != 1 || !strings.Contains(h.sess.Agent.System, active) {
			t.Fatal("stale or duplicated scratch instructions")
		}
		return active
	}
	first := prepare()
	firstSession := h.conv
	next, err := h.o.Connect(t.Context(), "local/a:high")
	if err != nil {
		t.Fatal(err)
	}
	h.carryConv(h.sess, next)
	h.setSession(next)
	if prepare() != first {
		t.Fatal("model switch replaced scratch")
	}
	h.newConv()
	if prepare() == first {
		t.Fatal("new conversation reused scratch")
	}
	h.loadConversation(firstSession)
	if prepare() != first {
		t.Fatal("same-run resume lost scratch")
	}
	h.loadConversation(saved)
	if prepare() == first {
		t.Fatal("other saved conversation reused scratch")
	}
}

func TestResumeRestoresModelWithoutChangingDefault(t *testing.T) {
	h, s := sessionModelHarness(t)
	old := h.sess.Agent
	cmd := h.resume(s.ID)
	if h.sess.Agent != old || h.conv != nil {
		t.Fatal("changed session before connection completed")
	}
	h.drive(cmd)
	if h.sess.Agent.LastInput() != 33 {
		t.Fatalf("resume lost compaction measurement: %d", h.sess.Agent.LastInput())
	}
	if h.sess.Name != "local/a:high" || h.sess.Agent.Model.Ref.Thinking != "high" || h.conv.ID != s.ID || h.lastInput != 33 || h.mode() != agent.ModePlan {
		t.Fatalf("wrong restored session: model=%s effort=%s", h.sess.Name, h.sess.Agent.Model.Ref.Thinking)
	}
	if len(h.sess.Agent.Messages()) != len(s.Messages) {
		t.Fatal("history lost")
	}
	cfg, _ := config.Load(h.o.Cwd)
	if cfg.Default != "local/b:low" {
		t.Fatal("resume overwrote default")
	}
	_, cmd = h.command("/new")
	h.drive(cmd)
	if h.sess.Name != "local/b:low" || h.sess.Agent.Model.Ref.Thinking != "low" || h.conv != nil || len(h.sess.Agent.Messages()) != 0 {
		t.Fatal("new session did not use default with empty history")
	}
	// Startup resume does not require a working default connection first.
	fresh := newModel(Options{Cwd: h.o.Cwd, ConfigPath: h.o.ConfigPath, Connect: h.o.Connect, Resume: s.ID, Mode: agent.NewModePolicy(agent.ModeBuild, nil)})
	(&harness{model: fresh}).drive(fresh.Init())
	if fresh.sess.Name != "local/a:high" || fresh.conv.ID != s.ID {
		t.Fatal("startup resume used default")
	}
}

func TestMissingSessionModelRequiresExplicitReplacement(t *testing.T) {
	h, s := sessionModelHarness(t)
	cfg, _ := config.Load(h.o.Cwd)
	c := cfg.Connections["local"]
	c.Models[0].Disabled = true
	if err := config.SaveConnection(h.o.ConfigPath, "local", c, ""); err != nil {
		t.Fatal(err)
	}
	old := h.sess.Agent
	h.drive(h.resume(s.ID))
	if h.sess.Agent != old || h.conv != nil || h.pal == nil || !strings.Contains(h.pal.level().title, "choose replacement") {
		t.Fatal("silently replaced missing model")
	}
	saved, _ := session.Load(h.o.Cwd, s.ID)
	if saved.Model != "local/a:high" {
		t.Fatal("failed resume modified saved model")
	}
	if len(h.pal.view) != 2 || h.pal.view[1].title != "local/b" {
		t.Fatalf("replacement choices: %+v", h.pal.view)
	}
	h.drive(h.pal.view[1].action(h.model))
	if h.sess.Name != "local/b" || h.conv.ID != s.ID || h.sess.Agent.Model.Ref.Thinking != "" {
		t.Fatal("replacement inherited incompatible saved effort")
	}
	saved, _ = session.Load(h.o.Cwd, s.ID)
	if saved.Model != "local/b" {
		t.Fatal("replacement not persisted")
	}
	cfg, _ = config.Load(h.o.Cwd)
	if cfg.Default != "local/b:low" {
		t.Fatal("replacement overwrote default")
	}
}

func TestCancelledResumeIgnoresLateConnection(t *testing.T) {
	h, s := sessionModelHarness(t)
	old := h.sess.Agent
	msgs := runCmd(h.resume(s.ID))
	h.closePalette()
	for _, msg := range msgs {
		h.Update(msg)
	}
	if h.sess.Agent != old || h.conv != nil {
		t.Fatal("cancelled request changed session")
	}
}

func TestResumeIgnoresSupersededModelSwitch(t *testing.T) {
	h, s := sessionModelHarness(t)
	late := runCmd(h.connect("local/b:low", false))
	h.drive(h.resume(s.ID))
	for _, msg := range late {
		h.Update(msg)
	}
	if h.sess.Name != "local/a:high" || h.conv.ID != s.ID {
		t.Fatal("older model switch overwrote resumed model")
	}
	saved, err := session.Load(h.o.Cwd, s.ID)
	if err != nil || saved.Model != "local/a:high" {
		t.Fatalf("older model switch changed saved session: %+v, %v", saved, err)
	}
}
