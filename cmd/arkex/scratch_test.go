//go:build unix

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/attrition-tech/arkex/internal/config"
)

// Exercise the CLI entry point, real HTTP adapter, policy, file tools and shell.
func TestCLIScratchWriteBashAndCleanup(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	for _, jsonMode := range []bool{false, true} {
		var dir string
		step := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Messages []struct {
					Role    string
					Content json.RawMessage
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			var system string
			_ = json.Unmarshal(body.Messages[0].Content, &system)
			_, tail, ok := strings.Cut(system, "Your temporary workspace is ")
			if !ok {
				t.Error("scratch instructions missing")
				w.WriteHeader(500)
				return
			}
			current, _, _ := strings.Cut(tail, ". Use this exact")
			if dir != "" && dir != current {
				t.Error("scratch changed during tool loop")
			}
			dir = current
			w.Header().Set("Content-Type", "text/event-stream")
			var delta any
			finish := "tool_calls"
			switch step {
			case 0:
				args, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, "from-write"), "content": "file-tool"})
				delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "w", "type": "function", "function": map[string]any{"name": "write", "arguments": string(args)}}}}
			case 1:
				data, err := os.ReadFile(filepath.Join(dir, "from-write"))
				if err != nil || string(data) != "file-tool" {
					t.Error("file tool failed", err)
				}
				args, _ := json.Marshal(map[string]string{"command": `test "$TMPDIR" = "$TMP" && test "$TMP" = "$TEMP" && printf shell > "$TMPDIR/from-bash"`})
				delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "b", "type": "function", "function": map[string]any{"name": "bash", "arguments": string(args)}}}}
			default:
				data, err := os.ReadFile(filepath.Join(dir, "from-bash"))
				if err != nil || string(data) != "shell" {
					t.Error("shell scratch environment failed", err)
				}
				delta = map[string]any{"content": "scratch verified"}
				finish = "stop"
			}
			chunk, _ := json.Marshal(map[string]any{"id": "c", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", chunk, finish)
			step++
		}))
		path, _ := config.GlobalPath()
		if err := config.SaveConnection(path, "fake", config.Connection{API: config.APIOpenAICompat, BaseURL: server.URL + "/v1", APIKey: "test", Models: []config.Model{{ID: "m"}}}, "fake/m"); err != nil {
			t.Fatal(err)
		}
		err := runRoot(t.Context(), rootFlags{print: "exercise scratch", mode: "auto", json: jsonMode}, nil)
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if step != 3 {
			t.Fatalf("requests=%d", step)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("CLI did not clean up scratch")
		}
		if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
			t.Fatal("scratch polluted workspace")
		}
	}
}
