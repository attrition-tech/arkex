package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/attrition-tech/arkex/internal/fileutil"
)

// The editors below change one config file in place. They work on generic
// JSON so keys arkex does not know about survive, and they write with mode
// 0600 because the file may hold API keys. A missing file is treated as "{}".

// SaveConnection adds or replaces connection id and, when defaultSel is
// non-empty, sets the default selector.
func SaveConnection(path, id string, c Connection, defaultSel string) error {
	return UpdateConnection(path, "", id, c, defaultSel)
}

// UpdateConnection stores c under newID and, when oldID is set and differs,
// removes oldID in the same write. The default selector and profiles that
// pointed at oldID follow the rename. defaultSel, when non-empty, becomes
// the default afterwards.
func UpdateConnection(path, oldID, newID string, c Connection, defaultSel string) error {
	if newID == "" {
		return errors.New("connection id is required")
	}
	pv, err := toGeneric(c)
	if err != nil {
		return err
	}
	// Compat is a value type, so it always marshals; drop it when empty.
	if pm, ok := pv.(map[string]any); ok {
		dropEmpty(pm, "compat")
		if models, ok := pm["models"].([]any); ok {
			for _, mv := range models {
				if mm, ok := mv.(map[string]any); ok {
					dropEmpty(mm, "compat")
				}
			}
		}
	}
	return edit(path, func(root map[string]any) error {
		cs := connections(root)
		if oldID != "" && oldID != newID {
			if _, ok := cs[oldID]; !ok {
				return fmt.Errorf("connection %q is not in %s", oldID, path)
			}
			delete(cs, oldID)
			if d, _ := root["default"].(string); d != "" && providerOf(d) == oldID {
				root["default"] = newID + d[len(oldID):]
			}
			if profiles, ok := root["profiles"].(map[string]any); ok {
				for _, pv := range profiles {
					if p, ok := pv.(map[string]any); ok {
						if sel, _ := p["model"].(string); sel != "" && providerOf(sel) == oldID {
							p["model"] = newID + sel[len(oldID):]
						}
					}
				}
			}
		}
		cs[newID] = pv
		if defaultSel != "" {
			root["default"] = defaultSel
		}
		return nil
	})
}

// RemoveConnection deletes connection id. If the default selector pointed at
// one of its models it is cleared too.
func RemoveConnection(path, id string) error {
	return edit(path, func(root map[string]any) error {
		ps := connections(root)
		if _, ok := ps[id]; !ok {
			return fmt.Errorf("connection %q is not in %s", id, path)
		}
		delete(ps, id)
		if d, _ := root["default"].(string); d != "" && providerOf(d) == id {
			delete(root, "default")
		}
		return nil
	})
}

// RemoveModel deletes one model from connection provID. The default is
// cleared if it pointed at that model.
func RemoveModel(path, provID, modelID string) error {
	return edit(path, func(root map[string]any) error {
		return withModel(root, path, provID, modelID, func(models []any, i int) []any {
			return append(models[:i], models[i+1:]...)
		}, func() {
			if d, _ := root["default"].(string); d == provID+"/"+modelID {
				delete(root, "default")
			}
		})
	})
}

// SetDisabled flips the disabled flag on a connection (modelID == "") or on
// one of its models.
func SetDisabled(path, provID, modelID string, disabled bool) error {
	return edit(path, func(root map[string]any) error {
		if modelID == "" {
			p, ok := connections(root)[provID].(map[string]any)
			if !ok {
				return fmt.Errorf("connection %q is not in %s", provID, path)
			}
			setFlag(p, "disabled", disabled)
			return nil
		}
		return withModel(root, path, provID, modelID, func(models []any, i int) []any {
			if m, ok := models[i].(map[string]any); ok {
				setFlag(m, "disabled", disabled)
			}
			return models
		}, nil)
	})
}

