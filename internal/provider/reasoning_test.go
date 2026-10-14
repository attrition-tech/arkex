package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/attrition-tech/arkex/internal/config"
)

func TestReasoningControls(t *testing.T) {
	ref := config.ModelRef{Conn: config.Connection{BaseURL: "https://custom.example/v1"}, Model: config.Model{ID: "custom-model"}}
	if got, _, _ := ReasoningControls(ref); len(got) != 0 {
		t.Fatalf("unknown model advertised controls: %v", got)
	}
	ref.Model.ReasoningLevels, ref.Model.ReasoningDefault = []string{"low", "high"}, "low"
	got, def, _ := ReasoningControls(ref)
	if !reflect.DeepEqual(got, []string{"low", "high"}) || def != "low" {
		t.Fatal("configured capabilities were ignored")
	}
	ref.Model.ReasoningChecked = true
	if _, _, source := ReasoningControls(ref); !strings.Contains(source, "accepted by server") {
		t.Fatal("request acceptance was presented as catalog discovery")
	}
}

func TestSetThinkingResponses(t *testing.T) {
	p, err := openai.New(openai.WithAPIKey("test"), openai.WithUseResponsesAPI())
	if err != nil {
		t.Fatal(err)
	}
	lm, err := p.LanguageModel(t.Context(), "subscription-model")
	if err != nil {
		t.Fatal(err)
	}
	m := &Model{LM: lm, Ref: config.ModelRef{Conn: config.Connection{Kind: config.KindSubscription, Subscription: "chatgpt"}, Model: config.Model{ID: "subscription-model", ReasoningLevels: []string{"low", "high", "xhigh"}}}}
	if err := m.SetThinking("xhigh"); err != nil {
		t.Fatal(err)
	}
	po := m.Opts[lm.Provider()].(*openai.ResponsesProviderOptions)
	if po.ReasoningEffort == nil || *po.ReasoningEffort != "xhigh" || po.ReasoningSummary == nil || *po.ReasoningSummary != "auto" || po.Store == nil || *po.Store {
		t.Fatal("wrong subscription payload")
	}
	if err := m.SetThinking("off"); err == nil || m.Ref.Thinking != "xhigh" {
		t.Fatal("unsupported off changed current selection")
	}
	if err := m.SetThinking(""); err != nil {
		t.Fatal(err)
	}
	if m.Opts[lm.Provider()].(*openai.ResponsesProviderOptions).ReasoningEffort != nil {
		t.Fatal("Default did not remove effort")
	}
	m.Ref.Model.ReasoningLevels = []string{"off", "low", "high"}
	if err := m.SetThinking("off"); err != nil {
		t.Fatal(err)
	}
	if *m.Opts[lm.Provider()].(*openai.ResponsesProviderOptions).ReasoningEffort != "none" {
		t.Fatal("off must become none")
	}
}

func TestSetThinkingToggle(t *testing.T) {
	for _, preset := range []config.Thinking{config.ThinkingDeepSeek, config.ThinkingQwen, config.ThinkingQwenChatTemplate} {
		ref := config.ModelRef{ConnID: "local", Conn: config.Connection{API: config.APIOpenAICompat, BaseURL: "http://localhost:1/v1", Compat: config.Compat{Thinking: preset}}, Model: config.Model{ID: "custom"}}
		m, err := Open(t.Context(), ref, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, level := range []string{"on", "off", ""} {
			if err := m.SetThinking(level); err != nil {
				t.Fatal(err)
			}
			po := m.Opts[m.LM.Provider()].(*openaicompat.ProviderOptions)
			if po.ReasoningEffort != nil {
				t.Fatalf("toggle sent invalid numeric effort: %v", po.ReasoningEffort)
			}
			if level == "" {
				if len(po.ExtraBody) != 0 {
					t.Fatal("default kept toggle")
				}
				continue
			}
			enabled := level == "on"
			switch preset {
			case config.ThinkingDeepSeek:
				want := "disabled"
				if enabled {
					want = "enabled"
				}
				if po.ExtraBody["thinking"].(map[string]any)["type"] != want {
					t.Fatal("wrong DeepSeek toggle")
				}
			case config.ThinkingQwen:
				if po.ExtraBody["enable_thinking"] != enabled {
					t.Fatal("wrong Qwen toggle")
				}
			case config.ThinkingQwenChatTemplate:
				if po.ExtraBody["chat_template_kwargs"].(map[string]any)["enable_thinking"] != enabled {
					t.Fatal("wrong template toggle")
				}
			}
		}
		if err := m.SetThinking("high"); err == nil {
			t.Fatal("toggle-only model accepted effort")
		}
	}
}

func checkReply(w http.ResponseWriter, status int, param string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status >= 400 {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "Invalid " + param, "param": param, "type": "invalid_request_error"}})
	} else {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	}
}

