// Package config loads and merges arkex configuration files.
//
// Global config lives at ~/.arkex/config.json; a project may add
// .arkex/config.json in the working directory, which overrides the global
// file key by key. String values support "$ENV", "${ENV}" interpolation and
// a leading "!command" that is executed at resolve time.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dantearo/arkex/internal/tools"
)

// Kind says what a connection is, which decides how it is set up and shown:
// a server you run (URL first, key optional), a hosted vendor you pay per
// token (key first, URL from a preset), a consumer subscription signed in
// with OAuth, or an external agent runtime arkex fronts.
type Kind string

const (
	KindLLMServer    Kind = "llm-server"
	KindAPIKey       Kind = "api-key"
	KindSubscription Kind = "subscription"
	KindRuntime      Kind = "runtime"
)

// Label is the human name of the kind.
func (k Kind) Label() string {
	switch k {
	case KindLLMServer:
		return "LLM server"
	case KindAPIKey:
		return "API key"
	case KindSubscription:
		return "Subscription"
	case KindRuntime:
		return "Runtime"
	}
	return string(k)
}

// API identifies which fantasy provider implementation a connection uses.
type API string

const (
	APIOpenAICompat API = "openai-compat"
	APIOpenAI       API = "openai"
	APIAnthropic    API = "anthropic"
	APIGoogle       API = "google"
)

// Thinking selects how the compat layer expresses reasoning controls.
type Thinking string

const (
	ThinkingNone             Thinking = ""
	ThinkingReasoningEffort  Thinking = "reasoning_effort"
	ThinkingDeepSeek         Thinking = "deepseek"
	ThinkingQwen             Thinking = "qwen"
	ThinkingQwenChatTemplate Thinking = "qwen-chat-template"
	ThinkingOpenRouter       Thinking = "openrouter"
)

// Compat holds provider/model quirks the compat layer translates into
// request fields. Model-level values override provider-level values.
type Compat struct {
	// Thinking preset; see Thinking constants.
	Thinking Thinking `json:"thinking,omitempty"`
	// ExtraBody is merged verbatim into the request root. Keys here win.
	ExtraBody map[string]any `json:"extraBody,omitempty"`
}

// Merge returns c overlaid with o (o wins where set).
func (c Compat) Merge(o Compat) Compat {
	out := c
	if o.Thinking != "" {
		out.Thinking = o.Thinking
	}
	if len(o.ExtraBody) > 0 {
		out.ExtraBody = map[string]any{}
		for k, v := range c.ExtraBody {
			out.ExtraBody[k] = v
		}
		for k, v := range o.ExtraBody {
			out.ExtraBody[k] = v
		}
	}
	return out
}

// Cost is USD per million tokens.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// Model describes one model served by a provider.
type Model struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Reasoning bool   `json:"reasoning,omitempty"`
	// ReasoningLevels advertises selectable controls, including "off" or
	// "on" only when supported. Empty means capabilities are unknown.
	ReasoningLevels  []string `json:"reasoningLevels,omitempty"`
	ReasoningDefault string   `json:"reasoningDefault,omitempty"`
	ContextWindow    int      `json:"contextWindow,omitempty"`
	MaxTokens        int      `json:"maxTokens,omitempty"`
	Cost             *Cost    `json:"cost,omitempty"`
	Compat           Compat   `json:"compat,omitempty"`
	// Disabled keeps the entry in the file but hides it from selection.
	Disabled bool `json:"disabled,omitempty"`
}

