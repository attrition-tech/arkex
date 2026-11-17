package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/fileutil"
)

func newConfigResetCmd() *cobra.Command {
	var all, yes, dryRun bool
	cmd := &cobra.Command{
		Use:     "reset",
		Aliases: []string{"flush"},
		Short:   "Back up and reset global configuration; --all also resets sign-ins and sessions",
		Long: `Reset global settings and connections, including API keys in config.json.
With --all, also reset local ChatGPT sign-ins and saved sessions for every workspace.
Close other Arkex processes first so they cannot recreate state during the reset.

Selected files are moved into a private .reset-backup-* directory in ARKEX_HOME
(default ~/.arkex). No backup is erased, and no remote OAuth access is revoked.
The executable, project .arkex files, workspaces and unrelated files are untouched.
Without --all, auth.json and saved sessions are kept. Without --yes, confirmation
is required. --dry-run prints the exact paths without changing anything.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			plan, err := planConfigReset(all)
			if err != nil {
				return err
			}
			plan.describe(cmd)
			if dryRun || len(plan.names) == 0 {
				return nil
			}
			if err := confirmConfigReset(cmd, yes); err != nil {
				return err
			}
			return plan.apply(cmd)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "also reset local sign-ins and all saved sessions")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm the reset without prompting")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be reset; change nothing")
	return cmd
}

type configResetPlan struct {
	home  string
	names []string
	all   bool
}

func planConfigReset(all bool) (configResetPlan, error) {
	dir, err := config.Dir()
	if err != nil {
		return configResetPlan{}, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return configResetPlan{}, err
	}
	plan := configResetPlan{home: dir, all: all}
	// Never reset the entire directory: ARKEX_HOME can contain the running
	// executable, unrelated files, symlinks or previous recovery backups.
	names := []string{"config.json", "config.json.bak"}
	if all {
		names = append(names, "auth.json", "sessions")
	}
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			plan.names = append(plan.names, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return configResetPlan{}, err
		}
	}
	return plan, nil
}

func (p configResetPlan) describe(cmd *cobra.Command) {
	out := cmd.OutOrStdout()
	if len(p.names) == 0 {
		fmt.Fprintln(out, "Nothing to reset in", p.home)
		return
	}
	fmt.Fprintln(out, "Close other Arkex processes first. Back up and reset:")
	for _, name := range p.names {
		fmt.Fprintln(out, " ", filepath.Join(p.home, name))
	}
	fmt.Fprintln(out, "Keep: executable, project settings, workspace files and existing backups.")
	if !p.all {
		fmt.Fprintln(out, "Keep: local ChatGPT sign-ins and saved sessions.")
	}
	fmt.Fprintln(out, "A private backup will retain the selected data, including any credentials; remote access is not revoked.")
}

func confirmConfigReset(cmd *cobra.Command, yes bool) error {
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	if yes {
		return nil
	}
	fmt.Fprint(cmd.OutOrStdout(), "Type reset to confirm (or use --yes in scripts): ")
	scanner := bufio.NewScanner(cmd.InOrStdin())
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "reset" {
		return errors.New("reset cancelled; nothing changed")
	}
	return cmd.Context().Err()
}

func (p configResetPlan) apply(cmd *cobra.Command) error {
	backup, err := p.backup(cmd.Context())
	if err != nil {
		return err
	}
	if backup != "" {
		fmt.Fprintln(cmd.OutOrStdout(), "Reset complete. Backup:", backup)
		fmt.Fprintln(cmd.OutOrStdout(), "To restore, close Arkex and move the selected entries from that backup to:", p.home)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Run arkex to set up your connections again. Project .arkex/config.json still applies.")
	return nil
}

func (p configResetPlan) backup(ctx context.Context) (string, error) {
	if len(p.names) == 0 {
		return "", ctx.Err()
	}
	locks := []string{"config.json"}
	if p.all {
		locks = append(locks, "auth.json")
	}
	for _, name := range locks {
		unlock, err := fileutil.LockContext(ctx, filepath.Join(p.home, name))
		if err != nil {
			return "", err
		}
		defer unlock()
	}
	backup, err := os.MkdirTemp(p.home, ".reset-backup-") // private, same filesystem
	if err != nil {
		return "", err
	}
	var moved []string
	for _, name := range p.names {
		err = ctx.Err()
		if err == nil {
			// Rename the entry itself; never follow a symlink into another tree.
			err = os.Rename(filepath.Join(p.home, name), filepath.Join(backup, name))
		}
		if err != nil {
			for i := len(moved) - 1; i >= 0; i-- {
				n := moved[i]
				if _, statErr := os.Lstat(filepath.Join(p.home, n)); !errors.Is(statErr, os.ErrNotExist) {
					err = errors.Join(err, fmt.Errorf("cannot restore %s without overwriting; recover it from %s", n, backup))
					continue
				}
				err = errors.Join(err, os.Rename(filepath.Join(backup, n), filepath.Join(p.home, n)))
			}
			_ = os.Remove(backup) // only removes an empty backup
			return "", fmt.Errorf("reset failed (backup location %s): %w", backup, err)
		}
		moved = append(moved, name)
	}
	return backup, nil
}
