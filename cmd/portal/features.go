package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/localclient"
)

// featureNames is the fixed set of capability gates, in the order they render.
// Keeping it a package var (not a map) makes list output deterministic.
var featureNames = []string{config.FeatureClipImage, config.FeatureClipText, config.FeatureNotify, config.FeatureExec}

func newFeaturesCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "features [name on|off]",
		Short: "Show or toggle the clip-image/clip-text/notify/exec capability gates",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFeatures(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), a, args)
		},
	}
}

// runFeatures reads/writes the capability gates via GET/PUT /v1/features, giving
// the gates their first-class consumer. It falls back to config.Store (same
// files, same semantics) when the daemon is down, so the CLI works whether or not
// `portal run` is up. Unknown names fail the same way up or down — validated
// locally — and a set echoes only the changed line.
func runFeatures(ctx context.Context, w, errw io.Writer, a *app.App, args []string) error {
	lc := localclient.New(a.Paths.APISock)

	switch len(args) {
	case 0: // LIST
		if m, err := lc.Features(ctx); err == nil {
			printFeatures(w, m)
			return nil
		}
		m := make(map[string]bool, len(featureNames))
		for _, n := range featureNames {
			m[n] = a.Cfg.FeatureEnabled(n)
		}
		printFeatures(w, m)
		return nil

	case 2: // SET
		name := args[0]
		on, ok := parseOnOff(args[1])
		if !ok {
			fmt.Fprintf(errw, "usage: %s features [name on|off]\n", app.Tool)
			return usageErr{}
		}
		if !knownFeature(name) {
			fmt.Fprintf(errw, "unknown feature: %s (known: clip-image, clip-text, notify, exec)\n", name)
			return usageErr{}
		}
		if _, err := lc.SetFeature(ctx, name, on); err == nil {
			fmt.Fprintf(w, "%s: %s\n", name, onOff(on))
			return nil
		} else if errors.Is(err, localclient.ErrFeatureUnknown) {
			// The daemon considers the name unknown — fail the same way as the
			// local check rather than writing through the fallback.
			fmt.Fprintf(errw, "unknown feature: %s (known: clip-image, clip-text, notify, exec)\n", name)
			return usageErr{}
		}
		// Daemon down: write config.Store directly (same file the daemon uses).
		if err := a.Cfg.SetFeature(name, on); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s: %s\n", name, onOff(on))
		return nil

	default: // len(args) == 1
		fmt.Fprintf(errw, "usage: %s features [name on|off]\n", app.Tool)
		return usageErr{}
	}
}

// printFeatures renders the gates in the fixed featureNames order so scripts see
// deterministic output regardless of map iteration order.
func printFeatures(w io.Writer, m map[string]bool) {
	for _, name := range featureNames {
		fmt.Fprintf(w, "%s: %s\n", name, onOff(m[name]))
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// parseOnOff maps an on/off token to (value, ok). ok is false for an
// unrecognized token so the caller can surface a usage error.
func parseOnOff(s string) (bool, bool) {
	switch s {
	case "on", "true", "1", "yes":
		return true, true
	case "off", "false", "0", "no":
		return false, true
	default:
		return false, false
	}
}

func knownFeature(name string) bool {
	for _, n := range featureNames {
		if n == name {
			return true
		}
	}
	return false
}
