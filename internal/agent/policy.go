package agent

import (
	"context"

	"github.com/attrition-tech/arkex/internal/config"
)

type Decision struct {
	Allowed bool
	Reason  string
}

// Policy controls tool availability. Filesystem confinement is enforced by
// the tools themselves, even when a policy allows a call.
type Policy interface {
	Decide(context.Context, ToolCall) (Decision, error)
}

type PolicyFunc func(context.Context, ToolCall) (Decision, error)

func (f PolicyFunc) Decide(ctx context.Context, call ToolCall) (Decision, error) {
	return f(ctx, call)
}

type ConfigPolicy struct{ Config *config.Config }

func (p ConfigPolicy) Decide(_ context.Context, call ToolCall) (Decision, error) {
	if p.Config != nil && p.Config.Permission(call.Name) == config.PermissionAllow {
		return Decision{Allowed: true, Reason: "allowed by config"}, nil
	}
	return Decision{Reason: "denied by config"}, nil
}

// AllowAll is used by tests and tool-free model probes. It cannot disable
// confinement enforced in the tools.
type AllowAll struct{}

func (AllowAll) Decide(context.Context, ToolCall) (Decision, error) {
	return Decision{Allowed: true, Reason: "all tools allowed"}, nil
}
