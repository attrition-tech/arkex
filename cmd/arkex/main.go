// Command arkex is a lightweight terminal coding agent.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/dantearo/arkex/internal/agent"
	"github.com/dantearo/arkex/internal/config"
	"github.com/dantearo/arkex/internal/modes"
	"github.com/dantearo/arkex/internal/prompt"
	"github.com/dantearo/arkex/internal/provider"
	"github.com/dantearo/arkex/internal/scratch"
	"github.com/dantearo/arkex/internal/session"
	"github.com/dantearo/arkex/internal/tools"
	"github.com/dantearo/arkex/internal/tui"
)

// Set by goreleaser via -ldflags.
var (
	version = "dev"
	commit  = ""
)

var errNoConnections = errors.New("no connections configured")

type rootFlags struct {
	model   string
	print   string
	json    bool
	yolo    bool
	noTools bool
	mode    string
	resume  string
}

func main() {
	provider.UserAgent = "arkex/" + version
	if stopProfile := startProfile(os.Getenv("ARKEX_CPUPROFILE")); stopProfile != nil {
		defer stopProfile()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := fang.Execute(ctx, newRoot(),
		fang.WithVersion(version),
		fang.WithCommit(commit),
		fang.WithNotifySignal(os.Interrupt, syscall.SIGTERM),
		fang.WithErrorHandler(printError),
	); err != nil {
		os.Exit(1)
	}
}

// printError is fang's default handler minus the title-case transform and
// trailing period, which mangle file paths and line:column references.
func printError(w io.Writer, styles fang.Styles, err error) {
	if f, ok := w.(*os.File); ok && !isTerminal(f) {
		_, _ = fmt.Fprintln(w, err.Error())
		return
	}
	_, _ = fmt.Fprintln(w, styles.ErrorHeader.String())
	_, _ = fmt.Fprintln(w, styles.ErrorText.UnsetTransform().Render(err.Error()))
	_, _ = fmt.Fprintln(w)
}

func newRoot() *cobra.Command {
	var f rootFlags
	root := &cobra.Command{
		Use:   "arkex [prompt]",
		Short: "A lightweight terminal coding agent that talks to your own models",
		Long: `arkex runs a coding agent in your terminal against any OpenAI-compatible
endpoint you configure. Without arguments it opens the interactive TUI.

Config lives in ~/.arkex/config.json (or $ARKEX_HOME/config.json) and may be
overridden per project by .arkex/config.json. Run "arkex config init" to
create a starter file.`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if c, _ := cmd.Flags().GetBool("continue"); c && f.resume == "" {
				f.resume = "latest"
			}
			return runRoot(cmd.Context(), f, args)
		},
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&f.model, "model", "m", "", `model or profile: "name", "connection/model" or "connection/model:thinking"`)
	root.Flags().StringVarP(&f.print, "print", "p", "", "run one prompt non-interactively and print the reply (use - to read stdin)")
	root.Flags().BoolVar(&f.json, "json", false, "with --print, emit newline-delimited JSON events instead of text")
	root.Flags().StringVar(&f.mode, "mode", "build", "starting mode: plan (read-only), build (ask per config) or auto (skip asks within trusted scope; obey denies)")
	root.Flags().BoolVar(&f.yolo, "yolo", false, "alias for --mode auto")
	root.Flags().BoolVar(&f.noTools, "no-tools", false, "disable all tools (pure chat)")
	root.Flags().StringVarP(&f.resume, "resume", "r", "", "resume a saved session from this directory by id (see /resume in the TUI)")
	root.Flags().BoolP("continue", "c", false, "resume the most recent session from this directory")

	root.AddCommand(newModelsCmd(&f), newConfigCmd(), newUpdateCmd())
	return root
}

