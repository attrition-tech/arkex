package tui

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/session"
)

type inboxDoneMsg struct {
	input  session.PendingInput
	action string
	err    error
}

type inputDeliveredMsg struct {
	owner  *session.Session
	input  session.PendingInput
	prompt int
}

// commitInput is called with the persistence lock held. Never publish a
// speculative snapshot: a failed save leaves the pending entry untouched.
func (p *contextPersistence) commitInput(ctx context.Context, next session.Session) error {
	if p.send == nil {
		return errors.New("session event loop is unavailable")
	}
	next.Updated = time.Now()
	if err := next.SaveWithContext(ctx); err != nil {
		return err
	}
	p.state = next
	p.send(contextSavedMsg{owner: p.owner, saved: next})
	return nil
}

func (p *contextPersistence) changeInput(ctx context.Context, input session.PendingInput, action string) inboxDoneMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := p.state
	next.Pending = slices.Clone(next.Pending)
	index := slices.IndexFunc(next.Pending, func(item session.PendingInput) bool { return item.ID == input.ID })
	result := inboxDoneMsg{input: input, action: action}
	if action == "add" {
		if index >= 0 { // a repeated local admission must not duplicate an item
			result.input = next.Pending[index]
			return result
		}
		input.Steer = input.Steer && p.steerOpen
		next.Pending = append(next.Pending, input)
		if next.Title == "" {
			next.Title = session.TitleFor([]fantasy.Message{input.Message})
		}
		result.input = input
	} else if index < 0 {
		result.err = errors.New("message is no longer pending; it may already be recorded")
		return result
	} else {
		result.input = next.Pending[index]
		switch action {
		case "steer":
			next.Pending[index].Steer = p.steerOpen
			result.input = next.Pending[index]
		case "restore", "delete":
			next.Pending = slices.Delete(next.Pending, index, index+1)
		}
	}
	result.err = p.commitInput(ctx, next)
	return result
}

// takeInput runs on the agent worker, never on the UI goroutine. Admission
// and the final empty check share mu. Steers accepted after that check become
// queued follow-ups, not instructions for an unrelated successor run.
func (p *contextPersistence) takeInput(ctx context.Context, history []fantasy.Message, completing bool) ([]fantasy.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var inputs []session.PendingInput
	if p.steerOpen {
		for _, input := range p.state.Pending {
			if input.Steer && input.Model == p.state.Model {
				inputs = append(inputs, input)
			}
		}
	}
	if completing && len(inputs) == 0 {
		p.steerOpen = false
		return nil, nil
	}
	if len(inputs) == 0 && p.started {
		return nil, nil
	}
	return p.recordInput(ctx, history, inputs)
}

// recordInput atomically moves entries into history and editable checkpoints.
// Both slices and prompt messages are immutable after publication to the UI.
// The caller holds mu; only the worker supplies live history.
func (p *contextPersistence) recordInput(ctx context.Context, history []fantasy.Message, inputs []session.PendingInput) ([]fantasy.Message, error) {
	next := p.state
	next.History = slices.Clone(next.History)
	next.Prompts = slices.Clone(next.Prompts)
	next.Pending = slices.Clone(next.Pending)
	messages := slices.Clone(history)
	var added []fantasy.Message
	var delivered []inputDeliveredMsg
	for _, input := range inputs {
		prompt := next.Checkpoint(messages, input.Text, input.Message, input.Model)
		messages = append(messages, input.Message)
		added = append(added, input.Message)
		next.Pending = slices.DeleteFunc(next.Pending, func(item session.PendingInput) bool { return item.ID == input.ID })
		delivered = append(delivered, inputDeliveredMsg{owner: p.owner, input: input, prompt: prompt})
	}
	next.Update(messages, session.Usage{Model: next.Model, Input: next.UsageIn, Output: next.UsageOut})
	next.RunPending = true
	if err := p.commitInput(ctx, next); err != nil {
		return nil, err
	}
	p.started = true
	for _, msg := range delivered {
		p.send(msg)
	}
	return added, nil
}

func (p *contextPersistence) takeQueued(ctx context.Context, history []fantasy.Message, id string) (fantasy.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fantasy.Message{}, err
	}
	for _, input := range p.state.Pending {
		if input.ID == id && input.Model == p.state.Model {
			input.Steer = false
			_, err := p.recordInput(ctx, history, []session.PendingInput{input})
			return input.Message, err
		}
	}
	return fantasy.Message{}, errors.New("queued message changed or belongs to a different model; open /queue")
}

// Keep a draft until its durable receipt. Input events are replayed in order
// afterward, so typing during disk IO is neither dropped nor sent twice.
func (m *model) inboxCommand(input session.PendingInput, action string) tea.Cmd {
	if !m.running {
		if err := m.prepareAgent(); err != nil {
			return m.sessionError(err)
		}
	}
	p := m.persistence
	if p == nil {
		return m.sessionError(errors.New("no active session"))
	}
	m.inboxSaving = true
	m.refresh()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return p.changeInput(ctx, input, action)
	}
}

