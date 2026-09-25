package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/prompt"
	"github.com/attrition-tech/arkex/internal/provider"
	"github.com/attrition-tech/arkex/internal/tools"
)

func instructionFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		".git/HEAD": "fixture", "AGENTS.md": "root rule",
		"sub/AGENTS.md": "sub rule", "sub/deep/AGENTS.md": "deep rule",
		"other/AGENTS.md": "unrelated rule",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestInstructionsBeforeWriteAndInNextRequest(t *testing.T) {
	root := instructionFixture(t)
	target := filepath.Join(root, "sub", "deep", "new.txt")
	toolCall := func(w http.ResponseWriter) {
		sse(w, delta(`{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"write","arguments":"{\"path\":\"sub/deep/new.txt\",\"content\":\"done\"}"}}]}`, ""), delta(`{}`, "tool_calls"))
	}
	fake := &fakeServer{script: []func(http.ResponseWriter){toolCall, func(w http.ResponseWriter) {
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Error("write executed before model saw instructions")
		}
		toolCall(w)
	}, func(w http.ResponseWriter) { sse(w, delta(`{"content":"finished"}`, "stop")) }}}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	model, err := provider.Open(t.Context(), config.ModelRef{Conn: config.Connection{BaseURL: server.URL}, Model: config.Model{ID: "test"}}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Model: model, Tools: tools.Default(root), Policy: AllowAll{}, System: prompt.Build(prompt.Options{Cwd: root})}
	if err := a.Run(t.Context(), "write it", nil); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "done" {
		t.Fatalf("write: %q %v", body, err)
	}
	request, _ := json.Marshal(fake.requests[1])
	text := string(request)
	if !strings.Contains(text, "sub rule") || !strings.Contains(text, "deep rule") || strings.Contains(text, "unrelated rule") {
		t.Fatalf("wrong instructions: %s", text)
	}
	if strings.Index(text, "sub rule") > strings.Index(text, "deep rule") {
		t.Fatal("instructions not outermost first")
	}
	if strings.Count(text, "root rule") != 1 {
		t.Fatal("root instructions duplicated")
	}
}

func TestInstructionsScopeRefreshAndPermissions(t *testing.T) {
	root := instructionFixture(t)
	a := &Agent{Tools: tools.Default(root), Policy: AllowAll{}, System: prompt.Build(prompt.Options{Cwd: root})}
	call := ToolCall{ID: "c", Name: "bash", Input: `{"workdir":"sub/deep","command":"echo should-not-run"}`}
	res, bad, err := a.runOne(t.Context(), call, func(Event) {})
	if err != nil || bad || !strings.Contains(res.Output, "Action not executed") {
		t.Fatalf("%+v %v %v", res, bad, err)
	}
	a.SetMessages([]fantasy.Message{{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "c", Output: fantasy.ToolResultOutputContentText{Text: res.Output}}}}})
	tool, _ := a.Tools.Get("bash")
	if text, err := a.toolInstructions(t.Context(), tool, json.RawMessage(call.Input)); err != nil || text != "" {
		t.Fatalf("duplicate: %q %v", text, err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "AGENTS.md"), []byte("sub rule\nchanged rule"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := a.toolInstructions(t.Context(), tool, json.RawMessage(call.Input))
	if err != nil || !strings.Contains(updated, "changed rule") {
		t.Fatalf("refresh: %q %v", updated, err)
	}
	a.messages = append(a.messages, fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "d", Output: fantasy.ToolResultOutputContentText{Text: updated}}}})
	if err := os.WriteFile(filepath.Join(root, "sub", "AGENTS.md"), []byte("sub rule"), 0o600); err != nil {
		t.Fatal(err)
	}
	if text, err := a.toolInstructions(t.Context(), tool, json.RawMessage(call.Input)); err != nil || !strings.Contains(text, "sub rule") {
		t.Fatalf("refresh: %q %v", text, err)
	}
	a.SetMessages(nil) // A branch/compaction no longer contains the guidance.
	if text, _ := a.toolInstructions(t.Context(), tool, json.RawMessage(call.Input)); !strings.Contains(text, "deep rule") {
		t.Fatal("lost instructions not reloaded")
	}
	a.Policy = PolicyFunc(func(_ context.Context, c ToolCall) (Decision, error) {
		return Decision{Allowed: c.Name != "read", Reason: "read denied"}, nil
	})
	res, bad, err = a.runOne(t.Context(), call, func(Event) {})
	if err != nil || !bad || strings.Contains(res.Output, "changed rule") || !strings.Contains(res.Output, "read denied") {
		t.Fatalf("permission bypass: %+v %v %v", res, bad, err)
	}
}

func TestInstructionsReadAndEditTargets(t *testing.T) {
	root := instructionFixture(t)
	for _, name := range []string{"read", "edit"} {
		t.Run(name, func(t *testing.T) {
			a := &Agent{Tools: tools.Default(root), Policy: AllowAll{}, System: prompt.Build(prompt.Options{Cwd: root})}
			res, bad, err := a.runOne(t.Context(), ToolCall{Name: name, Input: `{"path":"sub/deep/new.txt"}`}, func(Event) {})
			if err != nil || bad || !strings.Contains(res.Output, "deep rule") {
				t.Fatalf("instructions missing: %+v %v %v", res, bad, err)
			}
		})
	}
}
