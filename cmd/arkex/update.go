package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/attrition-tech/arkex/internal/update"
)

func newUpdateCmd() *cobra.Command {
	var (
		check   bool
		channel string
		url     string
	)
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update arkex to the latest release",
		Long: `update downloads the latest release for this platform, verifies its
ed25519 signature and SHA-256 checksum against keys built into this binary,
runs the new binary once, and then atomically replaces the current executable.

The release host is baked into official builds; override it with --url or
$` + update.EnvBaseURL + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			view := newUpdateView(cmd.ErrOrStderr())
			view.start()
			defer view.finish()
			c := &update.Client{
				BaseURL:        url,
				Channel:        channel,
				CurrentVersion: version,
				UserAgent:      "arkex/" + version,
				Log:            view.logf,
				Progress:       view.progress,
			}
			m, newer, err := c.Check(cmd.Context())
			if err != nil {
				return err
			}
			if !newer {
				_, _ = fmt.Fprintf(out, "arkex %s is up to date (%s channel: %s)\n", version, channelName(channel), m.Version)
				return nil
			}
			if check {
				_, _ = fmt.Fprintf(out, "arkex %s available (you have %s). Run: arkex update\n", m.Version, version)
				if m.Notes != "" {
					_, _ = fmt.Fprintln(out, m.Notes)
				}
				return nil
			}
			if view.terminal {
				view.logf("%s → %s", version, m.Version)
			}
			res, err := c.Apply(cmd.Context(), m)
			view.finish()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "updated arkex %s → %s (%s)\n", res.From, res.To, res.Executable)
			if m.Notes != "" {
				_, _ = fmt.Fprintln(out, m.Notes)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only report whether an update is available")
	cmd.Flags().StringVar(&channel, "channel", update.DefaultChannel, "release channel manifest to follow")
	cmd.Flags().StringVar(&url, "url", "", "release host base URL (default: built-in, or $"+update.EnvBaseURL+")")
	return cmd
}

func channelName(c string) string {
	if c == "" {
		return update.DefaultChannel
	}
	return c
}
