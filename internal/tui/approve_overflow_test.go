package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/dantearo/arkex/internal/agent"
)

func TestMultilineApprovalKeepsActionsVisible(t *testing.T) {
	m, _ := testModel(t)
	m.running = true
	input, _ := json.Marshal(map[string]string{"command": strings.Repeat("echo command\n", 40) + "echo COMMAND_END"})
	for _, trusted := range []bool{false, true} {
		call := agent.ToolCall{Name: "bash", Input: string(input), Grantable: trusted,
			Reason: "command names a path outside the workspace: /.bundle\n" + strings.Repeat("/node_modules\n/log/*\n!/log/.keep\n", 20) + "REASON_END"}
		if trusted {
			call.TrustDirectory, call.TrustAccess = "/outside", "read, changes and shell path checks"
		}
		for _, size := range [][2]int{{120, 36}, {60, 20}, {24, 8}, {100, 30}} {
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.Update(approvalMsg{call: call, reply: make(chan agent.Answer, 1)})
			lines := frameLines(t, m, "multiline approval")
			a := m.pending
			for _, hit := range a.hits {
				y := m.approvalButtonY() + hit.y
				if y < 0 || y >= m.height || y >= len(lines) {
					t.Fatalf("button outside screen at %v: %+v y=%d", size, hit, y)
				}
				label := ansi.Cut(ansi.Strip(lines[y]), hit.x0, hit.x1)
				if strings.TrimSpace(label) == "" || strings.Contains(label, "/log/") {
					t.Fatalf("button hit covers preview instead: %q", label)
				}
				if a.hitAt(hit.x0, hit.y) != hit.idx {
					t.Fatal("button not clickable")
				}
			}
			if !strings.Contains(a.preview, "REASON_END") {
				t.Fatal("reason was discarded")
			}
			if size[1] >= 20 {
				m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
				if !strings.Contains(ansi.Strip(m.View().Content), "COMMAND_END") {
					t.Fatal("command tail inaccessible")
				}
				frameLines(t, m, "scrolled multiline approval")
			}
		}
	}
}
