// Package setup holds the non-visual half of "add your models": endpoint
// presets, model discovery over the OpenAI-compatible /models route, and
// the heuristics that turn a bare model id into a sensible config entry.
// The TUI drives it; nothing here draws.
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/dantearo/arkex/internal/chatgpt"
	"github.com/dantearo/arkex/internal/config"
)

// Preset pre-fills the add form for a known service. Each connection kind
// has its own list; the "other" entry of each kind is the catch-all for
// anything speaking the OpenAI wire protocol.
type Preset struct {
	ID       string
	Label    string
	Hint     string
	Kind     config.Kind
	BaseURL  string // empty: ask
	NeedsKey bool   // false: key is optional
	Compat   config.Compat
}

// Presets in display order, grouped by kind. Local servers come first:
// arkex exists for people running their own models.
var Presets = []Preset{
	{ID: "ollama", Label: "Ollama", Hint: "localhost:11434, no key", Kind: config.KindLLMServer, BaseURL: "http://localhost:11434/v1"},
	{ID: "llamacpp", Label: "llama.cpp", Hint: "llama-server on localhost:8080", Kind: config.KindLLMServer, BaseURL: "http://localhost:8080/v1"},
	{ID: "lmstudio", Label: "LM Studio", Hint: "localhost:1234", Kind: config.KindLLMServer, BaseURL: "http://localhost:1234/v1"},
	{ID: "vllm", Label: "vLLM", Hint: "localhost:8000", Kind: config.KindLLMServer, BaseURL: "http://localhost:8000/v1"},
	{ID: "other-server", Label: "Other", Hint: "LiteLLM, SGLang, your own gateway — anything serving /v1", Kind: config.KindLLMServer},

	{ID: "openai", Label: "OpenAI", Hint: "api.openai.com", Kind: config.KindAPIKey, BaseURL: "https://api.openai.com/v1", NeedsKey: true, Compat: config.Compat{Thinking: config.ThinkingReasoningEffort}},
	{ID: "anthropic", Label: "Anthropic", Hint: "OpenAI-compatible endpoint; model list may need typing", Kind: config.KindAPIKey, BaseURL: "https://api.anthropic.com/v1", NeedsKey: true},
	{ID: "google", Label: "Google", Hint: "Gemini via the OpenAI-compatible endpoint", Kind: config.KindAPIKey, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", NeedsKey: true},
	{ID: "deepseek", Label: "DeepSeek", Hint: "api.deepseek.com", Kind: config.KindAPIKey, BaseURL: "https://api.deepseek.com/v1", NeedsKey: true, Compat: config.Compat{Thinking: config.ThinkingDeepSeek}},
	{ID: "openrouter", Label: "OpenRouter", Hint: "many hosted models behind one key", Kind: config.KindAPIKey, BaseURL: "https://openrouter.ai/api/v1", NeedsKey: true, Compat: config.Compat{Thinking: config.ThinkingOpenRouter}},
	{ID: "groq", Label: "Groq", Hint: "api.groq.com", Kind: config.KindAPIKey, BaseURL: "https://api.groq.com/openai/v1", NeedsKey: true},
	{ID: "mistral", Label: "Mistral", Hint: "api.mistral.ai", Kind: config.KindAPIKey, BaseURL: "https://api.mistral.ai/v1", NeedsKey: true},
	{ID: "moonshot", Label: "Moonshot", Hint: "Kimi, api.moonshot.ai", Kind: config.KindAPIKey, BaseURL: "https://api.moonshot.ai/v1", NeedsKey: true},
	{ID: "other-api", Label: "Other", Hint: "any hosted OpenAI-compatible API", Kind: config.KindAPIKey, NeedsKey: true},

	{ID: "chatgpt", Label: "ChatGPT", Hint: "sign in with your ChatGPT Plus/Pro/Team account; uses your plan, not API credit", Kind: config.KindSubscription, BaseURL: chatgpt.BaseURL, Compat: config.Compat{Thinking: config.ThinkingReasoningEffort}},
}

// PresetsFor returns the presets of one kind, in display order. The last
// entry is always that kind's "Other" catch-all.
func PresetsFor(kind config.Kind) []Preset {
	var out []Preset
	for _, p := range Presets {
		if p.Kind == kind {
			out = append(out, p)
		}
	}
	return out
}

// FindPreset returns the preset with the given id, or the llm-server
// catch-all when unknown.
func FindPreset(id string) Preset {
	for _, p := range Presets {
		if p.ID == id {
			return p
		}
	}
	return Other(config.KindLLMServer)
}

// Other returns the catch-all preset for kind.
func Other(kind config.Kind) Preset {
	ps := PresetsFor(kind)
	if len(ps) == 0 {
		return Preset{ID: "other-server", Label: "Other", Kind: config.KindLLMServer}
	}
	return ps[len(ps)-1]
}

// DetectPreset picks the preset whose host matches baseURL, falling back to
// the "Other" entry for the kind the URL implies (loopback → llm-server).
// Existing connections get their compat from it.
func DetectPreset(baseURL string) Preset {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return Other(config.KindLLMServer)
	}
	host := strings.ToLower(u.Hostname())
	for _, p := range Presets {
		if p.BaseURL == "" {
			continue
		}
		pu, err := url.Parse(p.BaseURL)
		if err == nil && pu.Hostname() == host && (pu.Port() == "" || pu.Port() == u.Port()) {
			return p
		}
	}
	if IsLocal(baseURL) {
		return Other(config.KindLLMServer)
	}
	return Other(config.KindAPIKey)
}

