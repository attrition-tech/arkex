package agent

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

	"charm.land/fantasy"

	"github.com/dantearo/arkex/internal/config"
	"github.com/dantearo/arkex/internal/provider"
	"github.com/dantearo/arkex/internal/tools"
)

// fakeServer speaks just enough of the OpenAI chat completions streaming
// protocol to drive the agent loop. Each call to handler pops one scripted
// response; request bodies are recorded for assertions.
type fakeServer struct {
	mu       sync.Mutex
	requests []map[string]any
	script   []func(w http.ResponseWriter)
}

func (f *fakeServer) handler(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Content-Type", "text/event-stream")
	f.script[idx](w)
}

func sse(w http.ResponseWriter, chunks ...string) {
	for _, c := range chunks {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

const chunkPrefix = `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":`

func delta(d, finish string) string {
	fr := "null"
	if finish != "" {
		fr = `"` + finish + `"`
	}
	return chunkPrefix + d + `,"finish_reason":` + fr + `}]}`
}

func TestRejectedRunClearsLastUsage(t *testing.T) {
	a := &Agent{lastUsage: fantasy.Usage{InputTokens: 31, OutputTokens: 7}}
	if err := a.Run(t.Context(), "hi", nil); err == nil || a.LastUsage().InputTokens != 0 {
		t.Fatal("rejected run retained previous usage")
	}
	a.lastUsage = fantasy.Usage{InputTokens: 19}
	if err := a.Continue(t.Context(), nil); err == nil || a.LastUsage().InputTokens != 0 {
		t.Fatal("rejected continuation retained previous usage")
	}
}

func usage() string {
	return `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
}

func TestRunToolLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("first\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := &fakeServer{script: []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			sse(w,
				delta(`{"role":"assistant","reasoning_content":"I should read the file."}`, ""),
				delta(`{"content":"Reading it now."}`, ""),
				delta(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":""}}]}`, ""),
				delta(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"hello.txt\"}"}}]}`, ""),
				delta(`{}`, "tool_calls"),
				usage(),
			)
		},
		func(w http.ResponseWriter) {
			sse(w,
				delta(`{"role":"assistant","content":"The file has two lines."}`, ""),
				delta(`{}`, "stop"),
				usage(),
			)
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer srv.Close()

	t.Setenv("FAKE_KEY", "sk-test")
	cfg := &config.Config{
		Connections: map[string]config.Connection{
			"fake": {
				API:     config.APIOpenAICompat,
				BaseURL: srv.URL + "/v1",
				APIKey:  "$FAKE_KEY",
				Compat:  config.Compat{Thinking: config.ThinkingDeepSeek},
				Models:  []config.Model{{ID: "m", Reasoning: true, MaxTokens: 4096}},
			},
		},
		Profiles: map[string]config.Profile{"fast": {Model: "fake/m", Thinking: "low"}},
		Default:  "fast",
	}
	ref, err := cfg.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	model, err := provider.Open(ctx, ref, srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	ag := &Agent{
		Model:  model,
		Tools:  tools.Default(dir),
		Policy: AllowAll{},
		System: "You are a test.",
	}
	var events []Event
	if err := ag.Run(ctx, "what's in hello.txt?", func(e Event) { events = append(events, e) }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if u := ag.LastUsage(); u.InputTokens != 20 || u.OutputTokens != 10 {
		t.Fatalf("last run usage = %+v", u)
	}

	// Request 1 carries compat fields and the system prompt.
	if len(fs.requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(fs.requests))
	}
	r1 := fs.requests[0]
	if got := r1["reasoning_effort"]; got != "low" {
		t.Errorf("reasoning_effort = %v, want low", got)
	}
	if th, _ := r1["thinking"].(map[string]any); th["type"] != "enabled" {
		t.Errorf("thinking = %v, want {type: enabled}", r1["thinking"])
	}
	if got := r1["max_tokens"]; got != nil && got != float64(4096) {
		if got2 := r1["max_completion_tokens"]; got2 != float64(4096) {
			t.Errorf("max tokens not sent: max_tokens=%v max_completion_tokens=%v", got, got2)
		}
	}
	msgs1 := r1["messages"].([]any)
	if first := msgs1[0].(map[string]any); first["role"] != "system" || first["content"] != "You are a test." {
		t.Errorf("first message = %v, want system prompt", first)
	}

	// Request 2 replays the assistant tool call and includes the tool result.
	msgs2 := fs.requests[1]["messages"].([]any)
	var sawToolResult, sawToolCall bool
	for _, m := range msgs2 {
		mm := m.(map[string]any)
		switch mm["role"] {
		case "assistant":
			if tc, ok := mm["tool_calls"].([]any); ok && len(tc) == 1 {
				sawToolCall = true
			}
		case "tool":
			content, _ := mm["content"].(string)
			if mm["tool_call_id"] == "call_1" && strings.Contains(content, "1\tfirst") && strings.Contains(content, "2\tsecond") {
				sawToolResult = true
			}
		}
	}
	if !sawToolCall {
		t.Errorf("second request lacks assistant tool_calls: %v", msgs2)
	}
	if !sawToolResult {
		t.Errorf("second request lacks tool result with file contents: %v", msgs2)
	}

	// Events: reasoning, text, tool call, decision, result, final text, run end.
	var text, reasoning strings.Builder
	var results []ToolResult
	var end *RunEnd
	for _, e := range events {
		switch e := e.(type) {
		case TextDelta:
			text.WriteString(e.Text)
		case ReasoningDelta:
			reasoning.WriteString(e.Text)
		case ToolResult:
			results = append(results, e)
		case RunEnd:
			end = &e
		}
	}
	if reasoning.String() != "I should read the file." {
		t.Errorf("reasoning = %q", reasoning.String())
	}
	if text.String() != "Reading it now.The file has two lines." {
		t.Errorf("text = %q", text.String())
	}
	if len(results) != 1 || results[0].IsError || results[0].Name != "read" {
		t.Errorf("tool results = %+v", results)
	}
	if end == nil || end.Steps != 2 || end.Usage.InputTokens != 20 || end.Err != nil {
		t.Errorf("run end = %+v", end)
	}

	// Conversation state: user, assistant(reasoning+text+call), tool, assistant.
	if got := len(ag.Messages()); got != 4 {
		t.Errorf("messages = %d, want 4", got)
	}
}

func TestRunDeniedToolIsReportedToModel(t *testing.T) {
	fs := &fakeServer{script: []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			sse(w,
				delta(`{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"rm -rf /\"}"}}]}`, ""),
				delta(`{}`, "tool_calls"),
			)
		},
		func(w http.ResponseWriter) {
			sse(w, delta(`{"role":"assistant","content":"Understood."}`, ""), delta(`{}`, "stop"))
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer srv.Close()

	cfg := &config.Config{
		Connections: map[string]config.Connection{"fake": {
			API: config.APIOpenAICompat, BaseURL: srv.URL, Models: []config.Model{{ID: "m"}},
		}},
		Permissions: map[string]config.Permission{},
	}
	ref, _ := cfg.Resolve("fake/m")
	model, err := provider.Open(context.Background(), ref, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ag := &Agent{Model: model, Tools: tools.Default(t.TempDir()), Policy: ConfigPolicy{Config: cfg}}
	var decisions []ToolDecision
	if err := ag.Run(context.Background(), "wipe it", func(e Event) {
		if d, ok := e.(ToolDecision); ok {
			decisions = append(decisions, d)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Allowed {
		t.Fatalf("decisions = %+v, want one denial", decisions)
	}
	msgs := fs.requests[1]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" || !strings.Contains(fmt.Sprint(last["content"]), "not permitted") {
		t.Errorf("model was not told about the denial: %v", last)
	}
}
