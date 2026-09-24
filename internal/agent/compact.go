package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

// Context management. Every request carries the whole conversation, so a
// long run of tool calls eventually fills the model's context window. The
// agent keeps the conversation fitting in three ways:
//
//   - before each request, when the previous one used compactAt of the
//     window, it compacts first;
//   - when a request is rejected for being too long anyway (unknown or
//     wrong window), it learns the window from the error, compacts and
//     retries once;
//   - the compaction itself trims the conversation until the summary
//     request is expected to fit, since that request is the biggest of all.
const (
	// compactAt is the share of the context window the last request may
	// use before the next one is preceded by a compaction.
	compactAt = 0.8
	// summaryBudget is the share of the window a summary request may take:
	// half, leaving room for the summary and for estimation error.
	summaryBudget = 0.5
	// Tool results longer than trimResultAt characters are cut to
	// trimHead + trimTail before summarising when the conversation must
	// shrink; the first and last lines usually carry the point.
	trimResultAt = 2000
	trimHead     = 1200
	trimTail     = 400
	// defaultCharsPerToken is the estimate used before any request reported
	// its token count. English prose is ~4, code and JSON ~3.
	defaultCharsPerToken = 3.5
)

// compactPrompt asks the model for a hand-off note: the next turn only sees
// this note, so it must carry everything a fresh reader would need.
const compactPrompt = `Write a hand-off summary of the conversation so far for an engineer who will continue the work without seeing it. Be concrete and complete; use plain prose and short lists. Cover:
- what the user asked for, including constraints and preferences they stated
- what was done: files read, created or changed (exact paths), commands run and their outcomes
- decisions made and why; open questions the user has not answered
- current state: what works, what is broken or unverified, and the exact next step
Quote identifiers, paths, error messages and numbers exactly. Do not add commentary about this summary itself.`

// CompactResult reports what a Compact replaced.
type CompactResult struct {
	Summary  string
	Dropped  int // messages replaced by the summary
	Trimmed  int // messages removed unread because even the summary request would not fit
	Usage    fantasy.Usage
	Finished fantasy.FinishReason
}

// DefaultContextWindow is an assumed budget, not a confirmed model limit.
const DefaultContextWindow = 262144

// ContextWindow returns the configured limit or the assumed budget.
func (a *Agent) ContextWindow() int {
	if a.Model == nil {
		return 0
	}
	if a.ContextWindowAssumed() {
		return DefaultContextWindow
	}
	return a.Model.Ref.Model.ContextWindow
}

func (a *Agent) ContextWindowAssumed() bool {
	return a.Model != nil && a.Model.Ref.Model.ContextWindow <= 0
}

// ContextInput includes cached prompt tokens: they still occupy context.
func ContextInput(u fantasy.Usage) int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// RestoreLastInput restores the saved measurement for the same model.
func (a *Agent) RestoreLastInput(tokens int64) { a.lastInput = max(tokens, 0) }

// SetContextWindow records the model's context window for this process;
// callers persist it to the config file if they want it to stick.
func (a *Agent) SetContextWindow(tokens int) {
	if a.Model != nil {
		a.Model.Ref.Model.ContextWindow = tokens
	}
}

// LastInput is the input token count the model reported for the most
// recent request, 0 before the first one or right after a compaction.
func (a *Agent) LastInput() int64 { return a.lastInput }

// needsCompact reports whether the last request used enough of the window
// that the next one should be preceded by a compaction.
func (a *Agent) needsCompact() bool {
	w := a.ContextWindow()
	return w > 0 && a.lastInput > 0 && len(a.messages) > 1 && float64(a.lastInput) >= compactAt*float64(w)
}

// Compact summarises the whole conversation on request (/compact) and
// replaces it with the summary as a user message plus a short assistant
// acknowledgement, so the user's next prompt keeps the roles alternating.
// Nothing changes when the request fails.
func (a *Agent) Compact(ctx context.Context) (CompactResult, error) {
	return a.compact(ctx, 0, true)
}

// compact summarises all but the last keep messages and rebuilds the
// conversation as summary [+ acknowledgement] + kept messages. Without the
// acknowledgement the summary is the last message, and the model answers
// it directly — what a mid-run compaction wants.
func (a *Agent) compact(ctx context.Context, keep int, ack bool) (CompactResult, error) {
	var res CompactResult
	if len(a.messages) <= keep {
		return res, errors.New("nothing to compact")
	}
	if a.Model == nil || a.Model.LM == nil {
		return res, errors.New("no model connected")
	}
	head, tail := a.messages[:len(a.messages)-keep], a.messages[len(a.messages)-keep:]
	res, err := a.summarise(ctx, head)
	if err != nil {
		return res, err
	}
	var next []fantasy.Message
	if ack {
		next = CompactedMessages(res.Summary)
	} else {
		next = []fantasy.Message{fantasy.NewUserMessage(CompactPrefix + res.Summary + compactSuffix)}
	}
	a.messages = append(next, tail...)
	a.lastInput = 0 // unknown until the next request
	return res, nil
}

