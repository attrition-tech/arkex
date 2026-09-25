// Package modes contains the non-TUI front ends: print (plain text) and
// json (newline-delimited events). Both consume agent.Event.
package modes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/sanitize"
)

// Runner runs one prompt and streams events; *agent.Agent implements it.
type Runner interface {
	Run(ctx context.Context, prompt string, emit func(agent.Event)) error
}

// Print runs one prompt and writes assistant text to out. Tool activity
// goes to errOut so stdout stays pipeable.
func Print(ctx context.Context, ag Runner, prompt string, out, errOut io.Writer) error {
	wroteText := false
	atLineStart := true
	var writeErr error
	write := func(n int, err error) {
		if writeErr == nil {
			writeErr = err
		}
	}
	// breakLine ends a partial text line before tool activity so the two
	// streams do not collide when both point at the terminal.
	breakLine := func() {
		if !atLineStart {
			write(fmt.Fprintln(out))
			atLineStart = true
		}
	}
	err := ag.Run(ctx, prompt, func(e agent.Event) {
		switch e := e.(type) {
		case agent.TextDelta:
			wroteText = true
			text := sanitize.Terminal(e.Text)
			write(fmt.Fprint(out, text))
			atLineStart = strings.HasSuffix(text, "\n")
		case agent.ToolCall:
			breakLine()
			write(fmt.Fprintf(errOut, "→ %s %s\n", e.Name, oneLine(sanitize.Terminal(e.Input), 100)))
		case agent.ToolDecision:
			if !e.Allowed {
				write(fmt.Fprintf(errOut, "  ✗ %s\n", sanitize.Terminal(e.Reason)))
			}
		case agent.ToolResult:
			if e.IsError {
				write(fmt.Fprintf(errOut, "  ✗ %s\n", oneLine(sanitize.Terminal(e.Output), 200)))
			} else if e.Summary != "" {
				write(fmt.Fprintf(errOut, "  ✓ %s (%s)\n", sanitize.Terminal(e.Summary), e.Duration))
			}
		case agent.RunEnd:
			if wroteText && !atLineStart {
				write(fmt.Fprintln(out))
			}
			if e.Err != nil {
				write(fmt.Fprintf(errOut, "error: %s\n", sanitize.Terminal(e.Err.Error())))
			}
			write(fmt.Fprintf(errOut, "[%d step(s), %d in / %d out tokens]\n", e.Steps, e.Usage.InputTokens, e.Usage.OutputTokens))
		}
	})
	if err != nil {
		return err
	}
	return writeErr
}

// JSON runs one prompt and writes every event as one JSON object per line.
func JSON(ctx context.Context, ag Runner, prompt string, out io.Writer) error {
	enc := json.NewEncoder(out)
	return ag.Run(ctx, prompt, func(e agent.Event) {
		env := map[string]any{"type": eventName(e), "data": e}
		if re, ok := e.(agent.RunEnd); ok && re.Err != nil {
			env["data"] = map[string]any{"steps": re.Steps, "usage": re.Usage, "error": re.Err.Error()}
		}
		_ = enc.Encode(env)
	})
}

func eventName(e agent.Event) string {
	switch e.(type) {
	case agent.TurnStart:
		return "turn_start"
	case agent.RetryWait:
		return "retry_wait"
	case agent.RequestRestart:
		return "request_restart"
	case agent.TextDelta:
		return "text_delta"
	case agent.ReasoningDelta:
		return "reasoning_delta"
	case agent.ReasoningTime:
		return "reasoning_time"
	case agent.RequestTiming:
		return "request_timing"
	case agent.ToolCallStart:
		return "tool_call_start"
	case agent.ToolCallInputDelta:
		return "tool_call_input_delta"
	case agent.ToolCall:
		return "tool_call"
	case agent.ToolDecision:
		return "tool_decision"
	case agent.ToolResult:
		return "tool_result"
	case agent.TurnEnd:
		return "turn_end"
	case agent.Compacting:
		return "compacting"
	case agent.RunEnd:
		return "run_end"
	}
	return "unknown"
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}