func (m *model) enqueueDraft(steer bool) tea.Cmd {
	if m.sess.Agent == nil {
		m.openModels(m.o.StartupNote)
		return nil
	}
	text := m.takeImageMentions(strings.TrimSpace(m.input.Value()))
	if text == "" && len(m.attachments) == 0 {
		return nil
	}
	// Snapshot file contents now, not when the queued turn starts.
	notes := m.shellNotes
	send := expandMentions(m.o.Cwd, text) + m.takeShellNotes()
	m.shellNotes = notes // consumed only after a successful admission
	if send == "" {
		send = "(see the attached image)"
	}
	input := session.PendingInput{ID: rand.Text(), Text: text,
		Message: fantasy.NewUserMessage(send, m.fileParts()...), Model: m.sess.Name,
		Steer: steer && m.running}
	return m.inboxCommand(input, "add")
}

func (m *model) inboxDone(msg inboxDoneMsg) tea.Cmd {
	m.inboxSaving = false
	if msg.err != nil {
		m.appendSystem("Message not saved: " + msg.err.Error() + ". Your draft is unchanged.")
	} else {
		switch msg.action {
		case "add":
			m.hist.add(msg.input.Text)
			m.input.Reset()
			m.attachments, m.shellNotes, m.comp = nil, nil, nil
		case "restore":
			m.setInput(msg.input.Text)
			for _, part := range msg.input.Message.Content {
				if file, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
					m.attachments = append(m.attachments, attachment{name: file.Filename, mediaType: file.MediaType, data: file.Data})
				}
			}
		}
		stillPending := m.conv != nil && slices.ContainsFunc(m.conv.Pending, func(input session.PendingInput) bool { return input.ID == msg.input.ID })
		if stillPending && (msg.action == "add" || msg.action == "steer") {
			if msg.input.Steer {
				m.flash("Steering saved · applies after the current step")
			} else {
				m.flash("Queued and saved · runs as a separate turn")
			}
		}
	}
	m.resizeInput()
	m.refresh()
	deferred := m.inboxDeferred
	m.inboxDeferred = nil
	m.inboxReplaying = true
	var commands []tea.Cmd
	var approvals []tea.Msg
	for _, event := range deferred {
		if _, ok := event.(approvalMsg); ok {
			approvals = append(approvals, event)
			continue
		}
		_, cmd := m.Update(event)
		commands = append(commands, cmd)
	}
	// Keystrokes typed into the visible composer must never answer a modal
	// that appeared during its save. Present that modal after replaying them.
	for _, event := range approvals {
		if m.running {
			_, cmd := m.Update(event)
			commands = append(commands, cmd)
		}
	}
	m.inboxReplaying = false
	if !m.inboxSaving && msg.err == nil {
		commands = append(commands, m.startQueued(false))
	}
	return tea.Batch(commands...)
}

func (m *model) startQueued(resume bool) tea.Cmd {
	if m.running || m.inboxSaving || m.inboxReplaying || m.conv == nil || len(m.conv.Pending) == 0 || m.sess.Agent == nil {
		return nil
	}
	if m.conv.QueuePaused && !resume {
		return nil
	}
	input := m.conv.Pending[0]
	if input.Model != m.sess.Name {
		m.conv.QueuePaused = true
		m.appendSystem("Queue paused: the next message was saved for " + input.Model + ". Switch back to that model before resuming /queue.")
		m.refresh()
		return nil
	}
	m.conv.QueuePaused = false
	var persistence *contextPersistence
	cmd := m.startRun(func(ctx context.Context, ag *agent.Agent, emit func(agent.Event)) error {
		msg, err := persistence.takeQueued(ctx, ag.Messages(), input.ID)
		if err != nil {
			return err
		}
		return ag.RunMessage(ctx, msg, emit)
	})
	persistence = m.persistence
	return cmd
}

func (m *model) refreshPendingPalette() {
	if m.pal == nil {
		return
	}
	for i, level := range m.pal.levels {
		if level.title == "Pending messages" {
			m.pal.levels = m.pal.levels[:i+1]
			m.pal.levels[i].items = pendingItems(m)
			m.pal.sel = 0
			m.pal.filter()
			return
		}
	}
}

func pendingItems(m *model) []paletteItem {
	items := []paletteItem{{title: "Queue current draft", hint: "cmd+enter", action: func(m *model) tea.Cmd {
		m.closePalette()
		return m.enqueueDraft(false)
	}}}
	if m.conv == nil || len(m.conv.Pending) == 0 {
		return append(items, paletteItem{title: "No pending messages"})
	}
	if !m.running {
		items = append(items, paletteItem{title: "Resume pending messages", action: func(m *model) tea.Cmd {
			m.closePalette()
			return m.startQueued(true)
		}})
	}
	for i, input := range m.conv.Pending {
		mode := "Queued"
		if input.Steer {
			mode = "Steering pending"
		}
		text := strings.Join(strings.Fields(input.Text), " ")
		if text == "" {
			text = "Attached image"
		}
		items = append(items, paletteItem{title: fmt.Sprintf("%d. %s", i+1, text), detail: mode + " · " + input.Model,
			sub: func(m *model) []paletteItem {
				return []paletteItem{
					{title: "Steer instead", action: func(m *model) tea.Cmd {
						m.closePalette()
						return m.inboxCommand(input, "steer")
					}},
					{title: "Restore to composer", detail: "Withdraw from the queue to edit; resend when ready", action: func(m *model) tea.Cmd {
						m.closePalette()
						if m.input.Value() != "" || len(m.attachments) != 0 {
							return m.sessionError(errors.New("save or clear your current draft first"))
						}
						return m.inboxCommand(input, "restore")
					}},
					{title: "Delete pending message", action: func(m *model) tea.Cmd {
						m.closePalette()
						return m.inboxCommand(input, "delete")
					}},
				}
			}})
	}
	return items
}
