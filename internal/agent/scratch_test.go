package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/tools"
)

func TestScratchWritesStayConfinedEvenWithAllowPolicy(t *testing.T) {
	root, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	write := &tools.Write{Root: root, TempDir: first}
	a := &Agent{Tools: tools.NewRegistry(write), Policy: ConfigPolicy{Config: &config.Config{}}}
	call := func(dir string) bool {
		args, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, "file"), "content": "allowed"})
		_, bad, err := a.runOne(t.Context(), ToolCall{Name: "write", Input: string(args)}, func(Event) {})
		if err != nil {
			t.Fatal(err)
		}
		return !bad
	}
	if !call(first) || call(second) {
		t.Fatal("wrong initial scratch boundary")
	}
	write.TempDir = second
	if call(first) || !call(second) {
		t.Fatal("previous scratch remains writable")
	}
	a.Policy = ConfigPolicy{Config: &config.Config{Permissions: map[string]config.Permission{"write": config.PermissionDeny}}}
	if call(second) {
		t.Fatal("scratch bypassed explicit deny")
	}
	data, err := os.ReadFile(filepath.Join(second, "file"))
	if err != nil || string(data) != "allowed" {
		t.Fatalf("incorrect scratch content: %s %v", data, err)
	}
}
