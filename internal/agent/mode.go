package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/attrition-tech/arkex/internal/tools"
)

// Mode is the user's current risk posture. It wraps the configured
// permission policy rather than replacing it.
type Mode string

const (
	// ModePlan permits only read-only tools; the model is asked to plan.
	ModePlan Mode = "plan"
	// ModeBuild applies the configured permissions (ask by default).
	ModeBuild Mode = "build"
	// ModeAuto skips config asks, but respects denies and workspace boundaries.
	ModeAuto Mode = "auto"
)

// Modes lists every mode in cycle order.
var Modes = []Mode{ModeBuild, ModePlan, ModeAuto}

// ParseMode accepts a mode name case-insensitively.
func ParseMode(s string) (Mode, error) {
	for _, m := range Modes {
		if strings.EqualFold(s, string(m)) {
			return m, nil
		}
	}
	return "", fmt.Errorf("unknown mode %q (want plan, build or auto)", s)
}

// Next returns the mode after m in cycle order.
func (m Mode) Next() Mode {
	for i, x := range Modes {
		if x == m {
			return Modes[(i+1)%len(Modes)]
		}
	}
	return ModeBuild
}

// ModePolicy layers a switchable Mode over a base Policy. It is safe to
// change the mode while a run is in progress; the next decision sees it.
type ModePolicy struct {
	mu    sync.RWMutex
	mode  Mode
	base  Policy
	scope *Scope
	asker Asker
}

// NewModePolicy returns a policy starting in mode with base handling Build.
func NewModePolicy(mode Mode, base Policy) *ModePolicy {
	return &ModePolicy{mode: mode, base: base}
}

// Mode returns the current mode.
func (p *ModePolicy) Mode() Mode {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mode
}

// SetMode switches modes.
func (p *ModePolicy) SetMode(m Mode) {
	p.mu.Lock()
	p.mode = m
	p.mu.Unlock()
}

// SetBase replaces the policy consulted in Build mode.
func (p *ModePolicy) SetBase(base Policy) {
	p.mu.Lock()
	p.base = base
	p.mu.Unlock()
}

// SetScope installs the workspace boundary and the Asker consulted when a
// call leaves it. A nil asker means such calls are denied (print mode).
func (p *ModePolicy) SetScope(scope *Scope, asker Asker) {
	p.mu.Lock()
	p.scope, p.asker = scope, asker
	p.mu.Unlock()
}

// Decide applies explicit denies first, then mode permissions and directory
// trust. Every detected outside path must be covered before a call can run.
func (p *ModePolicy) Decide(ctx context.Context, call ToolCall) (Decision, error) {
	p.mu.RLock()
	mode, base, scope, asker := p.mode, p.base, p.scope, p.asker
	p.mu.RUnlock()

	if denies, ok := base.(interface{ Denies(string) bool }); ok && denies.Denies(call.Name) {
		return Decision{Allowed: false, Reason: "denied by config"}, nil
	}
	if mode == ModePlan && !tools.IsReadOnly(call.Name) {
		return Decision{Allowed: false, Reason: "plan mode permits only read-only tools; ask the user to switch to build mode"}, nil
	}

	if scope != nil && call.Name == "bash" {
		var in struct {
			Workdir string `json:"workdir"`
		}
		_ = json.Unmarshal([]byte(call.Input), &in)
		call.Workdir = scope.bashWorkdir(in.Workdir)
	}
	reach, paths := scope.classifyPaths(call)
	var pending []string
	seen := map[string]bool{}
	for _, path := range paths {
		if !seen[path] && !scope.trusted(path, reach) {
			seen[path] = true
			pending = append(pending, path)
		}
	}
	boundaryCall := func(path string) ToolCall {
		c := call
		c.Grantable = true
		c.TrustDirectory = scope.trustDirectory(path)
		c.TrustAccess = "read-only"
		c.Reason = "reads outside the workspace: " + path
		if reach == ReachOutsideWrite {
			c.TrustAccess = "read, changes and shell path checks"
			c.Reason = "writes outside the workspace: " + path
			if call.Name == "bash" {
				c.Reason = "command names a path outside the workspace: " + path
			}
		}
		return c
	}
	if len(pending) > 0 {
		call = boundaryCall(pending[0])
	}

	var dec Decision
	switch mode {
	case ModePlan:
		dec = Decision{Allowed: true, Reason: "read-only tool"}
	case ModeAuto:
		dec = Decision{Allowed: true, Reason: "auto mode"}
	default:
		if base == nil {
			return Decision{Allowed: false, Reason: "no base policy configured"}, nil
		}
		var err error
		if dec, err = base.Decide(ctx, call); err != nil {
			return Decision{}, err
		}
	}
	if !dec.Allowed || len(pending) == 0 {
		return dec, nil
	}
	for i, path := range pending {
		if scope.trusted(path, reach) {
			continue
		}
		c := boundaryCall(path)
		a := dec.Answer
		if i != 0 || !dec.Asked {
			if asker == nil {
				return Decision{Allowed: false, Reason: c.Reason + "; no approver is available"}, nil
			}
			var err error
			a, err = asker.Ask(ctx, c)
			if err != nil {
				return Decision{}, err
			}
			if a == Deny {
				return Decision{Allowed: false, Reason: "denied by user", Asked: true}, nil
			}
		}
		if a == AllowSession {
			scope.trust(c.TrustDirectory, reach)
		}
	}
	return Decision{Allowed: true, Reason: "approved by user", Asked: true}, nil
}
