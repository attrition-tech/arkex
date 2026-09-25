package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/session"
)

func TestPrintResumeUsesSavedModelUnlessOverridden(t *testing.T) {
	for _, override := range []string{"", "local/b:low"} {
		t.Run("override="+override, func(t *testing.T) {
			t.Setenv("ARKEX_HOME", t.TempDir())
			cwd := t.TempDir()
			t.Chdir(cwd)
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
			s.Update([]fantasy.Message{fantasy.NewUserMessage("saved prompt")}, "local/a:high", "build", 0, 0)
			effort := "high"
			s.Effort = &effort
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
			if err := runRoot(t.Context(), rootFlags{resume: s.ID, model: override, print: "continue", mode: "build", noTools: true}, nil); err != nil {
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
	s.Update([]fantasy.Message{fantasy.NewUserMessage("saved")}, "local/a", "build", 9, 4)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	for i, jsonMode := range []bool{false, true} {
		if err := runRoot(t.Context(), rootFlags{resume: s.ID, print: "continue", json: jsonMode, mode: "build", noTools: true}, nil); err != nil {
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
