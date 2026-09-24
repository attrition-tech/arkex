package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Problem is one finding from Check.
type Problem struct {
	// Level is "error" (arkex cannot use the file, or a model cannot be
	// opened) or "warning" (works, but probably not as intended).
	Level string
	// Path is the file the problem is in; empty for merged-config findings.
	Path string
	// Where names the JSON location, e.g. `connections.local.models[2]`.
	Where string
	Msg   string
}

func (p Problem) String() string {
	var b strings.Builder
	b.WriteString(p.Level)
	if p.Path != "" {
		b.WriteString(" ")
		b.WriteString(p.Path)
	}
	if p.Where != "" {
		b.WriteString(" ")
		b.WriteString(p.Where)
	}
	b.WriteString(": ")
	b.WriteString(p.Msg)
	return b.String()
}

// Report is the outcome of checking the global and project config files.
type Report struct {
	Files    []string // files that exist and were checked
	Problems []Problem
}

// Errors reports whether any problem is an error.
func (r Report) Errors() int {
	n := 0
	for _, p := range r.Problems {
		if p.Level == "error" {
			n++
		}
	}
	return n
}

// Check validates the global config and the project config for cwd (if
// any), then the merged result. It never fails: unreadable files become
// problems.
func Check(cwd string) Report {
	var r Report
	gp, err := GlobalPath()
	if err != nil {
		r.Problems = append(r.Problems, Problem{Level: "error", Msg: err.Error()})
		return r
	}
	paths := []string{gp}
	if cwd != "" {
		paths = append(paths, ProjectPath(cwd))
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		r.Files = append(r.Files, p)
		r.Problems = append(r.Problems, checkFile(p)...)
	}
	if r.Errors() > 0 {
		return r
	}
	cfg, err := Load(cwd)
	if err != nil {
		r.Problems = append(r.Problems, Problem{Level: "error", Msg: err.Error()})
		return r
	}
	r.Problems = append(r.Problems, checkMerged(cfg)...)
	return r
}

var (
	knownTop        = []string{"version", "connections", "profiles", "default", "permissions", "ui"}
	knownConnection = []string{"name", "kind", "subscription", "api", "baseUrl", "apiKey", "headers", "compat", "models", "disabled"}
	knownKinds      = []string{string(KindLLMServer), string(KindAPIKey), string(KindSubscription), string(KindRuntime)}
	knownModel      = []string{"id", "name", "reasoning", "reasoningLevels", "reasoningDefault", "contextWindow", "maxTokens", "cost", "compat", "disabled"}
	knownCompat     = []string{"thinking", "extraBody"}
	knownCost       = []string{"input", "output", "cacheRead", "cacheWrite"}
	knownProfile    = []string{"model", "thinking"}
	knownUI         = []string{"mouse", "theme"}
	knownAPIs       = []string{string(APIOpenAICompat), string(APIOpenAI), string(APIAnthropic), string(APIGoogle)}
	knownThinking   = []string{string(ThinkingReasoningEffort), string(ThinkingDeepSeek), string(ThinkingQwen), string(ThinkingQwenChatTemplate), string(ThinkingOpenRouter)}
	knownPerms      = []string{string(PermissionAsk), string(PermissionAllow), string(PermissionDeny)}
	// KnownTools lists the built-in tool names permissions may refer to.
	KnownTools = []string{"read", "edit", "write", "bash"}
)