func runRoot(ctx context.Context, f rootFlags, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	configPath, err := config.GlobalPath()
	if err != nil {
		return err
	}
	asker := tui.NewAsker()

	startMode, err := agent.ParseMode(f.mode)
	if err != nil {
		return err
	}
	if f.yolo {
		startMode = agent.ModeAuto
	}
	// One mode policy is shared by every agent this process builds, so a
	// mode switch in the TUI survives /model.
	modePolicy := agent.NewModePolicy(startMode, nil)
	grants := &agent.Grants{} // "allow this session" answers, kept across model switches
	home, _ := os.UserHomeDir()
	scope := agent.NewScope(cwd, home)
	var temps scratch.Store
	cleanup := true
	defer func() {
		if cleanup {
			if err := temps.Close(); err != nil {
				fmt.Fprintln(os.Stderr, "scratch cleanup:", err)
			}
		}
	}()
	prepare := func(ag *agent.Agent, id string) error {
		dir, err := temps.Dir(id)
		if err != nil {
			return fmt.Errorf("creating scratch directory: %w", err)
		}
		scope.SetScratch(dir)
		ag.System += prompt.ScratchNote(dir)
		if tool, ok := ag.Tools.Get("bash"); ok {
			tool.(*tools.Bash).TempDir = dir
		}
		return nil
	}

	// connect resolves a selector against a fresh config read and builds a
	// ready agent. The TUI calls it again when the user switches models.
	connect := func(ctx context.Context, selector string, interactive bool) (*agent.Agent, config.ModelRef, error) {
		cfg, err := config.Load(cwd)
		if err != nil {
			return nil, config.ModelRef{}, err
		}
		if len(cfg.Connections) == 0 {
			return nil, config.ModelRef{}, errNoConnections
		}
		ref, err := cfg.Resolve(selector)
		if err != nil {
			return nil, config.ModelRef{}, err
		}
		model, err := provider.Open(ctx, ref, nil)
		if err != nil {
			return nil, config.ModelRef{}, err
		}
		ag := &agent.Agent{
			Model:  model,
			System: prompt.Build(prompt.Options{Cwd: cwd, Now: time.Now(), Model: ref.String()}),
		}
		if !f.noTools {
			ag.Tools = tools.Default(cwd)
		} else {
			ag.Tools = tools.NewRegistry()
		}
		if interactive {
			modePolicy.SetBase(agent.ConfigPolicy{Config: cfg, Asker: asker, Grants: grants})
			modePolicy.SetScope(scope, asker)
		} else {
			// "ask" tools and anything leaving the workspace are denied in print mode.
			modePolicy.SetBase(agent.ConfigPolicy{Config: cfg})
			modePolicy.SetScope(scope, nil)
		}
		ag.Policy = modePolicy
		return ag, ref, nil
	}

	// Non-interactive: --print, or a positional prompt, or piped stdin.
	promptText := f.print
	if promptText == "" && len(args) > 0 {
		promptText = strings.Join(args, " ")
	}
	if promptText == "-" || (promptText == "" && !isTerminal(os.Stdin)) {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		promptText = strings.TrimSpace(string(b))
		if promptText == "" {
			return errors.New("empty prompt on stdin")
		}
	}
	if promptText != "" {
		var conv *session.Session
		selector := f.model
		if f.resume != "" {
			conv, err = loadSession(cwd, f.resume)
			if err != nil {
				return err
			}
			if selector == "" {
				selector = conv.Model
				if selector == "" {
					return errors.New("session has no saved model; select one with --model")
				}
			}
		}
		ag, ref, err := connect(ctx, selector, false)
		if err != nil {
			if errors.Is(err, errNoConnections) {
				return fmt.Errorf("%w; run `arkex` and press a in the Connections panel, or `arkex config init`", err)
			}
			return err
		}
		if modePolicy.Mode() == agent.ModePlan {
			ag.System += prompt.PlanNote(ag.Tools)
		}
		// --continue/--resume in print mode: load the saved conversation,
		// answer, and save it back. Plain one-shot runs are not persisted.
		if conv != nil {
			ag.SetMessages(conv.Messages)
			previousModel := conv.Model
			if conv.Effort != nil && *conv.Effort != "" {
				previousModel = strings.TrimSuffix(previousModel, ":"+*conv.Effort)
			}
			if previousModel == ref.String() {
				ag.RestoreLastInput(conv.LastInput)
			}
			if conv.Effort != nil && previousModel == ref.String() && f.model == "" {
				if err := ag.Model.SetThinking(*conv.Effort); err != nil {
					return fmt.Errorf("restoring reasoning setting: %w", err)
				}
				ref = ag.Model.Ref
			}
		}
		id := ""
		if conv != nil {
			id = conv.ID
		}
		if err := prepare(ag, id); err != nil {
			return err
		}
		if f.json {
			err = modes.JSON(ctx, ag, promptText, os.Stdout)
		} else {
			err = modes.Print(ctx, ag, promptText, os.Stdout, os.Stderr)
		}
		if conv != nil {
			usage := ag.LastUsage()
			conv.Update(ag.Messages(), displayName(ref), string(modePolicy.Mode()), conv.UsageIn+usage.InputTokens, conv.UsageOut+usage.OutputTokens)
			conv.LastInput = ag.LastInput()
			level := ag.Model.Ref.Thinking
			conv.Effort = &level
			if serr := conv.Save(); serr != nil && err == nil {
				err = fmt.Errorf("saving session: %w", serr)
			}
		}
		return err
	}

	if !isTerminal(os.Stdout) {
		return errors.New("stdout is not a terminal; pass a prompt or --print for non-interactive use")
	}
	opts := tui.Options{
		Prepare:     prepare,
		Cwd:         cwd,
		Version:     version,
		Resume:      f.resume,
		ResumeModel: f.model,
		UI:          uiPrefs(cwd),
		Asker:       asker,
		Mode:        modePolicy,
		ConfigPath:  configPath,
		UserAgent:   provider.UserAgent,
		Connect: func(ctx context.Context, selector string) (tui.Connection, error) {
			ag, ref, err := connect(ctx, selector, true)
			if err != nil {
				return tui.Connection{}, err
			}
			return tui.Connection{Agent: ag, Name: displayName(ref)}, nil
		},
	}
	// Interactive mode always opens; a model that cannot be connected is
	// reported inside the Connections panel instead of aborting.
	if f.resume != "" {
		// Resume connects its saved model inside the TUI, even when the global
		// default is unavailable. An explicit --model is a deliberate override.
		err := tui.Run(ctx, opts)
		cleanup = !errors.Is(err, tui.ErrRunStillActive)
		return err
	}
	if ag, ref, err := connect(ctx, f.model, true); err == nil {
		opts.Session = tui.Connection{Agent: ag, Name: displayName(ref)}
	} else if f.model != "" {
		return err // an explicit --model that does not work is a hard error
	} else if !errors.Is(err, errNoConnections) {
		opts.StartupNote = err.Error()
	}
	err = tui.Run(ctx, opts)
	cleanup = !errors.Is(err, tui.ErrRunStillActive)
	return err
}

