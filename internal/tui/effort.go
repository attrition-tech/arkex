package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/provider"
)

func (m *model) effortLabel() string {
	if m.sess.Agent == nil || m.sess.Agent.Model == nil {
		return ""
	}
	level := m.sess.Agent.Model.Ref.Thinking
	if level == "" {
		return "Default"
	}
	return strings.ToUpper(level[:1]) + level[1:]
}

func effortItems(m *model) []paletteItem {
	if m.sess.Agent == nil || m.sess.Agent.Model == nil {
		return []paletteItem{{title: "Connect a model first", cmd: "/connections"}}
	}
	ref := m.sess.Agent.Model.Ref
	levels, def, source := provider.ReasoningControls(ref)
	detail := source
	if def != "" {
		detail = "default: " + def + " · " + detail
	}
	if len(levels) > 0 && !slices.Contains(levels, "off") && !ref.Model.ReasoningChecked {
		detail = "always on · " + detail
	}
	items := []paletteItem{{title: "Default", cmd: "/effort default", detail: detail}}
	for _, level := range levels {
		items = append(items, paletteItem{title: strings.ToUpper(level[:1]) + level[1:], cmd: "/effort " + level})
	}
	for i := range items {
		if (i == 0 && ref.Thinking == "") || (i > 0 && levels[i-1] == ref.Thinking) {
			items[i].hint = "selected"
		}
	}
	if ref.Conn.Kind != config.KindSubscription && ref.Conn.Kind != config.KindRuntime &&
		(ref.Conn.API == "" || ref.Conn.API == config.APIOpenAICompat) {
		items = append(items, paletteItem{title: "Check reasoning…", detail: "Test this model's accepted request settings", action: func(m *model) tea.Cmd { return m.confirmReasoningCheck() }})
	}
	if m.running {
		items[0].detail = "Stop the current run before changing reasoning."
	}
	return items
}

func (m *model) setEffort(level string) {
	if m.running {
		m.appendSystem("cannot change reasoning while working; stop the run first")
	} else if m.sess.Agent == nil || m.sess.Agent.Model == nil {
		m.appendSystem("connect a model first with /connections")
	} else {
		if level == "default" {
			level = ""
		}
		if err := m.sess.Agent.Model.SetThinking(level); err != nil {
			m.appendSystem(err.Error())
		} else {
			m.sess.Name = m.sess.Agent.Model.Ref.String()
			if level != "" {
				m.sess.Name += ":" + level
			}
			m.saveConv()
			m.flash("reasoning: " + m.effortLabel() + " · applies to the next request")
		}
	}
	m.refresh()
}

func (m *model) confirmReasoningCheck() tea.Cmd {
	if m.running || m.sess.Agent == nil || m.sess.Agent.Model == nil {
		return m.openPaletteSub("Reasoning check unavailable", []paletteItem{
			{title: "Use an idle, connected model first", action: func(m *model) tea.Cmd { return m.closePalette() }},
		})
	}
	return m.openPaletteSub("Check reasoning · "+m.sess.Agent.Model.Ref.String(), []paletteItem{
		{title: "Cancel", action: func(m *model) tea.Cmd { return m.closePalette() }},
		{title: "Run check", hint: "may incur cost", detail: fmt.Sprintf("Up to %d synthetic requests", provider.ReasoningCheckMaxRequests), action: func(m *model) tea.Cmd { return m.startReasoningCheck() }},
		{title: "64 output tokens requested each"},
		{title: "3 min total · 15 s per request"},
		{title: "No history, attachments or tools"},
		{title: "Acceptance, not reasoning proof"},
	})
}

type reasoningCheckedMsg struct {
	owner  *palette
	model  *provider.Model
	ref    config.ModelRef
	result provider.CheckedReasoning
	err    error
}

func (m *model) startReasoningCheck() tea.Cmd {
	if m.running || m.sess.Agent == nil || m.sess.Agent.Model == nil {
		return m.confirmReasoningCheck()
	}
	mdl := m.sess.Agent.Model
	ref := mdl.Ref
	cmd := m.openPaletteSub("Checking reasoning · esc cancels", []paletteItem{
		{title: "Cancel check", action: func(m *model) tea.Cmd { return m.closePalette() }},
		{title: "Testing " + ref.String()},
		{title: "3 min max · settings unchanged"},
	})
	p := m.pal
	ctx, cancel := context.WithCancel(context.Background())
	p.busy, p.cancel = true, cancel
	p.input.Blur()
	return tea.Batch(cmd, func() tea.Msg {
		defer cancel()
		result, err := provider.CheckReasoning(ctx, ref, nil)
		return reasoningCheckedMsg{owner: p, model: mdl, ref: ref, result: result, err: err}
	})
}

func (m *model) reasoningChecked(msg reasoningCheckedMsg) tea.Cmd {
	if m.pal != msg.owner || m.running || m.sess.Agent == nil || m.sess.Agent.Model != msg.model {
		return nil // cancelled, superseded, or a different model/session
	}
	items := []paletteItem{{title: "Keep current settings", action: func(m *model) tea.Cmd { return m.closePalette() }}}
	if msg.err != nil {
		items = append(items, paletteItem{title: "Check stopped · settings unchanged", detail: msg.err.Error()})
	} else if len(msg.result.Levels) == 0 {
		items = append(items,
			paletteItem{title: "Inconclusive · settings unchanged", detail: "No controls passed validation"},
			paletteItem{title: "Success alone proves no support"},
		)
	} else {
		items = append(items,
			paletteItem{title: "Accepted by server", detail: strings.Join(msg.result.Levels, " · ")},
			paletteItem{title: "Format: " + string(msg.result.Protocol)},
			paletteItem{title: "Not proof of reasoning behavior"},
			paletteItem{title: "Use these options for this model", detail: "Replaces controls · use Default", action: func(m *model) tea.Cmd { return m.useCheckedReasoning(msg) }},
		)
	}
	return m.openPaletteSub("Reasoning check result", items)
}

func (m *model) useCheckedReasoning(msg reasoningCheckedMsg) tea.Cmd {
	if m.running || m.sess.Agent == nil || m.sess.Agent.Model != msg.model {
		return m.closePalette()
	}
	next := *msg.model
	next.Ref.Model.ReasoningLevels = slices.Clone(msg.result.Levels)
	next.Ref.Model.ReasoningChecked = true
	if next.Ref.Compat().Thinking != msg.result.Protocol || !slices.Contains(msg.result.Levels, next.Ref.Model.ReasoningDefault) {
		next.Ref.Model.ReasoningDefault = "" // the check does not discover defaults
	}
	next.Ref.Model.Compat.Thinking = msg.result.Protocol
	next.Ref.Conn.Models = slices.Clone(next.Ref.Conn.Models)
	for i, md := range next.Ref.Conn.Models {
		if md.ID == next.Ref.Model.ID {
			next.Ref.Conn.Models[i] = next.Ref.Model
		}
	}
	if err := next.SetThinking(""); err != nil {
		return m.sessionError(err)
	}
	if err := config.SaveCheckedReasoning(m.o.ConfigPath, msg.ref, next.Ref.Model); err != nil {
		return m.sessionError(err)
	}
	msg.model.Ref, msg.model.Opts = next.Ref, next.Opts
	m.sess.Name = next.Ref.String()
	m.saveConv()
	m.refresh()
	m.flash("checked reasoning saved · Default applies to the next request")
	return m.openPaletteSub("Reasoning effort", effortItems(m))
}
