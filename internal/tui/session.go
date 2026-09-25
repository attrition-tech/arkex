package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"

	"github.com/dantearo/arkex/internal/agent"
	"github.com/dantearo/arkex/internal/sanitize"
	"github.com/dantearo/arkex/internal/session"
)

const recentSessions = 5

// saveConv persists the current conversation. Called after every run; the
// session file is created on the first saved turn.
func (m *model) saveConv() {
	if m.conv == nil || m.sess.Agent == nil {
		return
	}
	m.conv.Update(m.sess.Agent.Messages(), m.sess.Name, string(m.mode()), m.usageIn, m.usageOut)
	m.conv.LastInput = m.lastInput
	if m.sess.Agent.Model != nil {
		level := m.sess.Agent.Model.Ref.Thinking
		m.conv.Effort = &level
	}
	if err := m.conv.Save(); err != nil {
		m.appendSystem("could not save the session: " + err.Error())
	}
}

// newConv forgets the current conversation (already saved after its last
// turn) so the next prompt starts a new session file.
func (m *model) newConv() {
	m.cancelPromptEdit()
	m.editAfterStop, m.removeOnStop = 0, 0
	m.requestTimings, m.runDuration = nil, 0
	m.conv = nil
	m.usageIn, m.usageOut, m.lastInput = 0, 0, 0
	if m.sess.Agent != nil {
		m.sess.Agent.SetMessages(nil)
	}
}

// carryConv moves the current conversation into a newly connected agent so
// a model switch continues the same session. Reasoning parts are dropped
// when the provider changes: providers only accept their own (often
// signed) reasoning blocks back.
func (m *model) carryConv(old, next Connection) {
	if old.Agent == nil || next.Agent == nil {
		return
	}
	msgs := old.Agent.Messages()
	if providerOf(old.Name) != providerOf(next.Name) {
		msgs = stripReasoning(msgs)
	}
	next.Agent.SetMessages(msgs)
}

// providerOf returns the provider prefix of a "provider/model[:level]"
// display name.
func providerOf(name string) string {
	p, _, _ := strings.Cut(name, "/")
	return p
}

// stripReasoning returns a copy of msgs without ReasoningParts and without
// provider-specific options on the remaining parts. Assistant messages left
// with no content are dropped.
func stripReasoning(msgs []fantasy.Message) []fantasy.Message {
	out := make([]fantasy.Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Role != fantasy.MessageRoleAssistant {
			out = append(out, msg)
			continue
		}
		var parts []fantasy.MessagePart
		for _, p := range msg.Content {
			switch p := p.(type) {
			case fantasy.ReasoningPart:
				continue
			case fantasy.TextPart:
				p.ProviderOptions = nil
				parts = append(parts, p)
			case fantasy.ToolCallPart:
				p.ProviderOptions = nil
				parts = append(parts, p)
			default:
				parts = append(parts, p)
			}
		}
		if len(parts) == 0 {
			continue
		}
		msg.Content = parts
		msg.ProviderOptions = nil
		out = append(out, msg)
	}
	return out
}

// resume loads a saved session into the agent and rebuilds the transcript.
func (m *model) resume(id string) tea.Cmd {
	return m.resumeWithModel(id, "")
}

func (m *model) resumeWithModel(id, override string) tea.Cmd {
	if m.running {
		m.appendSystem("cannot resume while working; press esc twice to cancel first")
		return nil
	}
	var s *session.Session
	var err error
	if id == "latest" {
		s, err = session.Latest(m.o.Cwd)
		if err == nil && s == nil {
			err = fmt.Errorf("no saved sessions in %s", m.o.Cwd)
		}
	} else {
		s, err = session.Load(m.o.Cwd, id)
	}
	if err != nil {
		m.appendSystem(err.Error())
		return nil
	}
	if s.State != "" {
		m.appendSystem("restore this session from Archived or Trash in /sessions before resuming")
		return nil
	}
	if override != "" {
		return m.connectSession(s, override, false)
	}
	if m.o.Connect == nil && m.sess.Agent != nil {
		// An embedded TUI may supply a single agent without a connector.
		base := s.Model
		if s.Effort != nil && *s.Effort != "" {
			base = strings.TrimSuffix(base, ":"+*s.Effort)
		}
		current := m.sess.Name
		if m.sess.Agent.Model != nil {
			current = m.sess.Agent.Model.Ref.String()
		}
		if base == current {
			if err := restoreSessionEffort(&m.sess, s); err != nil {
				return m.chooseSessionModel(s, err)
			}
			m.loadConversation(s)
			return nil
		}
	}
	if s.Model == "" {
		return m.chooseSessionModel(s, fmt.Errorf("this session has no saved model"))
	}
	return m.connectSession(s, s.Model, true)
}

