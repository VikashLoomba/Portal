package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/localapi"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/localclient"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/logfile"
)

func newStatusCmd(a *app.App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show box, service state, ssh master, active forwards (default)",
		RunE: func(cmd *cobra.Command, args []string) error {
			watch, _ := cmd.Flags().GetBool("watch")
			if watch {
				return runStatusWatch(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), a)
			}
			return runStatusTo(cmd.Context(), cmd.OutOrStdout(), a)
		},
	}
	cmd.Flags().BoolP("watch", "w", false, "continuously re-render on daemon state changes")
	return cmd
}

// runStatusWatch streams GET /v1/events and re-renders the status block on every
// snapshot/state line, exiting cleanly (nil) when the stream ends — which the
// daemon's own shutdown produces because localapi.Serve wires its http.Server
// BaseContext to the Serve ctx, so cancelling it cancels the /v1/events handler
// and EOFs the stream (§5.2). A watch has nothing to watch when the daemon is
// down, so an unreachable daemon prints one polite line and exits non-zero.
func runStatusWatch(ctx context.Context, w, errw io.Writer, a *app.App) error {
	lc := localclient.New(a.Paths.APISock)
	if !lc.Available(ctx) {
		fmt.Fprintf(errw, "%s status --watch needs the running daemon; run `%s status` instead\n", app.Tool, app.Tool)
		return errSilent
	}
	events, errc, err := lc.Events(ctx)
	if err != nil {
		fmt.Fprintf(errw, "%s status --watch needs the running daemon; run `%s status` instead\n", app.Tool, app.Tool)
		return errSilent
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			// snapshot/state carry a full Status; notify/tick are ignored.
			if ev.Type == "snapshot" || ev.Type == "state" {
				if ev.Status != nil {
					renderStatus(w, viewFromStatus(a, *ev.Status))
				}
			}
		case <-errc:
			return nil
		}
	}
}

// statusAgentView is the connected agent's identity as rendered by the status
// command. It is nil unless the daemon has completed its handshake (the sha is
// already truncated to <=12 runes by the view builders).
type statusAgentView struct {
	pid    int
	sha    string
	kernel string
}

// statusView is the fully-resolved input to renderStatus: the single source
// that both the daemon-up (viewFromStatus) and daemon-down (viewFromLocal)
// paths funnel through, so the two renderings cannot drift (§5.2). Every
// early-return the old runStatus made is encoded as a struct field
// (hostKnown/masterUp) rather than re-implemented per path.
type statusView struct {
	host       string
	label      string
	hostKnown  bool
	loaded     bool
	stateLines []string
	masterUp   bool
	masterPID  int
	sock       string
	agent      *statusAgentView
	forwards   []string
}

// renderStatus reproduces the historical runStatus output byte-for-byte (§5.2).
// The ONLY permitted delta versus the pre-API layout is the agent line, which
// appears iff v.agent != nil.
func renderStatus(w io.Writer, v statusView) {
	if v.hostKnown {
		fmt.Fprintf(w, "dev box: %s\n", v.host)
	} else {
		fmt.Fprintf(w, "dev box: <not configured>\n")
	}
	fmt.Fprintf(w, "service (%s):\n", v.label)

	if !v.loaded {
		fmt.Fprintf(w, "  not loaded (run: %s install)\n", app.Tool)
	} else {
		for _, line := range v.stateLines {
			fmt.Fprintln(w, line)
		}
	}
	fmt.Fprintln(w)

	if !v.hostKnown {
		return
	}
	if !v.masterUp {
		fmt.Fprintf(w, "ssh master: DOWN (host=%s sock=%s)\n", v.host, v.sock)
		return
	}
	fmt.Fprintf(w, "ssh master: UP (pid=%d) host=%s\n", v.masterPID, v.host)
	if v.agent != nil {
		fmt.Fprintf(w, "agent: pid=%d sha=%s kernel=%s\n", v.agent.pid, v.agent.sha, v.agent.kernel)
	}
	fmt.Fprintln(w, "active forwards (local listeners owned by master):")
	// Bash: lsof | awk 'NR>1 {print "  " $9}' | sort -u — emits the verbatim
	// NAME column, so an IPv4+IPv6 dual-stack listener produces TWO lines.
	for _, ln := range v.forwards {
		fmt.Fprintf(w, "  %s\n", ln)
	}
}

// truncSHA narrows a git sha to its first 12 runes, matching the elided agent
// line's historical width.
func truncSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// viewFromStatus builds a statusView from the daemon's Status aggregate. The
// agent line is present iff the daemon reported a handshaked agent.
func viewFromStatus(a *app.App, st localapi.Status) statusView {
	v := statusView{
		host:       st.Host,
		label:      a.Paths.Label,
		hostKnown:  st.Host != "",
		loaded:     st.Service.Loaded,
		stateLines: st.Service.StateLines,
		masterUp:   st.Master.Up,
		masterPID:  st.Master.Pid,
		sock:       a.Paths.Sock,
	}
	for _, f := range st.Forwards {
		v.forwards = append(v.forwards, f.Name)
	}
	if st.Agent != nil {
		v.agent = &statusAgentView{
			pid:    st.Agent.Pid,
			sha:    truncSHA(st.Agent.SHA),
			kernel: st.Agent.Kernel,
		}
	}
	return v
}

