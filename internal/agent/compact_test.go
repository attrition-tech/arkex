package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/provider"
	"github.com/attrition-tech/arkex/internal/session"
	"github.com/attrition-tech/arkex/internal/tools"
)

type compactModel struct {
	fantasy.LanguageModel
	calls   []fantasy.Call
	respond func(context.Context, fantasy.Call, int) (fantasy.StreamResponse, error)
}

func (m *compactModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls = append(m.calls, call)
	return m.respond(ctx, call, len(m.calls))
}

func summaryResponse(text string, reason fantasy.FinishReason) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: text}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: reason, Usage: fantasy.Usage{InputTokens: 17, OutputTokens: 9}})
	}
}

func compactAgent(t *testing.T, window int) (*Agent, *compactModel, *session.Session) {
	t.Helper()
	t.Setenv("ARKEX_HOME", t.TempDir())
	s := session.New(t.TempDir())
	m := &compactModel{respond: func(context.Context, fantasy.Call, int) (fantasy.StreamResponse, error) {
		return summaryResponse("Keep the user's constraints; tests are still pending.", fantasy.FinishReasonStop), nil
	}}
	a := &Agent{Model: &provider.Model{LM: m, Ref: config.ModelRef{Model: config.Model{ContextWindow: window}}}, Policy: AllowAll{}, StoreContext: s.SaveContext}
	return a, m, s
}

func textMsg(role fantasy.MessageRole, n int) fantasy.Message {
	return fantasy.Message{Role: role, Content: []fantasy.MessagePart{fantasy.TextPart{Text: strings.Repeat("x", n)}}}
}

func TestCompactPreservesHistoryRecentMessagesAndFreshPrompt(t *testing.T) {
	a, lm, s := compactAgent(t, 8192)
	before := []fantasy.Message{
		fantasy.NewUserMessage("Original approval: local edits only. Secret test identifier AZ-719. " + strings.Repeat("old facts ", 2000)),
		textMsg(fantasy.MessageRoleAssistant, 5000),
		fantasy.NewUserMessage("Recent request: verify, do not publish."),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "c", ToolName: "read", Input: `{"path":"a.txt"}`}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "c", Output: fantasy.ToolResultOutputContentText{Text: "Exact result: 13 failures"}}}},
		fantasy.NewUserMessage("Explain those failures before editing."),
	}
	a.SetMessages(before)
	var stages []string
	res, err := a.autoCompact(t.Context(), func(e Event) {
		if p, ok := e.(CompactProgress); ok {
			stages = append(stages, p.Stage)
		}
	}, 1, "test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Trimmed != 0 || res.Dropped != 2 || !strings.Contains(res.Summary, res.HistoryPath) {
		t.Fatalf("result: %+v", res)
	}
	if !reflect.DeepEqual(a.Messages()[2:], before[2:]) {
		t.Fatal("recent exchange or fresh prompt changed")
	}
	loaded, err := session.Load(s.Cwd, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.History[:len(before)], before) || !reflect.DeepEqual(loaded.Messages, a.Messages()) {
		t.Fatal("durable history/replacement differs")
	}
	// The model can actually read the source, even though the fake summary
	// deliberately omitted AZ-719 and the original authorization.
	read := &tools.Read{Root: s.Cwd}
	input, _ := json.Marshal(map[string]any{"path": res.HistoryPath, "limit": 100})
	got, err := read.Run(t.Context(), input)
	if err != nil || !strings.Contains(got.Output, "AZ-719") {
		t.Fatalf("history retrieval: %v %+v", err, got)
	}
	for _, want := range []string{"saving original history", "waiting for model", "receiving summary", "validating summary", "saving compacted context"} {
		if !strings.Contains(strings.Join(stages, ","), want) {
			t.Fatalf("missing phase %q: %v", want, stages)
		}
	}
	for _, call := range lm.calls {
		if len(call.Tools) != 0 || call.MaxOutputTokens == nil || *call.MaxOutputTokens != 1024 {
			t.Fatal("summary did not get its own request settings")
		}
	}
}