// Connection describes one place models come from and the models it
// serves. Its id (the map key) is the first half of a "conn/model"
// selector.
type Connection struct {
	// Subscription selects the OAuth provider; empty preserves legacy ChatGPT connections.
	Subscription string            `json:"subscription,omitempty"`
	Name         string            `json:"name,omitempty"`
	Kind         Kind              `json:"kind"`
	API          API               `json:"api"`
	BaseURL      string            `json:"baseUrl"`
	APIKey       string            `json:"apiKey,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Compat       Compat            `json:"compat,omitempty"`
	Models       []Model           `json:"models"`
	// Disabled keeps the connection configured but makes all of its models
	// unavailable for selection.
	Disabled bool `json:"disabled,omitempty"`
}

// EnabledModels returns the models that can be selected.
func (c Connection) EnabledModels() []Model {
	if c.Disabled {
		return nil
	}
	var out []Model
	for _, m := range c.Models {
		if !m.Disabled {
			out = append(out, m)
		}
	}
	return out
}

// Profile names a model plus a thinking level.
type Profile struct {
	Model    string `json:"model"` // "provider/model-id"
	Thinking string `json:"thinking,omitempty"`
}

// Permission is a policy decision default for a tool.
type Permission string

const (
	PermissionAsk   Permission = "ask"
	PermissionAllow Permission = "allow"
	PermissionDeny  Permission = "deny"
)

// UI holds interface preferences.
type UI struct {
	// Mouse enables mouse reporting in the TUI (default on).
	Mouse *bool `json:"mouse,omitempty"`
	// Theme names a colour theme; empty means the default.
	Theme string `json:"theme,omitempty"`
	// Bell enables terminal notifications for attention and run completion.
	// Off by default; the terminal controls the sound or visual alert.
	Bell *bool `json:"bell,omitempty"`
}

// MouseOn reports the effective mouse setting.
func (u UI) MouseOn() bool { return u.Mouse == nil || *u.Mouse }

// Config is the merged arkex configuration.
type Config struct {
	// Version is the file format version; see CurrentVersion.
	Version     int                   `json:"version,omitempty"`
	Connections map[string]Connection `json:"connections"`
	Profiles    map[string]Profile    `json:"profiles,omitempty"`
	Default     string                `json:"default,omitempty"`
	Permissions map[string]Permission `json:"permissions,omitempty"`
	UI          UI                    `json:"ui,omitempty"`
}

// Dir returns the global config directory (~/.arkex or $ARKEX_HOME).
func Dir() (string, error) {
	if d := os.Getenv("ARKEX_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".arkex"), nil
}

// GlobalPath returns the path of the global config file.
func GlobalPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// ProjectPath returns the project-local config path for cwd.
func ProjectPath(cwd string) string {
	return filepath.Join(cwd, ".arkex", "config.json")
}

// Load reads the global config and, if present, the project config, and
// merges them. A missing global file yields an empty Config, not an error.
func Load(cwd string) (*Config, error) {
	cfg := &Config{Connections: map[string]Connection{}, Profiles: map[string]Profile{}, Permissions: map[string]Permission{}}

	gp, err := GlobalPath()
	if err != nil {
		return nil, err
	}
	if err := readInto(gp, cfg); err != nil {
		return nil, err
	}
	if cwd != "" {
		if err := readProjectInto(ProjectPath(cwd), cfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func readInto(path string, cfg *Config) error {
	part, err := readPart(path)
	if err != nil || part == nil {
		return err
	}
	cfg.merge(part)
	return nil
}

// readProjectInto merges a project config, which is repository content and
// so untrusted: it may pick a default model, profiles and UI settings, and
// may tighten permissions, but it may not define connections (they carry
// credentials, run "!commands", and choose where tokens are sent) or loosen
// a permission the global config asks or denies.
func readProjectInto(path string, cfg *Config) error {
	part, err := readPart(path)
	if err != nil || part == nil {
		return err
	}
	if len(part.Connections) > 0 {
		return fmt.Errorf("%s: project configs may not define connections; move them to the global config (arkex config path)", path)
	}
	for tool, p := range part.Permissions {
		if !p.stricter(cfg.Permission(tool)) {
			delete(part.Permissions, tool)
		}
	}
	cfg.merge(part)
	return nil
}

// readPart decodes one config file; a missing file yields nil, nil.
func readPart(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var part Config
	if err := decode(path, b, &part); err != nil {
		return nil, err
	}
	return &part, nil
}

// stricter reports whether p asks more of the user than other does.
func (p Permission) stricter(other Permission) bool {
	rank := func(x Permission) int {
		switch x {
		case PermissionDeny:
			return 2
		case PermissionAsk:
			return 1
		}
		return 0
	}
	return rank(p) > rank(other)
}

func (c *Config) merge(o *Config) {
	c.Version = CurrentVersion
	if o.UI.Mouse != nil {
		c.UI.Mouse = o.UI.Mouse
	}
	if o.UI.Bell != nil {
		c.UI.Bell = o.UI.Bell
	}
	if o.UI.Theme != "" {
		c.UI.Theme = o.UI.Theme
	}
	for k, v := range o.Connections {
		c.Connections[k] = v
	}
	for k, v := range o.Profiles {
		c.Profiles[k] = v
	}
	for k, v := range o.Permissions {
		c.Permissions[k] = v
	}
	if o.Default != "" {
		c.Default = o.Default
	}
}

// ModelRef is a resolved "conn/model" pair.
type ModelRef struct {
	ConnID   string
	Conn     Connection
	Model    Model
	Thinking string
}

// Compat returns the effective compat for the model.
func (r ModelRef) Compat() Compat { return r.Conn.Compat.Merge(r.Model.Compat) }

// String returns "provider/model".
func (r ModelRef) String() string { return r.ConnID + "/" + r.Model.ID }

// Resolve turns a user-facing selector into a ModelRef. The selector may be
// a profile name, "provider/model-id", or "provider/model-id:thinking". An
// empty selector uses the default profile.
func (c *Config) Resolve(selector string) (ModelRef, error) {
	if selector == "" {
		selector = c.Default
	}
	if selector == "" {
		return ModelRef{}, errors.New("no model selected and no default profile configured")
	}
	thinking := ""
	if p, ok := c.Profiles[selector]; ok {
		selector = p.Model
		thinking = p.Thinking
	}
	if i := strings.LastIndex(selector, ":"); i > 0 && !strings.Contains(selector[i:], "/") {
		thinking = selector[i+1:]
		selector = selector[:i]
	}
	provID, modelID, ok := strings.Cut(selector, "/")
	if !ok {
		return ModelRef{}, fmt.Errorf("unknown profile or model %q (expected provider/model)", selector)
	}
	prov, ok := c.Connections[provID]
	if !ok {
		return ModelRef{}, fmt.Errorf("unknown connection %q", provID)
	}
	if prov.Disabled {
		return ModelRef{}, fmt.Errorf("connection %q is disabled", provID)
	}
	for _, m := range prov.Models {
		if m.ID == modelID {
			if m.Disabled {
				return ModelRef{}, fmt.Errorf("model %q is disabled", selector)
			}
			return ModelRef{ConnID: provID, Conn: prov, Model: m, Thinking: thinking}, nil
		}
	}
	return ModelRef{}, fmt.Errorf("connection %q has no model %q", provID, modelID)
}

// ConnectionIDs returns the configured connection ids in sorted order.
func (c *Config) ConnectionIDs() []string {
	ids := make([]string, 0, len(c.Connections))
	for id := range c.Connections {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Permission returns the configured permission for a tool, defaulting to
// ask for mutating tools and allow for read-only ones.
func (c *Config) Permission(tool string) Permission {
	if p, ok := c.Permissions[tool]; ok {
		return p
	}
	if tools.IsReadOnly(tool) {
		return PermissionAllow
	}
	return PermissionAsk
}
