package tui

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/attrition-tech/arkex/internal/session"
)

// maxHistory bounds the prompt history kept per directory.
const maxHistory = 500

// history is the prompt history for one directory: what the user typed
// (prompts, /commands, !shell), newest last. It lives next to the
// directory's sessions as one JSON string per line so multi-line prompts
// survive.
type history struct {
	path    string
	entries []string
	// idx is the entry being shown; len(entries) means "not navigating".
	idx   int
	draft string // the input before navigation started
}

func historyPath(cwd string) string {
	dir, err := session.Dir(cwd)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "history")
}

// loadHistory reads the last maxHistory entries; a missing or unreadable
// file yields an empty history.
func loadHistory(cwd string) *history {
	h := &history{path: historyPath(cwd)}
	if h.path == "" {
		return h
	}
	f, err := os.Open(h.path)
	if err != nil {
		return h
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var s string
		if json.Unmarshal(sc.Bytes(), &s) == nil && s != "" {
			h.entries = append(h.entries, s)
		}
	}
	if len(h.entries) > maxHistory {
		h.entries = h.entries[len(h.entries)-maxHistory:]
	}
	h.idx = len(h.entries)
	return h
}

// add records an entry (skipping a repeat of the last one) and appends it
// to the file. Navigation state is reset.
func (h *history) add(text string) {
	h.idx = len(h.entries)
	h.draft = ""
	if text == "" || (len(h.entries) > 0 && h.entries[len(h.entries)-1] == text) {
		return
	}
	h.entries = append(h.entries, text)
	h.idx = len(h.entries)
	if h.path == "" {
		return
	}
	if len(h.entries) > maxHistory {
		h.entries = h.entries[len(h.entries)-maxHistory:]
		h.idx = len(h.entries)
		h.rewrite()
		return
	}
	line, err := json.Marshal(text)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(line, '\n'))
}

// rewrite stores the in-memory entries, used when the file grew past the
// cap.
func (h *history) rewrite() {
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return
	}
	var buf []byte
	for _, e := range h.entries {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		buf = append(append(buf, line...), '\n')
	}
	tmp := h.path + ".tmp"
	if os.WriteFile(tmp, buf, 0o600) == nil {
		_ = os.Rename(tmp, h.path)
	}
}

// prev moves one entry back from the current input, returning the text to
// show and false when already at the oldest entry.
func (h *history) prev(current string) (string, bool) {
	if h.idx == 0 || len(h.entries) == 0 {
		return "", false
	}
	if h.idx == len(h.entries) {
		h.draft = current
	}
	h.idx--
	return h.entries[h.idx], true
}

// next moves one entry forward; past the newest entry it restores the
// draft. It returns false when not navigating.
func (h *history) next() (string, bool) {
	if h.idx >= len(h.entries) {
		return "", false
	}
	h.idx++
	if h.idx == len(h.entries) {
		return h.draft, true
	}
	return h.entries[h.idx], true
}

// Tab walks the current transcript's user messages, not persisted input history.
func (m *model) focusPrompt(older bool) {
	start := len(m.blocks)
	if m.promptFocus != nil {
		for i, b := range m.blocks {
			if b == m.promptFocus {
				start = i
				break
			}
		}
	} else if !older {
		return
	}
	dir := 1
	if older {
		dir = -1
	}
	for i := start + dir; i >= 0 && i <= len(m.blocks); i += dir {
		if i == len(m.blocks) {
			m.promptFocus = nil
			m.stickBottom = true
			m.refresh()
			return
		}
		if m.blocks[i].kind != blockUser {
			continue
		}
		m.promptFocus = m.blocks[i]
		m.stickBottom = false
		m.clearSelection()
		m.refresh()
		for _, s := range m.spans {
			if s.b == m.promptFocus {
				if s.top < m.vp.YOffset() || s.bottom > m.vp.YOffset()+m.vp.Height() {
					m.vp.ScrollDown(s.top - m.vp.YOffset())
				}
				break
			}
		}
		return
	}
}
