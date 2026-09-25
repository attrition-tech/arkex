package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/attrition-tech/arkex/internal/tools"
	"mvdan.cc/sh/v3/syntax"
)

// Reach says how far a tool call goes relative to the workspace.
type Reach int

const (
	// ReachInside stays within the workspace: the mode and config rule.
	ReachInside Reach = iota
	// ReachOutsideRead reads a file outside the workspace.
	ReachOutsideRead
	// ReachOutsideWrite changes something outside the workspace, or runs a
	// command that names a path outside it.
	ReachOutsideWrite
)

// Scope tracks the workspace boundary and explicitly trusted directory
// subtrees for this process. Shell checks are heuristic, not a sandbox.
type Scope struct {
	Root string // absolute workspace directory
	Home string // user's home, for ~ expansion and display; may be empty

	mu      sync.Mutex
	scratch string
	granted []string // directories whose subtree may be read
	changes []string // directories trusted for changes and shell path checks
}

// NewScope returns a scope rooted at root. The root is resolved through
// symlinks (macOS /tmp is /private/tmp) so that paths compared against it
// after their own resolution line up.
func NewScope(root, home string) *Scope {
	return &Scope{Root: canonical(filepath.Clean(root)), Home: canonical(filepath.Clean(home))}
}

// canonical resolves symlinks in abs. When the path does not exist yet
// (a file about to be written) the deepest existing ancestor is resolved
// and the missing tail appended, so `link/new.txt` with link -> /etc is
// seen as /etc/new.txt rather than as something inside the workspace.
func canonical(abs string) string {
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	dir, base := filepath.Split(abs)
	dir = filepath.Clean(dir)
	if dir == abs || base == "" {
		return abs // filesystem root or nothing left to strip
	}
	return filepath.Join(canonical(dir), base)
}

// systemPrefixes are directories a shell command may name without leaving
// the workspace in any meaningful sense: toolchains and devices.
var systemPrefixes = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib64", "/dev", "/proc", "/sys",
	"/etc",
	"/opt/homebrew", "/nix", "/System", "/Library", "/Applications",
}

// Classify reports where call reaches and, for anything outside the
// workspace, the offending path (with the home directory shortened to ~).
func (s *Scope) Classify(call ToolCall) (Reach, string) {
	reach, paths := s.classifyPaths(call)
	if len(paths) == 0 {
		return ReachInside, ""
	}
	return reach, paths[0]
}

func (s *Scope) classifyPaths(call ToolCall) (Reach, []string) {
	if s == nil || s.Root == "" {
		return ReachInside, nil
	}
	var in struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		Workdir string `json:"workdir"`
	}
	_ = json.Unmarshal([]byte(call.Input), &in)

	if call.Name == "bash" {
		cwd := s.bashWorkdir(in.Workdir)
		var paths []string
		if !s.inside(cwd) {
			paths = append(paths, cwd)
		}
		paths = append(paths, s.commandPaths(in.Command, cwd)...)
		for i := range paths {
			paths[i] = s.display(paths[i])
		}
		return ReachOutsideWrite, paths
	}
	if in.Path == "" {
		return ReachInside, nil
	}
	abs := canonical(s.resolve(in.Path))
	if s.inside(abs) {
		return ReachInside, nil
	}
	if tools.IsReadOnly(call.Name) {
		return ReachOutsideRead, []string{s.display(abs)}
	}
	return ReachOutsideWrite, []string{s.display(abs)}
}

// Match tools.resolvePath: only ~ expands in a workdir field. The child
// process starts in the physical directory, including through symlinks.
func (s *Scope) bashWorkdir(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = s.Home + p[1:]
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.Root, p)
	}
	return canonical(filepath.Clean(p))
}

// inside compares the symlink-resolved path against the resolved root, so
// a link inside the workspace cannot smuggle a write out of it.
func (s *Scope) inside(abs string) bool {
	abs = canonical(abs)
	s.mu.Lock()
	scratch := s.scratch
	s.mu.Unlock()
	return within(s.Root, abs) || (scratch != "" && within(scratch, abs))
}

// SetScratch replaces, rather than accumulates, the active conversation grant.
func (s *Scope) SetScratch(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scratch = canonical(dir)
}

