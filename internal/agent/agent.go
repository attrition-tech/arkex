package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/attrition-tech/arkex/internal/provider"
	"github.com/attrition-tech/arkex/internal/tools"
)

// Agent owns one conversation and drives the model/tool loop.
type Agent struct {
	Model  *provider.Model
	Tools  *tools.Registry
	Policy Policy
	System string
	// Retry asks before restarting partial output or after automatic retries.
	// Nil returns the error (noninteractive consumers never wait for input).
	Retry func(context.Context, error, bool) bool
	// MaxSteps, when > 0, caps model↔tool round trips; connection probes
	// use 1. Zero (the default) means unbounded: a run ends when the model
	// stops calling tools, ctx is cancelled, or the model looks stuck (see
	// PausedError and stuck).
	MaxSteps int

	messages []fantasy.Message
	// lastInput is the input token count the most recent request reported;
	// see compact.go for how it drives compaction.
	lastInput int64
	lastUsage fantasy.Usage
}

// LastUsage is the usage reported by the most recent run, including failed runs.
// Like Messages, it must only be read after the run has returned.
func (a *Agent) LastUsage() fantasy.Usage { return a.lastUsage }

// Messages returns the conversation so far (excluding the system prompt).
func (a *Agent) Messages() []fantasy.Message { return a.messages }

// SetMessages replaces the conversation, e.g. when resuming a session.
func (a *Agent) SetMessages(m []fantasy.Message) { a.messages = m }

// PausedError is returned when a run was stopped while the model still
// wanted to call tools: it repeated itself (Reason says how) or hit an
// explicit MaxSteps. The conversation is intact: the last tool results are
// recorded, so Continue can pick up where it stopped.
type PausedError struct {
	Steps  int
	Reason string
}

func (e *PausedError) Error() string {
	return fmt.Sprintf("paused after %d tool steps: %s", e.Steps, e.Reason)
}

// Run appends a user message and loops until the model stops calling tools,
// looks stuck, or ctx is cancelled. Events are delivered synchronously to
// emit from the calling goroutine.
func (a *Agent) Run(ctx context.Context, userText string, emit func(Event)) error {
	return a.RunMessage(ctx, fantasy.NewUserMessage(userText), emit)
}

// RunMessage is Run for a prepared user message, e.g. text plus image parts
// (fantasy.NewUserMessage(text, files...)).
func (a *Agent) RunMessage(ctx context.Context, msg fantasy.Message, emit func(Event)) error {
	a.lastUsage = fantasy.Usage{}
	if a.Policy == nil {
		return errors.New("agent: Policy is required")
	}
	a.messages = append(a.messages, msg)
	// keep=1: a compaction before the first request must preserve the
	// prompt the user just typed, verbatim.
	return a.loop(ctx, emit, 1)
}

// Continue resumes the model/tool loop without a new user message, after a
// Run ended with a PausedError. Every tool call already has its result,
// so the model simply gets asked for its next step.
func (a *Agent) Continue(ctx context.Context, emit func(Event)) error {
	a.lastUsage = fantasy.Usage{}
	if a.Policy == nil {
		return errors.New("agent: Policy is required")
	}
	if len(a.messages) == 0 {
		return errors.New("nothing to continue")
	}
	return a.loop(ctx, emit, 0)
}