func TestCompactChunksCoverAllHistoryIncludingRetry(t *testing.T) {
	a, lm, _ := compactAgent(t, 4096)
	before := []fantasy.Message{fantasy.NewUserMessage("first sentinel " + strings.Repeat("🙂 العربية ", 1500) + " last sentinel")}
	a.SetMessages(before)
	var read strings.Builder
	var first string
	lm.respond = func(_ context.Context, call fantasy.Call, n int) (fantasy.StreamResponse, error) {
		text := call.Prompt[1].Content[0].(fantasy.TextPart).Text
		_, chunk, ok := strings.Cut(text, "Next history chunk (JSON data, possibly a fragment):\n")
		if !ok || !utf8.ValidString(chunk) {
			t.Fatal("invalid history chunk")
		}
		if n == 1 {
			first = chunk
			return nil, errors.New("prompt is too long for this model")
		}
		if n == 2 && (len(chunk) >= len(first) || !strings.HasPrefix(first, chunk)) {
			t.Fatal("overflow retry did not shrink the same unread chunk")
		}
		read.WriteString(chunk)
		return summaryResponse("Both sentinels and the user constraints are important.", fantasy.FinishReasonStop), nil
	}
	res, err := a.Compact(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(before)
	if read.String() != string(want) || res.Trimmed != 0 || len(lm.calls) < 3 {
		t.Fatal("history bytes skipped or duplicated")
	}
	if res.Usage.InputTokens != int64((len(lm.calls)-1)*17) {
		t.Fatal("summary usage lost")
	}
}

func TestCompactRejectsBadResponsesWithoutReplacingHistory(t *testing.T) {
	for _, kind := range []string{"length", "empty", "missing finish", "error", "oversized", "tool call", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			a, lm, s := compactAgent(t, 8192)
			before := []fantasy.Message{textMsg(fantasy.MessageRoleUser, 16000)}
			a.SetMessages(before)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			lm.respond = func(context.Context, fantasy.Call, int) (fantasy.StreamResponse, error) {
				switch kind {
				case "length":
					return summaryResponse("partial text", fantasy.FinishReasonLength), nil
				case "empty":
					return summaryResponse(" ", fantasy.FinishReasonStop), nil
				case "error":
					return nil, errors.New("connection failed")
				case "oversized":
					return summaryResponse(strings.Repeat("x", 9000), fantasy.FinishReasonStop), nil
				case "cancel":
					cancel()
					return summaryResponse("complete but canceled", fantasy.FinishReasonStop), nil
				case "tool call":
					return func(y func(fantasy.StreamPart) bool) { y(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall}) }, nil
				default:
					return func(y func(fantasy.StreamPart) bool) {
						y(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "cut off"})
					}, nil
				}
			}
			_, err := a.Compact(ctx)
			if err == nil || !reflect.DeepEqual(a.Messages(), before) {
				t.Fatalf("accepted %s: %v", kind, err)
			}
			loaded, err := session.Load(s.Cwd, s.ID)
			if err != nil || !reflect.DeepEqual(loaded.Messages, before) {
				t.Fatal("original not saved before failed generation", err)
			}
		})
	}
}