// SetContextWindow records the context window (tokens) of one model, e.g.
// after the provider revealed it in an overflow error. tokens <= 0 removes
// the key.
func SetContextWindow(path, provID, modelID string, tokens int) error {
	return edit(path, func(root map[string]any) error {
		return withModel(root, path, provID, modelID, func(models []any, i int) []any {
			if m, ok := models[i].(map[string]any); ok {
				if tokens > 0 {
					m["contextWindow"] = tokens
				} else {
					delete(m, "contextWindow")
				}
			}
			return models
		}, nil)
	})
}

// SetDefault sets the default selector.
func SetDefault(path, selector string) error {
	return edit(path, func(root map[string]any) error {
		if selector == "" {
			delete(root, "default")
		} else {
			root["default"] = selector
		}
		return nil
	})
}

// withModel locates modelID under provID, applies fn to the models slice
// and stores the result, then runs after (may be nil).
func withModel(root map[string]any, path, provID, modelID string, fn func(models []any, i int) []any, after func()) error {
	p, ok := connections(root)[provID].(map[string]any)
	if !ok {
		return fmt.Errorf("connection %q is not in %s", provID, path)
	}
	models, _ := p["models"].([]any)
	for i, mv := range models {
		m, _ := mv.(map[string]any)
		if id, _ := m["id"].(string); id == modelID {
			p["models"] = fn(models, i)
			if after != nil {
				after()
			}
			return nil
		}
	}
	return fmt.Errorf("model %s/%s is not in %s", provID, modelID, path)
}

func dropEmpty(m map[string]any, key string) {
	if v, ok := m[key].(map[string]any); ok && len(v) == 0 {
		delete(m, key)
	}
}

func setFlag(m map[string]any, key string, v bool) {
	if v {
		m[key] = true
	} else {
		delete(m, key)
	}
}

func providerOf(selector string) string {
	for i := 0; i < len(selector); i++ {
		if selector[i] == '/' {
			return selector[:i]
		}
	}
	return ""
}

func connections(root map[string]any) map[string]any {
	ps, _ := root["connections"].(map[string]any)
	if ps == nil {
		ps = map[string]any{}
		root["connections"] = ps
	}
	return ps
}

func toGeneric(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetUI sets one key under "ui" (nil deletes it).
func SetUI(path, key string, value any) error {
	return edit(path, func(root map[string]any) error {
		ui, _ := root["ui"].(map[string]any)
		if ui == nil {
			ui = map[string]any{}
		}
		if value == nil {
			delete(ui, key)
		} else {
			ui[key] = value
		}
		if len(ui) == 0 {
			delete(root, "ui")
		} else {
			root["ui"] = ui
		}
		return nil
	})
}

// MigrateFile upgrades path to CurrentVersion on disk and reports whether
// it changed. The previous contents are kept in path + ".bak".
func MigrateFile(path string) (bool, error) {
	prev, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	root := map[string]any{}
	if err := json.Unmarshal(prev, &root); err != nil {
		return false, describeJSONError(path, prev, err)
	}
	from, err := versionOf(root)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if from == CurrentVersion {
		return false, nil
	}
	return true, edit(path, func(map[string]any) error { return nil })
}

// BackupPath is where edit keeps the previous version of a file.
func BackupPath(path string) string { return path + ".bak" }

// edit reads path as generic JSON, migrates it, applies fn, and writes the
// result back atomically (tmp file + rename). The previous file is copied
// to BackupPath(path) first so a bad edit or migration can be undone.
func edit(path string, fn func(root map[string]any) error) error {
	if path == "" {
		return errors.New("no config file path")
	}
	unlock, err := fileutil.Lock(path)
	if err != nil {
		return err
	}
	defer unlock()
	root := map[string]any{}
	prev, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(bytes.TrimSpace(prev)) > 0 {
			if err := json.Unmarshal(prev, &root); err != nil {
				return describeJSONError(path, prev, err)
			}
		}
	case errors.Is(err, os.ErrNotExist):
		prev = nil
	default:
		return err
	}
	if _, err := Migrate(root); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := fn(root); err != nil {
		return err
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if prev != nil {
		if err := fileutil.Write(BackupPath(path), prev); err != nil {
			return fmt.Errorf("backing up %s: %w", path, err)
		}
	}
	return fileutil.Write(path, append(out, '\n'))
}
