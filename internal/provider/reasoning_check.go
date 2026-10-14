package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/config"
	oai "github.com/openai/openai-go/v3"
)

// ReasoningCheckMaxRequests includes the baseline, all supported payload
// formats, their invalid-value controls, and every selectable value.
const ReasoningCheckMaxRequests = 35

// CheckedReasoning records request validation, not whether the model actually
// uses the controls. Empty Levels means the check was inconclusive.
type CheckedReasoning struct {
	Protocol config.Thinking
	Levels   []string
}

// CheckReasoning is an opt-in, tool-free check for one Chat Completions model.
// It never sends conversation data or edits configuration. The current format
// is tried first, then other formats Arkex can send. Effort controls take
// precedence over a toggle-only result. No HTTP failure triggers a retry.
func CheckReasoning(ctx context.Context, ref config.ModelRef, client *http.Client) (CheckedReasoning, error) {
	if ref.Conn.Kind == config.KindSubscription || ref.Conn.Kind == config.KindRuntime ||
		(ref.Conn.API != "" && ref.Conn.API != config.APIOpenAICompat) {
		return CheckedReasoning{}, errors.New("reasoning checks require an OpenAI-compatible Chat Completions connection")
	}
	compat := ref.Compat()
	// These overrides would test a different model, prompt, cap or control
	// than the one shown in the picker. Never silently remove manual settings.
	for _, key := range []string{"model", "messages", "tools", "tool_choice", "stream", "n", "max_tokens", "max_completion_tokens", "reasoning_effort", "thinking", "reasoning", "enable_thinking", "chat_template_kwargs"} {
		if _, ok := compat.ExtraBody[key]; ok {
			return CheckedReasoning{}, fmt.Errorf("compat.extraBody overrides %s; configure reasoning manually or remove the override before checking", key)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	checkClient := http.Client{}
	if client != nil {
		checkClient = *client
	}
	checkClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("reasoning checks do not follow redirects")
	}
	ref.Thinking = ""
	mdl, err := Open(ctx, ref, &checkClient)
	if err != nil {
		return CheckedReasoning{}, err
	}
	// A tiny baseline distinguishes a broken connection from rejected controls.
	baseline, err := reasoningCheckRequest(ctx, mdl, compat, "", "")
	if err != nil {
		return CheckedReasoning{}, err
	}
	if !baseline.accepted {
		return CheckedReasoning{}, errors.New("baseline request did not complete; settings unchanged")
	}
	protocols := []config.Thinking{config.ThinkingReasoningEffort, config.ThinkingDeepSeek, config.ThinkingOpenRouter, config.ThinkingQwen, config.ThinkingQwenChatTemplate}
	if i := slices.Index(protocols, compat.Thinking); i > 0 {
		protocols = append([]config.Thinking{compat.Thinking}, slices.Delete(protocols, i, i+1)...)
	}
	var fallback CheckedReasoning
	for _, protocol := range protocols {
		compat.Thinking = protocol
		var levels []string
		for _, group := range reasoningCheckGroups(protocol) {
			control, err := reasoningCheckRequest(ctx, mdl, compat, group.controlLevel, group.field)
			if err != nil {
				return CheckedReasoning{}, err
			}
			if !control.rejected {
				// Accepted invalid input is inconclusive, not proof of ignoring
				// the field. An unrelated validation error proves nothing either.
				continue
			}
			var accepted []string
			for _, level := range group.levels {
				result, err := reasoningCheckRequest(ctx, mdl, compat, level, "")
				if err != nil {
					return CheckedReasoning{}, err
				}
				if result.accepted {
					accepted = append(accepted, level)
				} else if !result.rejected {
					// Do not publish a partial list after an unexplained failure.
					accepted = nil
					break
				}
			}
			levels = append(levels, accepted...)
		}
		ordered := make([]string, 0, len(levels))
		for _, level := range ThinkingLevels {
			if slices.Contains(levels, level) {
				ordered = append(ordered, level)
			}
		}
		result := CheckedReasoning{Protocol: protocol, Levels: ordered}
		if slices.ContainsFunc(ordered, func(s string) bool { return s != "off" && s != "on" }) {
			return result, nil
		}
		if len(fallback.Levels) == 0 && len(ordered) > 0 {
			fallback = result
		}
	}
	return fallback, nil
}

type reasoningCheckGroup struct {
	field, controlLevel string
	levels              []string
}

func reasoningCheckGroups(protocol config.Thinking) []reasoningCheckGroup {
	efforts := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	switch protocol {
	case config.ThinkingReasoningEffort:
		return []reasoningCheckGroup{{"reasoning_effort", "low", append([]string{"off"}, efforts...)}}
	case config.ThinkingDeepSeek:
		return []reasoningCheckGroup{{"reasoning_effort", "low", efforts}, {"thinking.type", "off", []string{"off", "on"}}}
	case config.ThinkingOpenRouter:
		return []reasoningCheckGroup{{"reasoning.effort", "low", efforts}, {"reasoning.enabled", "off", []string{"off", "on"}}}
	case config.ThinkingQwen:
		return []reasoningCheckGroup{{"enable_thinking", "off", []string{"off", "on"}}}
	case config.ThinkingQwenChatTemplate:
		return []reasoningCheckGroup{{"chat_template_kwargs.enable_thinking", "off", []string{"off", "on"}}}
	}
	return nil
}

type reasoningCheckResponse struct{ accepted, rejected bool }

func reasoningCheckRequest(ctx context.Context, mdl *Model, compat config.Compat, level, invalidField string) (reasoningCheckResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return reasoningCheckResponse{}, err
	}
	po, err := OpenAICompatOptions(compat, level)
	if err != nil {
		return reasoningCheckResponse{}, err
	}
	if invalidField != "" {
		if po.ExtraBody == nil {
			po.ExtraBody = map[string]any{}
		}
		// Use raw ExtraBody so the invalid value reaches the server rather
		// than being rejected by the SDK's local enum validation.
		root, child, nested := strings.Cut(invalidField, ".")
		if nested {
			po.ExtraBody[root].(map[string]any)[child] = "__arkex_invalid__"
		} else {
			po.ExtraBody[root] = "__arkex_invalid__"
		}
	}
	response, err := mdl.LM.Generate(ctx, fantasy.Call{
		Prompt:          fantasy.Prompt{fantasy.NewUserMessage("Reply with the single word OK.")},
		MaxOutputTokens: new(int64(64)),
		ProviderOptions: fantasy.ProviderOptions{mdl.LM.Provider(): po},
	})
	if err != nil {
		if ctx.Err() != nil {
			return reasoningCheckResponse{}, ctx.Err()
		}
		var pe *fantasy.ProviderError
		if errors.As(err, &pe) {
			if pe.StatusCode == http.StatusBadRequest || pe.StatusCode == http.StatusUnprocessableEntity {
				// Only a validation error naming a control is evidence of
				// rejection. Do not print error dumps or echoed credentials.
				fields := reasoningCheckGroups(compat.Thinking)
				for _, group := range fields {
					if invalidField != "" && group.field != invalidField {
						continue
					}
					root, leaf, nested := strings.Cut(group.field, ".")
					var apiErr *oai.Error
					if errors.As(err, &apiErr) && (apiErr.Param == root || apiErr.Param == group.field) {
						return reasoningCheckResponse{rejected: true}, nil
					}
					message := strings.ToLower(pe.Message)
					if strings.Contains(message, group.field) || (nested && strings.Contains(message, root) && strings.Contains(message, leaf)) {
						return reasoningCheckResponse{rejected: true}, nil
					}
				}
				return reasoningCheckResponse{}, nil
			}
			return reasoningCheckResponse{}, fmt.Errorf("reasoning check stopped: HTTP %d; settings unchanged", pe.StatusCode)
		}
		return reasoningCheckResponse{}, errors.New("reasoning check request failed or returned an invalid completion; settings unchanged")
	}
	if response == nil || (response.FinishReason != fantasy.FinishReasonStop && response.FinishReason != fantasy.FinishReasonLength) {
		return reasoningCheckResponse{}, errors.New("reasoning check received an incomplete or abnormal completion; settings unchanged")
	}
	// Length is expected with a small cap, especially for reasoning models.
	// This establishes only request acceptance, not a completed answer or
	// successful reasoning. No output/timing comparison is used as evidence.
	return reasoningCheckResponse{accepted: true}, nil
}
