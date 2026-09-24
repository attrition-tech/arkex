package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"

	"charm.land/fantasy"
)

// Loop detection: instead of a fixed step budget, a run pauses when the
// model keeps issuing the same tool calls and keeps getting the same
// results. Legitimate long runs (edit, test, edit, test…) produce different
// inputs or outputs each step and never trigger it.
const (
	stuckWindow  = 10 // steps looked at
	stuckRepeats = 5  // identical steps within the window that count as stuck
)

// stepSig identifies what one step asked and learned: a hash over the tool
// calls paired with their results, plus the tool names for messages.
type stepSig struct {
	hash  string // "" for a step without tool calls
	names string // e.g. "read" or "bash, edit"
}

// stuck reports how many times the latest step's signature appears in the
// last stuckWindow steps once that reaches stuckRepeats, with the step's
// tool names; otherwise 0. Only the latest step is checked so the pause
// happens the moment the threshold is crossed.
func stuck(steps []stepSig) (int, string) {
	if len(steps) == 0 {
		return 0, ""
	}
	last := steps[len(steps)-1]
	if last.hash == "" { // a step without tool calls ends the run anyway
		return 0, ""
	}
	n := 0
	for _, s := range steps[max(0, len(steps)-stuckWindow):] {
		if s.hash == last.hash {
			n++
		}
	}
	if n < stuckRepeats {
		return 0, ""
	}
	return n, last.names
}

// stepSignature hashes one step's tool calls paired with their results
// (name, arguments, output). Two steps with the same signature asked the
// same things and got the same answers.
func stepSignature(calls []fantasy.ToolCallPart, results []fantasy.MessagePart) stepSig {
	if len(calls) == 0 {
		return stepSig{}
	}
	outputs := make(map[string]string, len(results))
	for _, r := range results {
		tr, ok := r.(fantasy.ToolResultPart)
		if !ok {
			continue
		}
		switch o := tr.Output.(type) {
		case fantasy.ToolResultOutputContentText:
			outputs[tr.ToolCallID] = o.Text
		case fantasy.ToolResultOutputContentError:
			if o.Error != nil {
				outputs[tr.ToolCallID] = "error: " + o.Error.Error()
			}
		}
	}
	h := sha256.New()
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.ToolName)
		for _, field := range []string{c.ToolName, c.Input, outputs[c.ToolCallID]} {
			_, _ = io.WriteString(h, field) // hash.Hash writes never fail
			h.Write([]byte{0})
		}
	}
	return stepSig{hash: hex.EncodeToString(h.Sum(nil)), names: strings.Join(names, ", ")}
}
