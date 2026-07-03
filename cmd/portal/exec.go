package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/localclient"
)

func newExecCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:           "exec [flags] -- <cmd...>",
		Short:         "Run a command through the portal daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			if dash < 0 || dash >= len(args) {
				return usageErr{msg: "usage: portal exec -- <cmd...>"}
			}
			argv := args[dash:]

			lc := localclient.New(a.Paths.APISock)
			code, err := lc.Exec(cmd.Context(), argv, os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				printExecError(cmd, err)
				return errSilent
			}
			if code != 0 {
				return exitCodeErr{code: code}
			}
			return nil
		},
	}
}

func printExecError(cmd *cobra.Command, err error) {
	var apiErr *localclient.APIError
	if errors.As(err, &apiErr) && apiErr.Code != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "portal exec: %s: %s\n", apiErr.Code, apiErr.Message)
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "portal exec: %v\n", err)
}
