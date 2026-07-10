package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"

	"github.com/spf13/cobra"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/keychain"
)

const keychainHelp = `Manage credentials remembered by portal in the macOS Keychain on THIS Mac.

Use "portal keychain list" to show remembered labels and
"portal keychain forget <label>" to remove one.

The request commands "portal keychain run ..." and "portal keychain askpass ..."
are dev-box-only commands used by agents through portald; they are not Mac
subcommands.`

type rememberedCredentialStore interface {
	List() ([]string, error)
	Delete(context.Context, string) error
}

type keychainCommandDeps struct {
	openStore func() (rememberedCredentialStore, error)
	audit     *audit.Log
	host      string
}

// newKeychainCmd builds the Mac-side remembered-credential management command.
// The store is opened lazily so constructing the root command never touches the
// Keychain or prevents unrelated commands from running.
func newKeychainCmd(a *app.App) *cobra.Command {
	host := ""
	if a.Transport != nil {
		host = a.Transport.Describe().Host
	}
	return newKeychainCmdWithDeps(keychainCommandDeps{
		openStore: productionRememberedCredentialStore,
		audit:     a.Audit,
		host:      host,
	})
}

func productionRememberedCredentialStore() (rememberedCredentialStore, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("macOS Keychain is unavailable on this platform")
	}
	return keychain.New()
}

func newKeychainCmdWithDeps(deps keychainCommandDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "keychain",
		Short:         "Manage credentials remembered in this Mac's Keychain",
		Long:          keychainHelp,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageErr{msg: "usage: portal keychain <list|forget>"}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List credential labels remembered on this Mac",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageErr{msg: "usage: portal keychain list"}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := deps.openStore()
			if err != nil {
				return fmt.Errorf("portal keychain: %w", err)
			}
			labels, err := store.List()
			if err != nil {
				return fmt.Errorf("portal keychain: list remembered credentials: %w", err)
			}
			labels = append([]string(nil), labels...)
			sort.Strings(labels)
			for _, label := range labels {
				fmt.Fprintln(cmd.OutOrStdout(), label)
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "forget <label>",
		Short: "Forget one credential remembered on this Mac",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageErr{msg: "usage: portal keychain forget <label>"}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := deps.openStore()
			if err != nil {
				return fmt.Errorf("portal keychain: %w", err)
			}
			label := args[0]
			if err := store.Delete(cmd.Context(), label); err != nil {
				return fmt.Errorf("portal keychain: forget %q: %w", label, err)
			}
			deps.audit.CredForgotten(deps.host, label)
			fmt.Fprintf(cmd.OutOrStdout(), "forgotten credential %q\n", label)
			return nil
		},
	})

	return cmd
}
