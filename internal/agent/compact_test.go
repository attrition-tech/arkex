package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/dantearo/arkex/internal/config"
	"github.com/dantearo/arkex/internal/provider"
	"github.com/dantearo/arkex/internal/tools"
)

func TestAssumedContextThreshold(t *testing.T) {
	a := &Agent{Model: &provider.Model{}}
	a.SetMessages([]fantasy.Message{fantasy.NewUserMessage("hello"), fantasy.NewUserMessage("again")})
	for _, tc := range []struct {
		tokens int64
		want   bool
	}{{209715, false}, {209716, true}} {
		a.RestoreLastInput(tc.tokens)
		if a.needsCompact() != tc.want {
			t.Fatalf("tokens=%d compact=%v", tc.tokens, a.needsCompact())
		}
	}
	a.SetContextWindow(1000000)
	if a.ContextWindowAssumed() || a.needsCompact() {
		t.Fatal("explicit window must override fallback")
	}
}

func TestAutoCompactBracketsProgressOnFailure(t *testing.T) {
	ag := openFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
	})
	ag.SetMessages([]fantasy.Message{fantasy.NewUserMessage("hello")})
	var states []bool
	_, err := ag.autoCompact(t.Context(), func(e Event) {
		if c, ok := e.(Compacting); ok {
			states = append(states, c.Active)
		}
	}, 0, "test")
	if err == nil || len(states) != 2 || !states[0] || states[1] {
		t.Fatalf("failure lifecycle: states=%v err=%v", states, err)
	}
}

// openFake returns an agent whose model talks to handler over the
// OpenAI-compatible chat API.
func openFake(t *testing.T, handler http.HandlerFunc) *Agent {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := &config.Config{
		Connections: map[string]config.Connection{
			"fake": {API: config.APIOpenAICompat, BaseURL: srv.URL + "/v1", APIKey: "$FAKE_KEY",
				Models: []config.Model{{ID: "m"}}},
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
	return &Agent{Model: model, Policy: AllowAll{}, System: "You are a test."}
}

func TestCompactReplacesMessagesWithSummary(t *testing.T) {
	var got map[string]any
	ag := openFake(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m",
		  "choices":[{"index":0,"message":{"role":"assistant","content":"  The user renamed foo.go to bar.go; tests pass.  "},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":900,"completion_tokens":20,"total_tokens":920}}`))
	})
	ag.SetMessages([]fantasy.Message{
		fantasy.NewUserMessage("rename foo.go to bar.go"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "done"}}},
		fantasy.NewUserMessage("run the tests"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "all green"}}},
	})

	res, err := ag.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "The user renamed foo.go to bar.go; tests pass." || res.Dropped != 4 || res.Usage.InputTokens != 900 {
		t.Fatalf("result %+v", res)
	}
	// The request carried the system prompt, every message and the
	// summarising instruction last; no streaming, no tools.
	msgs := got["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatalf("request had %d messages: %v", len(msgs), msgs)
	}
	if first := msgs[0].(map[string]any); first["role"] != "system" || first["content"] != "You are a test." {
		t.Fatalf("first message %v", first)
	}
	if last := msgs[5].(map[string]any); last["role"] != "user" || !strings.Contains(last["content"].(string), "hand-off summary") {
		t.Fatalf("last message %v", last)
	}
	if got["stream"] == true || got["tools"] != nil {
		t.Fatalf("stream=%v tools=%v", got["stream"], got["tools"])
	}
	// The conversation is now the summary plus an acknowledgement.
	after := ag.Messages()
	if len(after) != 2 || after[0].Role != fantasy.MessageRoleUser || after[1].Role != fantasy.MessageRoleAssistant {
		t.Fatalf("messages after compact: %+v", after)
	}
	if txt := after[0].Content[0].(fantasy.TextPart).Text; !strings.Contains(txt, "bar.go; tests pass.") || !strings.Contains(txt, "compacted") {
		t.Fatalf("summary message %q", txt)
	}
}