func TestReasoningCheckAcceptedFormats(t *testing.T) {
	for _, tc := range []struct {
		protocol config.Thinking
		levels   []string
	}{
		{config.ThinkingReasoningEffort, []string{"off", "low", "max"}},
		{config.ThinkingDeepSeek, []string{"off", "on", "low", "high", "max"}},
		{config.ThinkingOpenRouter, []string{"high"}},
		{config.ThinkingQwen, []string{"off", "on"}},
		{config.ThinkingQwenChatTemplate, []string{"on"}},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("X-Test") != "test-header" {
					t.Error("check did not use the configured endpoint and credentials")
				}
				if body["model"] != "custom:model" || body["max_tokens"] != float64(64) || body["temperature"] != float64(.2) || body["stream"] == true || body["tools"] != nil {
					t.Errorf("unbounded or unexpected request: %v", body)
				}
				msgs, _ := body["messages"].([]any)
				if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "Reply with the single word OK." {
					t.Error("check sent more than its synthetic prompt")
				}
				// Independent strict gateway: no model-name guesses and no
				// calls to Arkex's request formatter to derive expectations.
				field := map[config.Thinking]string{
					config.ThinkingReasoningEffort: "reasoning_effort", config.ThinkingDeepSeek: "thinking",
					config.ThinkingOpenRouter: "reasoning", config.ThinkingQwen: "enable_thinking",
					config.ThinkingQwenChatTemplate: "chat_template_kwargs",
				}[tc.protocol]
				for _, key := range []string{"reasoning_effort", "thinking", "reasoning", "enable_thinking", "chat_template_kwargs"} {
					if _, exists := body[key]; exists && key != field && (tc.protocol != config.ThinkingDeepSeek || key != "reasoning_effort") {
						checkReply(w, 422, key)
						return
					}
				}
				value, exists := body[field]
				if !exists { // baseline
					checkReply(w, 200, "")
					return
				}
				accepted, param := false, field
				switch tc.protocol {
				case config.ThinkingReasoningEffort:
					accepted = value == "none" || value == "low" || value == "max"
				case config.ThinkingDeepSeek:
					toggle, _ := value.(map[string]any)
					param = "thinking.type"
					accepted = toggle["type"] == "disabled"
					if toggle["type"] == "enabled" {
						effort := body["reasoning_effort"]
						accepted = effort == nil || effort == "low" || effort == "high" || effort == "max"
						param = "reasoning_effort"
					}
				case config.ThinkingOpenRouter:
					reasoning, _ := value.(map[string]any)
					accepted, param = reasoning["effort"] == "high", "reasoning.effort"
				case config.ThinkingQwen:
					_, accepted = value.(bool)
				case config.ThinkingQwenChatTemplate:
					kwargs, _ := value.(map[string]any)
					accepted, param = kwargs["enable_thinking"] == true && kwargs["preserve_thinking"] == true, "chat_template_kwargs.enable_thinking"
				}
				if !accepted {
					checkReply(w, 400, param)
					return
				}
				checkReply(w, 200, "")
			}))
			defer server.Close()
			t.Setenv("ARKEX_CHECK_KEY", "test-key")
			ref := config.ModelRef{Conn: config.Connection{API: config.APIOpenAICompat, BaseURL: server.URL + "/v1", APIKey: "$ARKEX_CHECK_KEY", Headers: map[string]string{"X-Test": "test-header"}, Compat: config.Compat{ExtraBody: map[string]any{"temperature": .2}}}, Model: config.Model{ID: "custom:model"}, Thinking: "high"}
			if tc.protocol == config.ThinkingDeepSeek {
				ref.Model.Compat.Thinking = config.ThinkingDeepSeek // preserve the configured enable/disable format
			}
			got, err := CheckReasoning(t.Context(), ref, nil)
			if err != nil || got.Protocol != tc.protocol || !slices.Equal(got.Levels, tc.levels) {
				t.Fatalf("check = %+v, %v; want %s %v", got, err, tc.protocol, tc.levels)
			}
			if n := requests.Load(); n > ReasoningCheckMaxRequests || n < 4 {
				t.Fatalf("request budget violated: %d", n)
			}
			if ref.Thinking != "high" || len(ref.Model.ReasoningLevels) != 0 || len(ref.Compat().ExtraBody) != 1 {
				t.Fatal("check mutated settings")
			}
		})
	}
}

