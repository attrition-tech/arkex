package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/dantearo/arkex/internal/agent"
	"github.com/dantearo/arkex/internal/config"
	"github.com/dantearo/arkex/internal/provider"
	"github.com/dantearo/arkex/internal/tools"
)

// fakeLM is an OpenAI-compatible chat endpoint answering from a script, one
// response per request, recording every request body.
type fakeLM struct {
	mu       sync.Mutex
	requests []map[string]any
	script   []string // raw SSE bodies or JSON completions
}

func (f *fakeLM) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	idx := len(f.requests) - 1
	f.mu.Unlock()
	if idx >= len(f.script) {
		http.Error(w, "no scripted response", http.StatusInternalServerError)
		return
	}
	resp := f.script[idx]
	if strings.HasPrefix(resp, "data:") {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	_, _ = io.WriteString(w, resp)
}

func chunk(delta, finish string) string {
	fr := "null"
	if finish != "" {
		fr = `"` + finish + `"`
	}
	return fmt.Sprintf("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":%s}]}\n\n", delta, fr)
}

func streamText(text string, promptTokens int) string {
	return chunk(`{"role":"assistant","content":"`+text+`"}`, "") + chunk(`{}`, "stop") +
		fmt.Sprintf("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":5,\"total_tokens\":%d}}\n\ndata: [DONE]\n\n", promptTokens, promptTokens+5)
}

func streamToolCall(id, path string) string {
	return chunk(`{"role":"assistant","tool_calls":[{"index":0,"id":"`+id+`","type":"function","function":{"name":"read","arguments":"{\"path\":\"`+path+`\"}"}}]}`, "") +
		chunk(`{}`, "tool_calls") + "data: [DONE]\n\n"
}

