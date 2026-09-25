package provider

import (
	"fmt"
	"slices"

	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/config"
)

// ReasoningControls prefers persisted discovery/manual metadata, then known
// thinking protocols.
// Unknown models must not inherit another model's effort or off capability.
func ReasoningControls(ref config.ModelRef) (levels []string, defaultLevel, source string) {
	if len(ref.Model.ReasoningLevels) > 0 {
		for _, level := range ref.Model.ReasoningLevels {
			if level == "on" && (ref.Conn.Kind == config.KindSubscription || ref.Compat().Thinking == config.ThinkingReasoningEffort) {
				continue
			}
			if level != "" && validLevel(level) && !slices.Contains(levels, level) {
				levels = append(levels, level)
			}
		}
		return levels, ref.Model.ReasoningDefault, "model configuration / discovery"
	}
	switch ref.Compat().Thinking {
	case config.ThinkingQwen, config.ThinkingQwenChatTemplate, config.ThinkingDeepSeek:
		return []string{"off", "on"}, "", "configured thinking protocol"
	}
	return nil, "", "capabilities not advertised"
}

// SetThinking changes only the next request's options, never the connection,
// conversation, global default, or an in-flight call. The caller must be idle.
func (m *Model) SetThinking(level string) error {
	levels, _, _ := ReasoningControls(m.Ref)
	if level != "" && !slices.Contains(levels, level) {
		return fmt.Errorf("reasoning %q is not advertised for %s", level, m.Ref.String())
	}
	var opts fantasy.ProviderOptions
	if m.Ref.Conn.Kind == config.KindSubscription {
		if level == "on" {
			return fmt.Errorf("select an explicit effort level for this subscription")
		}
		po, err := OpenAIResponsesOptions(level)
		if err != nil {
			return err
		}
		opts = fantasy.ProviderOptions{m.LM.Provider(): po}
	} else {
		compat := m.Ref.Compat()
		if level != "" && compat.Thinking == "" {
			return fmt.Errorf("configure compat.thinking before selecting reasoning on this connection")
		}
		po, err := OpenAICompatOptions(compat, level)
		if err != nil {
			return err
		}
		opts = fantasy.ProviderOptions{m.LM.Provider(): po}
	}
	m.Opts, m.Ref.Thinking = opts, level
	return nil
}