func (m *model) loadConversation(s *session.Session) {
	m.cancelPromptEdit()
	m.editAfterStop, m.removeOnStop = 0, 0
	s.ImportCheckpoints(func(text string) bool { _, ok := agent.CompactSummary(text); return ok })
	m.requestTimings, m.runDuration = nil, 0
	messages := s.Messages
	if providerOf(s.Model) != providerOf(m.sess.Name) {
		messages = stripReasoning(messages)
	}
	m.sess.Agent.SetMessages(messages)
	m.replaceTranscript(append([]*block{m.welcome}, blocksFromMessages(s.Messages)...))
	m.paused = false
	m.usageIn, m.usageOut, m.lastInput = s.UsageIn, s.UsageOut, s.LastInput
	if s.Model != m.sess.Name {
		m.lastInput = 0
	}
	m.sess.Agent.RestoreLastInput(m.lastInput)
	if md, err := agent.ParseMode(s.Mode); err == nil {
		m.setMode(md)
	}
	m.conv = s
	m.bindPromptCheckpoints()
	note := fmt.Sprintf("resumed %q · %d messages · %s", s.Title, len(s.Messages), session.Age(s.Updated, time.Now()))
	if s.Model != "" && s.Model != m.sess.Name {
		note += "\nreplacement model: " + m.sess.Name
	}
	m.flash(note)
	m.stickBottom = true
	if s.Model != m.sess.Name {
		m.saveConv()
	}
	m.refresh()
}

// blocksFromMessages rebuilds transcript blocks from saved messages. Tool
// cards get their status from the matching result part.
func blocksFromMessages(msgs []fantasy.Message) []*block {
	var out []*block
	byID := map[string]*block{}
	for _, msg := range msgs {
		switch msg.Role {
		case fantasy.MessageRoleUser:
			text := session.UserText(msg)
			if b, ok := blocksFromCompacted(text); ok {
				out = append(out, b)
				continue
			}
			var files []fantasy.FilePart
			for _, p := range msg.Content {
				if f, ok := p.(fantasy.FilePart); ok {
					files = append(files, f)
				}
			}
			out = append(out, userBlock(text, attachmentNames(files)))
		case fantasy.MessageRoleAssistant:
			for _, p := range msg.Content {
				switch p := p.(type) {
				case fantasy.TextPart:
					if p.Text == agent.CompactAck {
						continue
					}
					if strings.TrimSpace(p.Text) != "" {
						out = append(out, newBlock(blockAssistant, p.Text))
					}
				case fantasy.ReasoningPart:
					if strings.TrimSpace(p.Text) != "" {
						out = append(out, newBlock(blockReasoning, p.Text))
					}
				case fantasy.ToolCallPart:
					b := &block{kind: blockTool, id: p.ToolCallID, name: p.ToolName, status: "ok", args: toolArgs(p.Input)}
					byID[p.ToolCallID] = b
					out = append(out, b)
				}
			}
		case fantasy.MessageRoleTool:
			for _, p := range msg.Content {
				r, ok := p.(fantasy.ToolResultPart)
				if !ok {
					continue
				}
				b := byID[r.ToolCallID]
				if b == nil {
					continue
				}
				switch o := r.Output.(type) {
				case fantasy.ToolResultOutputContentText:
					b.output = sanitize.Terminal(o.Text)
				case fantasy.ToolResultOutputContentError:
					b.status = "error"
					if o.Error != nil {
						b.output = sanitize.Terminal(o.Error.Error())
					}
				}
			}
		}
	}
	return out
}

// recentLines lists this directory's latest sessions for the welcome
// screen.
func (m *model) recentLines() []string {
	list, err := session.List(m.o.Cwd)
	if err != nil || len(list) == 0 {
		return nil
	}
	now := time.Now()
	lines := []string{"", dimStyle.Render("  recent in this directory (/resume to pick):")}
	for i, s := range list {
		if i == recentSessions {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("  … %d more", len(list)-recentSessions)))
			break
		}
		lines = append(lines, "  "+dimStyle.Render("· ")+s.Title+dimStyle.Render("  "+session.Age(s.Updated, now)+"  /resume "+s.ID))
	}
	return lines
}

func (m *model) hasSessions() bool {
	list, err := session.List(m.o.Cwd)
	return err == nil && len(list) > 0
}

// sessionItems is the palette sub-list for /resume.
func sessionItems(m *model) []paletteItem {
	items := sessionStateItems(m, "")
	return append(items,
		paletteItem{group: "Manage", title: "Archived", sub: func(m *model) []paletteItem { return sessionStateItems(m, session.Archived) }},
		paletteItem{group: "Manage", title: "Trash", sub: func(m *model) []paletteItem { return sessionStateItems(m, session.Trash) }},
	)
}

func sessionStateItems(m *model, state string) []paletteItem {
	list, err := session.ListState(m.o.Cwd, state)
	if err != nil {
		return []paletteItem{{title: "Could not read sessions", action: func(m *model) tea.Cmd { return m.sessionError(err) }}}
	}
	now := time.Now()
	var items []paletteItem
	for _, s := range list {
		hint := ""
		if m.conv != nil && s.ID == m.conv.ID {
			hint = "current"
		}
		it := paletteItem{group: "Sessions · ctrl+o for actions", title: sanitize.Terminal(s.Title), hint: hint, cmd: "/resume " + s.ID, sessionID: s.ID, detail: sanitize.Terminal(s.Model) + " · " + session.Age(s.Updated, now)}
		if state != "" {
			it.action = func(m *model) tea.Cmd { return m.openSessionActions(s.ID) }
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		items = append(items, paletteItem{title: "No sessions here", action: func(*model) tea.Cmd { return nil }})
	}
	return items
}
