package session

import (
	"fmt"
	"reflect"

	"charm.land/fantasy"
)

// PromptCheckpoint points into shared history, so normal turns only store new
// messages. Compaction starts another segment without destroying checkpoints.
type PromptCheckpoint struct {
	Text   string          `json:"text"`
	Prompt fantasy.Message `json:"prompt"`
	Start  int             `json:"start"`
	End    int             `json:"end"`
	Model  string          `json:"model"`
}

func (s *Session) syncContext(msgs []fantasy.Message) {
	if s.ContextStart < 0 || s.ContextStart > len(s.History) {
		s.ContextStart = len(s.History)
	}
	old := s.History[s.ContextStart:]
	if len(msgs) >= len(old) && reflect.DeepEqual(msgs[:len(old)], old) {
		s.History = append(s.History, msgs[len(old):]...)
	} else {
		s.ContextStart = len(s.History)
		s.History = append(s.History, msgs...)
	}
}

// Checkpoint must run while the agent is idle, before appending its user message.
func (s *Session) Checkpoint(before []fantasy.Message, text string, prompt fantasy.Message, model string) int {
	s.syncContext(before)
	s.Prompts = append(s.Prompts, PromptCheckpoint{Text: text, Prompt: prompt,
		Start: s.ContextStart, End: len(s.History), Model: model})
	return len(s.Prompts) // one-based; zero means not editable
}

// ImportCheckpoints enables editing surviving prompts from older session files.
// Compaction's synthetic summary/ack pair is not a user-authored prompt.
func (s *Session) ImportCheckpoints(isSummary func(string) bool) {
	if len(s.Prompts) != 0 {
		return
	}
	s.History, s.ContextStart = nil, 0
	for i, msg := range s.Messages {
		if msg.Role == fantasy.MessageRoleUser && !isSummary(UserText(msg)) {
			s.Checkpoint(s.Messages[:i], UserText(msg), msg, s.Model)
		}
	}
	s.syncContext(s.Messages)
}

// Fork returns a new session at the context immediately before the chosen prompt.
// It neither changes the original session nor rolls back external side effects.
func (s *Session) Fork(index int) (*Session, error) {
	if index < 1 || index > len(s.Prompts) {
		return nil, fmt.Errorf("prompt checkpoint is unavailable")
	}
	p := s.Prompts[index-1]
	if p.Start < 0 || p.End < p.Start || p.End > len(s.History) {
		return nil, fmt.Errorf("prompt checkpoint is invalid")
	}
	next := New(s.Cwd)
	next.ParentID = s.ID
	next.Title = s.Title + " (edited)"
	next.Model, next.Mode, next.Effort = p.Model, s.Mode, s.Effort
	next.History = append([]fantasy.Message(nil), s.History[:p.End]...)
	next.ContextStart = p.Start
	next.Messages = append([]fantasy.Message(nil), s.History[p.Start:p.End]...)
	next.Prompts = append([]PromptCheckpoint(nil), s.Prompts[:index-1]...)
	return next, nil
}