// checkFile validates one file: syntax, types, version, unknown keys and
// per-connection sanity. Structural problems stop the check early because
// the rest would be noise.
func checkFile(path string) []Problem {
	c := &checker{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return c.errorf("", "%v", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return c.warnf("", "file is empty")
	}
	root := map[string]any{}
	if err := json.Unmarshal(data, &root); err != nil {
		// The message already carries path:line:col.
		return []Problem{{Level: "error", Msg: describeJSONError(path, data, err).Error()}}
	}
	from, err := versionOf(root)
	switch {
	case err != nil:
		c.errorf("version", "%v", err)
	case from > CurrentVersion:
		c.errorf("version", "file is version %d but this arkex understands %d; run: arkex update", from, CurrentVersion)
	case from < CurrentVersion:
		c.warnf("version", "file is version %d; the next edit upgrades it to %d (or run: arkex config migrate)", from, CurrentVersion)
	}
	var cfg Config
	if err := decode(path, data, &cfg); err != nil {
		c.out = append(c.out, Problem{Level: "error", Msg: err.Error()})
		return c.out
	}
	// decode succeeded, so migrating the raw root cannot fail; the key
	// checks below want the current layout.
	_, _ = Migrate(root)
	c.unknownKeys("", root, knownTop)

	if v, ok := root["connections"]; ok {
		if _, isMap := v.(map[string]any); !isMap {
			c.errorf("connections", "should be an object of connection id → connection")
		}
	}
	for _, id := range sortedKeys(cfg.Connections) {
		c.checkConnection(id, cfg.Connections[id], asMap(asMap(root["connections"])[id]))
	}
	if ps, ok := root["profiles"].(map[string]any); ok {
		for _, name := range sortedKeys(ps) {
			where := "profiles." + name
			pm, ok := ps[name].(map[string]any)
			if !ok {
				c.errorf(where, "should be an object like {\"model\": \"provider/model\"}")
				continue
			}
			c.unknownKeys(where, pm, knownProfile)
			if m, _ := pm["model"].(string); !strings.Contains(m, "/") {
				c.errorf(where+".model", "should be \"provider/model-id\", got %q", m)
			}
		}
	}
	if perms, ok := root["permissions"].(map[string]any); ok {
		for _, tool := range sortedKeys(perms) {
			where := "permissions." + tool
			if !contains(KnownTools, tool) {
				c.warnf(where, "unknown tool %q%s", tool, suggest(tool, KnownTools))
			}
			if v, _ := perms[tool].(string); !contains(knownPerms, v) {
				c.errorf(where, "should be one of %s, got %q", strings.Join(knownPerms, ", "), v)
			}
		}
	}
	if ui, ok := root["ui"].(map[string]any); ok {
		c.unknownKeys("ui", ui, knownUI)
	}
	return c.out
}

func (c *checker) checkConnection(id string, p Connection, raw map[string]any) {
	where := "connections." + id
	if raw == nil {
		c.errorf(where, "should be an object")
		return
	}
	c.unknownKeys(where, raw, knownConnection)
	switch {
	case p.Kind == "":
		c.errorf(where+".kind", "is required; one of %s", strings.Join(knownKinds, ", "))
	case !contains(knownKinds, string(p.Kind)):
		c.errorf(where+".kind", "unknown kind %q%s", p.Kind, suggest(string(p.Kind), knownKinds))
	}
	if p.API != "" && !contains(knownAPIs, string(p.API)) {
		c.errorf(where+".api", "unknown api %q%s", p.API, suggest(string(p.API), knownAPIs))
	}
	if p.Subscription != "" && (p.Kind != KindSubscription || p.Subscription != "chatgpt") {
		c.errorf(where+".subscription", "must be chatgpt on a subscription connection")
	}
	switch {
	case p.BaseURL == "":
		c.errorf(where+".baseUrl", "is required (e.g. \"http://localhost:11434/v1\")")
	case strings.HasPrefix(p.BaseURL, "http://") && !isLoopback(p.BaseURL):
		c.warnf(where+".baseUrl", "plain http to a remote host sends the api key unencrypted")
	case !strings.HasPrefix(p.BaseURL, "http://") && !strings.HasPrefix(p.BaseURL, "https://"):
		c.errorf(where+".baseUrl", "should start with http:// or https://")
	}
	if p.APIKey == "" && p.Kind != KindSubscription {
		c.warnf(where+".apiKey", "not set; fine for local servers, otherwise requests will be rejected")
	} else {
		c.checkEnvRefs(where+".apiKey", p.APIKey)
	}
	for k, v := range p.Headers {
		c.checkEnvRefs(where+".headers."+k, v)
	}
	c.checkCompat(where+".compat", p.Compat, asMap(raw["compat"]))
	if len(p.Models) == 0 {
		c.warnf(where+".models", "no models listed; add one with /models or in the file")
	}
	seen := map[string]bool{}
	rawModels, _ := raw["models"].([]any)
	for i, m := range p.Models {
		mw := fmt.Sprintf("%s.models[%d]", where, i)
		if i < len(rawModels) {
			c.unknownKeys(mw, asMap(rawModels[i]), knownModel)
			if cm := asMap(rawModels[i]); cm != nil {
				c.checkCompat(mw+".compat", m.Compat, asMap(cm["compat"]))
				if cost := asMap(cm["cost"]); cost != nil {
					c.unknownKeys(mw+".cost", cost, knownCost)
				}
			}
		}
		switch {
		case m.ID == "":
			c.errorf(mw+".id", "is required")
		case seen[m.ID]:
			c.errorf(mw+".id", "duplicate model id %q", m.ID)
		}
		seen[m.ID] = true
		if m.ContextWindow < 0 || m.MaxTokens < 0 {
			c.errorf(mw, "contextWindow and maxTokens cannot be negative")
		}
		for _, level := range m.ReasoningLevels {
			if !contains([]string{"off", "on", "minimal", "low", "medium", "high", "xhigh", "max"}, level) {
				c.errorf(mw+".reasoningLevels", "unknown reasoning control %q", level)
			}
		}
		if m.ReasoningDefault != "" && !contains(m.ReasoningLevels, m.ReasoningDefault) {
			c.errorf(mw+".reasoningDefault", "must be one of reasoningLevels")
		}
	}
}

func (c *checker) checkCompat(where string, cp Compat, raw map[string]any) {
	if raw != nil {
		c.unknownKeys(where, raw, knownCompat)
	}
	if cp.Thinking != "" && !contains(knownThinking, string(cp.Thinking)) {
		c.errorf(where+".thinking", "unknown preset %q%s", cp.Thinking, suggest(string(cp.Thinking), knownThinking))
	}
}

// checkEnvRefs warns about $VARS that are not set in this environment.
func (c *checker) checkEnvRefs(where, v string) {
	if strings.HasPrefix(v, "!") || strings.HasPrefix(v, "$!") {
		return
	}
	for _, m := range envRef.FindAllStringSubmatch(v, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if _, ok := os.LookupEnv(name); !ok {
			c.warnf(where, "$%s is not set in this shell", name)
		}
	}
}

// checkMerged validates cross-file references on the loaded config.
func checkMerged(cfg *Config) []Problem {
	c := &checker{}
	if len(cfg.Connections) == 0 {
		c.warnf("connections", "no connections configured; run arkex and use /models, or: arkex config init")
		return c.out
	}
	if cfg.Default == "" {
		c.warnf("default", "no default model; arkex will ask on start (pick one with /models)")
	} else if _, err := cfg.Resolve(""); err != nil {
		c.errorf("default", "%v", err)
	}
	for _, name := range sortedKeys(cfg.Profiles) {
		if _, err := cfg.Resolve(name); err != nil {
			c.errorf("profiles."+name, "%v", err)
		}
	}
	return c.out
}

type checker struct {
	path string
	out  []Problem
}

func (c *checker) errorf(where, format string, args ...any) []Problem {
	c.out = append(c.out, Problem{Level: "error", Path: c.path, Where: where, Msg: fmt.Sprintf(format, args...)})
	return c.out
}

func (c *checker) warnf(where, format string, args ...any) []Problem {
	c.out = append(c.out, Problem{Level: "warning", Path: c.path, Where: where, Msg: fmt.Sprintf(format, args...)})
	return c.out
}

// unknownKeys warns about keys arkex does not read, suggesting the closest
// known one when the difference looks like a typo or wrong case.
func (c *checker) unknownKeys(where string, m map[string]any, known []string) {
	if m == nil {
		return
	}
	for _, k := range sortedKeys(m) {
		if contains(known, k) {
			continue
		}
		w := k
		if where != "" {
			w = where + "." + k
		}
		if want := caseMatch(k, known); want != "" {
			// encoding/json matches keys case-insensitively, so this works
			// today but is one refactor away from silently not working.
			c.warnf(w, "should be spelled %q", want)
			continue
		}
		c.warnf(w, "unknown key%s", suggest(k, known))
	}
}

func caseMatch(k string, known []string) string {
	for _, want := range known {
		if strings.EqualFold(k, want) {
			return want
		}
	}
	return ""
}

// suggest returns ` (did you mean "x"?)` when k is a near miss of a known
// name: same letters ignoring case, one a prefix of the other, or an edit
// distance small relative to the length.
func suggest(k string, known []string) string {
	if want := caseMatch(k, known); want != "" {
		return fmt.Sprintf(" (did you mean %q?)", want)
	}
	lk := strings.ToLower(k)
	best, bestDist := "", max(2, len(lk)/3)+1
	for _, want := range known {
		lw := strings.ToLower(want)
		if len(lk) >= 3 && (strings.HasPrefix(lw, lk) || strings.HasPrefix(lk, lw)) {
			return fmt.Sprintf(" (did you mean %q?)", want)
		}
		if d := editDistance(lk, lw); d < bestDist {
			best, bestDist = want, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean %q?)", best)
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func isLoopback(url string) bool {
	rest := strings.TrimPrefix(url, "http://")
	host := rest
	if i := strings.IndexAny(rest, "/:"); i >= 0 {
		host = rest[:i]
	}
	return host == "localhost" || host == "127.0.0.1" || host == "[::1]" || host == "::1" || host == "0.0.0.0" || host == "host.docker.internal"
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
