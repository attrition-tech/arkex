// Package tools defines the tool interface the agent exposes to models and
// the built-in tools. Tools never talk to the UI; they return results and
// the agent decides how to surface them.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
)

// Tool is one callable capability exposed to the model.
type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON Schema for the tool input object.
	Schema() map[string]any
	// Run executes the tool. A non-nil error means the tool itself failed in
	// a way the model should see as an error result; the agent keeps going.
	Run(ctx context.Context, input json.RawMessage) (Result, error)
}

// Result is what a tool hands back to the model.
type Result struct {
	// Output is the text the model sees.
	Output string
	// Summary is a short one-line description for the UI (optional).
	Summary string
	// Detail is extra text for the UI only, never sent to the model: for
	// example the diff an edit produced. Lines starting with "-" or "+" are
	// rendered as removed/added.
	Detail string
}

// Registry is an ordered set of tools keyed by name.
type Registry struct {
	order  []Tool
	byName map[string]Tool
}

// NewRegistry builds a registry from tools; duplicate names panic because
// that is a programming error, not a runtime condition.
func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{byName: map[string]Tool{}}
	for _, t := range ts {
		if _, dup := r.byName[t.Name()]; dup {
			panic("tools: duplicate tool " + t.Name())
		}
		r.order = append(r.order, t)
		r.byName[t.Name()] = t
	}
	return r
}

// Get returns the tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// All returns tools in registration order.
func (r *Registry) All() []Tool { return r.order }

// Fantasy converts the registry into fantasy tool definitions.
func (r *Registry) Fantasy() []fantasy.Tool {
	out := make([]fantasy.Tool, 0, len(r.order))
	for _, t := range r.order {
		out = append(out, fantasy.FunctionTool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.Schema(),
		})
	}
	return out
}

// Default returns the built-in tool set rooted at dir.
func Default(dir string) *Registry {
	return NewRegistry(
		&Read{Root: dir},
		&Write{Root: dir},
		&Edit{Root: dir},
		&Bash{Dir: dir},
	)
}

// builtinCatalog shares the registration source with executable registries.
// Its instances are used only for metadata, never execution.
var builtinCatalog = Default("")

// IsBuiltin reports whether name is registered as a built-in tool.
func IsBuiltin(name string) bool {
	_, ok := builtinCatalog.Get(name)
	return ok
}

// IsReadOnly reports the built-in tool's declared capability. Tools without
// an explicit read-only declaration, including unknown names, fail closed.
func IsReadOnly(name string) bool {
	t, ok := builtinCatalog.Get(name)
	if !ok {
		return false
	}
	ro, ok := t.(interface{ ReadOnly() bool })
	return ok && ro.ReadOnly()
}

// decode unmarshals input into v with unknown-field rejection so a model
// hallucinating argument names gets a clear error instead of silent defaults.
func decode(input json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(input)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// truncate caps s at max bytes on a rune boundary and appends a note.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := cutAt(s, max)
	return s[:cut] + fmt.Sprintf("\n\n[output truncated: %d of %d bytes shown]", cut, len(s))
}

// limitedOutput drains all output while retaining only the displayed prefix.
// One extra byte lets cutAt detect a UTF-8 rune crossing the limit. os/exec
// serializes writes when stdout and stderr share this same writer.
type limitedOutput struct {
	buf   bytes.Buffer
	limit int
	total int64
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	b.total += int64(n)
	_, _ = b.buf.Write(p[:min(n, b.limit+1-b.buf.Len())])
	return n, nil
}

func (b *limitedOutput) writeString(s string) {
	b.total += int64(len(s))
	_, _ = b.buf.WriteString(s[:min(len(s), b.limit+1-b.buf.Len())])
}

func (b *limitedOutput) String() string {
	s := b.buf.String()
	if b.total <= int64(b.limit) {
		return s
	}
	cut := cutAt(s, b.limit)
	return s[:cut] + fmt.Sprintf("\n\n[output truncated: %d of %d bytes shown]", cut, b.total)
}

// cutAt returns the largest index <= max that does not split a rune.
func cutAt(s string, max int) int {
	cut := max
	for cut > 0 && cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return cut
}

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}