func TestCompactStorageFailureAndCancelAtCommit(t *testing.T) {
	for _, kind := range []string{"no store", "original failure", "replacement failure", "cancel before save", "cancel after commit"} {
		t.Run(kind, func(t *testing.T) {
			a, lm, s := compactAgent(t, 8192)
			before := []fantasy.Message{textMsg(fantasy.MessageRoleUser, 16000)}
			a.SetMessages(before)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			a.StoreContext = func(ctx context.Context, msgs []fantasy.Message) (string, error) {
				calls++
				if kind == "original failure" || (calls == 2 && kind == "replacement failure") {
					return "", errors.New("disk full")
				}
				if calls == 2 && kind == "cancel before save" {
					cancel()
				}
				path, err := s.SaveContext(ctx, msgs)
				if calls == 2 && kind == "cancel after commit" {
					cancel()
				}
				return path, err
			}
			if kind == "no store" {
				a.StoreContext = nil
			}
			_, err := a.Compact(ctx)
			if kind == "cancel after commit" {
				loaded, e := session.Load(s.Cwd, s.ID)
				if err != nil || e != nil || !reflect.DeepEqual(loaded.Messages, a.Messages()) || reflect.DeepEqual(a.Messages(), before) {
					t.Fatal("commit acknowledgement lost", err, e)
				}
			} else if err == nil || !reflect.DeepEqual(a.Messages(), before) {
				t.Fatal("failed transaction replaced messages", err)
			}
			if (kind == "no store" || kind == "original failure") && len(lm.calls) != 0 {
				t.Fatal("spent tokens before preserving history")
			}
		})
	}
}

