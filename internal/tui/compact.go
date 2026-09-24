package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dantearo/arkex/internal/agent"
	"github.com/dantearo/arkex/internal/config"
)

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
	done := make(chan struct{})
	m.runDone = done
	return tea.Batch(func() tea.Msg {
		defer close(done)
		res, err := ag.Compact(ctx)
		return compactDoneMsg{res: res, err: err}
	}, m.workingTick())
}

// compactDone records the outcome of a manual /compact in the transcript
// and persists the shorter conversation.
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
		m.appendSystem("compaction failed: " + msg.err.Error())
	default:
		m.lastInput = 0 // unknown until the next request
		m.usageIn += msg.res.Usage.InputTokens
		m.usageOut += msg.res.Usage.OutputTokens
		m.blocks = append(m.blocks, compactBlock(msg.res.Summary, msg.res.Dropped))
		if msg.res.Trimmed > 0 {
			m.appendSystem(fmt.Sprintf("the %d oldest messages no longer fit and were dropped unsummarised", msg.res.Trimmed))
		}
		m.saveConv()
	}
	m.refresh()
	if m.editAfterStop != 0 {
		index := m.editAfterStop
		m.editAfterStop = 0
		return m.beginPromptEdit(index)
	}
	return nil
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
