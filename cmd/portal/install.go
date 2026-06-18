package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vikashl/portal/internal/app"
	"github.com/vikashl/portal/internal/sshctl"
)

func filepathDir(p string) string { return filepath.Dir(p) }

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
			host, err := resolveInstallHost(a, arg, os.Stdin, os.Stderr)
			if err != nil {
				return usageErr{msg: fmt.Sprintf("no host given; run interactively or: %s install <ssh-host>", app.Tool)}
			}

			fmt.Printf("checking ssh to %s ...\n", host)
			ssh := sshctlSSH(a)
			// Pass os.Stderr directly so tools like Tailscale that print an
			// auth URL while establishing the connection are visible to the
			// user immediately (e.g. "To authenticate, visit: https://...").
			if err := ssh.Validate(cmd.Context(), host, os.Stderr); err == nil {
				fmt.Println("ok")
				if !ssh.HasSS(cmd.Context(), host) {
					fmt.Printf("WARNING: '%s' is reachable but has no 'ss' command — is it Linux? Port discovery may not work.\n", host)
				}
			} else {
				fmt.Println("FAILED")
				fmt.Printf("Could not reach '%s' with key-based auth. The background daemon needs\n", host)
				fmt.Printf("passwordless SSH — set up your key (ssh-copy-id %s) and optionally an\n", host)
				fmt.Println("entry in ~/.ssh/config, then re-run.")
				if isatty(os.Stdin) {
					fmt.Print("install anyway? [y/N] ")
					reader := bufio.NewReader(os.Stdin)
					ans, _ := reader.ReadString('\n')
					ans = strings.ToLower(strings.TrimSpace(ans))
					if ans != "y" && ans != "yes" {
						fmt.Println("aborted.")
						return fmt.Errorf("install aborted")
					}
				} else {
					return fmt.Errorf("ssh validation failed for %s", host)
				}
			}

			// Match bash: pre-create all four dirs before plist write.
			for _, d := range []string{
				a.Paths.ConfigDir,
				a.Paths.BinDir,
				filepathDir(a.Paths.Plist),
				filepathDir(a.Paths.Log),
			} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					return err
				}
			}
			if err := a.Cfg.WriteHost(host); err != nil {
				return err
			}
			fmt.Printf("configured dev box: %s  (saved to %s)\n", host, a.Paths.HostFile)

			self, err := app.ResolveSelf()
			if err != nil {
				return fmt.Errorf("resolve self: %w", err)
			}
			if self != a.Paths.BinPath {
				if err := copyFile(self, a.Paths.BinPath, 0o755); err != nil {
					return fmt.Errorf("copy binary: %w", err)
				}
				fmt.Printf("installed command -> %s\n", a.Paths.BinPath)
			} else {
				_ = os.Chmod(a.Paths.BinPath, 0o755)
			}

			if err := a.Service.Install(cmd.Context()); err != nil {
				return err
			}
			fmt.Printf("service loaded and started (%s)\n", a.Paths.Label)

			// Install the xdg-open wrapper on the dev box. Use a direct
			// ssh call rather than going through a.Transport — the
			// transport was built before the host was configured, so its
			// HostID is empty on a fresh install.
			if err := installXdgOpenWrapper(cmd.Context(), host, a); err != nil {
				fmt.Printf("WARNING: could not install xdg-open wrapper on %s: %v\n", host, err)
			} else {
				fmt.Printf("installed xdg-open wrapper on %s\n", host)
			}

			if !pathContains(os.Getenv("PATH"), a.Paths.BinDir) {
				fmt.Printf("NOTE: %s is not on your PATH. Add it to your shell profile:\n", a.Paths.BinDir)
				fmt.Printf("      export PATH=\"$HOME/.local/bin:$PATH\"\n")
			}
			fmt.Printf("try:  %s status\n", app.Tool)
			return nil
		},
	}
}

// sshctlSSH returns a fresh SSH transport bound to a placeholder host (used
// for `Validate`/`HasSS` calls which take the host as an arg, not from the
// transport's HostID).
func sshctlSSH(a *app.App) *sshctl.SSH {
	return sshctl.New(a.Paths.Sock, "", app.SSHOpts, a.Runner)
}