// Granted reports whether an outside read of path was already approved.
func (s *Scope) Granted(path string) bool {
	return s.trusted(path, ReachOutsideRead)
}

func (s *Scope) trusted(path string, reach Reach) bool {
	abs := canonical(s.resolve(path))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, dir := range s.changes {
		if within(dir, abs) {
			return true
		}
	}
	if reach == ReachOutsideRead {
		for _, dir := range s.granted {
			if within(dir, abs) {
				return true
			}
		}
	}
	return false
}

// Grant remembers that path's directory subtree may be read. A directory
// path grants itself; a file path grants its parent.
func (s *Scope) Grant(path string) {
	s.trust(s.trustDirectory(path), ReachOutsideRead)
}

func (s *Scope) trustDirectory(path string) string {
	abs := canonical(s.resolve(path))
	dir := abs
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		dir = filepath.Dir(abs)
	}
	return dir
}

func (s *Scope) trust(dir string, reach Reach) {
	s.mu.Lock()
	if reach == ReachOutsideRead {
		s.granted = append(s.granted, dir)
	} else {
		s.changes = append(s.changes, dir)
	}
	s.mu.Unlock()
}

// resolve turns p into a clean absolute path: ~ and $HOME expand, relative
// paths hang off the workspace root.
func (s *Scope) resolve(p string) string {
	return s.resolveAt(p, s.Root)
}

