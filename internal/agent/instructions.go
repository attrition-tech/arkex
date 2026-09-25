package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/prompt"
	"github.com/attrition-tech/arkex/internal/tools"
)

// toolInstructions discovers guidance before an action, not after its side
// effects. Returning it as a tool result gives the model a chance to revise
// the call. Use the active history rather than a process cache: compaction,
// resume and branching must not leave instructions marked seen but absent.
func (a *Agent) toolInstructions(ctx context.Context, tool tools.Tool, input json.RawMessage) (string, error) {
	var root string
	switch t := tool.(type) {
	case *tools.Read:
		root = t.Root
	case *tools.Write:
		root = t.Root
	case *tools.Edit:
		root = t.Root
	case *tools.Bash:
		root = t.Dir
	default:
		return "", nil
	}
	var in struct {
		Path    string `json:"path"`
		Workdir string `json:"workdir"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", nil // The tool supplies the normal argument error.
	}
	home, _ := os.UserHomeDir()
	scope := NewScope(root, home)
	var dir string
	if tool.Name() == "bash" {
		dir = scope.bashWorkdir(in.Workdir)
	} else {
		if in.Path == "" {
			return "", nil
		}
		path := canonical(scope.resolve(in.Path))
		dir = filepath.Dir(path)
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			dir = path
		}
	}
	var out strings.Builder
	for _, path := range prompt.AgentsFiles(dir) {
		args, _ := json.Marshal(map[string]string{"path": path})
		decision, err := a.Policy.Decide(ctx, ToolCall{Name: "read", Input: string(args)})
		if err != nil {
			return "", err
		}
		if !decision.Allowed {
			return "", fmt.Errorf("cannot load project instructions %s: %s", path, decision.Reason)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("load project instructions: %w", err)
		}
		block := prompt.Instructions(path, string(body))
		if a.hasInstructions(block) {
			continue
		}
		out.WriteString("\n" + block)
	}
	if out.Len() == 0 {
		return "", nil
	}
	return "Action not executed: newly discovered AGENTS.md instructions follow. Read them and issue a revised or repeated tool call if appropriate.\n" + out.String(), nil
}

func (a *Agent) hasInstructions(block string) bool {
	header, _, _ := strings.Cut(block, "\n")
	header += "\n"
	// Only the latest version counts, including when a file is reverted.
	for i := len(a.messages) - 1; i >= 0; i-- {
		message := a.messages[i]
		for j := len(message.Content) - 1; j >= 0; j-- {
			part := message.Content[j]
			if result, ok := part.(fantasy.ToolResultPart); ok {
				if text, ok := result.Output.(fantasy.ToolResultOutputContentText); ok && strings.Contains(text.Text, header) {
					return strings.Contains(text.Text, block)
				}
			}
		}
	}
	return strings.Contains(a.System, block)
}
