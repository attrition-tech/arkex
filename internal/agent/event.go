// Package agent runs the model ↔ tool loop. It has no UI dependencies; every
// consumer (TUI, print mode, JSON mode) observes the same Event stream.
package agent

import (
	"time"

	"charm.land/fantasy"
)

// Event is something the UI may want to show. Exactly one concrete type is
// sent per value. JSON tags define the stable --json wire format.
type Event interface{ isEvent() }

// TurnStart marks the beginning of a model request within a run.
type TurnStart struct {
	Step int `json:"step"`
}

// TextDelta is a chunk of assistant text.
type TextDelta struct {
	Text string `json:"text"`
}

// ReasoningDelta is a chunk of model reasoning/thinking text.
type ReasoningDelta struct {
	Text string `json:"text"`
	ID   string `json:"id,omitempty"`
}

// ToolCallStart fires when the model begins emitting a tool call.
type ToolCallStart struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ToolCallInputDelta streams partial JSON arguments.
type ToolCallInputDelta struct {
	ID    string `json:"id"`
	Delta string `json:"delta"`
}

// ToolCall is a complete tool call the agent is about to consider.
type ToolCall struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input string `json:"input"`
	// Reason is set by a policy when it hands the call to an Asker: why
	// this call needs the user, e.g. "writes outside the workspace: ~/x".
	Reason string `json:"reason,omitempty"`
	// Grantable is set by a policy that will honour Answer AllowSession for
	// this call, so the prompt can offer "allow for this session".
	Grantable bool `json:"grantable,omitempty"`
	// TrustDirectory scopes AllowSession to this directory, never a whole tool.
	TrustDirectory string `json:"trust_directory,omitempty"`
	TrustAccess    string `json:"trust_access,omitempty"`
	Workdir        string `json:"workdir,omitempty"`
}

// ToolDecision records the policy result for a call.
type ToolDecision struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

// ToolResult is the outcome of executing a tool.
type ToolResult struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Output   string        `json:"output"`
	Summary  string        `json:"summary,omitempty"`
	Detail   string        `json:"detail,omitempty"`
	IsError  bool          `json:"is_error"`
	Duration time.Duration `json:"duration_ns"`
}

// TurnEnd fires when a model request completes.
type TurnEnd struct {
	Step         int                  `json:"step"`
	Usage        fantasy.Usage        `json:"usage"`
	FinishReason fantasy.FinishReason `json:"finish_reason"`
}

// Compacting brackets automatic compaction, including failed/cancelled attempts.
type Compacting struct {
	Active bool `json:"active"`
}

// Compacted fires when the agent summarised the conversation on its own:
// before a request that would have run past compactAt of the context
// window, or to recover from a request the provider rejected as too long.
type Compacted struct {
	Summary string        `json:"summary"`
	Dropped int           `json:"dropped"` // messages the summary replaced
	Trimmed int           `json:"trimmed"` // messages removed unread to make the summary request fit
	Reason  string        `json:"reason"`
	Usage   fantasy.Usage `json:"usage"`
}

// CompactFailed fires when a pre-request compaction did not go through;
// the run continues, since the request may still fit.
type CompactFailed struct {
	Err error `json:"-"`
}

// ContextWindowLearned fires when an overflow error stated the model's
// context window and the agent adopted it for the rest of the process.
// Consumers that keep config may persist it.
type ContextWindowLearned struct {
	Tokens int `json:"tokens"`
}

// RunEnd fires once when the whole run (all steps) completes.
type RunEnd struct {
	Steps int           `json:"steps"`
	Usage fantasy.Usage `json:"usage"`
	Err   error         `json:"-"`
}

func (TurnStart) isEvent()            {}
func (TextDelta) isEvent()            {}
func (ReasoningDelta) isEvent()       {}
func (ToolCallStart) isEvent()        {}
func (ToolCallInputDelta) isEvent()   {}
func (ToolCall) isEvent()             {}
func (ToolDecision) isEvent()         {}
func (ToolResult) isEvent()           {}
func (TurnEnd) isEvent()              {}
func (Compacted) isEvent()            {}
func (Compacting) isEvent()           {}
func (CompactFailed) isEvent()        {}
func (ContextWindowLearned) isEvent() {}
func (RunEnd) isEvent()               {}
