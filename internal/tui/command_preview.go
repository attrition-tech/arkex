package tui

import (
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Multiline commands are scripts, not a flattened line of source code. Keep
// the summary literal rather than guessing the script's purpose from its text.
func commandTitle(command string) string {
	command = strings.TrimSpace(command)
	if strings.Contains(command, "\n") {
		return fmt.Sprintf("Multiline command · %d lines", strings.Count(command, "\n")+1)
	}
	return "$ " + command
}

// Shorten only an unambiguous leading `cd path && [source file &&] command`.
// This is display-only: the original command remains in expanded details.
func commandPreview(command, cwd string) (preview, directory string) {
	preview = command
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) == 0 {
		return
	}
	var chain []*syntax.Stmt
	var flatten func(*syntax.Stmt)
	flatten = func(stmt *syntax.Stmt) {
		if binary, ok := stmt.Cmd.(*syntax.BinaryCmd); ok && binary.Op == syntax.AndStmt && !stmt.Negated && !stmt.Background && len(stmt.Redirs) == 0 {
			flatten(binary.X)
			flatten(binary.Y)
		} else {
			chain = append(chain, stmt)
		}
	}
	flatten(file.Stmts[0])
	if len(chain) < 2 {
		return
	}
	first := chain[0]
	call, ok := first.Cmd.(*syntax.CallExpr)
	if !ok || first.Negated || first.Background || len(first.Redirs) != 0 || len(call.Assigns) != 0 || len(call.Args) != 2 || call.Args[0].Lit() != "cd" {
		return
	}
	path := call.Args[1].Lit()
	if path == "" && len(call.Args[1].Parts) == 1 {
		switch p := call.Args[1].Parts[0].(type) {
		case *syntax.SglQuoted:
			path = p.Value
		case *syntax.DblQuoted:
			if len(p.Parts) == 1 {
				if p, ok := p.Parts[0].(*syntax.Lit); ok {
					path = p.Value
				}
			}
		}
	}
	if path == "" || strings.ContainsAny(path, "*$?{\\") || strings.HasPrefix(path, "-") {
		return
	}
	directory = displayPath(cwd, path)
	i := 1
	if len(chain) > 2 {
		setup, ok := chain[i].Cmd.(*syntax.CallExpr)
		if ok && !chain[i].Negated && !chain[i].Background && len(chain[i].Redirs) == 0 && len(setup.Assigns) == 0 && len(setup.Args) == 2 && (setup.Args[0].Lit() == "source" || setup.Args[0].Lit() == ".") && setup.Args[1].Lit() != "" {
			i++
		}
	}
	preview = command[chain[i].Pos().Offset():]
	return
}