func TestCompactDeadlineAndRequestLimit(t *testing.T) {
	a, lm, _ := compactAgent(t, 4096)
	a.SetMessages([]fantasy.Message{textMsg(fantasy.MessageRoleUser, 2000)})
	lm.respond = func(ctx context.Context, _ fantasy.Call, _ int) (fantasy.StreamResponse, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > compactRequestDeadline {
			t.Error("unbounded summary request")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := a.Compact(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	lm.calls = nil
	lm.respond = func(context.Context, fantasy.Call, int) (fantasy.StreamResponse, error) {
		return summaryResponse("S", fantasy.FinishReasonStop), nil
	}
	a.SetMessages([]fantasy.Message{textMsg(fantasy.MessageRoleUser, 200000)})
	if _, err := a.Compact(t.Context()); err == nil || !strings.Contains(err.Error(), "request limit") || len(lm.calls) != compactMaxRequests {
		t.Fatalf("unbounded chunks: %d %v", len(lm.calls), err)
	}
}

func TestCompactionProjectsNextRequest(t *testing.T) {
	a, _, _ := compactAgent(t, 10000)
	a.SetMessages([]fantasy.Message{fantasy.NewUserMessage("hello"), textMsg(fantasy.MessageRoleAssistant, 12)})
	a.RestoreLastInput(7900)
	if a.needsCompact() {
		t.Fatal("below threshold")
	}
	a.messages = append(a.messages, textMsg(fantasy.MessageRoleUser, 1000))
	if !a.needsCompact() {
		t.Fatal("new prompt not included")
	}
	a.RestoreLastInput(7000)
	limit := int64(3500)
	a.Model.Limit = &limit
	if !a.needsCompact() {
		t.Fatal("output headroom not reserved")
	}
	a.Model.Limit = nil
	a.RestoreLastInput(0)
	a.System = strings.Repeat("instructions ", 1600)
	if !a.needsCompact() {
		t.Fatal("system instructions not counted")
	}
}

func TestCompactionFailureStopsInsteadOfSendingOversizedRequest(t *testing.T) {
	a, lm, _ := compactAgent(t, 8192)
	a.SetMessages([]fantasy.Message{textMsg(fantasy.MessageRoleUser, 28000), textMsg(fantasy.MessageRoleAssistant, 100)})
	lm.respond = func(context.Context, fantasy.Call, int) (fantasy.StreamResponse, error) {
		return summaryResponse("partial", fantasy.FinishReasonLength), nil
	}
	err := a.Run(t.Context(), "continue", nil)
	if err == nil || len(lm.calls) != 1 || a.LastUsage().InputTokens != 17 {
		t.Fatal("failure retried or usage lost", err, len(lm.calls), a.LastUsage())
	}
}

func TestOverflowRecoveryAndUsage(t *testing.T) {
	a, lm, _ := compactAgent(t, 0)
	a.SetMessages([]fantasy.Message{fantasy.NewUserMessage("old request"), textMsg(fantasy.MessageRoleAssistant, 600)})
	lm.respond = func(_ context.Context, call fantasy.Call, n int) (fantasy.StreamResponse, error) {
		if n == 1 {
			return nil, errors.New("maximum context length is 8192 tokens; prompt too long")
		}
		if n == 2 {
			return summaryResponse("Earlier work remains in history.", fantasy.FinishReasonStop), nil
		}
		return summaryResponse("Continued correctly.", fantasy.FinishReasonStop), nil
	}
	if err := a.Run(t.Context(), "new request", nil); err != nil {
		t.Fatal(err)
	}
	if a.ContextWindow() != 8192 || len(lm.calls) != 3 || a.LastUsage().InputTokens != 34 {
		t.Fatal("recovery or accounting failed")
	}
	last := lm.calls[2].Prompt
	if last[len(last)-1].Content[0].(fantasy.TextPart).Text != "new request" {
		t.Fatal("fresh prompt lost")
	}
}

func TestCancelDuringOverflowRecoveryPreservesCancellation(t *testing.T) {
	a, lm, _ := compactAgent(t, 8192)
	before := []fantasy.Message{fantasy.NewUserMessage("old request"), textMsg(fantasy.MessageRoleAssistant, 600)}
	a.SetMessages(before)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lm.respond = func(context.Context, fantasy.Call, int) (fantasy.StreamResponse, error) {
		if len(lm.calls) == 1 {
			return nil, errors.New("context length exceeded")
		}
		cancel()
		return nil, ctx.Err()
	}
	err := a.Run(ctx, "continue", nil)
	want := append(before, fantasy.NewUserMessage("continue"))
	if !errors.Is(err, context.Canceled) || len(lm.calls) != 2 || !reflect.DeepEqual(a.Messages(), want) {
		t.Fatalf("cancellation lost or original replaced: %v; calls=%d", err, len(lm.calls))
	}
}

func TestContextErrorsAndSummaryRecognition(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, context.Canceled, errors.New("rate limit exceeded"), nil} {
		if IsContextOverflow(err) {
			t.Fatal("misclassified overflow", err)
		}
	}
	for msg, want := range map[string]int{
		"maximum context length is 262144 tokens":            262144,
		"prompt is too long: 213462 tokens > 200000 maximum": 200000,
		"context size (8192)":                                8192,
		"context window of 32,768 tokens":                    32768,
		"limited to 128000 tokens":                           128000,
		"context length: 512":                                0,
	} {
		if got := ContextWindowFromError(errors.New(msg)); got != want {
			t.Fatalf("%q: %d != %d", msg, got, want)
		}
	}
	if _, ok := CompactSummary(CompactPrefix + "not a real summary"); ok {
		t.Fatal("ordinary user text mistaken for a summary")
	}
	for n, want := range map[int]string{500: "500", 8192: "8k", 1000000: "1M", 1500000: "1.5M"} {
		if formatTokens(n) != want {
			t.Fatal(n)
		}
	}
}

func TestCompactedHistoryFileIsPrivate(t *testing.T) {
	a, _, _ := compactAgent(t, 8192)
	a.SetMessages([]fantasy.Message{fantasy.NewUserMessage("private work")})
	r, err := a.Compact(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(r.HistoryPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(fmt.Sprint("history permissions: ", err))
	}
}

func TestCompactRetainsAttachmentsAcrossRepeatedCompaction(t *testing.T) {
	a, lm, _ := compactAgent(t, 32768)
	file := fantasy.FilePart{Filename: "evidence.png", MediaType: "image/png", Data: []byte(strings.Repeat("image bytes", 100000))}
	a.SetMessages([]fantasy.Message{fantasy.NewUserMessage("Inspect this evidence", file)})
	for range 2 {
		res, err := a.Compact(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		parts := a.Messages()[0].Content
		if len(parts) != 2 || !reflect.DeepEqual(parts[1], file) {
			t.Fatal("attachment changed, lost or duplicated")
		}
		data, err := os.ReadFile(res.HistoryPath)
		if err != nil {
			t.Fatal(err)
		}
		var history []fantasy.Message
		if err := json.Unmarshal(data, &history); err != nil || !reflect.DeepEqual(history[0].Content[1], file) {
			t.Fatal("original attachment not archived", err)
		}
	}
	if len(lm.calls) != 2 {
		t.Fatalf("base64 inflated summary requests: %d", len(lm.calls))
	}
	for _, call := range lm.calls {
		wire, _ := json.Marshal(call.Prompt)
		if len(wire) > 5000 || !strings.Contains(string(wire), "evidence.png") {
			t.Fatal("binary data fed to text summarizer")
		}
	}
}

func TestCompactCannotReplaceWithOversizedFreshPrompt(t *testing.T) {
	a, _, _ := compactAgent(t, 8192)
	before := []fantasy.Message{textMsg(fantasy.MessageRoleUser, 2000), textMsg(fantasy.MessageRoleAssistant, 1000), textMsg(fantasy.MessageRoleUser, 40000)}
	a.SetMessages(before)
	var states []bool
	_, err := a.autoCompact(t.Context(), func(e Event) {
		if c, ok := e.(Compacting); ok {
			states = append(states, c.Active)
		}
	}, 1, "test")
	if err == nil || !reflect.DeepEqual(a.Messages(), before) || !reflect.DeepEqual(states, []bool{true, false}) {
		t.Fatalf("unsafe replacement/lifecycle: %v %v", err, states)
	}
}

func TestSecondOverflowStopsWithoutRepeatedCompaction(t *testing.T) {
	a, lm, _ := compactAgent(t, 8192)
	a.SetMessages([]fantasy.Message{textMsg(fantasy.MessageRoleUser, 1000), textMsg(fantasy.MessageRoleAssistant, 50)})
	lm.respond = func(_ context.Context, _ fantasy.Call, n int) (fantasy.StreamResponse, error) {
		if n == 2 {
			return summaryResponse("S", fantasy.FinishReasonStop), nil
		}
		return nil, errors.New("context length exceeded")
	}
	if err := a.Run(t.Context(), "continue", nil); !IsContextOverflow(err) || len(lm.calls) != 3 {
		t.Fatalf("unbounded recovery: %v %d", err, len(lm.calls))
	}
}

func TestCompactionInstallsItsOwnDeadlines(t *testing.T) {
	a, lm, s := compactAgent(t, 8192)
	a.SetMessages([]fantasy.Message{fantasy.NewUserMessage("work")})
	a.StoreContext = func(ctx context.Context, msgs []fantasy.Message) (string, error) {
		deadline, ok := ctx.Deadline()
		if left := time.Until(deadline); !ok || left < 4*time.Minute || left > 5*time.Minute {
			t.Fatalf("overall deadline: %v %v", ok, left)
		}
		return s.SaveContext(ctx, msgs)
	}
	lm.respond = func(ctx context.Context, _ fantasy.Call, _ int) (fantasy.StreamResponse, error) {
		deadline, ok := ctx.Deadline()
		if left := time.Until(deadline); !ok || left < 80*time.Second || left > 90*time.Second {
			t.Fatalf("request deadline: %v %v", ok, left)
		}
		return summaryResponse("S", fantasy.FinishReasonStop), nil
	}
	if _, err := a.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLearnedContextCanBeReadDuringCompaction(t *testing.T) {
	a, _, _ := compactAgent(t, 8192)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 1000 {
			a.SetContextWindow(8192 + i)
		}
	}()
	for range 1000 {
		_ = a.ContextWindow()
		_ = a.ContextWindowAssumed()
	}
	<-done
	if a.ContextWindow() != 9191 || a.Model.Ref.Model.ContextWindow != 8192 {
		t.Fatal("learned window must not mutate shared provider options")
	}
}
