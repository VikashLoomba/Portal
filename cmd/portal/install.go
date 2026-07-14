package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/clipshim"
	"github.com/VikashLoomba/Portal/internal/setup"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/doctor"
)

type setupRunner interface {
	Validate(context.Context, string, bool) bool
	Configure(context.Context, string) error
	DeployRemote(context.Context, string)
	Verify(context.Context, string) *doctor.Report
	Close(context.Context)
}

var newSetupRunner = func(a *app.App, sink setup.Sink) setupRunner {
	return setup.New(a.Paths, a.Cfg, sink)
}

var installIsTTY = isatty

func newInstallCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "install [host]",
		Short: "Configure the dev box and install as a login agent",
		Long: `Configure the dev box (asks if not given) and install as a login agent
(auto-start + self-heal), then start it. The host can be an alias from
~/.ssh/config or user@hostname; key-based passwordless auth is required so
the headless launchd daemon can connect.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			arg := ""
			if len(args) == 1 {
				arg = args[0]
			}
			return runInstall(cmd.Context(), cmd.OutOrStdout(), os.Stdin, installIsTTY(os.Stdin), a, arg)
		},
	}
}

func runInstall(ctx context.Context, out io.Writer, in io.Reader, isTTY bool, a *app.App, arg string) error {
	host, err := resolveInstallHost(a, arg, in, isTTY, out)
	if err != nil {
		return usageErr{msg: fmt.Sprintf("no host given; run interactively or: %s install <ssh-host>", app.Tool)}
	}

	sink := setup.Sink(func(ev api.SetupEvent) {
		renderSetupEvent(out, host, a.Paths, ev)
	})
	r := newSetupRunner(a, sink)
	defer r.Close(ctx)

	if !r.Validate(ctx, host, false) {
		if !isTTY {
			return fmt.Errorf("ssh validation failed for %s", host)
		}
		fmt.Fprint(out, "install anyway? [y/N] ")
		reader := bufio.NewReader(in)
		ans, _ := reader.ReadString('\n')
		ans = strings.ToLower(strings.TrimSpace(ans))
		if ans != "y" && ans != "yes" {
			fmt.Fprintln(out, "aborted.")
			return fmt.Errorf("install aborted")
		}
		if !r.Validate(ctx, host, true) {
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("ssh validation failed for %s", host)
		}
	}

	if err := r.Configure(ctx, host); err != nil {
		return err
	}

	self, err := app.ResolveSelf()
	if err != nil {
		return fmt.Errorf("resolve self: %w", err)
	}
	if self != a.Paths.BinPath {
		if err := copyFile(self, a.Paths.BinPath, 0o755); err != nil {
			return fmt.Errorf("copy binary: %w", err)
		}
		fmt.Fprintf(out, "installed command -> %s\n", a.Paths.BinPath)
	} else {
		_ = os.Chmod(a.Paths.BinPath, 0o755)
	}

	if err := a.Service.Install(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "service loaded and started (%s)\n", a.Paths.Label)

	r.DeployRemote(ctx, host)

	if !pathContains(os.Getenv("PATH"), a.Paths.BinDir) {
		fmt.Fprintf(out, "NOTE: %s is not on your PATH. Add it to your shell profile:\n", a.Paths.BinDir)
		fmt.Fprintln(out, `      export PATH="$HOME/.local/bin:$PATH"`)
	}

	rep := r.Verify(ctx, host)
	renderDoctor(out, rep)

	fmt.Fprintf(out, "\ntry:  %s status\n", app.Tool)
	return nil
}

func renderSetupEvent(w io.Writer, host string, paths app.Paths, ev api.SetupEvent) {
	if ev.Step == "validate" && ev.Status == "warn" && ev.Error == nil {
		fmt.Fprintln(w, "ok")
	}
	if ev.Line != "" {
		io.WriteString(w, ev.Line)
		if ev.Status != "running" && !strings.HasSuffix(ev.Line, "\n") {
			fmt.Fprintln(w)
		}
	}

	switch ev.Step {
	case "validate":
		switch ev.Status {
		case "running":
			if ev.Line == "" {
				fmt.Fprintf(w, "checking ssh to %s ...\n", host)
			}
		case "ok":
			fmt.Fprintln(w, "ok")
		case "fail":
			renderValidationFailure(w, host)
		case "warn":
			if ev.Error != nil {
				renderValidationFailure(w, host)
			}
		}
	case "configure":
		if ev.Status == "ok" {
			fmt.Fprintf(w, "configured dev box: %s  (saved to %s)\n", host, paths.HostFile)
		}
	case "xdg-open":
		switch ev.Status {
		case "ok":
			fmt.Fprintf(w, "installed xdg-open wrapper on %s\n", host)
		case "warn":
			fmt.Fprintf(w, "WARNING: could not install xdg-open wrapper on %s: %s\n", host, setupErrorMessage(ev))
		}
	case "clip-shims":
		switch ev.Status {
		case "ok":
			fmt.Fprintf(w, "installed clipboard shims (xclip/wl-paste) on %s\n", host)
			fmt.Fprintln(w, "NOTE: keep your terminal's OSC 52 clipboard-WRITE disabled — a remote")
			fmt.Fprintln(w, "      could otherwise write your Mac clipboard and read it back.")
		case "warn":
			fmt.Fprintf(w, "WARNING: could not install clipboard shims on %s: %s\n", host, setupErrorMessage(ev))
			fmt.Fprintln(w, "         clipboard paste into coding agents will NOT work until this succeeds.")
			fmt.Fprintf(w, "         fix the cause above and re-run: %s install %s\n", app.Tool, host)
		}
	case "agent-symlink":
		if ev.Status == "warn" {
			fmt.Fprintf(w, "WARNING: could not update portald symlink on %s: %s\n", host, setupErrorMessage(ev))
		}
	case "doctor":
		if ev.Status == "running" {
			fmt.Fprintf(w, "\nrunning self-test (%s doctor) ...\n", app.Tool)
		}
	}
}

func renderValidationFailure(w io.Writer, host string) {
	fmt.Fprintln(w, "FAILED")
	fmt.Fprintf(w, "Could not reach '%s' with key-based auth. The background daemon needs\n", host)
	fmt.Fprintf(w, "passwordless SSH — set up your key (ssh-copy-id %s) and optionally an\n", host)
	fmt.Fprintln(w, "entry in ~/.ssh/config, then re-run.")
}

func setupErrorMessage(ev api.SetupEvent) string {
	if ev.Error == nil || ev.Error.Message == "" {
		return "unknown error"
	}
	return ev.Error.Message
}

func resolveInstallHost(a *app.App, arg string, in io.Reader, isTTY bool, errw io.Writer) (string, error) {
	if h := setup.NormalizeHost(arg); h != "" {
		return h, nil
	}
	if h, _ := a.Cfg.ReadHost(); h != "" {
		return h, nil
	}
	if !isTTY {
		return "", fmt.Errorf("no host and not a TTY")
	}
	fmt.Fprint(errw, "SSH host for your dev box (an alias from ~/.ssh/config, or user@hostname): ")
	reader := bufio.NewReader(in)
	line, _ := reader.ReadString('\n')
	h := setup.NormalizeHost(line)
	if h == "" {
		return "", fmt.Errorf("empty input")
	}
	return h, nil
}

func stripWhitespace(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func pathContains(path, dir string) bool {
	for _, p := range strings.Split(path, ":") {
		if p == dir {
			return true
		}
	}
	return false
}

// isatty reports whether f is a terminal. Plain stat-based check is enough
// for the install prompt heuristic; we don't need full termios introspection.
func isatty(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// removePortalWrappers removes everything portal deploys to the dev box's
// ~/.local/bin and shell rc files. The shared clipshim implementation restores
// backups and removes the environment marker blocks.
func removePortalWrappers(ctx context.Context, a *app.App) {
	clipshim.Remove(ctx, a.Transport)
}
