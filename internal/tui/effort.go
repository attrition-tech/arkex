package tui

import (
	"slices"
	"strings"

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
	if len(levels) > 0 && !slices.Contains(levels, "off") {
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