// summarise asks the model for a hand-off note covering msgs. The request
// is trimmed to the summary budget first; if the provider still rejects it
// as too long, the window is learned from the error and it is retried once
// at half the budget.
func (a *Agent) summarise(ctx context.Context, msgs []fantasy.Message) (CompactResult, error) {
	res := CompactResult{Dropped: len(msgs)}
	budget := summaryBudget
	window := a.ContextWindow()
	for attempt := 0; ; attempt++ {
		fitted, trimmed := a.fit(msgs, window, budget)
		prompt := make(fantasy.Prompt, 0, len(fitted)+2)
		if a.System != "" {
			prompt = append(prompt, fantasy.NewSystemMessage(a.System))
		}
		prompt = append(prompt, fitted...)
		ask := compactPrompt
		if trimmed > 0 {
			ask = fmt.Sprintf("(The %d earliest messages were removed because they no longer fit; summarise what remains and say that earlier history was lost.)\n\n", trimmed) + ask
		}
		prompt = append(prompt, fantasy.NewUserMessage(ask))

		// Generate, not Stream: nothing to show while it runs, and one
		// response object is simpler to check.
		resp, err := a.Model.LM.Generate(ctx, a.Model.Call(prompt, nil))
		if err != nil {
			if attempt == 0 && IsContextOverflow(err) {
				if w := ContextWindowFromError(err); w > 0 {
					a.SetContextWindow(w)
					window = w
				}
				if window <= 0 || a.ContextWindowAssumed() {
					// The window is unknown but this request is bigger
					// than it, so shrink relative to the request itself.
					window = estimateTokens(fitted, a.charsPerToken())
				}
				budget = summaryBudget / 2
				continue
			}
			return res, err
		}
		var sb strings.Builder
		for _, part := range resp.Content {
			if t, ok := part.(fantasy.TextContent); ok {
				sb.WriteString(t.Text)
			}
		}
		summary := strings.TrimSpace(sb.String())
		if summary == "" {
			return res, errors.New("model returned an empty summary")
		}
		res.Summary, res.Trimmed, res.Usage, res.Finished = summary, trimmed, resp.Usage, resp.FinishReason
		return res, nil
	}
}

// autoCompact is compact for the agent loop: it announces the result as an
// event. Mid-run (keep == 0) the summary is left unacknowledged so the
// model's next reply answers it directly; before a fresh prompt (keep > 0)
// the summary+ack pair precedes the prompt, as after a manual /compact.
func (a *Agent) autoCompact(ctx context.Context, emit func(Event), keep int, reason string) (CompactResult, error) {
	emit(Compacting{Active: true})
	defer emit(Compacting{Active: false})
	res, err := a.compact(ctx, keep, keep > 0)
	if err != nil {
		return res, err
	}
	emit(Compacted{Summary: res.Summary, Dropped: res.Dropped, Trimmed: res.Trimmed, Reason: reason, Usage: res.Usage})
	return res, nil
}

// fit returns msgs reduced until their estimated size is within budget ×
// window tokens: first long tool results are cut down (oldest first), then
// the oldest messages are dropped. The count of dropped messages is
// returned. msgs is not modified. An unknown window returns msgs as is.
func (a *Agent) fit(msgs []fantasy.Message, window int, budget float64) ([]fantasy.Message, int) {
	if window <= 0 {
		return msgs, 0
	}
	limit := int(budget * float64(window))
	ratio := a.charsPerToken()
	out := append([]fantasy.Message(nil), msgs...)
	over := func() bool { return estimateTokens(out, ratio) > limit }
	if !over() {
		return out, 0
	}
	for i := range out {
		if out[i].Role != fantasy.MessageRoleTool {
			continue
		}
		out[i] = trimToolResults(out[i])
		if !over() {
			return out, 0
		}
	}
	dropped := 0
	for len(out) > 1 && over() {
		out = out[1:]
		dropped++
		// The kept part must start with a user message: a tool result
		// without its call, or an assistant message first, is malformed
		// for most providers.
		for len(out) > 1 && out[0].Role != fantasy.MessageRoleUser {
			out = out[1:]
			dropped++
		}
	}
	return out, dropped
}

// charsPerToken derives the model's observed ratio from the last request
// (its reported input tokens against the current conversation's size),
// falling back to a prose/code average.
func (a *Agent) charsPerToken() float64 {
	if a.lastInput <= 0 {
		return defaultCharsPerToken
	}
	chars := 0
	for _, m := range a.messages {
		chars += messageChars(m)
	}
	if chars == 0 {
		return defaultCharsPerToken
	}
	ratio := float64(chars) / float64(a.lastInput)
	return min(max(ratio, 2), 8)
}

func estimateTokens(msgs []fantasy.Message, charsPerToken float64) int {
	chars := 0
	for _, m := range msgs {
		chars += messageChars(m)
	}
	return int(float64(chars) / charsPerToken)
}

