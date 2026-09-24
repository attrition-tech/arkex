package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestConcurrentConfigEditsPreserveEveryChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			err := edit(path, func(root map[string]any) error {
				root[fmt.Sprintf("setting%d", i)] = i
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		if root[fmt.Sprintf("setting%d", i)] != float64(i) {
			t.Fatalf("setting %d lost", i)
		}
	}
	backup, err := os.ReadFile(BackupPath(path))
	if err != nil || !json.Valid(backup) {
		t.Fatalf("invalid backup: %v", err)
	}
}

func TestSaveProviderCreatesAndMerges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	p := Connection{Kind: KindLLMServer, API: APIOpenAICompat, BaseURL: "https://x/v1", APIKey: "$K", Models: []Model{{ID: "m1", Reasoning: true, Compat: Compat{Thinking: ThinkingQwen}}}}
	if err := SaveConnection(path, "x", p, "x/m1"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}

	// Hand-edit: add a key arkex knows nothing about, plus permissions.
	var root map[string]any
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &root)
	root["someFutureKey"] = map[string]any{"keep": true}
	root["permissions"] = map[string]any{"bash": "allow"}
	b, _ = json.Marshal(root)
	_ = os.WriteFile(path, b, 0o600)

	// Second provider; default unchanged.
	if err := SaveConnection(path, "y", Connection{Kind: KindLLMServer, API: APIOpenAICompat, BaseURL: "https://y/v1", Models: []Model{{ID: "m2"}}}, ""); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ARKEX_HOME", filepath.Dir(path))
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Connections) != 2 || cfg.Connections["x"].Models[0].Compat.Thinking != ThinkingQwen || cfg.Connections["y"].BaseURL != "https://y/v1" {
		t.Fatalf("providers = %+v", cfg.Connections)
	}
	if cfg.Default != "x/m1" {
		t.Errorf("default = %q", cfg.Default)
	}
	if cfg.Permissions["bash"] != PermissionAllow {
		t.Errorf("permissions lost: %+v", cfg.Permissions)
	}
	b, _ = os.ReadFile(path)
	_ = json.Unmarshal(b, &root)
	if _, ok := root["someFutureKey"]; !ok {
		t.Error("unknown key dropped on save")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}

func TestEditors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("ARKEX_HOME", filepath.Dir(path))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	load := func() *Config {
		t.Helper()
		cfg, err := Load("")
		must(err)
		return cfg
	}
	must(SaveConnection(path, "x", Connection{Kind: KindLLMServer, API: APIOpenAICompat, BaseURL: "https://x/v1", Models: []Model{{ID: "a"}, {ID: "b"}}}, "x/a"))
	must(SaveConnection(path, "y", Connection{Kind: KindLLMServer, API: APIOpenAICompat, BaseURL: "https://y/v1", Models: []Model{{ID: "c"}}}, ""))

	// Disable a model: Resolve refuses it, the sibling still works.
	must(SetDisabled(path, "x", "b", true))
	cfg := load()
	if !cfg.Connections["x"].Models[1].Disabled {
		t.Fatal("model b not disabled")
	}
	if _, err := cfg.Resolve("x/b"); err == nil {
		t.Error("Resolve accepted a disabled model")
	}
	if _, err := cfg.Resolve("x/a"); err != nil {
		t.Errorf("Resolve(x/a) = %v", err)
	}
	must(SetDisabled(path, "x", "b", false))
	if load().Connections["x"].Models[1].Disabled {
		t.Fatal("model b still disabled")
	}

	// Disable a provider: every model under it is refused.
	must(SetDisabled(path, "y", "", true))
	if _, err := load().Resolve("y/c"); err == nil {
		t.Error("Resolve accepted a model of a disabled provider")
	}

	// Removing the default model clears the default; the other model stays.
	must(RemoveModel(path, "x", "a"))
	cfg = load()
	if cfg.Default != "" {
		t.Errorf("default = %q, want cleared", cfg.Default)
	}
	if len(cfg.Connections["x"].Models) != 1 || cfg.Connections["x"].Models[0].ID != "b" {
		t.Errorf("models = %+v", cfg.Connections["x"].Models)
	}

	must(SetDefault(path, "x/b"))
	if load().Default != "x/b" {
		t.Error("SetDefault did not stick")
	}
	must(RemoveConnection(path, "x"))
	cfg = load()
	if _, ok := cfg.Connections["x"]; ok || cfg.Default != "" || len(cfg.Connections) != 1 {
		t.Errorf("after RemoveConnection: default=%q providers=%v", cfg.Default, cfg.Connections)
	}
	if err := RemoveConnection(path, "nope"); err == nil {
		t.Error("RemoveConnection of unknown id succeeded")
	}
	if err := RemoveModel(path, "y", "nope"); err == nil {
		t.Error("RemoveModel of unknown id succeeded")
	}
}

func TestUpdateConnectionRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("ARKEX_HOME", filepath.Dir(path))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	conn := Connection{Kind: KindLLMServer, API: APIOpenAICompat, BaseURL: "http://localhost:1/v1", Models: []Model{{ID: "a"}}}
	must(SaveConnection(path, "old", conn, "old/a"))
	must(edit(path, func(root map[string]any) error {
		root["profiles"] = map[string]any{"fast": map[string]any{"model": "old/a"}, "other": map[string]any{"model": "x/a"}}
		return nil
	}))

	conn.BaseURL = "http://localhost:2/v1"
	must(UpdateConnection(path, "old", "new", conn, ""))
	cfg, err := Load("")
	must(err)
	if _, still := cfg.Connections["old"]; still {
		t.Error("old id kept")
	}
	if got := cfg.Connections["new"].BaseURL; got != "http://localhost:2/v1" {
		t.Errorf("baseUrl = %q", got)
	}
	if cfg.Default != "new/a" {
		t.Errorf("default = %q, want new/a", cfg.Default)
	}
	if cfg.Profiles["fast"].Model != "new/a" || cfg.Profiles["other"].Model != "x/a" {
		t.Errorf("profiles = %+v", cfg.Profiles)
	}
	// Same id: plain replace; unknown old id: error.
	must(UpdateConnection(path, "new", "new", conn, "new/a"))
	if err := UpdateConnection(path, "missing", "z", conn, ""); err == nil {
		t.Error("renaming an unknown connection succeeded")
	}
}
