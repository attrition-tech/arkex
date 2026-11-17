package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/session"
)

func TestPrintResumeUsesSavedModelUnlessOverridden(t *testing.T) {
	for _, override := range []string{"", "local/b:low"} {
		t.Run("override="+override, func(t *testing.T) {
			t.Setenv("ARKEX_HOME", t.TempDir())
			// Session identity must retain the launch path used by released
			// versions, even when tool confinement resolves it to a real path.
			cwd := filepath.Join(t.TempDir(), "workspace-link")
			if err := os.Symlink(t.TempDir(), cwd); err != nil {
				t.Fatal(err)
			}
			t.Chdir(cwd)
			t.Setenv("PWD", cwd)
			requests := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				requests <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"}}]}\n\ndata: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			path, _ := config.GlobalPath()
			conn := config.Connection{API: config.APIOpenAICompat, BaseURL: server.URL + "/v1", APIKey: "test", Compat: config.Compat{Thinking: config.ThinkingReasoningEffort}, Models: []config.Model{{ID: "a", ReasoningLevels: []string{"high"}}, {ID: "b", ReasoningLevels: []string{"low"}}}}
			if err := config.SaveConnection(path, "local", conn, "missing/default"); err != nil {
				t.Fatal(err)
			}
			s := session.New(cwd)
			s.Update([]fantasy.Message{fantasy.NewUserMessage("saved prompt")}, session.Usage{Model: "local/a:high"})
			effort := "high"
			s.Effort = &effort
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
			if err := runRoot(t.Context(), rootFlags{resume: s.ID, model: override, print: "continue", noTools: true}, nil); err != nil {
				t.Fatal(err)
			}
			wantModel, wantEffort := "a", "high"
			if override != "" {
				wantModel, wantEffort = "b", "low"
			}
			select {
			case body := <-requests:
				if body["model"] != wantModel || body["reasoning_effort"] != wantEffort {
					t.Fatalf("wrong model/effort: %v", body)
				}
			default:
				t.Fatal("no completion request")
			}
			saved, err := session.Load(cwd, s.ID)
			if err != nil || saved.Model != "local/"+wantModel+":"+wantEffort {
				t.Fatalf("wrong saved model: %+v, %v", saved, err)
			}
		})
	}
}

func TestPrintAndJSONResumeAccumulateUsage(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"}}]}\n\ndata: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":6,\"total_tokens\":23}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	path, _ := config.GlobalPath()
	if err := config.SaveConnection(path, "local", config.Connection{API: config.APIOpenAICompat, BaseURL: server.URL + "/v1", APIKey: "test", Models: []config.Model{{ID: "a"}}}, "local/a"); err != nil {
		t.Fatal(err)
	}
	s := session.New(cwd)
	s.Update([]fantasy.Message{fantasy.NewUserMessage("saved")}, session.Usage{Model: "local/a", Input: 9, Output: 4})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	for i, jsonMode := range []bool{false, true} {
		if err := runRoot(t.Context(), rootFlags{resume: s.ID, print: "continue", json: jsonMode, noTools: true}, nil); err != nil {
			t.Fatal(err)
		}
		saved, err := session.Load(cwd, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.UsageIn != 9+int64(i+1)*17 || saved.UsageOut != 4+int64(i+1)*6 || saved.LastInput != 17 {
			t.Fatalf("json=%v: usage %d/%d, context %d", jsonMode, saved.UsageIn, saved.UsageOut, saved.LastInput)
		}
	}
}

func TestPrintResumeRecoversArchivedHistoryThroughTools(t *testing.T) {
	for _, mode := range []string{"text", "attachment", "denied"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("ARKEX_HOME", t.TempDir())
			cwd := t.TempDir()
			t.Chdir(cwd)
			const original = "AZ-719: do not publish without approval."
			requests := make(chan string, 2)
			var count atomic.Int32
			args := `{"query":"AZ-719"}`
			if mode == "attachment" {
				args = `{"message":1,"attachment":2}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				requests <- string(body)
				w.Header().Set("Content-Type", "text/event-stream")
				if count.Add(1) == 1 {
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"h1\",\"type\":\"function\",\"function\":{\"name\":\"history\",\"arguments\":%q}}]}}]}\n\ndata: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", args)
				} else {
					_, _ = fmt.Fprint(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Checked saved history.\"}}]}\n\ndata: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}
			}))
			defer server.Close()
			cfg := config.Config{Connections: map[string]config.Connection{"local": {API: config.APIOpenAICompat, BaseURL: server.URL + "/v1", Models: []config.Model{{ID: "m"}}}}, Default: "local/m"}
			if mode == "denied" {
				cfg.Permissions = map[string]config.Permission{"history": config.PermissionDeny}
			}
			path, _ := config.GlobalPath()
			data, _ := json.Marshal(cfg)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			s := session.New(cwd)
			s.Model = "local/m"
			file := fantasy.FilePart{Filename: "original.png", MediaType: "image/png", Data: []byte{0, 17, 255}}
			if _, err := s.SaveContext(t.Context(), []fantasy.Message{fantasy.NewUserMessage(strings.Repeat("old text\n", 1000)+original, file)}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SaveContext(t.Context(), agent.CompactedMessages("Earlier details can be recovered from history.")); err != nil {
				t.Fatal(err)
			}
			if err := runRoot(t.Context(), rootFlags{resume: s.ID, print: "Recover the original evidence"}, nil); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 {
				t.Fatal("history tool did not complete its round trip")
			}
			first, second := <-requests, <-requests
			if strings.Contains(first, original) || strings.Contains(first, "ABH/") {
				t.Fatal("archive content was reinserted without a retrieval request")
			}
			want := original
			switch mode {
			case "attachment":
				want = "data:image/png;base64,ABH/"
			case "denied":
				want = "denied"
				if strings.Contains(second, original) {
					t.Fatal("history bypassed deny permission")
				}
			}
			if !strings.Contains(second, want) {
				t.Fatalf("missing historical result %q in provider request: %s", want, second)
			}
		})
	}
}
