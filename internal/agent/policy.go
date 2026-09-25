package agent

import (
	"context"
	"sync"

	"github.com/attrition-tech/arkex/internal/config"
)

// Decision is the outcome of a policy check.
type Decision struct {
	Allowed bool
	Reason  string
	// Asked is set when the user was prompted for this very call, so an
	// outer policy that must also ask does not prompt twice.
	Asked  bool
	Answer Answer // preserves scoped trust intent for the outer workspace policy
}

// Policy decides whether a tool call may run.
type Policy interface {
	Decide(ctx context.Context, call ToolCall) (Decision, error)
}

// PolicyFunc adapts a function to Policy.
type PolicyFunc func(ctx context.Context, call ToolCall) (Decision, error)

// Decide implements Policy.
func (f PolicyFunc) Decide(ctx context.Context, call ToolCall) (Decision, error) {
	return f(ctx, call)
}

// Answer is the user's reply to an approval prompt.
type Answer int

const (
	// Deny refuses this call.
	Deny Answer = iota
	// AllowOnce runs this call only.
	AllowOnce
	// AllowSession runs this call and, when the prompt offered it
	// (ToolCall.Grantable), stops asking for this tool until the process
	// exits. Policies that did not offer it treat it as AllowOnce.
	AllowSession
)

// Asker is consulted for tools whose configured permission is "ask" and
// for calls that leave the workspace. The TUI implements this with a
// prompt; print mode denies by default.
type Asker interface {
	Ask(ctx context.Context, call ToolCall) (Answer, error)
}

// Grants remembers the tools the user allowed for the rest of the session.
// One Grants is shared by every ConfigPolicy the process builds, so a
// model switch does not forget them. The zero value is ready to use.
type Grants struct {
	mu    sync.Mutex
	tools map[string]bool
}

// Has reports whether tool was granted for the session.
func (g *Grants) Has(tool string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tools[tool]
}

// Add grants tool for the session.
func (g *Grants) Add(tool string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tools == nil {
		g.tools = map[string]bool{}
	}
	g.tools[tool] = true
}

// ConfigPolicy applies config permissions and defers "ask" to an Asker.
type ConfigPolicy struct {
	Config *config.Config
	Asker  Asker
	// Grants holds session-wide approvals; nil disables "allow for this
	// session" (the prompt then offers once/deny only).
	Grants *Grants
}

// Denies is consulted before any mode override or session grant.
func (p ConfigPolicy) Denies(tool string) bool {
	return p.Config.Permission(tool) == config.PermissionDeny
}

// Decide implements Policy.
func (p ConfigPolicy) Decide(ctx context.Context, call ToolCall) (Decision, error) {
	switch p.Config.Permission(call.Name) {
	case config.PermissionAllow:
		return Decision{Allowed: true, Reason: "allowed by config"}, nil
	case config.PermissionDeny:
		return Decision{Allowed: false, Reason: "denied by config"}, nil
	}
	if p.Grants.Has(call.Name) {
		return Decision{Allowed: true, Reason: "allowed by user for this session"}, nil
	}
	if p.Asker == nil {
		return Decision{Allowed: false, Reason: "requires approval and no approver is available"}, nil
	}
	// A call already flagged by the workspace boundary (Reason set) is
	// special on its own; a session grant is only offered for plain ones.
	call.Grantable = call.TrustDirectory != "" || (p.Grants != nil && call.Reason == "")
	a, err := p.Asker.Ask(ctx, call)
	if err != nil {
		return Decision{}, err
	}
	switch a {
	case AllowSession:
		if call.TrustDirectory != "" {
			return Decision{Allowed: true, Reason: "approved by user", Asked: true, Answer: a}, nil
		}
		if call.Grantable {
			p.Grants.Add(call.Name)
			return Decision{Allowed: true, Reason: "approved by user for this session", Asked: true}, nil
		}
		fallthrough
	case AllowOnce:
		return Decision{Allowed: true, Reason: "approved by user", Asked: true}, nil
	}
	return Decision{Allowed: false, Reason: "denied by user", Asked: true}, nil
}

// AllowAll approves everything. Used for --yolo and tests.
type AllowAll struct{}

// Decide implements Policy.
func (AllowAll) Decide(context.Context, ToolCall) (Decision, error) {
	return Decision{Allowed: true, Reason: "all tools allowed"}, nil
}
