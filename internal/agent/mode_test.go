package agent

import (
	"context"
	"testing"
)

type recordPolicy struct{ calls int }

func (r *recordPolicy) Decide(context.Context, ToolCall) (Decision, error) {
	r.calls++
	return Decision{Allowed: false, Reason: "base"}, nil
}

func TestModePolicy(t *testing.T) {
	base := &recordPolicy{}
	p := NewModePolicy(ModePlan, base)
	ctx := context.Background()

	d, _ := p.Decide(ctx, ToolCall{Name: "read"})
	if !d.Allowed {
		t.Fatalf("plan should allow read: %+v", d)
	}
	d, _ = p.Decide(ctx, ToolCall{Name: "bash"})
	if d.Allowed || base.calls != 0 {
		t.Fatalf("plan should deny bash without consulting base: %+v calls=%d", d, base.calls)
	}

	p.SetMode(ModeBuild)
	d, _ = p.Decide(ctx, ToolCall{Name: "bash"})
	if d.Allowed || d.Reason != "base" || base.calls != 1 {
		t.Fatalf("build should defer to base: %+v calls=%d", d, base.calls)
	}

	p.SetMode(ModeAuto)
	d, _ = p.Decide(ctx, ToolCall{Name: "bash"})
	if !d.Allowed || base.calls != 1 {
		t.Fatalf("auto should allow without base: %+v calls=%d", d, base.calls)
	}
}

func TestModeCycleAndParse(t *testing.T) {
	if got := ModeBuild.Next().Next().Next(); got != ModeBuild {
		t.Fatalf("cycle of three should return to build, got %s", got)
	}
	if ModeBuild.Next() != ModePlan {
		t.Fatalf("build should cycle to plan, got %s", ModeBuild.Next())
	}
	if m, err := ParseMode("PLAN"); err != nil || m != ModePlan {
		t.Fatalf("ParseMode(PLAN) = %q, %v", m, err)
	}
	if _, err := ParseMode("yolo"); err == nil {
		t.Fatal("ParseMode(yolo) should fail")
	}
}
