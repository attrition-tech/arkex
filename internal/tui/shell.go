package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/attrition-tech/arkex/internal/sanitize"
	"github.com/attrition-tech/arkex/internal/tools"
)

// A prompt starting with "!" runs as a shell command right away, without
// the model and without approval: it is the user's own command. The result
// shows as a tool card and is attached to the next prompt so the model can
// see what the user just saw.

const (
	maxShellNotes     = 5
	maxShellNoteBytes = 16 * 1024
)

type shellNote struct {
	command string
	output  string
}

type shellDoneMsg struct {
	id  string
	res tools.Result
	err error
	dur time.Duration
}

// runShell starts command in the working directory and returns the tea.Cmd
// that reports its completion.
func (m *model) runShell(command string) tea.Cmd {
	m.shellSeq++
	id := fmt.Sprintf("shell-%d", m.shellSeq)
	b := &block{kind: blockTool, id: id, name: "bash", status: "running", args: map[string]any{"command": command}}
	m.blocks = append(m.blocks, b)
	m.stickBottom = true
	m.refresh()

	dir := m.o.Cwd
	return func() tea.Msg {
		in, _ := json.Marshal(map[string]any{"command": command})
		start := time.Now()
		res, err := (&tools.Bash{Dir: dir}).Run(context.Background(), in)
		return shellDoneMsg{id: id, res: res, err: err, dur: time.Since(start)}
	}
}

// finishShell fills in the card and queues the output for the next prompt.
func (m *model) finishShell(msg shellDoneMsg) {
	for _, b := range m.blocks {
		if b.kind != blockTool || b.id != msg.id {
			continue
		}
		b.output, b.summary, b.dur = sanitize.Terminal(msg.res.Output), sanitize.Terminal(msg.res.Summary), msg.dur.Round(time.Millisecond)
		b.status = "ok"
		if msg.err != nil {
			b.status = "error"
			if b.output == "" {
				b.output = sanitize.Terminal(msg.err.Error())
			}
		}
		command, _ := b.args["command"].(string)
		out := b.output
		if len(out) > maxShellNoteBytes {
			out = out[:maxShellNoteBytes] + "\n[truncated]"
		}
		m.shellNotes = append(m.shellNotes, shellNote{command: command, output: out})
		if len(m.shellNotes) > maxShellNotes {
			m.shellNotes = m.shellNotes[len(m.shellNotes)-maxShellNotes:]
		}
		break
	}
	m.refresh()
}

// takeShellNotes renders queued shell results as an attachment for the next
// prompt and clears the queue.
func (m *model) takeShellNotes() string {
	if len(m.shellNotes) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, n := range m.shellNotes {
		fmt.Fprintf(&sb, "\n\n<shell command=%q>\n%s\n</shell>", n.command, strings.TrimRight(n.output, "\n"))
	}
	m.shellNotes = nil
	return sb.String()
}