// loop drives model requests and tool execution until the model stops
// calling tools. keepOnFirst is how many trailing messages a compaction
// before the first request must keep verbatim (the fresh user prompt).
func (a *Agent) loop(ctx context.Context, emit func(Event), keepOnFirst int) error {
	if emit == nil {
		emit = func(Event) {}
	}
	var total fantasy.Usage
	var runErr error
	var recent []stepSig // one signature per tool step, for stuck
	steps := 0
	pause := ""
	recovered := false // an overflow was already compacted away for this step
	for pause == "" {
		steps++
		keep := 0
		if steps == 1 {
			keep = keepOnFirst
		}
		if a.needsCompact() {
			w := a.ContextWindow()
			reason := fmt.Sprintf("the last request used %d%% of the %s-token context window", a.lastInput*100/int64(w), formatTokens(w))
			if _, err := a.autoCompact(ctx, emit, keep, reason); err != nil {
				// Not fatal: the request may still fit. If it does not, the
				// overflow path below gets another go.
				emit(CompactFailed{Err: err})
			}
		}
		emit(TurnStart{Step: steps})

		turn, err := a.streamWithRetry(ctx, emit)
		total = addUsage(total, turn.usage)
		if err != nil {
			if IsContextOverflow(err) && !recovered && len(a.messages) > keep {
				recovered = true
				if w := ContextWindowFromError(err); w > 0 && w != a.ContextWindow() {
					a.SetContextWindow(w)
					emit(ContextWindowLearned{Tokens: w})
				}
				if _, cerr := a.autoCompact(ctx, emit, keep, "the request exceeded the model's context window"); cerr == nil {
					steps--
					continue
				} else {
					err = fmt.Errorf("%w (compacting to recover failed: %v)", err, cerr)
				}
			}
			runErr = err
			break
		}
		recovered = false
		if input := ContextInput(turn.usage); input > 0 {
			a.lastInput = input
		}
		if len(turn.parts) > 0 {
			a.messages = append(a.messages, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: turn.parts})
		}
		emit(TurnEnd{Step: steps, Usage: turn.usage, FinishReason: turn.finish})

		if len(turn.calls) == 0 {
			break
		}
		results, err := a.execute(ctx, turn.calls, emit)
		if len(results) > 0 {
			a.messages = append(a.messages, fantasy.Message{Role: fantasy.MessageRoleTool, Content: results})
		}
		if err != nil {
			runErr = err
			break
		}
		recent = append(recent, stepSignature(turn.calls, results))
		if a.MaxSteps > 0 && steps >= a.MaxSteps {
			pause = "the step limit was reached"
		} else if repeats, name := stuck(recent); repeats > 0 {
			pause = fmt.Sprintf("the model called %s %d times with the same arguments and got the same result each time", name, repeats)
		}
	}
	if runErr == nil && pause != "" {
		runErr = &PausedError{Steps: steps, Reason: pause}
	}
	a.lastUsage = total
	emit(RunEnd{Steps: steps, Usage: total, Err: runErr})
	return runErr
}

type turnResult struct {
	parts  []fantasy.MessagePart
	calls  []fantasy.ToolCallPart
	usage  fantasy.Usage
	finish fantasy.FinishReason
}

func (a *Agent) prompt() fantasy.Prompt {
	p := make(fantasy.Prompt, 0, len(a.messages)+1)
	if a.System != "" {
		p = append(p, fantasy.NewSystemMessage(a.System))
	}
	return append(p, a.messages...)
}