func resolveInstallHost(a *app.App, arg string, in *os.File, errw io.Writer) (string, error) {
	if h := stripWhitespace(arg); h != "" {
		return h, nil
	}
	if h, _ := a.Cfg.ReadHost(); h != "" {
		return h, nil
	}
	if !isatty(in) {
		return "", fmt.Errorf("no host and not a TTY")
	}
	fmt.Fprint(errw, "SSH host for your dev box (an alias from ~/.ssh/config, or user@hostname): ")
	reader := bufio.NewReader(in)
	line, _ := reader.ReadString('\n')
	h := stripWhitespace(line)
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

// xdgOpenWrapper is installed at ~/.local/bin/xdg-open on the dev box.
// It tries `portald open "$@"` first; if no client is active (exit 1),
// it falls through to the real xdg-open. The real binary location is
// resolved at call time via `command -v` so the wrapper never hard-codes
// a path.
//
// Design choices:
//   - Only intercepts when portald is reachable (cmd socket exists + client
//     connected). Falls through gracefully otherwise — safe even when the
//     user SSH's in directly without portal running.
//   - Does not shadow xclip/xsel or any other tool.
//   - The wrapper is a shell script, not a symlink, so it survives across
//     agent upgrades (the agent path changes with each SHA; the wrapper
//     resolves it at runtime via the `portald` symlink).
const xdgOpenWrapper = `#!/bin/sh
# Installed by portal. Relays xdg-open calls to the Mac client when a
# portal session is active; otherwise falls through to the real xdg-open.
_portald="${HOME}/.cache/portal/portald"
if [ -x "$_portald" ] && "$_portald" open "$@" 2>/dev/null; then
    exit 0
fi
# Fall through to the real xdg-open. Build a PATH that excludes the wrapper's
# own directory so we don't call ourselves recursively. Use fixed-string
# whole-line matching (-xF) so dots and other regex metacharacters in the
# directory path are treated literally.
_wrapper_dir=$(cd "$(dirname "$0")" && pwd)
_real=$(printf '%s' "$PATH" | tr ':' '\n' | grep -vxF "$_wrapper_dir" | tr '\n' ':' | xargs -I{} sh -c 'PATH={} command -v xdg-open 2>/dev/null' | head -1)
if [ -n "$_real" ]; then
    exec "$_real" "$@"
fi
# xdg-open not installed on this box — exit silently (headless server).
exit 0
`

// browserEnvSnippet is sourced by ~/.bashrc / ~/.zshrc on the dev box.
// It sets BROWSER=xdg-open so Python's webbrowser module (used by aws sso
// login, among others) delegates to xdg-open instead of falling through to
// w3m or other terminal browsers. This makes `aws sso login` open the URL
// via our wrapper and relay it to the Mac.
const browserEnvSnippet = `
# Added by portal — sets BROWSER so Python's webbrowser module uses xdg-open.
export BROWSER="${BROWSER:-xdg-open}"
`

// installXdgOpenWrapper writes the wrapper script to ~/.local/bin/xdg-open
// on the dev box. Uses a direct (non-multiplexed) ssh call with the given
// host so it works on a fresh install before the ControlMaster exists.
func installXdgOpenWrapper(ctx context.Context, host string, a *app.App) error {
	tr := sshctl.New(a.Paths.Sock, host, app.SSHOpts, a.Runner)

	// Write the BROWSER env snippet to ~/.config/portal/env.sh and source
	// it from ~/.bashrc and ~/.zshrc (if they exist). This ensures Python's
	// webbrowser module (used by aws sso login, etc.) delegates to xdg-open.
	envScript := `mkdir -p ~/.config/portal && cat > ~/.config/portal/env.sh`
	if _, _, err := tr.ExecBytes(ctx, []byte(browserEnvSnippet), "bash", "-c", shellQuoteRemote(envScript)); err != nil {
		return fmt.Errorf("write env snippet: %w", err)
	}
	// Source the snippet from each shell rc file that exists, idempotently.
	sourceSnippet := `
for rc in ~/.bashrc ~/.zshrc; do
    [ -f "$rc" ] || continue
    grep -qF "portal/env.sh" "$rc" && continue
    printf '\n[ -f ~/.config/portal/env.sh ] && . ~/.config/portal/env.sh\n' >> "$rc"
done`
	if _, err := tr.Exec(ctx, "", "bash", "-c", shellQuoteRemote(sourceSnippet)); err != nil {
		return fmt.Errorf("source env snippet: %w", err)
	}

	// Backup any pre-existing ~/.local/bin/xdg-open that isn't ours, so
	// uninstall can restore it. Skip the backup if it's already our wrapper.
	backupScript := `if [ -f ~/.local/bin/xdg-open ] && ! grep -qF "Installed by portal" ~/.local/bin/xdg-open 2>/dev/null; then cp ~/.local/bin/xdg-open ~/.local/bin/xdg-open.portal-backup; fi`
	_, _ = tr.Exec(ctx, "", "bash", "-c", shellQuoteRemote(backupScript))

	wrapScript := `mkdir -p ~/.local/bin && cat > ~/.local/bin/xdg-open.portal.tmp && chmod 0755 ~/.local/bin/xdg-open.portal.tmp && mv ~/.local/bin/xdg-open.portal.tmp ~/.local/bin/xdg-open`
	if _, _, err := tr.ExecBytes(ctx, []byte(xdgOpenWrapper), "bash", "-c", shellQuoteRemote(wrapScript)); err != nil {
		return fmt.Errorf("write wrapper: %w", err)
	}

	// The bootstrap.Manager also writes this symlink after upload, but it
	// may not have run yet on a fresh install (agent upload happens after
	// the service starts). Force it now using the known SHA.
	sha := a.Bootstrap.EmbeddedSHA()
	if sha != "" {
		symlinkScript := fmt.Sprintf(`ln -sf ~/.cache/portal/agent-%s ~/.cache/portal/portald 2>/dev/null || true`, sha)
		_, _ = tr.Exec(ctx, "", "bash", "-c", shellQuoteRemote(symlinkScript))
	}

	// Verify our wrapper landed correctly — check the file we just wrote
	// rather than resolving xdg-open through PATH (which is unreliable in
	// non-interactive ssh sessions and varies by distro).
	verifyScript := `grep -qF "Installed by portal" ~/.local/bin/xdg-open 2>/dev/null && echo ok || echo missing`
	out, _ := tr.Exec(ctx, "", "bash", "-c", shellQuoteRemote(verifyScript))
	if strings.TrimSpace(out) != "ok" {
		return fmt.Errorf("wrapper not found at ~/.local/bin/xdg-open on %s — check that the upload succeeded", host)
	}
	return nil
}

// removeXdgOpenWrapper removes the wrapper, portald symlink, and env snippet
// from the dev box, restoring any pre-existing xdg-open backed up at install.
func removeXdgOpenWrapper(ctx context.Context, a *app.App) {
	script := `
if [ -f ~/.local/bin/xdg-open.portal-backup ]; then
    mv ~/.local/bin/xdg-open.portal-backup ~/.local/bin/xdg-open
else
    rm -f ~/.local/bin/xdg-open
fi
rm -f ~/.cache/portal/portald
rm -f ~/.config/portal/env.sh
for rc in ~/.bashrc ~/.zshrc; do
    [ -f "$rc" ] || continue
    grep -qF "portal/env.sh" "$rc" || continue
    tmp=$(mktemp); grep -vF "portal/env.sh" "$rc" > "$tmp" && mv "$tmp" "$rc"
done`
	_, _ = a.Transport.Exec(ctx, "", "bash", "-c", shellQuoteRemote(script))
}

// shellQuoteRemote wraps a shell script in single quotes for safe remote
// execution via ssh (which joins argv with spaces and passes to sh -c).
func shellQuoteRemote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// silence unused-import for context (some build paths drop it).
var _ = context.Background