// viewFromLocal builds a statusView from the App's own adapters — today's
// file/lsof path, used when the daemon is unreachable. It preserves the old
// runStatus early returns: forwards/agent are fetched ONLY when host is set and
// the master is up. The agent line is populated only when a live in-process
// AgentClient has a handshake (a short-lived CLI invocation never does).
func viewFromLocal(ctx context.Context, a *app.App) statusView {
	host, _ := a.Cfg.ReadHost()
	v := statusView{
		host:      host,
		label:     a.Paths.Label,
		hostKnown: host != "",
		sock:      a.Paths.Sock,
	}
	if st, err := a.Service.Status(ctx); err == nil {
		v.loaded = st.Loaded
		v.stateLines = st.StateLines
	}
	if !v.hostKnown {
		return v
	}
	pid, _ := a.Transport.MasterPID(ctx)
	v.masterPID = pid
	v.masterUp = pid > 0
	if !v.masterUp {
		return v
	}
	lines, _ := a.Ports.MasterForwardLines(ctx, pid)
	v.forwards = lines
	if a.AgentClient != nil {
		if ack := a.AgentClient.HelloAck(); ack != nil {
			v.agent = &statusAgentView{
				pid:    ack.AgentPID,
				sha:    truncSHA(ack.AgentGitSHA),
				kernel: ack.Kernel,
			}
		}
	}
	return v
}

// runStatus keeps the historical signature so root.go's default RunE and
// run.go's runStatusCtx compile unchanged.
func runStatus(ctx context.Context, a *app.App) error {
	return runStatusTo(ctx, os.Stdout, a)
}

// runStatusTo renders `portal status` to w. It sources from the daemon over
// Paths.APISock when it answers, else falls back to the local file/lsof view.
// Any localclient error (no socket / dead socket / hung server) silently takes
// the local branch — a status invocation never spams stderr (§5.2).
func runStatusTo(ctx context.Context, w io.Writer, a *app.App) error {
	lc := localclient.New(a.Paths.APISock)
	if st, err := lc.Status(ctx); err == nil {
		renderStatus(w, viewFromStatus(a, st))
		return nil
	}
	renderStatus(w, viewFromLocal(ctx, a))
	return nil
}

func newPortsCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:     "ports",
		Aliases: []string{"list-remote"},
		Short:   "List loopback dev ports currently listening on the dev box",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPorts(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), a)
		},
	}
}

// runPorts lists remote loopback listeners. Daemon-up path first: GET /v1/ports
// serves the cached Snapshot (fixing the old dependency on a CLI-side agent).
// A daemon that is up but has no cached Snapshot yet (ErrNotConnected) prints
// the header only — we deliberately do NOT spin a CLI-side agent alongside the
// daemon's. Any dial/transport error falls through to today's exact behavior.
func runPorts(ctx context.Context, w, errw io.Writer, a *app.App) error {
	lc := localclient.New(a.Paths.APISock)
	ports, err := lc.Ports(ctx)
	if err == nil {
		host, _ := a.Cfg.ReadHost()
		fmt.Fprintf(w, "loopback dev ports listening on %s (will be forwarded):\n", host)
		for _, p := range ports {
			fmt.Fprintf(w, "  %d\n", p.Port)
		}
		return nil
	}
	if errors.Is(err, localclient.ErrNotConnected) {
		host, _ := a.Cfg.ReadHost()
		fmt.Fprintf(w, "loopback dev ports listening on %s (will be forwarded):\n", host)
		return nil
	}

	// Daemon down: today's exact behavior (own short-lived master probe).
	host, _ := a.Cfg.ReadHost()
	if host == "" {
		return fmt.Errorf("no dev box configured — run: %s install <ssh-host>", app.Tool)
	}
	if pid, _, err := a.Transport.EnsureMaster(ctx); err != nil || pid == 0 {
		fmt.Fprintf(errw, "could not reach %s\n", host)
		return errSilent
	}
	allow, _ := a.Cfg.AllowedPorts()
	desired, err := a.Discover.DesiredPorts(ctx, app.DenyPorts, allow)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "loopback dev ports listening on %s (will be forwarded):\n", host)
	for _, p := range desired {
		fmt.Fprintf(w, "  %d\n", p)
	}
	return nil
}

func newLogsCmd(a *app.App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs [-f|N]",
		Short: "Show recent log lines; -f to follow, N for last N lines",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := a.Paths.Log
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("no log yet at %s", path)
			}
			arg := ""
			if len(args) == 1 {
				arg = args[0]
			}
			switch arg {
			case "":
				out, err := logfile.Tail(path, 40)
				if err != nil {
					return err
				}
				fmt.Print(out)
				return nil
			case "-f", "--follow", "follow":
				return logfile.Follow(cmd.Context(), path, os.Stdout)
			default:
				n, err := strconv.Atoi(strings.TrimSpace(arg))
				if err != nil || n < 0 {
					// Bash falls back to `tail -n 40` only on Atoi failure
					// (`tail -n <bad>` returns non-zero). `tail -n 0` is
					// VALID and prints zero lines, so we honor that here.
					n = 40
				}
				if n == 0 {
					return nil
				}
				out, err := logfile.Tail(path, n)
				if err != nil {
					return err
				}
				fmt.Print(out)
				return nil
			}
		},
	}
	return cmd
}
