package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/dantearo/arkex/internal/config"
	"github.com/dantearo/arkex/internal/provider"
	"github.com/dantearo/arkex/internal/tools"
)

// toolCallTurn scripts one model turn that asks to read hello.txt.
func toolCallTurn(id string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		sse(w,
			delta(`{"role":"assistant","tool_calls":[{"index":0,"id":"`+id+`","type":"function","function":{"name":"read","arguments":"{\"path\":\"hello.txt\"}"}}]}`, ""),
			delta(`{}`, "tool_calls"),
			usage(),
		)
	}
}

// TestStuckPausesAndContinueResumes: a model that reads the same unchanged
// file over and over is paused on the fifth identical step; Continue gives
// it a fresh window.
func TestStuckPausesAndContinueResumes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := []func(http.ResponseWriter){}
	for i := 1; i <= stuckRepeats; i++ {
		script = append(script, toolCallTurn(fmt.Sprintf("call_%d", i)))
	}
	// Only reached after Continue.
	script = append(script, func(w http.ResponseWriter) {
		sse(w, delta(`{"role":"assistant","content":"done"}`, ""), delta(`{}`, "stop"), usage())
	})
	fs := &fakeServer{script: script}
	srv := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer srv.Close()
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := &config.Config{
		Connections: map[string]config.Connection{
			"fake": {API: config.APIOpenAICompat, BaseURL: srv.URL + "/v1", APIKey: "$FAKE_KEY", Models: []config.Model{{ID: "m"}}},
		},
		Default: "fake/m",
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
	ag := &Agent{Model: model, Tools: tools.Default(dir), Policy: AllowAll{}}

	var results int
	err = ag.Run(ctx, "read it", func(e Event) {
		if _, ok := e.(ToolResult); ok {
			results++
		}
	})
	var paused *PausedError
	if !errors.As(err, &paused) || paused.Steps != stuckRepeats || !strings.Contains(paused.Reason, "called read 5 times") {
		t.Fatalf("Run err = %v, want PausedError after %d identical steps", err, stuckRepeats)
	}
	if results != stuckRepeats {
		t.Fatalf("every tool call must have run before pausing, got %d results", results)
	}
	// user + (assistant, tool) × 5: the last tool result is recorded.
	msgs := ag.Messages()
	if len(msgs) != 1+2*stuckRepeats || msgs[len(msgs)-1].Role != fantasy.MessageRoleTool {
		t.Fatalf("messages after pause = %d (last %s), want %d ending in a tool result", len(msgs), msgs[len(msgs)-1].Role, 1+2*stuckRepeats)
	}
	if len(fs.requests) != stuckRepeats {
		t.Fatalf("requests before continue = %d, want %d", len(fs.requests), stuckRepeats)
	}

	var text string
	if err := ag.Continue(ctx, func(e Event) {
		if d, ok := e.(TextDelta); ok {
			text += d.Text
		}
	}); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if text != "done" || len(fs.requests) != stuckRepeats+1 {
		t.Fatalf("continue: text=%q requests=%d", text, len(fs.requests))
	}
	// The resumed request must not carry a new user message: the last
	// message sent is the tool result.
	sent, _ := fs.requests[stuckRepeats]["messages"].([]any)
	last, _ := sent[len(sent)-1].(map[string]any)
	if last["role"] != "tool" {
		t.Fatalf("continue sent a %v message last, want tool", last["role"])
	}
	if len(ag.Messages()) != 2+2*stuckRepeats {
		t.Fatalf("messages after continue = %d, want %d", len(ag.Messages()), 2+2*stuckRepeats)
	}
}

// stuck must key on call *and* result: re-running a command whose output
// changes is progress, and a step of unrelated calls in between must not
// break the count of the repeated one.
func TestStuckNeedsIdenticalResults(t *testing.T) {
	sig := func(name, input, out string) stepSig {
		return stepSignature([]fantasy.ToolCallPart{{ToolCallID: "x", ToolName: name, Input: input}},
			[]fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "x", Output: fantasy.ToolResultOutputContentText{Text: out}}})
	}
	same := sig("bash", `{"command":"go test"}`, "FAIL")
	var steps []stepSig
	for i := 0; i < stuckRepeats-1; i++ {
		steps = append(steps, same)
		if n, _ := stuck(steps); n != 0 {
			t.Fatalf("stuck after %d identical steps, want none before %d", i+1, stuckRepeats)
		}
	}
	// Same command, different output: not stuck.
	steps = append(steps, sig("bash", `{"command":"go test"}`, "ok"))
	if n, _ := stuck(steps); n != 0 {
		t.Fatal("a changed result must count as progress")
	}
	// An edit in between, then the same failing test again: that is the
	// fifth identical step inside the window.
	steps = append(steps, sig("edit", `{"path":"a.go"}`, "ok"), same)
	if n, name := stuck(steps); n != stuckRepeats || name != "bash" {
		t.Fatalf("stuck = %d %q, want %d bash", n, name, stuckRepeats)
	}
	// Old repeats fall out of the window.
	var spread []stepSig
	for i := 0; i < stuckRepeats-1; i++ {
		spread = append(spread, same)
	}
	for i := 0; i < stuckWindow; i++ {
		spread = append(spread, sig("read", fmt.Sprintf(`{"path":"%d"}`, i), "x"))
	}
	spread = append(spread, same)
	if n, _ := stuck(spread); n != 0 {
		t.Fatal("repeats older than the window must not count")
	}
	// Errors are results too: the same denied call five times is stuck.
	denied := stepSignature([]fantasy.ToolCallPart{{ToolCallID: "x", ToolName: "bash", Input: `{"command":"rm -rf /"}`}},
		[]fantasy.MessagePart{errPart("x", errors.New("not permitted"))})
	var deny []stepSig
	for i := 0; i < stuckRepeats; i++ {
		deny = append(deny, denied)
	}
	if n, _ := stuck(deny); n != stuckRepeats {
		t.Fatal("repeated denied calls must pause")
	}
	// A step without tool calls never counts.
	if n, _ := stuck([]stepSig{same, same, same, same, {}}); n != 0 {
		t.Fatal("a text-only step is not a repeat")
	}
}

// MaxSteps stays available for probes: one step, then a pause.
func TestMaxStepsCapsProbes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := &fakeServer{script: []func(http.ResponseWriter){toolCallTurn("call_1"), toolCallTurn("call_2")}}
	srv := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer srv.Close()
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := &config.Config{
		Connections: map[string]config.Connection{
			"fake": {API: config.APIOpenAICompat, BaseURL: srv.URL + "/v1", APIKey: "$FAKE_KEY", Models: []config.Model{{ID: "m"}}},
		},
		Default: "fake/m",
	}
	ref, err := cfg.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.Open(context.Background(), ref, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ag := &Agent{Model: model, Tools: tools.Default(dir), Policy: AllowAll{}, MaxSteps: 1}
	err = ag.Run(context.Background(), "read it", nil)
	var paused *PausedError
	if !errors.As(err, &paused) || paused.Steps != 1 || len(fs.requests) != 1 {
		t.Fatalf("err = %v, requests = %d; want a pause after exactly one step", err, len(fs.requests))
	}
}

func TestContinueWithoutHistoryFails(t *testing.T) {
	ag := &Agent{Policy: AllowAll{}}
	if err := ag.Continue(context.Background(), nil); err == nil {
		t.Fatal("Continue on an empty conversation must fail")
	}
}
