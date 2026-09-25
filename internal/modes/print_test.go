package modes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/attrition-tech/arkex/internal/agent"
)

// scripted replays a fixed event list and returns err from Run.
type scripted struct {
	events []agent.Event
	err    error
	prompt string
}

func (s *scripted) Run(_ context.Context, prompt string, emit func(agent.Event)) error {
	s.prompt = prompt
	for _, e := range s.events {
		emit(e)
	}
	return s.err
}

func TestPrintSeparatesTextFromToolActivity(t *testing.T) {
	r := &scripted{events: []agent.Event{
		agent.TurnStart{},
		agent.TextDelta{Text: "Let me look"}, // partial line, no newline
		agent.ToolCall{ID: "c1", Name: "bash", Input: "{\"command\":\n  \"ls   -la\"}"},
		agent.ToolDecision{ID: "c1", Name: "bash", Allowed: true},
		agent.ToolResult{ID: "c1", Name: "bash", Summary: "ls -la", Duration: 12 * time.Millisecond},
		agent.ToolCall{ID: "c2", Name: "edit", Input: `{"path":"x"}`},
		agent.ToolDecision{ID: "c2", Name: "edit", Allowed: false, Reason: "plan mode"},
		agent.ToolResult{ID: "c2", Name: "edit", IsError: true, Output: "denied\nby policy"},
		agent.TextDelta{Text: "Done."},
		agent.RunEnd{Steps: 2, Usage: fantasy.Usage{InputTokens: 100, OutputTokens: 7}},
	}}
	var out, errOut bytes.Buffer
	if err := Print(context.Background(), r, "hi", &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if r.prompt != "hi" {
		t.Fatalf("prompt = %q", r.prompt)
	}
	// stdout: only assistant text; the partial line is closed before tool
	// activity, and the final partial line is closed at the end.
	if got := out.String(); got != "Let me look\nDone.\n" {
		t.Fatalf("stdout = %q", got)
	}
	wantErr := "→ bash {\"command\": \"ls -la\"}\n" +
		"  ✓ ls -la (12ms)\n" +
		"→ edit {\"path\":\"x\"}\n" +
		"  ✗ plan mode\n" +
		"  ✗ denied by policy\n" +
		"[2 step(s), 100 in / 7 out tokens]\n"
	if got := errOut.String(); got != wantErr {
		t.Fatalf("stderr =\n%q\nwant\n%q", got, wantErr)
	}
}

func TestPrintDoesNotAddNewlineWhenTextAlreadyEnds(t *testing.T) {
	r := &scripted{events: []agent.Event{
		agent.TextDelta{Text: "line\n"},
		agent.RunEnd{Steps: 1},
	}}
	var out, errOut bytes.Buffer
	_ = Print(context.Background(), r, "p", &out, &errOut)
	if out.String() != "line\n" {
		t.Fatalf("stdout = %q", out.String())
	}
	// No text at all: nothing on stdout, not even a newline.
	out.Reset()
	r = &scripted{events: []agent.Event{agent.RunEnd{Steps: 1, Err: errors.New("boom")}}, err: errors.New("boom")}
	err := Print(context.Background(), r, "p", &out, &errOut)
	if err == nil || out.Len() != 0 || !strings.Contains(errOut.String(), "error: boom\n") {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
}

func TestJSONWritesOneEnvelopePerEventAndSerializesRunError(t *testing.T) {
	r := &scripted{events: []agent.Event{
		agent.TurnStart{},
		agent.TextDelta{Text: "hi"},
		agent.ToolCall{ID: "c1", Name: "read", Input: `{"path":"go.mod"}`},
		agent.ToolResult{ID: "c1", Name: "read", Output: "module x", Duration: time.Millisecond},
		agent.TurnEnd{},
		agent.RunEnd{Steps: 3, Usage: fantasy.Usage{InputTokens: 5}, Err: errors.New("stopped")},
	}}
	var out bytes.Buffer
	_ = JSON(context.Background(), r, "p", &out)

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("lines = %d:\n%s", len(lines), out.String())
	}
	var types []string
	var envs []map[string]any
	for _, l := range lines {
		var env map[string]any
		if err := json.Unmarshal([]byte(l), &env); err != nil {
			t.Fatalf("bad json %q: %v", l, err)
		}
		types = append(types, env["type"].(string))
		envs = append(envs, env)
	}
	if got := strings.Join(types, ","); got != "turn_start,text_delta,tool_call,tool_result,turn_end,run_end" {
		t.Fatalf("types = %s", got)
	}
	if d := envs[1]["data"].(map[string]any); d["text"] != "hi" {
		t.Fatalf("text_delta data = %v", d)
	}
	if d := envs[3]["data"].(map[string]any); d["output"] != "module x" || d["duration_ns"] != float64(time.Millisecond) {
		t.Fatalf("tool_result data = %v", d)
	}
	// error is not json-serializable as a field; the envelope must spell it out.
	d := envs[5]["data"].(map[string]any)
	if d["error"] != "stopped" || d["steps"] != float64(3) || d["usage"].(map[string]any)["input_tokens"] != float64(5) {
		t.Fatalf("run_end data = %v", d)
	}
}

func TestEventNameCoversEveryEvent(t *testing.T) {
	all := []agent.Event{
		agent.TurnStart{}, agent.TextDelta{}, agent.ReasoningDelta{}, agent.ToolCallStart{},
		agent.ToolCallInputDelta{}, agent.ToolCall{}, agent.ToolDecision{}, agent.ToolResult{},
		agent.ReasoningTime{}, agent.RequestTiming{},
		agent.RetryWait{}, agent.RequestRestart{},
		agent.TurnEnd{}, agent.Compacting{}, agent.RunEnd{},
	}
	seen := map[string]bool{}
	for _, e := range all {
		n := eventName(e)
		if n == "unknown" || seen[n] {
			t.Fatalf("event %T -> %q (dup=%v)", e, n, seen[n])
		}
		seen[n] = true
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("  a\n\tb   c ", 100); got != "a b c" {
		t.Fatalf("collapse = %q", got)
	}
	if got := oneLine("abcdefgh", 5); got != "abcd…" {
		t.Fatalf("cap = %q", got)
	}
}

type failedWriter struct{ err error }

func (w failedWriter) Write([]byte) (int, error) { return 0, w.err }

func TestPrintReportsOutputFailure(t *testing.T) {
	want := errors.New("output unavailable")
	for _, diagnostic := range []bool{false, true} {
		var good bytes.Buffer
		var out, errOut interface{ Write([]byte) (int, error) } = failedWriter{want}, &good
		if diagnostic {
			out, errOut = &good, failedWriter{want}
		}
		r := &scripted{events: []agent.Event{agent.TextDelta{Text: "hello"}, agent.RunEnd{}}}
		if err := Print(t.Context(), r, "test", out, errOut); !errors.Is(err, want) {
			t.Fatalf("diagnostic=%v: %v", diagnostic, err)
		}
	}
}
