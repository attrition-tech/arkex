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
	case *tools.List:
		root = t.Root
	case *tools.Search:
		root = t.Root
	case *tools.Remove:
		root = t.Root
	case *tools.Move:
		root = t.Root
	case *tools.Bash:
		root = t.Dir
	default:
		return "", nil
	}
	var in struct {
		Path        string `json:"path"`
		Workdir     string `json:"workdir"`
		Destination string `json:"destination"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", nil // The tool supplies the normal argument error.
	}
	entryAction := tool.Name() == "remove" || tool.Name() == "move"
	resolve := func(path string) string {
		if path == "~" || strings.HasPrefix(path, "~/") {
			home, _ := os.UserHomeDir()
			path = home + path[1:]
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		path = filepath.Clean(path)
		if entryAction {
			// Removing/moving a final symlink affects its directory entry,
			// not its target's instruction scope.
			return filepath.Join(instructionPath(filepath.Dir(path)), filepath.Base(path))
		}
		return instructionPath(path)
	}
	paths := []string{in.Path}
	if tool.Name() == "bash" {
		paths = []string{in.Workdir}
	} else if in.Path == "" && tool.Name() != "ls" && tool.Name() != "grep" {
		return "", nil
	}
	if tool.Name() == "move" && in.Destination != "" {
		paths = append(paths, in.Destination)
	}
	var out strings.Builder
	files := []string{}
	stat := os.Stat
	if entryAction {
		stat = os.Lstat
	}
	for _, path := range paths {
		dir := resolve(path)
		if tool.Name() != "bash" {
			if st, err := stat(dir); err != nil || !st.IsDir() {
				dir = filepath.Dir(dir)
			}
		}
		found, err := prompt.AgentsFiles(dir)
		if err != nil {
			return "", err
		}
		files = append(files, found...)
	}
	seen := map[string]bool{}
	for _, path := range files {
		if seen[path] {
			continue
		}
		seen[path] = true
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

// Resolve the existing ancestor of a not-yet-created destination. This is
// instruction discovery only; tools enforce writes separately with os.Root.
func instructionPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(instructionPath(parent), filepath.Base(path))
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