func jsonReply(text string) string {
	return `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"` + text + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}}`
}

// harness is a model whose background runs execute synchronously: startRun's
// command runs on the test goroutine, so queued events are applied to the
// model just before each returned message, in program order.
type harness struct {
	*model
	queued []tea.Msg
}

// drive runs a command synchronously and feeds its messages back, like the
// program would, until nothing is left.
func (h *harness) drive(cmd tea.Cmd) {
	for _, msg := range runCmd(cmd) {
		for _, q := range h.queued {
			h.Update(q)
		}
		h.queued = nil
		_, next := h.Update(msg)
		h.drive(next)
	}
}

// fakeAgentModel wires a chat-ready model to an agent backed by fakeLM.
func fakeAgentModel(t *testing.T, lm *fakeLM, contextWindow int) *harness {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(lm.handler))
	t.Cleanup(srv.Close)
	t.Setenv("FAKE_KEY", "sk-test")
	t.Setenv("ARKEX_HOME", t.TempDir())
	cfg := &config.Config{
		Connections: map[string]config.Connection{
			"fake": {API: config.APIOpenAICompat, BaseURL: srv.URL + "/v1", APIKey: "$FAKE_KEY", Models: []config.Model{{ID: "m", ContextWindow: contextWindow}}},
		},
		Default: "fake/m",
	}
	ref, err := cfg.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	lmodel, err := provider.Open(context.Background(), ref, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	m, _ := testModel(t)
	h := &harness{model: m}
	m.o.Connect = nil
	m.send = func(msg tea.Msg) { h.queued = append(h.queued, msg) }
	ag := &agent.Agent{Model: lmodel, Tools: tools.Default(m.o.Cwd), Policy: agent.AllowAll{}, System: "test"}
	m.setSession(Connection{Agent: ag, Name: "fake/m"})
	return h
}

// A model that re-reads the same unchanged file five times is paused with
// an explanation; an empty enter lets it continue.
func TestStuckRunPausesThenEnterContinues(t *testing.T) {
	const repeats = 5
	var script []string
	for i := 0; i < repeats; i++ {
		script = append(script, streamToolCall(fmt.Sprintf("c%d", i), "a.txt"))
	}
	lm := &fakeLM{script: append(script, streamText("done", 30))}
	m := fakeAgentModel(t, lm, 0)
	if err := os.WriteFile(filepath.Join(m.o.Cwd, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.setInput("read a.txt")
	_, cmd := m.Update(key("enter"))
	m.drive(cmd)

	tr := transcript(m.model)
	if !m.paused || m.running || !strings.Contains(tr, "paused after 5 tool steps: the model called read 5 times") {
		t.Fatalf("paused=%v running=%v\n%s", m.paused, m.running, tr)
	}
	if len(lm.requests) != repeats {
		t.Fatalf("requests = %d", len(lm.requests))
	}
	// Empty enter resumes; the next request carries the tool result, no
	// new user message, and the answer lands in the transcript.
	_, cmd = m.Update(key("enter"))
	m.drive(cmd)
	if m.paused || len(lm.requests) != repeats+1 || !strings.Contains(transcript(m.model), "done") {
		t.Fatalf("after continue: paused=%v requests=%d\n%s", m.paused, len(lm.requests), transcript(m.model))
	}
	msgs, _ := lm.requests[repeats]["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" {
		t.Fatalf("continue must resend the tool result last, got %v", last["role"])
	}
	// /continue with nothing paused is a friendly no-op.
	m.command("/continue")
	if !strings.Contains(transcript(m.model), "nothing to continue") {
		t.Fatal("/continue without a pause should explain itself")
	}
}

func TestAutoCompactBeforePromptWhenContextIsNearlyFull(t *testing.T) {
	lm := &fakeLM{script: []string{
		streamText("first answer", 850), // 85% of a 1000-token window
		jsonReply("Summary: the user said hi."),
		streamText("second answer", 40),
	}}
	m := fakeAgentModel(t, lm, 1000)

	m.setInput("hi")
	_, cmd := m.Update(key("enter"))
	m.drive(cmd)
	if m.lastInput != 850 || m.sess.Agent.LastInput() != 850 {
		t.Fatalf("lastInput=%d agent=%d", m.lastInput, m.sess.Agent.LastInput())
	}
	if f := ansi.Strip(m.footer()); !strings.Contains(f, "85%") {
		t.Fatalf("footer = %q", f)
	}

	m.setInput("and now?")
	_, cmd = m.Update(key("enter"))
	m.drive(cmd)
	if len(lm.requests) != 3 {
		t.Fatalf("requests = %d, want compaction + prompt", len(lm.requests))
	}
	tr := transcript(m.model)
	if !strings.Contains(tr, "second answer") ||
		strings.Contains(tr, "context compacted because") {
		t.Fatalf("transcript:\n%s", tr)
	}
	foundSummary := false
	for _, b := range m.blocks {
		if b.name == "context compacted · 2 messages summarised" && b.text.String() == "Summary: the user said hi." {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Fatal("inspectable compaction summary lost")
	}
	// The prompt after compaction is sent on top of the summary pair only.
	msgs, _ := lm.requests[2]["messages"].([]any)
	if len(msgs) != 4 { // system, summary(user), ack(assistant), new prompt
		t.Fatalf("messages after compaction = %d, want 4", len(msgs))
	}
	first, _ := msgs[1].(map[string]any)
	if c, _ := first["content"].(string); !strings.Contains(c, "Summary: the user said hi.") {
		t.Fatalf("summary not sent: %v", first["content"])
	}
	if m.lastInput != 40 {
		t.Fatalf("lastInput after the new turn = %d", m.lastInput)
	}
	// The saved session holds the compacted conversation.
	if got := len(m.sess.Agent.Messages()); got != 4 {
		t.Fatalf("agent messages = %d", got)
	}
}

func TestGatewayUsageSurvivesStatsAndCompacts(t *testing.T) {
	for _, tc := range []struct {
		name           string
		window, tokens int
		cached         bool
	}{
		{"gateway-fallback", 0, 354624, false},
		{"cached-context", 1000, 850, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := streamText("first answer", tc.tokens)
			if tc.cached {
				response = strings.ReplaceAll(response, `"prompt_tokens":850`, `"prompt_tokens":850,"prompt_tokens_details":{"cached_tokens":800}`)
			}
			response = strings.ReplaceAll(response, "data: [DONE]", "data: {\"choices\":[],\"usage\":{\"completion_tokens\":7,\"output_tokens\":7,\"throughput\":2.55,\"total_time\":3.17,\"ttft\":0.43}}\n\ndata: [DONE]")
			lm := &fakeLM{script: []string{response, jsonReply("Summary: user said hi."), streamText("second answer", 40)}}
			m := fakeAgentModel(t, lm, tc.window)
			m.setInput("hi")
			_, cmd := m.Update(key("enter"))
			m.drive(cmd)
			if m.lastInput != int64(tc.tokens) || m.sess.Agent.LastInput() != int64(tc.tokens) {
				t.Fatalf("context UI=%d agent=%d want=%d", m.lastInput, m.sess.Agent.LastInput(), tc.tokens)
			}
			if tc.cached && m.usageIn != 50 {
				t.Fatalf("uncached billing changed: %d", m.usageIn)
			}
			if tc.window == 0 && (!strings.Contains(ansi.Strip(m.footer()), "ctx*") || m.sess.Agent.Model.Ref.Model.ContextWindow != 0) {
				t.Fatal("fallback must be labelled and not persisted as confirmed")
			}
			m.setInput("continue")
			_, cmd = m.Update(key("enter"))
			m.drive(cmd)
			if len(lm.requests) != 3 || m.lastInput != 40 {
				t.Fatalf("missing compaction or stale usage: requests=%d context=%d", len(lm.requests), m.lastInput)
			}
		})
	}
}

func TestManualCompactCommand(t *testing.T) {
	lm := &fakeLM{script: []string{streamText("ok", 10), jsonReply("S")}}
	m := fakeAgentModel(t, lm, 0)
	m.command("/compact")
	if !strings.Contains(transcript(m.model), "nothing to compact yet") {
		t.Fatal("empty conversation must not be compacted")
	}
	m.setInput("hi")
	_, cmd := m.Update(key("enter"))
	m.drive(cmd)
	_, cmd = m.command("/compact")
	if !m.compacting || !strings.Contains(ansi.Strip(m.footer()), "compacting") {
		t.Fatal("manual compaction must animate before waiting for the response")
	}
	m.drive(cmd)
	if m.compacting || len(lm.requests) != 2 || !strings.Contains(transcript(m.model), "context compacted") {
		t.Fatalf("requests=%d\n%s", len(lm.requests), transcript(m.model))
	}
}
