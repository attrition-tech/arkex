package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/attrition-tech/arkex/internal/session"
)

type sessionConnectedMsg struct {
	gen   int
	owner *palette
	ctx   context.Context
	saved *session.Session // nil starts a new conversation
	conn  Connection
	err   error
}

func restoreSessionEffort(c *Connection, s *session.Session) error {
	if s.Effort == nil || c.Agent.Model == nil {
		return nil
	}
	if err := c.Agent.Model.SetThinking(*s.Effort); err != nil {
		return fmt.Errorf("saved reasoning setting unavailable: %w", err)
	}
	c.Name = c.Agent.Model.Ref.String()
	if *s.Effort != "" {
		c.Name += ":" + *s.Effort
	}
	return nil
}

// Keep the current session untouched until the requested model is ready.
// The palette owns cancellation, so late results cannot switch a session.
func (m *model) connectSession(s *session.Session, selector string, restore bool) tea.Cmd {
	m.connectionGen++
	gen := m.connectionGen
	m.status = ""
	if m.o.Connect == nil {
		return m.chooseSessionModel(s, errors.New("no connector available"))
	}
	label := selector
	if label == "" {
		label = "configured default"
	}
	cmd := m.openPaletteSub("Connecting · "+label, []paletteItem{{title: "Connecting…", hint: "esc to cancel"}})
	p := m.pal
	p.busy = true
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	p.cancel = cancel
	connect := m.o.Connect
	return tea.Batch(cmd, func() tea.Msg {
		c, err := connect(ctx, selector)
		if err == nil && restore && s != nil {
			err = restoreSessionEffort(&c, s)
		}
		return sessionConnectedMsg{gen: gen, owner: p, ctx: ctx, saved: s, conn: c, err: err}
	})
}

func (m *model) sessionConnected(msg sessionConnectedMsg) tea.Cmd {
	if m.pal != msg.owner || msg.gen != m.connectionGen {
		return nil
	}
	if msg.ctx.Err() != nil && msg.err == nil {
		msg.err = msg.ctx.Err()
	}
	if msg.err != nil {
		return m.chooseSessionModel(msg.saved, msg.err)
	}
	cmd := m.closePalette()
	m.setSession(msg.conn)
	if msg.saved != nil {
		m.loadConversation(msg.saved)
	} else {
		m.newConv()
		m.replaceTranscript([]*block{m.welcome})
		m.paused = false
		m.renderWelcome()
		m.refresh()
	}
	return cmd
}

func (m *model) chooseSessionModel(s *session.Session, err error) tea.Cmd {
	title := "Default unavailable · choose a model"
	if s != nil {
		title = "Session model unavailable · choose replacement"
	}
	items := []paletteItem{{title: "Cancel · keep current session", detail: err.Error(), action: func(m *model) tea.Cmd { return m.closePalette() }}}
	for _, item := range modelItems(m) {
		selector := item.title
		item.cmd = ""
		item.action = func(m *model) tea.Cmd { return m.connectSession(s, selector, false) }
		items = append(items, item)
	}
	if len(items) == 1 {
		items = append(items, paletteItem{title: "Configure models, then retry", cmd: "/connections"})
	}
	return m.openPaletteSub(title, items)
}

func (m *model) newSession() tea.Cmd {
	if m.o.Connect != nil {
		return m.connectSession(nil, "", false)
	}
	// Without a connector, the supplied agent is the only available default.
	m.newConv()
	m.replaceTranscript([]*block{m.welcome})
	m.paused = false
	m.refresh()
	return nil
}
