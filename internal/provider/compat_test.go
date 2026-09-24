package provider

import (
	"reflect"
	"testing"

	"charm.land/fantasy/providers/openai"

	"github.com/dantearo/arkex/internal/config"
)

func TestOpenAICompatOptions(t *testing.T) {
	eff := func(s string) *openai.ReasoningEffort { e := openai.ReasoningEffort(s); return &e }

	tests := []struct {
		name      string
		compat    config.Compat
		level     string
		wantEff   *openai.ReasoningEffort
		wantExtra map[string]any
	}{
		{name: "no preset, no level", compat: config.Compat{}, level: ""},
		{name: "no preset with level sends nothing", compat: config.Compat{}, level: "high"},
		{
			name: "reasoning_effort high", compat: config.Compat{Thinking: config.ThinkingReasoningEffort}, level: "high",
			wantEff: eff("high"),
		},
		{
			name: "reasoning_effort off sends none", compat: config.Compat{Thinking: config.ThinkingReasoningEffort}, level: "off",
			wantEff: eff("none"),
		},
		{
			name: "deepseek enabled", compat: config.Compat{Thinking: config.ThinkingDeepSeek}, level: "medium",
			wantEff:   eff("medium"),
			wantExtra: map[string]any{"thinking": map[string]any{"type": "enabled"}},
		},
		{
			name: "deepseek off", compat: config.Compat{Thinking: config.ThinkingDeepSeek}, level: "off",
			wantExtra: map[string]any{"thinking": map[string]any{"type": "disabled"}},
		},
		{
			name: "qwen on", compat: config.Compat{Thinking: config.ThinkingQwen}, level: "low",
			wantExtra: map[string]any{"enable_thinking": true},
		},
		{
			name: "qwen off", compat: config.Compat{Thinking: config.ThinkingQwen}, level: "off",
			wantExtra: map[string]any{"enable_thinking": false},
		},
		{
			name: "qwen chat template on", compat: config.Compat{Thinking: config.ThinkingQwenChatTemplate}, level: "high",
			wantExtra: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true, "preserve_thinking": true}},
		},
		{
			name: "qwen chat template off", compat: config.Compat{Thinking: config.ThinkingQwenChatTemplate}, level: "off",
			wantExtra: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}},
		},
		{
			name: "openrouter on", compat: config.Compat{Thinking: config.ThinkingOpenRouter}, level: "xhigh",
			wantExtra: map[string]any{"reasoning": map[string]any{"effort": "xhigh"}},
		},
		{
			name: "openrouter off", compat: config.Compat{Thinking: config.ThinkingOpenRouter}, level: "off",
			wantExtra: map[string]any{"reasoning": map[string]any{"enabled": false}},
		},
		{
			name: "user extraBody wins over preset",
			compat: config.Compat{
				Thinking:  config.ThinkingQwen,
				ExtraBody: map[string]any{"enable_thinking": "custom", "top_k": 20},
			},
			level:     "medium",
			wantExtra: map[string]any{"enable_thinking": "custom", "top_k": 20},
		},
		{
			name:      "extraBody without level",
			compat:    config.Compat{ExtraBody: map[string]any{"top_k": 20}},
			level:     "",
			wantExtra: map[string]any{"top_k": 20},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := OpenAICompatOptions(tt.compat, tt.level)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.ReasoningEffort, tt.wantEff) {
				t.Errorf("ReasoningEffort = %v, want %v", deref(got.ReasoningEffort), deref(tt.wantEff))
			}
			if !reflect.DeepEqual(got.ExtraBody, tt.wantExtra) {
				t.Errorf("ExtraBody = %#v, want %#v", got.ExtraBody, tt.wantExtra)
			}
		})
	}
}

func TestOpenAICompatOptionsRejectsBadInput(t *testing.T) {
	if _, err := OpenAICompatOptions(config.Compat{}, "ultra"); err == nil {
		t.Error("expected error for unknown level")
	}
	if _, err := OpenAICompatOptions(config.Compat{Thinking: "bogus"}, "low"); err == nil {
		t.Error("expected error for unknown preset")
	}
}

func deref(e *openai.ReasoningEffort) string {
	if e == nil {
		return "<nil>"
	}
	return string(*e)
}