func (s *Scope) resolveAt(p, cwd string) string {
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		p = s.Home + p[1:]
	case strings.HasPrefix(p, "$HOME/") || p == "$HOME":
		p = s.Home + p[len("$HOME"):]
	case strings.HasPrefix(p, "${HOME}/") || p == "${HOME}":
		p = s.Home + p[len("${HOME}"):]
	}
	// filepath.IsAbs deliberately rejects current-drive rooted paths on
	// Windows. Shell input still uses POSIX syntax, where /outside is rooted;
	// anchor it to the workdir's volume rather than treating it as a child.
	if runtime.GOOS == "windows" && filepath.VolumeName(p) == "" && (strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`)) {
		p = filepath.VolumeName(cwd) + filepath.FromSlash(p)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

func (s *Scope) resolveShellAt(p, cwd string) string {
	s.mu.Lock()
	scratch := s.scratch
	s.mu.Unlock()
	if scratch != "" {
		for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
			prefix := "$" + name
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				return filepath.Clean(scratch + strings.TrimPrefix(p, prefix))
			}
		}
	}
	return s.resolveAt(p, cwd)
}

// display shortens abs for a prompt: ~/… under home, untouched otherwise.
func (s *Scope) display(abs string) string {
	abs = filepath.ToSlash(abs)
	home := filepath.ToSlash(s.Home)
	if s.Home != "" && s.Home != "/" && within(s.Home, filepath.FromSlash(abs)) {
		if abs == home {
			return "~"
		}
		return "~" + abs[len(home):]
	}
	return abs
}

// within reports whether abs is dir or below it. Both must be clean and
// absolute; a sibling that merely shares a name prefix does not count.
func within(dir, abs string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// commandEscapes parses shell words without executing or expanding commands.
// Known search-pattern arguments are data, but quoted paths, redirects and
// nested commands still count. This remains a heuristic, not a shell sandbox.
func (s *Scope) commandEscapes(cmd string) string {
	paths := s.commandPaths(cmd, s.Root)
	if len(paths) > 0 {
		return paths[0]
	}
	return ""
}

func (s *Scope) commandPaths(cmd, startDir string) []string {
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return s.commandPathsFallback(cmd, startDir)
	}
	var paths []string
	add := func(path string) {
		if path != "" {
			paths = append(paths, path)
		}
	}
	var scan func(syntax.Node, string)
	scan = func(node syntax.Node, cwd string) {
		ignored := map[*syntax.Word]bool{}
		syntax.Walk(node, func(n syntax.Node) bool {
			switch n := n.(type) {
			case *syntax.BinaryCmd:
				if n.Op == syntax.AndStmt {
					scan(n.X, cwd)
					scan(n.Y, s.dirAfterSuccess(n.X, cwd))
					return false
				}
			case *syntax.CallExpr:
				searchPatterns(n.Args, ignored)
				if dir, ok := s.cdDir(n, cwd); ok {
					// A bare directory operand can itself be a symlink outside.
					add(s.outsideWord(dir, cwd))
				}
			case *syntax.Word:
				if !ignored[n] {
					word, _ := shellWord(n.Parts)
					add(s.outsideWord(word, cwd))
				}
			}
			// Even a pattern can contain $(cat /outside); inspect its children.
			return true
		})
	}
	scan(file, startDir)
	return paths
}

// Only && guarantees that a preceding cd succeeded. Do not carry a directory
// out of a subshell, pipeline, background command, negation or alternative.
// Unsupported/dynamic shell control flow keeps the original conservative base.
func (s *Scope) dirAfterSuccess(stmt *syntax.Stmt, cwd string) string {
	if stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown {
		return cwd
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		if dir, ok := s.cdDir(cmd, cwd); ok {
			return dir
		}
		if len(cmd.Args) > 0 {
			if name, _ := shellWord(cmd.Args[0].Parts); name == "cd" {
				return s.Root // unknown destination: do not trust an earlier cd
			}
		}
	case *syntax.BinaryCmd:
		if cmd.Op == syntax.AndStmt {
			return s.dirAfterSuccess(cmd.Y, s.dirAfterSuccess(cmd.X, cwd))
		}
	}
	return cwd
}

func (s *Scope) cdDir(call *syntax.CallExpr, cwd string) (string, bool) {
	if len(call.Args) < 2 {
		return "", false
	}
	name, ok := shellWord(call.Args[0].Parts)
	if !ok || name != "cd" {
		return "", false
	}
	i, physical := 1, false
	for i < len(call.Args)-1 {
		flag, static := shellWord(call.Args[i].Parts)
		if !static {
			return "", false
		}
		i++
		switch flag {
		case "--":
			if i != len(call.Args)-1 {
				return "", false
			}
		case "-L":
			physical = false
		case "-P":
			physical = true
		default:
			return "", false
		}
	}
	target, ok := shellWord(call.Args[i].Parts)
	if !ok || target == "" || strings.HasPrefix(target, "-") || strings.ContainsAny(target, "*?[{\\") {
		return "", false
	}
	dir := s.resolveShellAt(target, cwd)
	if physical {
		dir = canonical(dir)
	}
	return dir, true
}

func (s *Scope) outsideWord(word, cwd string) string {
	// POSIX shells (including Git Bash) implement this device independently
	// of the Windows current-drive path that resolveShellAt would produce.
	if word == "/dev/null" {
		return ""
	}
	// Preserve URLs (including query strings containing '=') as one word.
	// An option such as --output=/outside still names a filesystem path.
	if strings.HasPrefix(word, "-") {
		if _, value, ok := strings.Cut(word, "="); ok {
			word = value
		}
	}
	if !looksLikePath(word) {
		return ""
	}
	abs := canonical(s.resolveShellAt(word, cwd))
	if s.inside(abs) || (exempt(abs) && !within(s.Home, abs)) {
		return ""
	}
	return abs
}

// shellWord joins static parts, retaining HOME and scratch variables. An unknown
// expansion returns the known prefix and false: /outside/$name still names
// an outside directory. Walk also visits any nested commands.
func shellWord(parts []syntax.WordPart) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			v, ok := shellWord(p.Parts)
			b.WriteString(v)
			if !ok {
				return b.String(), false
			}
		case *syntax.ParamExp:
			if p.Param == nil || (p.Param.Value != "HOME" && p.Param.Value != "TMPDIR" && p.Param.Value != "TMP" && p.Param.Value != "TEMP") || p.Index != nil || p.Exp != nil || p.Slice != nil || p.Repl != nil || p.Excl || p.Length || p.Width || p.Names != 0 {
				return b.String(), false
			}
			b.WriteString("$" + p.Param.Value)
		default:
			return b.String(), false
		}
	}
	return b.String(), true
}

// searchPatterns recognizes grep/rg's positional pattern and -e/--regexp.
// File arguments (-f/--file included) are never exempted. Unknown options
// stop positional inference rather than guessing that a path is a pattern.
func searchPatterns(args []*syntax.Word, ignored map[*syntax.Word]bool) {
	if len(args) == 0 {
		return
	}
	name, ok := shellWord(args[0].Parts)
	if !ok {
		return
	}
	switch filepath.Base(name) {
	case "grep", "egrep", "fgrep", "rg":
	default:
		return
	}
	pattern, options, explicit := false, true, false
	var positional *syntax.Word
	for i := 1; i < len(args); i++ {
		arg, ok := shellWord(args[i].Parts)
		if !ok {
			if strings.HasPrefix(arg, "-") {
				explicit = true // unknown dynamic option: do not infer a pattern
			} else if !pattern {
				positional = args[i]
			}
			pattern = true // a dynamic positional pattern is still one argument
			continue
		}
		if options && arg == "--" {
			options = false
			continue
		}
		if options && strings.HasPrefix(arg, "--") {
			flag, _, attached := strings.Cut(arg, "=")
			switch flag {
			case "--regexp":
				pattern, explicit = true, true
				ignored[args[i]] = true
				if !attached && i+1 < len(args) {
					i++
					ignored[args[i]] = true
				}
			case "--file", "--max-count", "--after-context", "--before-context", "--context", "--glob", "--iglob", "--type", "--type-not", "--include", "--exclude", "--exclude-dir":
				if flag == "--file" {
					pattern, explicit = true, true
				}
				if !attached {
					i++ // consumed value is still scanned as a possible path
				}
			case "--recursive", "--line-number", "--ignore-case", "--fixed-strings", "--extended-regexp", "--hidden", "--no-ignore", "--files-with-matches", "--count", "--invert-match", "--word-regexp", "--quiet":
			default:
				pattern, explicit = true, true
			}
			continue
		}
		if options && strings.HasPrefix(arg, "-") && arg != "-" {
			for j := 1; j < len(arg); j++ {
				c := arg[j]
				if strings.ContainsRune("efmABCgtT", rune(c)) {
					if c == 'e' || c == 'f' {
						pattern, explicit = true, true
					}
					value := args[i]
					if j+1 == len(arg) && i+1 < len(args) {
						i++
						value = args[i]
					}
					if c == 'e' {
						ignored[value] = true
					}
					break
				}
				if !strings.ContainsRune("rRniIFEvwxlLchHosqUaS0123456789", rune(c)) {
					pattern, explicit = true, true
				}
			}
			continue
		}
		if !pattern {
			positional = args[i]
			pattern = true
		}
	}
	if positional != nil && !explicit {
		ignored[positional] = true
	}
}

// Keep the old conservative scan for malformed or unsupported shell syntax.
func (s *Scope) commandPathsFallback(cmd, cwd string) []string {
	var paths []string
	for _, word := range strings.FieldsFunc(cmd, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '|' || r == '&' ||
			r == '(' || r == ')' || r == '"' || r == '\'' || r == '<' || r == '>' || r == '='
	}) {
		p := word
		if i := strings.IndexByte(p, ':'); i > 0 && !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~") && filepath.VolumeName(p) == "" {
			// scp-style host:path or flag=value already split on '='.
			continue
		}
		if abs := s.outsideWord(p, cwd); abs != "" {
			paths = append(paths, abs)
		}
	}
	return paths
}

// looksLikePath accepts absolute, ~, known environment and parent-relative words.
func looksLikePath(w string) bool {
	if runtime.GOOS == "windows" && (filepath.VolumeName(w) != "" || strings.HasPrefix(w, `\`)) {
		return true
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if w == "$"+name || strings.HasPrefix(w, "$"+name+"/") {
			return true
		}
	}
	switch {
	case w == "/", w == "~", w == "$HOME", w == "${HOME}":
		return true
	case strings.HasPrefix(w, "/"), strings.HasPrefix(w, "~/"),
		strings.HasPrefix(w, "$HOME/"), strings.HasPrefix(w, "${HOME}/"),
		w == "..", strings.HasPrefix(w, "../"):
		return true
	}
	return false
}

func exempt(abs string) bool {
	for _, pre := range systemPrefixes {
		// Resolve aliases such as macOS /etc -> /private/etc before comparing.
		if within(canonical(pre), abs) {
			return true
		}
	}
	return false
}
