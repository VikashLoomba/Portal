package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/upgrade"
)

// upgradeVerifyTimeout bounds the self-test exec of the downloaded binary. It
// only runs `version`, so a slow answer means something is wrong with the file.
const upgradeVerifyTimeout = 30 * time.Second

// upgradeDeps is the injectable boundary for one `upgrade` run. Production
// wires the real release client, the installed binary path, and the launchd
// service; tests substitute an httptest-backed client and a fake restarter.
type upgradeDeps struct {
	// Releases resolves and downloads the newest published release.
	Releases *upgrade.Client
	// BinPath is the installed binary the swap replaces.
	BinPath string
	// Current is the RUNNING process's version, used only when the installed
	// binary cannot be interrogated (missing, or it fails to run).
	Current string
	// GOOS/GOARCH identify the host; a non-arm64-darwin host is refused.
	GOOS   string
	GOARCH string
	// Restart reloads the daemon after a successful swap. A nil Restart skips
	// the reload (the CLI-only case: nothing is loaded to restart).
	Restart func(context.Context) error
	// Verify runs the downloaded binary's own `version` and returns its output.
	// Injectable so tests can exercise the mismatch and failure branches
	// without a real Mach-O.
	Verify func(context.Context, string) (string, error)
}

func newUpgradeCmd(a *app.App) *cobra.Command {
	var checkOnly bool
	var force bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update portal to the latest published release and reload the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := upgradeDeps{
				Releases: &upgrade.Client{},
				BinPath:  a.Paths.BinPath,
				Current:  version,
				GOOS:     runtime.GOOS,
				GOARCH:   runtime.GOARCH,
				Restart:  a.Service.Restart,
				Verify:   verifyUpgradeBinary,
			}
			return runUpgrade(cmd.Context(), cmd.OutOrStdout(), deps, checkOnly, force)
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "Report whether a newer release exists; download nothing")
	cmd.Flags().BoolVar(&force, "force", false, "Re-install the latest release even when already current")
	return cmd
}

// runUpgrade resolves the newest release, and unless --check or an
// already-current version stops it, downloads that asset beside the installed
// binary, proves the download runs and reports the expected tag, atomically
// renames it into place, and reloads the daemon.
//
// The order is load-bearing: nothing replaces the working binary until the
// replacement has executed successfully, and the swap is a rename (atomic, and
// safe while the daemon is executing the old inode) rather than a truncating
// copy that could leave a half-written binary if it failed midway.
func runUpgrade(ctx context.Context, out io.Writer, deps upgradeDeps, checkOnly, force bool) error {
	if !upgrade.Supported(deps.GOOS, deps.GOARCH) {
		return fmt.Errorf("no published binary for %s/%s — releases ship %s only; build from source instead",
			deps.GOOS, deps.GOARCH, upgrade.AssetName)
	}

	rel, err := deps.Releases.Latest(ctx)
	if err != nil {
		return err
	}

	// Compare against the INSTALLED binary, not the process running this
	// command. They are the same file in the common case, but not when a
	// freshly built ./portal from a checkout is invoked against a stale
	// install — and it is the installed binary the daemon executes, so
	// comparing the invoker would report "up to date" while leaving the
	// daemon running old code.
	current := installedVersion(ctx, deps)
	cmpResult := upgrade.Compare(current, rel.Tag)
	switch {
	case checkOnly && cmpResult < 0:
		fmt.Fprintf(out, "update available: %s -> %s\nrun '%s upgrade' to install it\n",
			current, rel.Tag, app.Tool)
		return nil
	case checkOnly:
		fmt.Fprintf(out, "up to date: %s (latest release %s)\n", current, rel.Tag)
		return nil
	case cmpResult >= 0 && !force:
		fmt.Fprintf(out, "up to date: %s (latest release %s)\nre-install it anyway with --force\n",
			current, rel.Tag)
		return nil
	}

	if deps.BinPath == "" {
		return fmt.Errorf("no installed binary path known; run '%s install' first", app.Tool)
	}
	if _, err := os.Stat(deps.BinPath); err != nil {
		return fmt.Errorf("no installed binary at %s — run '%s install <ssh-host>' first",
			deps.BinPath, app.Tool)
	}

	// Stage beside the target so the rename stays within one filesystem (a
	// cross-device rename would fail, and a copy would not be atomic).
	tmp := filepath.Join(filepath.Dir(deps.BinPath), "."+filepath.Base(deps.BinPath)+".upgrade")
	defer os.Remove(tmp)

	fmt.Fprintf(out, "downloading %s %s...\n", upgrade.AssetName, rel.Tag)
	if err := deps.Releases.Download(ctx, rel, tmp); err != nil {
		return err
	}

	// Self-test before the swap: a truncated, wrong-architecture, or
	// wrong-version download must never replace a working binary.
	if deps.Verify != nil {
		line, err := deps.Verify(ctx, tmp)
		if err != nil {
			return fmt.Errorf("downloaded binary failed its self-test (left %s in place): %w",
				deps.BinPath, err)
		}
		if !strings.Contains(line, rel.Tag) {
			return fmt.Errorf("downloaded binary reports %q, expected %s (left %s in place)",
				strings.TrimSpace(line), rel.Tag, deps.BinPath)
		}
	}

	if err := os.Rename(tmp, deps.BinPath); err != nil {
		return fmt.Errorf("install %s: %w", deps.BinPath, err)
	}
	fmt.Fprintf(out, "installed %s -> %s\n", rel.Tag, deps.BinPath)

	if deps.Restart != nil {
		if err := deps.Restart(ctx); err != nil {
			// The binary IS updated; only the reload failed. Say so precisely
			// rather than implying the upgrade did not happen.
			return fmt.Errorf("upgraded to %s, but reloading the daemon failed (run '%s restart'): %w",
				rel.Tag, app.Tool, err)
		}
		fmt.Fprintf(out, "daemon reloaded — now running %s\n", rel.Tag)
	}
	return nil
}

// verifyUpgradeBinary executes the downloaded file's own `version` subcommand
// and returns its output. Running the candidate is a stronger check than any
// header inspection: a wrong-architecture, truncated, or non-executable file
// fails here, before it can replace the installed binary.
func verifyUpgradeBinary(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, upgradeVerifyTimeout)
	defer cancel()
	outBytes, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(outBytes)))
	}
	return string(outBytes), nil
}

// installedVersion reports the version of the binary at BinPath — the one the
// daemon executes and the one the swap replaces. It falls back to the running
// process's own version when that binary is absent or refuses to run, so a
// broken install still resolves to something orderable rather than aborting
// before the download that would repair it.
func installedVersion(ctx context.Context, deps upgradeDeps) string {
	if deps.BinPath == "" || deps.Verify == nil {
		return deps.Current
	}
	if _, err := os.Stat(deps.BinPath); err != nil {
		return deps.Current
	}
	line, err := deps.Verify(ctx, deps.BinPath)
	if err != nil {
		return deps.Current
	}
	if v := parseVersionField(line); v != "" {
		return v
	}
	return deps.Current
}

// parseVersionField extracts the version token from a `portal version` line
// ("portal v0.8.0 (commit abc)" -> "v0.8.0"). An unrecognized shape returns
// empty so the caller can fall back rather than compare against garbage.
func parseVersionField(line string) string {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 || fields[0] != app.Tool {
		return ""
	}
	return fields[1]
}
