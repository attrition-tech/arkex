package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/attrition-tech/arkex/internal/config"
)

func TestScratchBoundaryAndModes(t *testing.T) {
	s, _, _ := testScope(t)
	first, second := t.TempDir(), t.TempDir()
	s.SetScratch(first)
	if err := os.Symlink(second, filepath.Join(first, "escape")); err != nil {
		t.Skip(err)
	}
	for _, tool := range []string{"read", "write", "edit", "bash"} {
		for _, tc := range []struct {
			path   string
			inside bool
		}{
			{filepath.Join(first, "ok"), true},
			{filepath.Join(second, "outside"), false},
			{filepath.Join(first, "escape", "outside"), false},
			{first + "-sibling/file", false},
		} {
			input := map[string]string{"path": tc.path}
			if tool == "bash" {
				input = map[string]string{"command": `echo hi > "` + filepath.ToSlash(tc.path) + `"`}
			}
			raw, _ := json.Marshal(input)
			call := ToolCall{Name: tool, Input: string(raw)}
			reach, _ := s.Classify(call)
			if (reach == ReachInside) != tc.inside {
				t.Fatalf("%s %s: %v", tool, tc.path, reach)
			}
			for _, mode := range []Mode{ModeAuto, ModePlan, ModeBuild} {
				p := NewModePolicy(mode, ConfigPolicy{Config: &config.Config{Permissions: map[string]config.Permission{tool: config.PermissionAsk}}})
				p.SetScope(s, nil)
				d, err := p.Decide(t.Context(), call)
				want := tc.inside && (mode == ModeAuto || mode == ModePlan && tool == "read")
				if err != nil || d.Allowed != want {
					t.Fatalf("%s %s allowed=%v want=%v err=%v", mode, tool, d.Allowed, want, err)
				}
				p.SetBase(ConfigPolicy{Config: &config.Config{Permissions: map[string]config.Permission{tool: config.PermissionDeny}}})
				d, _ = p.Decide(t.Context(), call)
				if d.Allowed {
					t.Fatal("scratch bypassed deny")
				}
			}
		}
	}
	s.SetScratch(second)
	for _, variable := range []string{"TMPDIR", "TMP", "TEMP"} {
		for _, tc := range []struct {
			suffix string
			inside bool
		}{{"/ok", true}, {"/../escape", false}} {
			raw, _ := json.Marshal(map[string]string{"command": "echo hi > \"${" + variable + "}" + tc.suffix + "\""})
			if reach, _ := s.Classify(ToolCall{Name: "bash", Input: string(raw)}); (reach == ReachInside) != tc.inside {
				t.Fatalf("scratch env boundary: %s %s", variable, tc.suffix)
			}
		}
	}
	raw, _ := json.Marshal(map[string]string{"path": filepath.Join(first, "old")})
	if reach, _ := s.Classify(ToolCall{Name: "write", Input: string(raw)}); reach == ReachInside {
		t.Fatal("previous scratch grant survived switch")
	}
}
