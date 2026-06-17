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

			fmt.Printf("checking passwordless ssh to %s ... ", host)
			ssh := sshctlSSH(a)
			if err := ssh.Validate(cmd.Context(), host); err == nil {
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

// silence unused-import for context (some build paths drop it).
var _ = context.Background
