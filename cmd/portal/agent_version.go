package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/vikashl/portal/internal/app"
	"github.com/vikashl/portal/internal/bootstrap"
)

// newAgentVersionCmd is hidden by default; it prints the embedded agent's
// git SHA. Useful for diagnostics ("which version did `portal install`
// upload?") and CI parity checks.
func newAgentVersionCmd(_ *app.App) *cobra.Command {
	return &cobra.Command{
		Use:    "agent-version",
		Short:  "Print the embedded agent's git SHA",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(bootstrap.EmbeddedSHA())
			return nil
		},
	}
}
