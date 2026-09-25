package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/dantearo/arkex/internal/config"
	"github.com/dantearo/arkex/internal/prompt"
	"github.com/dantearo/arkex/internal/tools"
)

func TestToolCapabilitiesAgreeAcrossPolicyAndPrompt(t *testing.T) {
	p := NewModePolicy(ModePlan, AllowAll{})
	cfg := &config.Config{}
	for _, tc := range []struct {
		name                 string
		registered, readOnly bool
	}{
		{"read", true, true},
		{"write", true, false},
		{"edit", true, false},
		{"bash", true, false},
		{"grep", false, false},
		{"find", false, false},
		{"ls", false, false},
		{"future-tool", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tools.IsBuiltin(tc.name) != tc.registered || tools.IsReadOnly(tc.name) != tc.readOnly {
				t.Fatal("incorrect registered capability")
			}
			d, err := p.Decide(context.Background(), ToolCall{Name: tc.name})
			if err != nil || d.Allowed != tc.readOnly {
				t.Fatalf("plan decision: %+v, %v", d, err)
			}
			want := config.PermissionAsk
			if tc.readOnly {
				want = config.PermissionAllow
			}
			if got := cfg.Permission(tc.name); got != want {
				t.Fatalf("default permission %v, want %v", got, want)
			}
			if tc.registered {
				tool, _ := tools.Default("").Get(tc.name)
				note := prompt.PlanNote(tools.NewRegistry(tool))
				label := "Registered tools refused by this mode: "
				if tc.readOnly {
					label = "Read-only tools permitted by this mode: "
				}
				if !strings.Contains(note, label+tc.name+".") {
					t.Fatalf("prompt disagrees with policy: %s", note)
				}
			}
		})
	}
}

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
