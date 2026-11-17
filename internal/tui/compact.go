package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"

	"github.com/attrition-tech/arkex/internal/agent"
	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/session"
)

// Disk work belongs to the run's worker. Only a committed, immutable snapshot
// crosses back to the UI, before the worker sends its run-complete message.
type contextSavedMsg struct {
	owner *session.Session
	saved session.Session
	err   error
}

type contextPersistence struct {
	owner *session.Session
	state session.Session
	send  func(tea.Msg)
}

func (p *contextPersistence) store(ctx context.Context, messages []fantasy.Message) (string, error) {
	if p.send == nil {
		return "", errors.New("session event loop is unavailable")
	}
	path, err := p.state.SaveContext(ctx, messages)
	if err != nil {
		return "", err
	}
	// Do not turn a committed save into failure when cancellation races it.
	// SaveContext copies slices before changing them, so subsequent saves
	// cannot mutate this snapshot while the UI consumes it.
	p.send(contextSavedMsg{owner: p.owner, saved: p.state})
	return path, nil
}

// Finish on the same worker as compaction, before announcing run completion.
// Otherwise the final usage save can block the UI on the very lock that Esc
// just cancelled. A cancelled run gets a short best-effort save of partial work.
func (p *contextPersistence) finish(ctx context.Context, messages []fantasy.Message, usage fantasy.Usage, lastInput int64) {
	if p.send == nil {
		return
	}
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 250*time.Millisecond)
		defer cancel()
	}
	next := p.state
	next.History = append([]fantasy.Message(nil), next.History...)
	next.Update(messages, session.Usage{Model: next.Model, Input: next.UsageIn + usage.InputTokens, Output: next.UsageOut + usage.OutputTokens})
	next.LastInput = lastInput
	err := next.SaveWithContext(ctx)
	p.send(contextSavedMsg{owner: p.owner, saved: next, err: err})
}

func (m *model) contextSaved(msg contextSavedMsg) {
	if m.conv == msg.owner {
		if msg.err != nil {
			m.appendSystem("could not save the session: " + msg.err.Error())
			m.refresh()
		} else {
			*m.conv = msg.saved
		}
	}
}

// compact is the manual /compact: it summarises the whole conversation in
// the background. The agent also compacts on its own inside a run when the
// context window is nearly full or a request overflowed it; those arrive as
// agent.Compacted events and are handled in applyEvent.
func (m *model) compact() tea.Cmd {
	switch {
	case m.sess.Agent == nil:
		m.appendSystem("no model connected — /models to add one")
	case m.running:
		m.appendSystem("still working; wait or press esc twice to cancel before /compact")
	case len(m.sess.Agent.Messages()) == 0:
		m.appendSystem("nothing to compact yet")
	}
	if m.sess.Agent == nil || m.running || len(m.sess.Agent.Messages()) == 0 {
		m.refresh()
		return nil
	}
	if err := m.prepareAgent(); err != nil {
		m.appendSystem(err.Error())
		m.refresh()
		return nil
	}
	m.disarmConfirmation()
	m.running = true
	m.compacting = true
	m.compactStarted = time.Now()
	m.compactStage = "preparing history"
	m.startedAt = time.Now()
	m.requestTimings, m.runDuration = nil, 0
	m.runGen++
	m.frame, m.step = 0, 0
	m.status = ""
	m.stickBottom = true
	m.refresh()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	ag := m.sess.Agent
	send := m.send
	persistence := m.persistence
	done := make(chan struct{})
	m.runDone = done
	return tea.Batch(func() tea.Msg {
		defer close(done)
		co := newCoalescer(send)
		res, err := ag.CompactWithProgress(ctx, co.push)
		co.flush()
		persistence.finish(ctx, ag.Messages(), res.Usage, ag.LastInput())
		return compactDoneMsg{res: res, err: err}
	}, m.workingTick())
}

// compactDone records a manual /compact's outcome after the worker has saved.
func (m *model) compactDone(msg compactDoneMsg) tea.Cmd {
	m.disarmConfirmation()
	m.running = false
	m.compacting = false
	m.cancel = nil
	m.status = ""
	switch {
	case errors.Is(msg.err, context.Canceled):
		m.appendSystem("compaction cancelled")
	case msg.err != nil:
		m.appendSystem("compaction failed; original context preserved: " + msg.err.Error())
	}
	m.refresh()
	return m.finishPromptAction()
}

// learnedContextWindow reports a window the agent picked up from an
// overflow error and saves it to the model's config entry so the context
// gauge and pre-emptive compaction work from the next start too.
func (m *model) learnedContextWindow(tokens int) {
	note := fmt.Sprintf("learned this model's context window: %d tokens", tokens)
	ag := m.sess.Agent
	if m.o.ConfigPath != "" && ag != nil && ag.Model != nil {
		ref := ag.Model.Ref
		if err := config.SetContextWindow(m.o.ConfigPath, ref.ConnID, ref.Model.ID, tokens); err != nil {
			note += " (for this session only; saving to config failed: " + err.Error() + ")"
		} else {
			note += " — saved to " + m.o.ConfigPath
		}
	}
	m.appendSystem(note)
}

// compactBlock is the transcript card for a summary: collapsed like
// reasoning (a click shows it), labelled with what it replaced.
func compactBlock(summary string, dropped int) *block {
	b := newBlock(blockReasoning, summary)
	b.name = fmt.Sprintf("context compacted · %d messages summarised", dropped)
	return b
}

// blocksFromCompacted turns the summary pair written by
// agent.CompactedMessages back into one card; ok is false for anything
// else. A user message whose text starts with the compaction prefix is
// the summary; the assistant acknowledgement that follows it is dropped.
func blocksFromCompacted(userText string) (*block, bool) {
	summary, ok := agent.CompactSummary(userText)
	if !ok {
		return nil, false
	}
	return compactBlock(summary, 0), true
}
