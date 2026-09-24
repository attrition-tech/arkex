package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// CurrentVersion is the config format this build writes. Files without a
// "version" key are version 0, the format used before versioning existed.
const CurrentVersion = 2

// ErrNewerConfig is returned when a file was written by a newer arkex.
var ErrNewerConfig = errors.New("config written by a newer arkex")

// migrations[i] upgrades a generic JSON root from version i to i+1. Each
// step must be idempotent enough to run on a partially edited file.
var migrations = []func(root map[string]any) error{
	// 0 → 1: the first versioned format. Nothing structural changed; the
	// step only stamps the version so later ones know where they stand.
	func(map[string]any) error { return nil },
	// 1 → 2: "providers" became "connections" and every entry has a kind.
	// Entries with an API key to a remote host are vendors (api-key);
	// everything else is a server the user runs (llm-server).
	func(root map[string]any) error {
		ps, ok := root["providers"].(map[string]any)
		if !ok {
			delete(root, "providers")
			return nil
		}
		delete(root, "providers")
		for _, v := range ps {
			p, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if _, has := p["kind"]; has {
				continue
			}
			key, _ := p["apiKey"].(string)
			url, _ := p["baseUrl"].(string)
			if key != "" && !isLoopback(url) {
				p["kind"] = string(KindAPIKey)
			} else {
				p["kind"] = string(KindLLMServer)
			}
		}
		root["connections"] = ps
		return nil
	},
}

// versionOf reads the version key (0 when absent).
func versionOf(root map[string]any) (int, error) {
	v, ok := root["version"]
	if !ok {
		return 0, nil
	}
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) || f < 0 {
		return 0, fmt.Errorf(`"version" must be a non-negative integer, got %v`, v)
	}
	return int(f), nil
}

// Migrate upgrades root in place to CurrentVersion and reports whether
// anything changed. A root from a newer version is left alone and returns
// ErrNewerConfig.
func Migrate(root map[string]any) (bool, error) {
	from, err := versionOf(root)
	if err != nil {
		return false, err
	}
	if from > CurrentVersion {
		return false, fmt.Errorf("%w (file is version %d, this arkex understands %d; run: arkex update)", ErrNewerConfig, from, CurrentVersion)
	}
	if from == CurrentVersion {
		return false, nil
	}
	for v := from; v < CurrentVersion; v++ {
		if err := migrations[v](root); err != nil {
			return false, fmt.Errorf("migrating config from version %d: %w", v, err)
		}
	}
	root["version"] = CurrentVersion
	return true, nil
}

// decode parses one config file into a generic root, migrates it, and then
// fills cfg. Errors carry the path and, for syntax errors, line:column.
func decode(path string, data []byte, cfg *Config) error {
	root := map[string]any{}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return describeJSONError(path, data, err)
	}
	if _, err := Migrate(root); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	b, err := json.Marshal(root)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return describeJSONError(path, data, err)
	}
	return nil
}

// describeJSONError turns encoding/json errors into messages that name the
// place in the file rather than a byte offset.
func describeJSONError(path string, data []byte, err error) error {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		line, col := lineCol(data, syn.Offset)
		return fmt.Errorf("%s:%d:%d: %s", path, line, col, syntaxHint(syn.Error()))
	case errors.As(err, &typ):
		field := typ.Field
		if field == "" {
			field = "top level"
		}
		return fmt.Errorf("%s: %s should be %s, not %s", path, field, describeGoType(typ.Type), describeJSONValue(typ.Value))
	}
	return fmt.Errorf("%s: %w", path, err)
}

func describeJSONValue(v string) string {
	switch v {
	case "string", "number", "bool", "array":
		return "a " + v
	case "object":
		return "an object"
	}
	return v
}

// describeGoType names a Go type the way a JSON author would think of it.
func describeGoType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int64, reflect.Float64:
		return "a number"
	case reflect.Slice:
		return "a list [...]"
	case reflect.Map, reflect.Struct:
		return "an object {...}"
	case reflect.Pointer:
		return describeGoType(t.Elem())
	}
	return t.String()
}

func syntaxHint(msg string) string {
	switch {
	case strings.Contains(msg, "unexpected end of JSON input"):
		return "unexpected end of file (missing a closing } or ]?)"
	case strings.Contains(msg, "after object key:value pair"):
		return "expected , or } after a value (trailing comma or missing comma?)"
	case strings.Contains(msg, "looking for beginning of object key string"):
		return "expected a quoted key (trailing comma before } or unquoted key?)"
	case strings.Contains(msg, "looking for beginning of value"):
		return "expected a value (comments and single quotes are not JSON)"
	}
	return msg
}

// lineCol converts a byte offset to a 1-based line and column.
func lineCol(data []byte, off int64) (int, int) {
	if off > int64(len(data)) {
		off = int64(len(data))
	}
	head := data[:off]
	line := bytes.Count(head, []byte{'\n'}) + 1
	col := int(off) - bytes.LastIndexByte(head, '\n')
	return line, col
}