// IsLocal reports whether baseURL points at the local machine.
func IsLocal(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "::1" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NormalizeBaseURL trims whitespace and a trailing slash and requires an
// http(s) scheme with a host.
func NormalizeBaseURL(s string) (string, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s == "" {
		return "", errors.New("base URL is required")
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("expected http(s)://host[/path], got %q", s)
	}
	return s, nil
}

// IsPlainHTTP reports whether baseURL uses http to a host other than the
// local machine, which sends the API key in cleartext.
func IsPlainHTTP(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "::1" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return false
	}
	return true
}

// SuggestName derives a provider id from a base URL: the most specific
// meaningful DNS label ("llm.matrx.example" → "matrx"; "localhost" → "local").
func SuggestName(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return "custom"
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return "local"
	}
	labels := strings.Split(host, ".")
	generic := map[string]bool{"api": true, "www": true, "llm": true, "ai": true, "v1": true, "inference": true, "models": true, "gateway": true}
	// Drop TLD and a public-suffix-ish second label (co.uk, com.au).
	if n := len(labels); n >= 2 {
		labels = labels[:n-1]
		if n >= 3 && len(labels[len(labels)-1]) <= 3 {
			labels = labels[:len(labels)-1]
		}
	}
	for i := len(labels) - 1; i >= 0; i-- {
		if !generic[labels[i]] && labels[i] != "" {
			return sanitizeID(labels[i])
		}
	}
	if len(labels) > 0 {
		return sanitizeID(labels[len(labels)-1])
	}
	return "custom"
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "custom"
	}
	return b.String()
}

// ValidID reports whether s can be a provider id (used in "provider/model").
func ValidID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// ListModels fetches GET <baseURL>/models and returns the sorted model ids.
// apiKey is the resolved secret (may be empty for local servers).
func ListModels(ctx context.Context, client *http.Client, baseURL, apiKey, userAgent string) ([]string, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%s: check the API key", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /models: %s", resp.Status)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil || list.Data == nil {
		// Some servers return a bare array.
		var arr []struct {
			ID string `json:"id"`
		}
		if err2 := json.Unmarshal(body, &arr); err2 != nil {
			return nil, fmt.Errorf("GET /models: response is not an OpenAI model list")
		}
		list.Data = arr
	}
	seen := map[string]bool{}
	var ids []string
	for _, m := range list.Data {
		if m.ID != "" && !seen[m.ID] {
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil, errors.New("GET /models returned no models")
	}
	return ids, nil
}

// GuessModel builds a config entry for id from naming conventions. It only
// sets what the compat layer needs to talk to the model; users can refine
// the file later. The provider-level compat (from the preset) is not
// duplicated here: only set model-level thinking when the id implies a
// family-specific protocol.
func GuessModel(id string, preset Preset) config.Model {
	m := config.Model{ID: id}
	l := strings.ToLower(id)
	switch {
	case strings.Contains(l, "deepseek"):
		m.Reasoning = true
		if preset.Compat.Thinking == "" {
			m.Compat.Thinking = config.ThinkingDeepSeek
		}
	case strings.Contains(l, "qwen3") || strings.Contains(l, "qwq") || strings.Contains(l, "qwen-") && strings.Contains(l, "think"):
		m.Reasoning = true
		if preset.Compat.Thinking == "" {
			m.Compat.Thinking = config.ThinkingQwen
		}
	case strings.HasPrefix(l, "o1") || strings.HasPrefix(l, "o3") || strings.HasPrefix(l, "o4") ||
		strings.Contains(l, "gpt-5") || strings.Contains(l, "gpt-6") || strings.Contains(l, "gpt-oss"):
		m.Reasoning = true
		if preset.Compat.Thinking == "" {
			m.Compat.Thinking = config.ThinkingReasoningEffort
		}
	case strings.Contains(l, "claude") || strings.Contains(l, "gemini") ||
		strings.Contains(l, "glm-4.5") || strings.Contains(l, "glm-5") || strings.Contains(l, "kimi-k2") || strings.Contains(l, "kimi-k3") ||
		strings.Contains(l, "minimax-m") || strings.Contains(l, "-r1") || strings.Contains(l, "reason") || strings.Contains(l, "think"):
		m.Reasoning = true
	}
	return m
}

// Describe summarises a guessed model for display.
func Describe(m config.Model, preset Preset) string {
	c := preset.Compat.Merge(m.Compat)
	switch {
	case c.Thinking != "":
		return fmt.Sprintf("reasoning · thinking=%s", c.Thinking)
	case m.Reasoning:
		return "reasoning"
	default:
		return "chat"
	}
}