// uiPrefs reads the ui section; a broken config yields defaults here and
// is reported by connect.
func uiPrefs(cwd string) config.UI {
	cfg, err := config.Load(cwd)
	if err != nil {
		return config.UI{}
	}
	return cfg.UI
}

func loadSession(cwd, id string) (*session.Session, error) {
	if id != "latest" {
		s, err := session.Load(cwd, id)
		if err == nil && s.State != "" {
			return nil, errors.New("restore this session from Archived or Trash in /sessions before resuming")
		}
		return s, err
	}
	s, err := session.Latest(cwd)
	if err == nil && s == nil {
		err = fmt.Errorf("no saved sessions in %s", cwd)
	}
	return s, err
}

func displayName(ref config.ModelRef) string {
	s := ref.String()
	if ref.Thinking != "" {
		s += ":" + ref.Thinking
	}
	return s
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// ---- models ----

func newModelsCmd(f *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "models", Short: "List and test configured models"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List configured connections, models and profiles",
			RunE: func(cmd *cobra.Command, _ []string) error {
				cwd, _ := os.Getwd()
				cfg, err := config.Load(cwd)
				if err != nil {
					return err
				}
				w := cmd.OutOrStdout()
				for _, pid := range cfg.ConnectionIDs() {
					p := cfg.Connections[pid]
					off := ""
					if p.Disabled {
						off = "  DISABLED"
					}
					_, _ = fmt.Fprintf(w, "%s  (%s, %s, %s)%s\n", pid, p.Kind, p.API, p.BaseURL, off)
					for _, m := range p.Models {
						extra := ""
						if m.Reasoning {
							extra = "  reasoning"
						}
						if c := p.Compat.Merge(m.Compat); c.Thinking != "" {
							extra += "  thinking=" + string(c.Thinking)
						}
						if m.Disabled {
							extra += "  DISABLED"
						}
						mark := " "
						if pid+"/"+m.ID == cfg.Default {
							mark = "*"
						}
						_, _ = fmt.Fprintf(w, "%s %s/%s%s\n", mark, pid, m.ID, extra)
					}
				}
				if len(cfg.Connections) == 0 {
					_, _ = fmt.Fprintln(w, "no connections configured; run `arkex` and press a in the Connections panel")
				}
				if len(cfg.Profiles) > 0 {
					_, _ = fmt.Fprintln(w, "\nprofiles:")
					for name, p := range cfg.Profiles {
						mark := " "
						if name == cfg.Default {
							mark = "*"
						}
						t := ""
						if p.Thinking != "" {
							t = ":" + p.Thinking
						}
						_, _ = fmt.Fprintf(w, "%s %s → %s%s\n", mark, name, p.Model, t)
					}
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "test [selector]",
			Short: "Send a tiny request to a model and report what came back",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cwd, _ := os.Getwd()
				cfg, err := config.Load(cwd)
				if err != nil {
					return err
				}
				sel := f.model
				if len(args) == 1 {
					sel = args[0]
				}
				ref, err := cfg.Resolve(sel)
				if err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
				defer cancel()
				model, err := provider.Open(ctx, ref, nil)
				if err != nil {
					return err
				}
				ag := &agent.Agent{Model: model, Tools: tools.NewRegistry(), Policy: agent.AllowAll{}, MaxSteps: 1}
				w := cmd.OutOrStdout()
				_, _ = fmt.Fprintf(w, "testing %s …\n", displayName(ref))
				var text, reasoning int
				start := time.Now()
				err = ag.Run(ctx, "Reply with the single word: pong", func(e agent.Event) {
					switch e := e.(type) {
					case agent.TextDelta:
						text += len(e.Text)
						_, _ = fmt.Fprint(w, e.Text)
					case agent.ReasoningDelta:
						reasoning += len(e.Text)
					case agent.RunEnd:
						_, _ = fmt.Fprintf(w, "\n\nok in %s · %d text chars · %d reasoning chars · usage %d in / %d out / %d reasoning\n",
							time.Since(start).Round(time.Millisecond), text, reasoning,
							e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.ReasoningTokens)
					}
				})
				return err
			},
		},
	)
	return cmd
}

