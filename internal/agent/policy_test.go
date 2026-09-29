package agent

import (
	"testing"

	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/tools"
)

func TestConfigPolicyAvailability(t *testing.T) {
	for _, tool := range tools.Default("").All() {
		for _, denied := range []bool{false, true} {
			cfg := &config.Config{Permissions: map[string]config.Permission{}}
			if denied {
				cfg.Permissions[tool.Name()] = config.PermissionDeny
			}
			p := ConfigPolicy{Config: cfg}
			d, err := p.Decide(t.Context(), ToolCall{Name: tool.Name()})
			if err != nil || d.Allowed == denied {
				t.Fatalf("%s denied=%v: %+v %v", tool.Name(), denied, d, err)
			}
		}
	}
	for _, p := range []ConfigPolicy{{}, {Config: &config.Config{}}} {
		if d, _ := p.Decide(t.Context(), ToolCall{Name: "unknown"}); d.Allowed {
			t.Fatal("unknown tool was allowed")
		}
	}
}
