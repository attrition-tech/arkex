package provider

import (
	"reflect"
	"testing"

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
