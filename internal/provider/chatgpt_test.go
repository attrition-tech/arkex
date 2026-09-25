package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"

	"github.com/attrition-tech/arkex/internal/chatgpt"
	"github.com/attrition-tech/arkex/internal/config"
)

const fakeStream = `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":0,"model":"gpt-5.3-codex","output":[],"status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello "}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"there"}

event: response.output_item.done
data: {"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello there","annotations":[]}]}}

event: response.completed
data: {"type":"response.completed","sequence_number":5,"response":{"id":"resp_1","object":"response","created_at":0,"model":"gpt-5.3-codex","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello there","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}

`

func TestOpenChatGPTStreams(t *testing.T) {
	type seen struct {
		hdr  http.Header
		body map[string]any
	}
	got := make(chan seen, 1)
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Method != http.MethodPost {
			http.Error(w, r.Method+" "+r.URL.Path, 404)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			http.Error(w, "body: "+err.Error(), 400)
			return
		}
		got <- seen{r.Header.Clone(), m}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, fakeStream) //nolint:errcheck
	}))
	defer be.Close()

	store := &chatgpt.Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	if err := store.Put("cg", chatgpt.Tokens{AccessToken: "at", RefreshToken: "rt", AccountID: "acct_9", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	old := AuthStore
	AuthStore = func() (*chatgpt.Store, error) { return store, nil }
	defer func() { AuthStore = old }()

	ref := config.ModelRef{
		ConnID:   "cg",
		Conn:     config.Connection{Kind: config.KindSubscription, API: config.APIOpenAI, BaseURL: be.URL},
		Model:    config.Model{ID: "gpt-5.3-codex", Reasoning: true, MaxTokens: 100},
		Thinking: "max",
	}
	m, err := Open(context.Background(), ref, be.Client())
	if err != nil {
		t.Fatal(err)
	}
	if m.Limit != nil {
		t.Fatal("subscription model kept a max output tokens limit the backend rejects")
	}
	po, ok := m.Opts[m.LM.Provider()].(*openai.ResponsesProviderOptions)
	if !ok || po.ReasoningEffort == nil || *po.ReasoningEffort != openai.ReasoningEffortXHigh {
		t.Fatalf("options %#v", m.Opts)
	}

	prompt := fantasy.Prompt{
		fantasy.NewSystemMessage("You are arkex."),
		fantasy.NewUserMessage("hi"),
	}
	seq, err := m.LM.Stream(context.Background(), m.Call(prompt, nil))
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var finish fantasy.FinishReason
	for part := range seq {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeFinish:
			finish = part.FinishReason
		case fantasy.StreamPartTypeError:
			t.Fatalf("stream error: %v", part.Error)
		}
	}
	if text.String() != "hello there" || finish != fantasy.FinishReasonStop {
		t.Fatalf("text %q finish %q", text.String(), finish)
	}

	s := <-got
	if s.hdr.Get("Authorization") != "Bearer at" || s.hdr.Get("ChatGPT-Account-ID") != "acct_9" || s.hdr.Get("originator") != chatgpt.Originator {
		t.Fatalf("headers %v", s.hdr)
	}
	if !strings.HasPrefix(s.hdr.Get("User-Agent"), chatgpt.Originator+"/") || !strings.Contains(s.hdr.Get("User-Agent"), UserAgent) {
		t.Fatalf("user agent %q", s.hdr.Get("User-Agent"))
	}
	if s.body["instructions"] != "You are arkex." || s.body["store"] != false || s.body["stream"] != true {
		t.Fatalf("body %v", s.body)
	}
	if _, ok := s.body["max_output_tokens"]; ok {
		t.Fatal("max_output_tokens sent")
	}
	in, _ := s.body["input"].([]any)
	if len(in) != 1 {
		t.Fatalf("input still carries the system message: %v", in)
	}
	if first, _ := in[0].(map[string]any); first["role"] != "user" {
		t.Fatalf("first input item %v", first)
	}
	reasoning, _ := s.body["reasoning"].(map[string]any)
	if reasoning["effort"] != "xhigh" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning %v", s.body["reasoning"])
	}
	inc, _ := s.body["include"].([]any)
	if len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Fatalf("include %v", inc)
	}
}

func TestOpenChatGPTNotSignedIn(t *testing.T) {
	store := &chatgpt.Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	old := AuthStore
	AuthStore = func() (*chatgpt.Store, error) { return store, nil }
	defer func() { AuthStore = old }()
	ref := config.ModelRef{ConnID: "cg", Conn: config.Connection{Kind: config.KindSubscription}, Model: config.Model{ID: "gpt-5.3"}}
	_, err := Open(context.Background(), ref, nil)
	if err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("err %v", err)
	}
}

func TestUnknownSubscriptionDoesNotFallBack(t *testing.T) {
	ref := config.ModelRef{Conn: config.Connection{Kind: config.KindSubscription, Subscription: "removed-provider"}}
	if _, err := Open(t.Context(), ref, nil); err == nil || !strings.Contains(err.Error(), "unknown subscription") {
		t.Fatalf("expected rejection before credential lookup, got %v", err)
	}
}

func TestOpenAIResponsesOptions(t *testing.T) {
	cases := map[string]*openai.ReasoningEffort{
		"":       nil,
		"off":    new(openai.ReasoningEffortNone),
		"low":    new(openai.ReasoningEffortLow),
		"high":   new(openai.ReasoningEffortHigh),
		"xhigh":  new(openai.ReasoningEffortXHigh),
		"max":    new(openai.ReasoningEffortXHigh),
		"medium": new(openai.ReasoningEffortMedium),
	}
	for level, want := range cases {
		o, err := OpenAIResponsesOptions(level)
		if err != nil {
			t.Fatalf("%q: %v", level, err)
		}
		switch {
		case want == nil && o.ReasoningEffort != nil:
			t.Errorf("%q: effort %v, want unset", level, *o.ReasoningEffort)
		case want != nil && (o.ReasoningEffort == nil || *o.ReasoningEffort != *want):
			t.Errorf("%q: effort %v, want %v", level, o.ReasoningEffort, *want)
		}
		if o.Store == nil || *o.Store || len(o.Include) != 1 || o.Include[0] != openai.IncludeReasoningEncryptedContent {
			t.Errorf("%q: store/include %+v", level, o)
		}
	}
	if _, err := OpenAIResponsesOptions("turbo"); err == nil {
		t.Fatal("bad level accepted")
	}
}