// messageChars is the text size of a message plus a per-part allowance
// for the structure around it. Images are counted as a flat 1500 tokens'
// worth, which is what most providers charge for one.
func messageChars(m fantasy.Message) int {
	n := 0
	for _, p := range m.Content {
		n += 16
		switch p := p.(type) {
		case fantasy.TextPart:
			n += len(p.Text)
		case fantasy.ReasoningPart:
			n += len(p.Text)
		case fantasy.ToolCallPart:
			n += len(p.ToolName) + len(p.Input)
		case fantasy.ToolResultPart:
			n += len(toolResultText(p))
		case fantasy.FilePart:
			n += 1500 * 4
		}
	}
	return n
}

func toolResultText(p fantasy.ToolResultPart) string {
	switch o := p.Output.(type) {
	case fantasy.ToolResultOutputContentText:
		return o.Text
	case fantasy.ToolResultOutputContentError:
		if o.Error != nil {
			return o.Error.Error()
		}
	}
	return ""
}

// trimToolResults returns a copy of a tool message whose long text results
// are cut to their head and tail.
func trimToolResults(m fantasy.Message) fantasy.Message {
	parts := make([]fantasy.MessagePart, len(m.Content))
	for i, p := range m.Content {
		tr, ok := p.(fantasy.ToolResultPart)
		if !ok {
			parts[i] = p
			continue
		}
		if t, ok := tr.Output.(fantasy.ToolResultOutputContentText); ok && len(t.Text) > trimResultAt {
			cut := len(t.Text) - trimHead - trimTail
			tr.Output = fantasy.ToolResultOutputContentText{
				Text: t.Text[:trimHead] + fmt.Sprintf("\n[… %d characters trimmed for the summary …]\n", cut) + t.Text[len(t.Text)-trimTail:],
			}
		}
		parts[i] = tr
	}
	return fantasy.Message{Role: m.Role, Content: parts, ProviderOptions: m.ProviderOptions}
}

// CompactPrefix opens the user message that carries a summary, and
// CompactAck is the assistant reply that follows it. Both are fixed so a
// transcript rebuilt from a saved session can recognise them.
const (
	CompactPrefix = "Summary of the conversation so far (earlier messages were compacted to save context):\n\n"
	compactSuffix = "\n\nContinue from this state."
	CompactAck    = "Understood. I have the summary and will continue from there."
)

// CompactedMessages is the conversation that stands in for a summarised
// one: the summary as the user's message, acknowledged by the assistant.
func CompactedMessages(summary string) []fantasy.Message {
	return []fantasy.Message{
		fantasy.NewUserMessage(CompactPrefix + summary + compactSuffix),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: CompactAck}}},
	}
}

// CompactSummary returns the summary carried by a message built with
// CompactedMessages, and false for any other text.
func CompactSummary(userText string) (string, bool) {
	if !strings.HasPrefix(userText, CompactPrefix) {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(userText, CompactPrefix), compactSuffix), true
}

// IsContextOverflow reports whether err looks like the provider rejecting
// the request for exceeding the model's context window. Providers word this
// differently; this matches the common phrasings.
func IsContextOverflow(err error) bool {
	// A Go request deadline is unrelated to the model's context window.
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	s := strings.ToLower(err.Error())
	if !strings.Contains(s, "context") && !strings.Contains(s, "prompt") && !strings.Contains(s, "token") {
		return false
	}
	for _, hint := range []string{"context length", "context window", "context limit", "context_length", "too long", "too many tokens", "maximum context", "exceed"} {
		if strings.Contains(s, hint) {
			return true
		}
	}
	return false
}

// windowPatterns pick the context window out of overflow messages:
//
//	OpenAI/vLLM:  "This model's maximum context length is 262144 tokens"
//	Anthropic:    "prompt is too long: 213462 tokens > 200000 maximum"
//	llama.cpp:    "the request exceeds the available context size (8192)"
//	generic:      "context window of 128000 tokens", "context length: 32768"
var windowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)maximum context length (?:is|of) (\d[\d,_]*)`),
	regexp.MustCompile(`(?i)context (?:window|length|size|limit)(?: is| of|:)? (\d[\d,_]*)`),
	regexp.MustCompile(`(?i)> ?(\d[\d,_]*) maximum`),
	regexp.MustCompile(`(?i)context size \((\d[\d,_]*)\)`),
	regexp.MustCompile(`(?i)limit(?:ed)? (?:to|of) (\d[\d,_]*) tokens`),
}

// formatTokens renders a token count the way model cards do: 8k, 128k, 1M.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000 && n%100_000 == 0:
		if n%1_000_000 == 0 {
			return fmt.Sprintf("%dM", n/1_000_000)
		}
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return strconv.Itoa(n)
}

// ContextWindowFromError extracts the model's context window from a
// provider's overflow message, 0 when it does not state one. Values under
// 1024 are ignored as noise.
func ContextWindowFromError(err error) int {
	if err == nil {
		return 0
	}
	s := err.Error()
	for _, re := range windowPatterns {
		if m := re.FindStringSubmatch(s); m != nil {
			n, convErr := strconv.Atoi(strings.NewReplacer(",", "", "_", "").Replace(m[1]))
			if convErr == nil && n >= 1024 {
				return n
			}
		}
	}
	return 0
}
