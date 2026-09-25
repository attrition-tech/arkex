// Package provider turns arkex config into fantasy language models and
// translates arkex compat settings into provider-specific request options.
package provider

import (
	"fmt"

	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/attrition-tech/arkex/internal/config"
)

// ThinkingLevels lists the accepted thinking levels, lowest to highest.
var ThinkingLevels = []string{"off", "on", "minimal", "low", "medium", "high", "xhigh", "max"}

func validLevel(level string) bool {
	if level == "" {
		return true
	}
	for _, l := range ThinkingLevels {
		if l == level {
			return true
		}
	}
	return false
}

// OpenAICompatOptions builds the provider options for an openai-compat
// request from the effective compat settings and thinking level.
//
// The returned map is the exact extra_body that will be merged into the
// request root, plus an optional reasoning_effort. User-supplied
// compat.extraBody is applied last and wins over preset fields.
func OpenAICompatOptions(c config.Compat, level string) (*openaicompat.ProviderOptions, error) {
	if !validLevel(level) {
		return nil, fmt.Errorf("unknown thinking level %q (want one of %v)", level, ThinkingLevels)
	}
	opts := &openaicompat.ProviderOptions{}
	extra := map[string]any{}
	enabled := level != "" && level != "off"

	if level != "" {
		switch c.Thinking {
		case config.ThinkingNone:
			// no thinking controls sent
		case config.ThinkingReasoningEffort:
			if level == "on" {
				return nil, fmt.Errorf("reasoning_effort requires an explicit effort level")
			}
			if enabled {
				opts.ReasoningEffort = effort(level)
			} else {
				opts.ReasoningEffort = new(openai.ReasoningEffortNone)
			}
		case config.ThinkingDeepSeek:
			if enabled {
				extra["thinking"] = map[string]any{"type": "enabled"}
				if level != "on" {
					opts.ReasoningEffort = effort(level)
				}
			} else {
				extra["thinking"] = map[string]any{"type": "disabled"}
			}
		case config.ThinkingQwen:
			extra["enable_thinking"] = enabled
		case config.ThinkingQwenChatTemplate:
			kw := map[string]any{"enable_thinking": enabled}
			if enabled {
				kw["preserve_thinking"] = true
			}
			extra["chat_template_kwargs"] = kw
		case config.ThinkingOpenRouter:
			if level == "on" {
				extra["reasoning"] = map[string]any{"enabled": true}
			} else if enabled {
				extra["reasoning"] = map[string]any{"effort": level}
			} else {
				extra["reasoning"] = map[string]any{"enabled": false}
			}
		default:
			return nil, fmt.Errorf("unknown thinking preset %q", c.Thinking)
		}
	}

	for k, v := range c.ExtraBody {
		extra[k] = v
	}
	if len(extra) > 0 {
		opts.ExtraBody = extra
	}
	return opts, nil
}

func effort(level string) *openai.ReasoningEffort {
	e := openai.ReasoningEffort(level)
	return &e
}