// stream performs one model request, emitting deltas and collecting the
// assistant message parts in arrival order.
func (a *Agent) stream(ctx context.Context, emit func(Event)) (turnResult, error) {
	start := time.Now()
	var timing RequestTiming
	ctx, networkTiming := traceRequest(ctx, start)
	defer func() {
		timing.Total = time.Since(start)
		timing.Dispatch, timing.Connection = networkTiming()
		emit(timing)
	}()
	var tr turnResult
	var tls []fantasy.Tool
	if a.Tools != nil {
		tls = a.Tools.Fantasy()
	}
	call := a.Model.Call(a.prompt(), tls)
	timing.Prepare = time.Since(start)
	seq, err := a.Model.LM.Stream(ctx, call)
	if err != nil {
		return tr, err
	}

	type block struct {
		text  strings.Builder
		meta  fantasy.ProviderMetadata
		start time.Time
	}
	texts := map[string]*block{}
	reasons := map[string]*block{}
	var streamErr error
	finished := false

	for part := range seq {
		if ctx.Err() != nil {
			return tr, ctx.Err()
		}
		if timing.FirstToken == 0 {
			switch part.Type {
			case fantasy.StreamPartTypeTextDelta, fantasy.StreamPartTypeReasoningDelta, fantasy.StreamPartTypeReasoningStart, fantasy.StreamPartTypeToolInputDelta:
				if part.Delta != "" {
					timing.FirstToken = time.Since(start)
				}
			case fantasy.StreamPartTypeToolInputStart, fantasy.StreamPartTypeToolCall:
				timing.FirstToken = time.Since(start)
			}
		}
		switch part.Type {
		case fantasy.StreamPartTypeTextStart:
			texts[part.ID] = &block{meta: part.ProviderMetadata}
		case fantasy.StreamPartTypeTextDelta:
			b, ok := texts[part.ID]
			if !ok {
				b = &block{}
				texts[part.ID] = b
			}
			b.text.WriteString(part.Delta)
			if part.Delta != "" {
				emit(TextDelta{Text: part.Delta})
			}
		case fantasy.StreamPartTypeTextEnd:
			if b, ok := texts[part.ID]; ok {
				if part.ProviderMetadata != nil {
					b.meta = part.ProviderMetadata
				}
				tr.parts = append(tr.parts, fantasy.TextPart{Text: b.text.String(), ProviderOptions: fantasy.ProviderOptions(b.meta)})
				delete(texts, part.ID)
			}
		case fantasy.StreamPartTypeReasoningStart:
			b := &block{meta: part.ProviderMetadata, start: time.Now()}
			b.text.WriteString(part.Delta)
			reasons[part.ID] = b
			if part.Delta != "" {
				emit(ReasoningDelta{Text: part.Delta, ID: part.ID})
			}
		case fantasy.StreamPartTypeReasoningDelta:
			b, ok := reasons[part.ID]
			if !ok {
				b = &block{start: time.Now()}
				reasons[part.ID] = b
			}
			b.text.WriteString(part.Delta)
			if part.ProviderMetadata != nil {
				b.meta = part.ProviderMetadata
			}
			if part.Delta != "" {
				emit(ReasoningDelta{Text: part.Delta, ID: part.ID})
			}
		case fantasy.StreamPartTypeReasoningEnd:
			if b, ok := reasons[part.ID]; ok {
				if b.text.Len() > 0 {
					emit(ReasoningTime{ID: part.ID, Duration: time.Since(b.start)})
				}
				if part.ProviderMetadata != nil {
					b.meta = part.ProviderMetadata
				}
				tr.parts = append(tr.parts, fantasy.ReasoningPart{Text: b.text.String(), ProviderOptions: fantasy.ProviderOptions(b.meta)})
				delete(reasons, part.ID)
			}
		case fantasy.StreamPartTypeToolInputStart:
			emit(ToolCallStart{ID: part.ID, Name: part.ToolCallName})
		case fantasy.StreamPartTypeToolInputDelta:
			if part.Delta != "" {
				emit(ToolCallInputDelta{ID: part.ID, Delta: part.Delta})
			}
		case fantasy.StreamPartTypeToolCall:
			call := fantasy.ToolCallPart{
				ToolCallID:       part.ID,
				ToolName:         part.ToolCallName,
				Input:            part.ToolCallInput,
				ProviderExecuted: part.ProviderExecuted,
				ProviderOptions:  fantasy.ProviderOptions(part.ProviderMetadata),
			}
			tr.parts = append(tr.parts, call)
			if !part.ProviderExecuted {
				tr.calls = append(tr.calls, call)
			}
		case fantasy.StreamPartTypeFinish:
			finished = true
			tr.usage = part.Usage
			tr.finish = part.FinishReason
		case fantasy.StreamPartTypeError:
			streamErr = part.Error
			if streamErr == nil {
				streamErr = errors.New("provider returned an error without details")
			}
		}
	}

	// Flush blocks the provider never closed.
	for _, b := range texts {
		tr.parts = append(tr.parts, fantasy.TextPart{Text: b.text.String(), ProviderOptions: fantasy.ProviderOptions(b.meta)})
	}
	for _, b := range reasons {
		tr.parts = append(tr.parts, fantasy.ReasoningPart{Text: b.text.String(), ProviderOptions: fantasy.ProviderOptions(b.meta)})
	}

	if streamErr != nil {
		// SSE adapters may report a missing finish marker as unexpected EOF
		// instead of ending the iterator. Keep the cause for diagnostics.
		if !finished && errors.Is(streamErr, io.ErrUnexpectedEOF) && retryableRequestError(streamErr) {
			return tr, errors.Join(ErrIncompleteResponse, streamErr)
		}
		return tr, streamErr
	}
	if ctx.Err() != nil {
		return tr, ctx.Err()
	}
	if !finished {
		return tr, ErrIncompleteResponse
	}
	for _, part := range tr.parts {
		if text, ok := part.(fantasy.TextPart); ok && malformedToolText(text.Text) {
			return tr, ErrMalformedToolResponse
		}
	}
	// A truncated stream may leave tool calls with unusable arguments; do
	// not execute them.
	if tr.finish == fantasy.FinishReasonLength && len(tr.calls) > 0 {
		tr.calls = nil
		return tr, errors.New("response hit the output token limit mid tool call")
	}
	return tr, ctx.Err()
}

