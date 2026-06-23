package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/bootstrap"
)

// versionLine is the single-line version string shared by the `version`
// subcommand and the `--version`/`-v` flag, so all three agree. It pairs the
// release version (from -ldflags "-X main.version=…", e.g. a git tag) with the
// build commit SHA embedded alongside the agent — the latter pins exactly which
// commit produced the binary even on an untagged/dev build.
func versionLine() string {
	v := version
	if sha := bootstrap.EmbeddedSHA(); sha != "" {
		return fmt.Sprintf("%s %s (commit %s)", app.Tool, v, sha)
	}
	return fmt.Sprintf("%s %s", app.Tool, v)
}

// newVersionCmd prints the portal version. `--version` and `-v` are provided
// automatically by Cobra (root.Version is set); this adds the matching
// `version` subcommand so all three spellings work and print the same line.
func newVersionCmd(_ *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the portal version and build commit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(versionLine())
			return nil
		},
	}
}