// ---- config ----

const starterConfig = `{
  "version": 2,
  "connections": {
    "local": {
      "kind": "llm-server",
      "api": "openai-compat",
      "baseUrl": "http://localhost:11434/v1",
      "apiKey": "$LOCAL_API_KEY",
      "compat": { "thinking": "qwen" },
      "models": [
        { "id": "qwen3:8b", "reasoning": true, "contextWindow": 32768, "maxTokens": 8192 }
      ]
    }
  },
  "profiles": {
    "default": { "model": "local/qwen3:8b", "thinking": "medium" }
  },
  "default": "default",
  "permissions": {
    "read": "allow",
    "bash": "ask",
    "write": "ask",
    "edit": "ask"
  }
}
`

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Manage arkex configuration"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "path",
			Short: "Print the global config file path",
			RunE: func(cmd *cobra.Command, _ []string) error {
				p, err := config.GlobalPath()
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), p)
				return nil
			},
		},
		&cobra.Command{
			Use:   "init",
			Short: "Write a starter config file if none exists",
			RunE: func(cmd *cobra.Command, _ []string) error {
				p, err := config.GlobalPath()
				if err != nil {
					return err
				}
				if _, err := os.Stat(p); err == nil {
					return fmt.Errorf("%s already exists", p)
				}
				if err := os.MkdirAll(dirOf(p), 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(p, []byte(starterConfig), 0o600); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\nEdit baseUrl, apiKey and models, then run: arkex models test\n", p)
				return nil
			},
		},
		&cobra.Command{
			Use:   "check",
			Short: "Validate the global and project config files",
			Long: `Checks ~/.arkex/config.json and ./.arkex/config.json for syntax errors,
misspelled keys, unknown values, unset $ENV references and a default model
that does not resolve. Exits 1 when it finds an error.`,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				r := config.Check(cwd)
				out := cmd.OutOrStdout()
				if len(r.Files) == 0 {
					_, _ = fmt.Fprintln(out, "no config files found; run: arkex config init (or arkex, then /connections)")
					return nil
				}
				for _, f := range r.Files {
					_, _ = fmt.Fprintln(out, "checked", f)
				}
				for _, p := range r.Problems {
					_, _ = fmt.Fprintln(out, p)
				}
				if n := r.Errors(); n > 0 {
					return fmt.Errorf("%d error(s) in the config", n)
				}
				if len(r.Problems) == 0 {
					_, _ = fmt.Fprintln(out, "ok")
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "migrate",
			Short: "Upgrade the global config file to the current format",
			Long: `Rewrites ~/.arkex/config.json in the format this arkex expects (version ` + fmt.Sprint(config.CurrentVersion) + `).
arkex also does this on its next edit; the previous file is kept as config.json.bak.`,
			RunE: func(cmd *cobra.Command, _ []string) error {
				p, err := config.GlobalPath()
				if err != nil {
					return err
				}
				if _, err := os.Stat(p); err != nil {
					return fmt.Errorf("%s does not exist", p)
				}
				changed, err := config.MigrateFile(p)
				if err != nil {
					return err
				}
				if changed {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "upgraded %s to version %d (previous copy: %s)\n", p, config.CurrentVersion, config.BackupPath(p))
				} else {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s is already version %d\n", p, config.CurrentVersion)
				}
				return nil
			},
		},
	)
	return cmd
}

func dirOf(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[:i]
	}
	return "."
}