// execute runs tool calls sequentially through the policy and returns the
// tool-result parts to send back. Every call gets a result part even when
// denied or failed, so the transcript stays well-formed.
func (a *Agent) execute(ctx context.Context, calls []fantasy.ToolCallPart, emit func(Event)) ([]fantasy.MessagePart, error) {
	results := make([]fantasy.MessagePart, 0, len(calls))
	for _, c := range calls {
		call := ToolCall{ID: c.ToolCallID, Name: c.ToolName, Input: c.Input}
		emit(call)

		res, isErr, err := a.runOne(ctx, call, emit)
		if err != nil {
			// Context cancellation or approver failure: record what we have and stop.
			results = append(results, errPart(call.ID, err))
			return results, err
		}
		if isErr {
			results = append(results, errPart(call.ID, errors.New(res.Output)))
		} else {
			results = append(results, fantasy.ToolResultPart{
				ToolCallID: call.ID,
				Output:     fantasy.ToolResultOutputContentText{Text: res.Output},
			})
		}
	}
	return results, nil
}

func (a *Agent) runOne(ctx context.Context, call ToolCall, emit func(Event)) (tools.Result, bool, error) {
	tool, ok := a.Tools.Get(call.Name)
	if !ok {
		out := fmt.Sprintf("unknown tool %q", call.Name)
		emit(ToolResult{ID: call.ID, Name: call.Name, Output: out, IsError: true})
		return tools.Result{Output: out}, true, nil
	}
	if !json.Valid([]byte(call.Input)) && strings.TrimSpace(call.Input) != "" {
		out := "tool arguments were not valid JSON"
		emit(ToolResult{ID: call.ID, Name: call.Name, Output: out, IsError: true})
		return tools.Result{Output: out}, true, nil
	}
	input := json.RawMessage(call.Input)
	if strings.TrimSpace(call.Input) == "" {
		input = json.RawMessage("{}")
	}

	dec, err := a.Policy.Decide(ctx, call)
	if err != nil {
		return tools.Result{}, false, err
	}
	emit(ToolDecision{ID: call.ID, Name: call.Name, Allowed: dec.Allowed, Reason: dec.Reason})
	if !dec.Allowed {
		out := "tool call was not permitted: " + dec.Reason
		emit(ToolResult{ID: call.ID, Name: call.Name, Output: out, IsError: true})
		return tools.Result{Output: out}, true, nil
	}

	if instructions, err := a.toolInstructions(ctx, tool, input); err != nil {
		emit(ToolResult{ID: call.ID, Name: call.Name, Output: err.Error(), IsError: true})
		return tools.Result{Output: err.Error()}, true, nil
	} else if instructions != "" {
		emit(ToolResult{ID: call.ID, Name: call.Name, Output: instructions})
		return tools.Result{Output: instructions}, false, nil
	}

	start := time.Now()
	res, runErr := tool.Run(ctx, input)
	dur := time.Since(start)
	if ctx.Err() != nil {
		return tools.Result{}, false, ctx.Err()
	}
	if runErr != nil {
		out := res.Output
		if out == "" {
			out = runErr.Error()
		} else if !strings.Contains(out, runErr.Error()) {
			out = strings.TrimRight(out, "\n") + "\n[error: " + runErr.Error() + "]"
		}
		emit(ToolResult{ID: call.ID, Name: call.Name, Output: out, Summary: res.Summary, IsError: true, Duration: dur})
		return tools.Result{Output: out, Summary: res.Summary}, true, nil
	}
	emit(ToolResult{ID: call.ID, Name: call.Name, Output: res.Output, Summary: res.Summary, Detail: res.Detail, Duration: dur})
	return res, false, nil
}

func errPart(id string, err error) fantasy.ToolResultPart {
	return fantasy.ToolResultPart{
		ToolCallID: id,
		Output:     fantasy.ToolResultOutputContentError{Error: err},
	}
}

func addUsage(a, b fantasy.Usage) fantasy.Usage {
	return fantasy.Usage{
		InputTokens:         a.InputTokens + b.InputTokens,
		OutputTokens:        a.OutputTokens + b.OutputTokens,
		TotalTokens:         a.TotalTokens + b.TotalTokens,
		ReasoningTokens:     a.ReasoningTokens + b.ReasoningTokens,
		CacheCreationTokens: a.CacheCreationTokens + b.CacheCreationTokens,
		CacheReadTokens:     a.CacheReadTokens + b.CacheReadTokens,
	}
}
