package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"
	"github.com/dantearo/arkex/internal/sanitize"
	"github.com/dantearo/arkex/internal/session"
)

type promptEdit struct {
	index       int
	sessionID   string
	draft       string
	attachments []attachment
}

func promptEditItems(m *model) []paletteItem {
	var items []paletteItem
	if m.conv != nil {
		for i := len(m.conv.Prompts); i > 0; i-- {
			index := i
			label := strings.Join(strings.Fields(sanitize.Terminal(m.conv.Prompts[i-1].Text)), " ")
			if label == "" {
				label = "Image prompt"
			}
			items = append(items, paletteItem{title: fmt.Sprintf("%d · %s", i, ansi.Truncate(label, 45, "…")),
				action: func(m *model) tea.Cmd { return m.requestPromptEdit(index) }})
		}
	}
	if len(items) == 0 {
		items = append(items, paletteItem{title: "No saved prompts to edit", action: func(m *model) tea.Cmd { return m.closePalette() }})
	}
	return items
}

func (m *model) requestPromptEdit(index int) tea.Cmd {
	if m.conv == nil || index < 1 || index > len(m.conv.Prompts) {
		m.flash("No checkpoint is available for this prompt")
		return nil
	}
	if m.running {
		return m.openPaletteSub("Stop the current run to edit?", []paletteItem{
			{title: "Keep working", action: func(m *model) tea.Cmd { return m.closePalette() }},
			{title: "Stop and edit", detail: "Completed commands and file changes will remain.", action: func(m *model) tea.Cmd {
				cmd := m.closePalette()
				// Completion may have arrived while the dialog was open.
				if !m.running {
					return m.beginPromptEdit(index)
				}
				m.editAfterStop = index
				m.cancelRun()
				return cmd
			}},
		})
	}
	return m.beginPromptEdit(index)
}

func (m *model) beginPromptEdit(index int) tea.Cmd {
	if m.running || m.conv == nil || index < 1 || index > len(m.conv.Prompts) {
		return nil
	}
	cmd := m.closePalette()
	if m.editingPrompt != nil {
		m.cancelPromptEdit()
	}
	p := m.conv.Prompts[index-1]
	m.editingPrompt = &promptEdit{index: index, sessionID: m.conv.ID, draft: m.input.Value(),
		attachments: append([]attachment(nil), m.attachments...)}
	m.promptFocus, m.comp = nil, nil
	m.attachments = nil
	for _, part := range p.Prompt.Content {
		if f, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
			m.attachments = append(m.attachments, attachment{name: f.Filename, mediaType: f.MediaType, data: f.Data})
		}
	}
	m.setInput(p.Text)
	m.flashText = ""
	m.refresh()
	return cmd
}

func (m *model) cancelPromptEdit() {
	if e := m.editingPrompt; e != nil {
		m.editingPrompt = nil
		m.attachments = e.attachments
		m.comp = nil
		m.setInput(e.draft)
		m.refresh()
	}
}

func (m *model) confirmPromptResend() tea.Cmd {
	return m.openPaletteSub("Resend from this prompt?", []paletteItem{
		{title: "Keep editing", action: func(m *model) tea.Cmd { return m.closePalette() }},
		{title: "Resend in a new branch", detail: "Later messages excluded. File changes remain.", action: func(m *model) tea.Cmd { return m.resendPrompt() }},
	})
}

func (m *model) resendPrompt() tea.Cmd {
	e := m.editingPrompt
	if m.running || e == nil || m.conv == nil || m.conv.ID != e.sessionID || m.sess.Agent == nil {
		return m.closePalette()
	}
	next, err := m.conv.Fork(e.index)
	if err == nil {
		// Never abandon the original branch if it could not be saved.
		m.conv.Update(m.sess.Agent.Messages(), m.sess.Name, string(m.mode()), m.usageIn, m.usageOut)
		err = m.conv.Save()
	}
	if err != nil {
		return m.sessionError(err)
	}
	if providerOf(next.Model) != providerOf(m.sess.Name) {
		next.Messages = stripReasoning(next.Messages)
	}
	text := m.takeImageMentions(strings.TrimSpace(m.input.Value()))
	files := m.fileParts()
	m.editingPrompt = nil
	m.attachments = nil
	m.input.Reset()
	m.comp = nil
	m.closePalette()
	m.conv = next
	m.sess.Agent.SetMessages(next.Messages)
	m.sess.Agent.RestoreLastInput(0)
	m.usageIn, m.usageOut, m.lastInput = 0, 0, 0
	m.replaceTranscript(append([]*block{m.welcome}, blocksFromMessages(next.Messages)...))
	m.bindPromptCheckpoints()
	m.resizeInput()
	m.hist.add(text)
	m.flash("New branch · original conversation kept in Sessions")
	return m.submit(text, files)
}

// Match surviving user messages backwards. Repeated prompts remain distinct;
// compacted prompts are still available through the palette checkpoint list.
func (m *model) bindPromptCheckpoints() {
	if m.conv == nil {
		return
	}
	i := len(m.conv.Prompts) - 1
	for b := len(m.blocks) - 1; b >= 0 && i >= 0; b-- {
		if m.blocks[b].kind != blockUser {
			continue
		}
		text := m.blocks[b].text.String()
		for i >= 0 {
			p := m.conv.Prompts[i]
			i--
			if text == p.Text || text == session.UserText(p.Prompt) {
				m.blocks[b].prompt = i + 2
				break
			}
		}
	}
}
