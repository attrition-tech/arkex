package agent

import (
	"context"
	"testing"

	"github.com/dantearo/arkex/internal/config"
)

// scriptedAsker answers each prompt with the next scripted Answer and
// records what it was asked.
type scriptedAsker struct {
	answers []Answer
	calls   []ToolCall
}

func (a *scriptedAsker) Ask(_ context.Context, call ToolCall) (Answer, error) {
	a.calls = append(a.calls, call)
	if len(a.answers) == 0 {
		return Deny, nil
	}
	ans := a.answers[0]
	a.answers = a.answers[1:]
	return ans, nil
}

func TestConfigPolicySessionGrant(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{} // bash defaults to "ask"
	ask := &scriptedAsker{answers: []Answer{AllowSession, Deny}}
	grants := &Grants{}
	p := ConfigPolicy{Config: cfg, Asker: ask, Grants: grants}

	d, err := p.Decide(ctx, ToolCall{Name: "bash", Input: `{"command":"ls"}`})
	if err != nil || !d.Allowed || !d.Asked {
		t.Fatalf("first call: %+v %v", d, err)
	}
	if len(ask.calls) != 1 || !ask.calls[0].Grantable {
		t.Fatalf("prompt must be offered as grantable: %+v", ask.calls)
	}
	// Second bash call runs without a prompt; a different tool still asks.
	d, _ = p.Decide(ctx, ToolCall{Name: "bash", Input: `{"command":"rm x"}`})
	if !d.Allowed || d.Asked || len(ask.calls) != 1 {
		t.Fatalf("granted tool must not prompt again: %+v asks=%d", d, len(ask.calls))
	}
	d, _ = p.Decide(ctx, ToolCall{Name: "write", Input: `{"path":"x"}`})
	if d.Allowed || len(ask.calls) != 2 {
		t.Fatalf("other tools keep asking: %+v asks=%d", d, len(ask.calls))
	}
	// A fresh ConfigPolicy sharing the same Grants (model switch) remembers.
	p2 := ConfigPolicy{Config: cfg, Asker: ask, Grants: grants}
	if d, _ := p2.Decide(ctx, ToolCall{Name: "bash"}); !d.Allowed || len(ask.calls) != 2 {
		t.Fatalf("grants must survive a policy rebuild: %+v asks=%d", d, len(ask.calls))
	}
}

func TestConfigPolicyWithoutGrantsTreatsSessionAsOnce(t *testing.T) {
	ctx := context.Background()
	ask := &scriptedAsker{answers: []Answer{AllowSession, Deny}}
	p := ConfigPolicy{Config: &config.Config{}, Asker: ask}
	d, _ := p.Decide(ctx, ToolCall{Name: "bash"})
	if !d.Allowed || ask.calls[0].Grantable {
		t.Fatalf("no Grants: prompt must not be grantable, call still allowed: %+v %+v", d, ask.calls[0])
	}
	if d, _ := p.Decide(ctx, ToolCall{Name: "bash"}); d.Allowed || len(ask.calls) != 2 {
		t.Fatalf("nothing remembered without Grants: %+v asks=%d", d, len(ask.calls))
	}
}

func TestWorkspaceTrustIsNotAToolGrant(t *testing.T) {
	s, _, _ := testScope(t)
	ctx := context.Background()
	ask := &scriptedAsker{answers: []Answer{AllowSession, Deny, Deny}}
	grants := &Grants{}
	p := NewModePolicy(ModeBuild, ConfigPolicy{Config: &config.Config{}, Asker: ask, Grants: grants})
	p.SetScope(s, ask)
	call := ToolCall{Name: "write", Input: `{"path":"~/notes/a.md"}`}
	if d, _ := p.Decide(ctx, call); !d.Allowed || !ask.calls[0].Grantable || grants.Has("write") {
		t.Fatalf("directory trust must not grant the tool: %+v %+v", d, ask.calls)
	}
	if d, _ := p.Decide(ctx, ToolCall{Name: "write", Input: `{"path":"src/a.go"}`}); d.Allowed {
		t.Fatal("build must still apply configured tool approval")
	}
	p.SetMode(ModeAuto)
	if d, _ := p.Decide(ctx, call); !d.Allowed || len(ask.calls) != 2 {
		t.Fatalf("auto must remember directory trust: %+v %+v", d, ask.calls)
	}
	if d, _ := p.Decide(ctx, ToolCall{Name: "write", Input: `{"path":"~/notes-other/a.md"}`}); d.Allowed || len(ask.calls) != 3 {
		t.Fatal("directory trust must not cover a sibling with the same prefix")
	}
}
