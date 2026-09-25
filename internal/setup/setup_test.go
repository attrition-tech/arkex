package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/attrition-tech/arkex/internal/config"
)

func TestSuggestName(t *testing.T) {
	cases := map[string]string{
		"https://llm.matrx.example/v1":        "matrx",
		"https://api.openai.com/v1":           "openai",
		"http://localhost:11434/v1":           "local",
		"http://127.0.0.1:8000/v1":            "local",
		"https://inference.acme.co.uk/v1":     "acme",
		"https://my-gateway.internal:8443/v1": "my-gateway",
		"http://gpu-box/v1":                   "gpu-box",
		"nonsense":                            "custom",
	}
	for in, want := range cases {
		if got := SuggestName(in); got != want {
			t.Errorf("SuggestName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	if got, err := NormalizeBaseURL("  https://x.example/v1/ "); err != nil || got != "https://x.example/v1" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "x.example/v1", "ftp://x/v1"} {
		if _, err := NormalizeBaseURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestListModels(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		if gotAuth != "Bearer sk-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"zeta"},{"id":"alpha"},{"id":"alpha"},{"id":""}]}`))
	}))
	defer srv.Close()

	ids, err := ListModels(context.Background(), nil, srv.URL+"/v1", "sk-1", "arkex/test")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q", gotPath)
	}
	if len(ids) != 2 || ids[0] != "alpha" || ids[1] != "zeta" {
		t.Errorf("ids = %v (want sorted, de-duplicated, no blanks)", ids)
	}
	if _, err := ListModels(context.Background(), nil, srv.URL+"/v1", "wrong", ""); err == nil {
		t.Error("401 not reported")
	}

	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"only"}]`))
	}))
	defer bare.Close()
	if ids, err := ListModels(context.Background(), nil, bare.URL, "", ""); err != nil || len(ids) != 1 {
		t.Errorf("bare array: %v %v", ids, err)
	}
}

func TestGuessModel(t *testing.T) {
	custom := Other(config.KindLLMServer)
	cases := []struct {
		id        string
		reasoning bool
		thinking  config.Thinking
	}{
		{"DeepSeek-V4.1-Flash", true, config.ThinkingDeepSeek},
		{"Qwen3.8-Flash-Next-FP8", true, config.ThinkingQwen},
		{"qwen2.5-coder-7b", false, ""},
		{"gpt-5.6-sol", true, config.ThinkingReasoningEffort},
		{"llama-3.3-70b-instruct", false, ""},
		{"claude-opus-5", true, ""},
	}
	for _, c := range cases {
		m := GuessModel(c.id, custom)
		if m.Reasoning != c.reasoning || m.Compat.Thinking != c.thinking {
			t.Errorf("GuessModel(%q) = reasoning %v thinking %q; want %v %q", c.id, m.Reasoning, m.Compat.Thinking, c.reasoning, c.thinking)
		}
	}
	// Provider-level preset compat must not be duplicated per model.
	var openrouter Preset
	for _, p := range Presets {
		if p.ID == "openrouter" {
			openrouter = p
		}
	}
	if m := GuessModel("deepseek/deepseek-r1", openrouter); m.Compat.Thinking != "" || !m.Reasoning {
		t.Errorf("openrouter deepseek = %+v", m)
	}
}

func TestDetectPreset(t *testing.T) {
	cases := map[string]string{
		"https://openrouter.ai/api/v1": "openrouter",
		"https://API.OPENAI.com/v1":    "openai",
		"http://localhost:11434/v1":    "ollama",
		"http://localhost:8000/v1":     "vllm",
		"http://localhost:9999/v1":     "other-server", // loopback, unknown port
		"http://127.0.0.1:8765/v1":     "other-server",
		"https://llm.matrx.example/v1": "other-api",
		"not a url":                    "other-server",
	}
	for in, want := range cases {
		if got := DetectPreset(in).ID; got != want {
			t.Errorf("DetectPreset(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestPresetsFor(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Presets {
		if seen[p.ID] {
			t.Errorf("duplicate preset id %q", p.ID)
		}
		seen[p.ID] = true
		if p.Kind == "" {
			t.Errorf("preset %q has no kind", p.ID)
		}
	}
	for _, kind := range []config.Kind{config.KindLLMServer, config.KindAPIKey} {
		ps := PresetsFor(kind)
		if len(ps) < 2 {
			t.Fatalf("%s: %d presets", kind, len(ps))
		}
		for _, p := range ps {
			if p.Kind != kind {
				t.Errorf("%s list contains %q of kind %s", kind, p.ID, p.Kind)
			}
		}
		if last := ps[len(ps)-1]; last.Label != "Other" || last.BaseURL != "" {
			t.Errorf("%s: last preset %+v is not the Other catch-all", kind, last)
		}
		if Other(kind).ID != ps[len(ps)-1].ID {
			t.Errorf("Other(%s) = %s", kind, Other(kind).ID)
		}
	}
	if FindPreset("openai").BaseURL != "https://api.openai.com/v1" {
		t.Error("FindPreset(openai)")
	}
	if FindPreset("nope").ID != "other-server" {
		t.Errorf("FindPreset(nope) = %s", FindPreset("nope").ID)
	}
}

func TestIsPlainHTTP(t *testing.T) {
	cases := map[string]bool{
		"http://llm.example.io/v1":    true,
		"https://llm.example.io/v1":   false,
		"http://localhost:11434/v1":   false,
		"http://127.0.0.1:8765/v1":    false,
		"http://192.168.1.20:8000/v1": false,
		"http://[::1]:8000/v1":        false,
		"http://8.8.8.8/v1":           true,
	}
	for u, want := range cases {
		if got := IsPlainHTTP(u); got != want {
			t.Errorf("IsPlainHTTP(%q) = %v, want %v", u, got, want)
		}
	}
}