func TestReasoningCheckInconclusiveAndBudget(t *testing.T) {
	for _, mode := range []string{"permissive", "reject-all", "unrelated-validation", "unrelated-type"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if mode == "unrelated-type" {
					// A generic type error must not confirm thinking.type.
					// Valid controls succeed; only sentinel values trigger it.
					b, _ := json.Marshal(body)
					if strings.Contains(string(b), "__arkex_invalid__") {
						checkReply(w, 422, "request type")
						return
					}
				} else if mode != "permissive" && n > 1 {
					param := "temperature"
					if mode == "reject-all" {
						for _, key := range []string{"reasoning_effort", "thinking", "reasoning", "enable_thinking", "chat_template_kwargs"} {
							if body[key] != nil {
								param = key
								break
							}
						}
					}
					checkReply(w, 422, param)
					return
				}
				checkReply(w, 200, "")
			}))
			defer server.Close()
			got, err := CheckReasoning(t.Context(), config.ModelRef{Conn: config.Connection{BaseURL: server.URL + "/v1"}, Model: config.Model{ID: "m"}}, nil)
			if err != nil || len(got.Levels) != 0 {
				t.Fatalf("unverified controls advertised: %+v, %v", got, err)
			}
			if requests.Load() > ReasoningCheckMaxRequests || (mode == "reject-all" && requests.Load() != ReasoningCheckMaxRequests) {
				t.Fatalf("maximum request count = %d, want <= %d", requests.Load(), ReasoningCheckMaxRequests)
			}
		})
	}
}

func TestReasoningCheckFailureDiscardsPartialResults(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				if n == 2 {
					checkReply(w, 400, "reasoning_effort") // invalid-value control
				} else if n < 4 {
					checkReply(w, 200, "") // baseline, then Off accepted
				} else {
					checkReply(w, status, "SECRET_ECHO_MUST_NOT_BE_SHOWN")
				}
			}))
			defer server.Close()
			got, err := CheckReasoning(t.Context(), config.ModelRef{Conn: config.Connection{BaseURL: server.URL + "/v1"}, Model: config.Model{ID: "m"}}, nil)
			if err == nil || len(got.Levels) != 0 || requests.Load() != 4 || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("failure leaked a partial result, retried, or exposed body: %+v, %v; requests %d", got, err, requests.Load())
			}
		})
	}
	for _, cancelEarly := range []bool{true, false} {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			<-r.Context().Done()
		}))
		if cancelEarly {
			cancel()
		}
		got, err := CheckReasoning(ctx, config.ModelRef{Conn: config.Connection{BaseURL: server.URL + "/v1"}, Model: config.Model{ID: "m"}}, nil)
		cancel()
		server.Close()
		if len(got.Levels) != 0 || (!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) || requests.Load() > 1 {
			t.Fatalf("cancellation/deadline escaped: %+v, %v", got, err)
		}
	}
}