func TestCompactKeepsMessagesOnFailure(t *testing.T) {
	ag := openFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	})
	before := []fantasy.Message{fantasy.NewUserMessage("hi")}
	ag.SetMessages(before)
	if _, err := ag.Compact(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if len(ag.Messages()) != 1 {
		t.Fatalf("messages changed on failure: %+v", ag.Messages())
	}

	empty := openFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`))
	})
	empty.SetMessages(before)
	if _, err := empty.Compact(context.Background()); err == nil || !strings.Contains(err.Error(), "empty summary") {
		t.Fatalf("empty summary error = %v", err)
	}
	if _, err := (&Agent{}).Compact(context.Background()); err == nil {
		t.Fatal("empty conversation should error")
	}
}

func TestIsContextOverflow(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, fmt.Errorf("request failed: %w", context.DeadlineExceeded), context.Canceled} {
		if IsContextOverflow(err) {
			t.Fatalf("network/run deadline must not trigger model compaction: %v", err)
		}
	}
	yes := []string{
		"This model's maximum context length is 128000 tokens. However, your messages resulted in 130000 tokens",
		"prompt is too long: 210000 tokens > 200000 maximum",
		"input length and `max_tokens` exceed context limit: 199000 + 4096 > 200000",
		"request too large: the context window is 32768 tokens",
	}
	no := []string{"", "connection refused", "invalid api key", "rate limit exceeded", "model not found: context-7b"}
	for _, s := range yes {
		if !IsContextOverflow(errors.New(s)) {
			t.Errorf("should match: %q", s)
		}
	}
	for _, s := range no {
		var err error
		if s != "" {
			err = errors.New(s)
		}
		if IsContextOverflow(err) {
			t.Errorf("should not match: %q", s)
		}
	}
}

func TestContextWindowFromError(t *testing.T) {
	cases := map[string]int{
		"This model's maximum context length is 262144 tokens. However, you requested 245761 tokens": 262144,
		"prompt is too long: 213462 tokens > 200000 maximum":                                         200000,
		"the request exceeds the available context size (8192)":                                      8192,
		"request too large: the context window is 32,768 tokens":                                     32768,
		"context length: 131072, prompt: 140000":                                                     131072,
		"this model is limited to 128000 tokens of context":                                          128000,
		"maximum context length is 512 tokens":                                                       0, // too small to be real
		"context length exceeded":                                                                    0,
		"connection refused":                                                                         0,
	}
	for msg, want := range cases {
		if got := ContextWindowFromError(errors.New(msg)); got != want {
			t.Errorf("%q: got %d, want %d", msg, got, want)
		}
	}
	if ContextWindowFromError(nil) != 0 {
		t.Error("nil error must give 0")
	}
}

func TestFormatTokens(t *testing.T) {
	for n, want := range map[int]string{500: "500", 8192: "8k", 128000: "128k", 262144: "262k", 1_000_000: "1M", 1_500_000: "1.5M"} {
		if got := formatTokens(n); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
}

// textMsg builds a user or assistant message of n characters.
func textMsg(role fantasy.MessageRole, n int) fantasy.Message {
	return fantasy.Message{Role: role, Content: []fantasy.MessagePart{fantasy.TextPart{Text: strings.Repeat("x", n)}}}
}

func toolMsg(n int) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
		fantasy.ToolResultPart{ToolCallID: "c", Output: fantasy.ToolResultOutputContentText{Text: strings.Repeat("y", n)}},
	}}
}

func TestFitTrimsToolResultsBeforeDroppingMessages(t *testing.T) {
	ag := &Agent{}
	// ~3.5 chars/token by default: a 10000-char tool result is ~2900
	// tokens, far over half of a 2000-token window (1000 tokens).
	msgs := []fantasy.Message{
		textMsg(fantasy.MessageRoleUser, 100),
		textMsg(fantasy.MessageRoleAssistant, 100),
		toolMsg(10000),
		textMsg(fantasy.MessageRoleAssistant, 100),
	}
	out, dropped := ag.fit(msgs, 2000, 0.5)
	if dropped != 0 || len(out) != 4 {
		t.Fatalf("trimming should have been enough: dropped=%d len=%d", dropped, len(out))
	}
	got := toolResultText(out[2].Content[0].(fantasy.ToolResultPart))
	if len(got) > trimHead+trimTail+80 || !strings.Contains(got, "characters trimmed") {
		t.Fatalf("tool result not trimmed: %d chars", len(got))
	}
	// The input slice is untouched.
	if len(toolResultText(msgs[2].Content[0].(fantasy.ToolResultPart))) != 10000 {
		t.Fatal("fit modified its input")
	}
	if estimateTokens(out, defaultCharsPerToken) > 1000 {
		t.Fatalf("still over budget: %d tokens", estimateTokens(out, defaultCharsPerToken))
	}
}

func TestFitDropsOldestAndKeepsUserFirst(t *testing.T) {
	ag := &Agent{}
	msgs := []fantasy.Message{
		textMsg(fantasy.MessageRoleUser, 3000),
		textMsg(fantasy.MessageRoleAssistant, 3000),
		textMsg(fantasy.MessageRoleUser, 3000),
		textMsg(fantasy.MessageRoleAssistant, 3000),
		textMsg(fantasy.MessageRoleUser, 3000),
		textMsg(fantasy.MessageRoleAssistant, 300),
	}
	// Budget: 0.5 × 4000 = 2000 tokens ≈ 7000 chars. Only the last user
	// message plus the short reply fit — and dropping the first four
	// lands on a user message, so exactly four go.
	out, dropped := ag.fit(msgs, 4000, 0.5)
	if dropped != 4 || len(out) != 2 || out[0].Role != fantasy.MessageRoleUser {
		t.Fatalf("dropped=%d len=%d first=%v", dropped, len(out), out[0].Role)
	}
	// If the cut would land on an assistant message it moves on to the
	// next user message even though the assistant one would fit.
	msgs2 := []fantasy.Message{
		textMsg(fantasy.MessageRoleUser, 6000),
		textMsg(fantasy.MessageRoleAssistant, 100),
		textMsg(fantasy.MessageRoleUser, 100),
	}
	out, dropped = ag.fit(msgs2, 1000, 0.5) // 500 tokens ≈ 1750 chars
	if dropped != 2 || len(out) != 1 || out[0].Role != fantasy.MessageRoleUser {
		t.Fatalf("dropped=%d len=%d first=%v", dropped, len(out), out[0].Role)
	}
	// Unknown window: nothing changes.
	if out, dropped := ag.fit(msgs, 0, 0.5); dropped != 0 || len(out) != len(msgs) {
		t.Fatal("unknown window must not trim")
	}
	// The kept messages are still the first (so the last one survives).
	if out, _ := ag.fit(msgs, 1000, 0.5); len(out) < 1 {
		t.Fatal("fit must keep at least one message")
	}
}

func TestCharsPerTokenComesFromLastRequest(t *testing.T) {
	ag := &Agent{}
	ag.SetMessages([]fantasy.Message{textMsg(fantasy.MessageRoleUser, 4000)})
	if r := ag.charsPerToken(); r != defaultCharsPerToken {
		t.Fatalf("before any request: %v", r)
	}
	ag.lastInput = 1000 // (4000+16)/1000
	if r := ag.charsPerToken(); r < 4 || r > 4.1 {
		t.Fatalf("observed ratio %v", r)
	}
	ag.lastInput = 10 // absurd: clamped to 8
	if r := ag.charsPerToken(); r != 8 {
		t.Fatalf("clamped ratio %v", r)
	}
}

// overflowJSON is what an OpenAI-compatible server says when the prompt
// does not fit; window is the number it states.
func overflowJSON(window, got int) string {
	return `{"error":{"message":"This model's maximum context length is ` + itoa(window) + ` tokens. However, your messages resulted in ` + itoa(got) + ` tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`
}

func itoa(n int) string { return fmt.Sprint(n) }

// requestMessages returns the messages of the idx-th recorded request as
// (role, content) pairs; content is "" for non-string content.
func requestMessages(t *testing.T, fs *fakeServer, idx int) [][2]string {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if idx >= len(fs.requests) {
		t.Fatalf("only %d requests were made", len(fs.requests))
	}
	msgs, _ := fs.requests[idx]["messages"].([]any)
	var out [][2]string
	for _, mv := range msgs {
		m, _ := mv.(map[string]any)
		c, _ := m["content"].(string)
		out = append(out, [2]string{fmt.Sprint(m["role"]), c})
	}
	return out
}

func openFakeServer(t *testing.T, fs *fakeServer, window int) *Agent {
	t.Helper()
	ag := openFake(t, fs.handler)
	ag.Tools = tools.Default(t.TempDir())
	ag.SetContextWindow(window)
	return ag
}

// A run that overflows mid-way learns the window from the error, compacts
// the conversation, retries the same step and finishes.
func TestOverflowMidRunLearnsWindowCompactsAndRetries(t *testing.T) {
	fs := &fakeServer{script: []func(http.ResponseWriter){
		// step 1: a tool call (reads a file that does not exist; fine)
		func(w http.ResponseWriter) {
			sse(w,
				delta(`{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"nope.txt\"}"}}]}`, ""),
				delta(`{}`, "tool_calls"), usage())
		},
		// step 2: rejected as too long
		func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, overflowJSON(4096, 5000))
		},
		// the summary request (non-streaming)
		func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"User asked to read nope.txt; it does not exist."},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`)
		},
		// step 2 again: now the model answers
		func(w http.ResponseWriter) {
			sse(w, delta(`{"role":"assistant","content":"The file is missing."}`, ""), delta(`{}`, "stop"), usage())
		},
	}}
	ag := openFakeServer(t, fs, 0)
	var events []Event
	err := ag.Run(context.Background(), "read nope.txt", func(e Event) { events = append(events, e) })
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if ag.ContextWindow() != 4096 {
		t.Fatalf("window not learned: %d", ag.ContextWindow())
	}
	var learned *ContextWindowLearned
	var compacted *Compacted
	var turnStarts, turnEnds int
	for _, e := range events {
		switch e := e.(type) {
		case ContextWindowLearned:
			learned = &e
		case Compacted:
			compacted = &e
		case TurnStart:
			turnStarts++
		case TurnEnd:
			turnEnds++
		}
	}
	if learned == nil || learned.Tokens != 4096 {
		t.Fatalf("ContextWindowLearned = %+v", learned)
	}
	if compacted == nil || compacted.Dropped != 3 || !strings.Contains(compacted.Reason, "exceeded") {
		t.Fatalf("Compacted = %+v", compacted)
	}
	if turnEnds != 2 || turnStarts != 3 { // the failed step started but never ended
		t.Fatalf("turnStarts=%d turnEnds=%d", turnStarts, turnEnds)
	}
	end := events[len(events)-1].(RunEnd)
	if end.Steps != 2 || end.Err != nil {
		t.Fatalf("RunEnd = %+v", end)
	}
	// The retried request carried the summary as the (single, unacked)
	// last message so the model answered it directly.
	got := requestMessages(t, fs, 3)
	if len(got) != 2 || got[0][0] != "system" || got[1][0] != "user" || !strings.Contains(got[1][1], "nope.txt; it does not exist") || !strings.Contains(got[1][1], "Continue from this state") {
		t.Fatalf("retried request messages: %v", got)
	}
	// The conversation now is summary + the final answer.
	after := ag.Messages()
	if len(after) != 2 || after[0].Role != fantasy.MessageRoleUser || after[1].Role != fantasy.MessageRoleAssistant {
		t.Fatalf("messages after run: %d", len(after))
	}
	if ag.LastInput() != 10 {
		t.Fatalf("lastInput = %d", ag.LastInput())
	}
}

// When the window is known and the last request came close to it, the
// next request is preceded by a compaction that keeps the fresh prompt
// verbatim behind the summary+ack pair.
func TestPreRequestCompactKeepsFreshPrompt(t *testing.T) {
	fs := &fakeServer{script: []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			sse(w, delta(`{"role":"assistant","content":"first"}`, ""), delta(`{}`, "stop"),
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":900,"completion_tokens":5,"total_tokens":905}}`)
		},
		func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"S"},"finish_reason":"stop"}]}`)
		},
		func(w http.ResponseWriter) {
			sse(w, delta(`{"role":"assistant","content":"second"}`, ""), delta(`{}`, "stop"), usage())
		},
	}}
	ag := openFakeServer(t, fs, 1000)
	if err := ag.Run(context.Background(), "one", nil); err != nil {
		t.Fatal(err)
	}
	if ag.LastInput() != 900 || !ag.needsCompact() {
		t.Fatalf("lastInput=%d needsCompact=%v", ag.LastInput(), ag.needsCompact())
	}
	var reasons []string
	if err := ag.Run(context.Background(), "two", func(e Event) {
		if c, ok := e.(Compacted); ok {
			reasons = append(reasons, c.Reason)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 1 || reasons[0] != "the last request used 90% of the 1k-token context window" {
		t.Fatalf("reasons = %q", reasons)
	}
	// Summary request saw only the old messages, not "two".
	for _, m := range requestMessages(t, fs, 1) {
		if m[1] == "two" {
			t.Fatal("the fresh prompt must not be summarised")
		}
	}
	got := requestMessages(t, fs, 2)
	if len(got) != 4 || got[1][0] != "user" || !strings.Contains(got[1][1], "S") || got[2][1] != CompactAck || got[3][1] != "two" {
		t.Fatalf("second run request: %v", got)
	}
}

// With no known window and an error that does not state one, the
// summary request is shrunk relative to itself and the run still recovers.
func TestOverflowWithoutStatedWindowStillRecovers(t *testing.T) {
	overflow := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"the prompt is too long for this model","type":"invalid_request_error"}}`)
	}
	var summaryReqs int
	fs := &fakeServer{script: []func(http.ResponseWriter){
		overflow, // step 1 rejected
		overflow, // first summary attempt rejected too
		func(w http.ResponseWriter) { // second, smaller summary attempt
			summaryReqs++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"S"},"finish_reason":"stop"}]}`)
		},
		func(w http.ResponseWriter) {
			sse(w, delta(`{"role":"assistant","content":"ok"}`, ""), delta(`{}`, "stop"), usage())
		},
	}}
	ag := openFakeServer(t, fs, 0)
	old := make([]fantasy.Message, 0, 6)
	for i := 0; i < 3; i++ {
		old = append(old, textMsg(fantasy.MessageRoleUser, 4000), textMsg(fantasy.MessageRoleAssistant, 4000))
	}
	ag.SetMessages(old)
	var compacted Compacted
	err := ag.Run(context.Background(), "go", func(e Event) {
		if c, ok := e.(Compacted); ok {
			compacted = c
		}
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if ag.Model.Ref.Model.ContextWindow != 0 || !ag.ContextWindowAssumed() || ag.ContextWindow() != DefaultContextWindow {
		t.Fatalf("no window was stated, none should be learned: %d", ag.ContextWindow())
	}
	// The second summary attempt carried fewer old messages than the first.
	first, second := requestMessages(t, fs, 1), requestMessages(t, fs, 2)
	if len(second) >= len(first) || compacted.Trimmed == 0 {
		t.Fatalf("second attempt not smaller: %d vs %d, trimmed=%d", len(second), len(first), compacted.Trimmed)
	}
	// The fresh prompt "go" was kept and follows the summary+ack pair.
	got := requestMessages(t, fs, 3)
	if got[len(got)-1][1] != "go" || got[len(got)-2][1] != CompactAck {
		t.Fatalf("retried request: %v", got)
	}
}

// A second overflow in the same step is not retried again.
func TestOverflowRecoveryHappensOnce(t *testing.T) {
	overflow := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, overflowJSON(4096, 5000))
	}
	summary := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"S"},"finish_reason":"stop"}]}`)
	}
	fs := &fakeServer{script: []func(http.ResponseWriter){overflow, summary, overflow}}
	ag := openFakeServer(t, fs, 0)
	ag.SetMessages([]fantasy.Message{textMsg(fantasy.MessageRoleUser, 100), textMsg(fantasy.MessageRoleAssistant, 100)})
	err := ag.Run(context.Background(), "go", nil)
	if err == nil || !IsContextOverflow(err) {
		t.Fatalf("err = %v", err)
	}
	if n := len(fs.requests); n != 3 {
		t.Fatalf("requests = %d, want overflow, summary, overflow and stop", n)
	}
}
