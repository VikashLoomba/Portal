package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/VikashLoomba/Portal/internal/bootstrap"
	"github.com/VikashLoomba/Portal/internal/clipshim"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/transport"
)

const browserEnvSnippet = `
# Added by portal — sets BROWSER so Python's webbrowser module uses xdg-open.
export BROWSER="${BROWSER:-xdg-open}"
`

var deploySteps = []struct {
	name string
	code string
}{
	{name: "xdg-open", code: "xdg_open_failed"},
	{name: "clip-shims", code: "clip_shims_failed"},
	{name: "agent-symlink", code: "agent_symlink_failed"},
}

// DeployRemote converges the remote setup in its locked step order.
func (r *Runner) DeployRemote(ctx context.Context, host string) {
	tr, err := r.setupTransport(ctx, host)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		for _, step := range deploySteps {
			r.emit(api.SetupEvent{Step: step.name, Status: "running"})
			r.emit(api.SetupEvent{Step: step.name, Status: "warn", Error: errorDetail(step.code, err)})
		}
		return
	}

	r.emit(api.SetupEvent{Step: "xdg-open", Status: "running"})
	if err := installXDGOpen(ctx, host, tr); err != nil {
		r.emit(api.SetupEvent{Step: "xdg-open", Status: "warn", Error: errorDetail("xdg_open_failed", err)})
	} else {
		r.emit(api.SetupEvent{Step: "xdg-open", Status: "ok"})
	}
	if ctx.Err() != nil {
		return
	}

	r.emit(api.SetupEvent{Step: "clip-shims", Status: "running"})
	if err := clipshim.Ensure(ctx, tr); err != nil {
		r.emit(api.SetupEvent{Step: "clip-shims", Status: "warn", Error: errorDetail("clip_shims_failed", err)})
	} else {
		r.emit(api.SetupEvent{Step: "clip-shims", Status: "ok"})
	}
	if ctx.Err() != nil {
		return
	}

	r.emit(api.SetupEvent{Step: "agent-symlink", Status: "running"})
	sha := bootstrap.EmbeddedSHA()
	if sha == "" {
		err = errors.New("embedded agent SHA is empty")
	} else {
		script := fmt.Sprintf(`ln -sf ~/.cache/portal/agent-%s ~/.cache/portal/portald`, sha)
		_, _, err = tr.Exec(ctx, nil, "bash", "-c", shellQuote(script))
	}
	if err != nil {
		r.emit(api.SetupEvent{Step: "agent-symlink", Status: "warn", Error: errorDetail("agent_symlink_failed", err)})
	} else {
		r.emit(api.SetupEvent{Step: "agent-symlink", Status: "ok"})
	}
}

func installXDGOpen(ctx context.Context, host string, tr transport.Transport) error {
	envScript := `mkdir -p ~/.config/portal && cat > ~/.config/portal/env.sh`
	if _, _, err := tr.Exec(ctx, []byte(browserEnvSnippet), "bash", "-c", shellQuote(envScript)); err != nil {
		return fmt.Errorf("write env snippet: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	sourceSnippet := `
for rc in ~/.bashrc ~/.zshrc; do
    [ -f "$rc" ] || continue
    grep -qF "portal/env.sh" "$rc" && continue
    printf '\n[ -f ~/.config/portal/env.sh ] && . ~/.config/portal/env.sh\n' >> "$rc"
done`
	if _, _, err := tr.Exec(ctx, nil, "bash", "-c", shellQuote(sourceSnippet)); err != nil {
		return fmt.Errorf("source env snippet: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	backupScript := `if [ -f ~/.local/bin/xdg-open ] && ! grep -qF "Installed by portal" ~/.local/bin/xdg-open 2>/dev/null; then cp ~/.local/bin/xdg-open ~/.local/bin/xdg-open.portal-backup; fi`
	_, _, _ = tr.Exec(ctx, nil, "bash", "-c", shellQuote(backupScript))
	if err := ctx.Err(); err != nil {
		return err
	}

	wrapScript := `mkdir -p ~/.local/bin && cat > ~/.local/bin/xdg-open.portal.tmp && chmod 0755 ~/.local/bin/xdg-open.portal.tmp && mv ~/.local/bin/xdg-open.portal.tmp ~/.local/bin/xdg-open`
	if _, _, err := tr.Exec(ctx, []byte(clipshim.XDGOpenWrapper), "bash", "-c", shellQuote(wrapScript)); err != nil {
		return fmt.Errorf("write wrapper: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	verifyScript := `grep -qF "Installed by portal" ~/.local/bin/xdg-open 2>/dev/null && echo ok || echo missing`
	out, _, _ := tr.Exec(ctx, nil, "bash", "-c", shellQuote(verifyScript))
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(out) != "ok" {
		return fmt.Errorf("wrapper not found at ~/.local/bin/xdg-open on %s — check that the upload succeeded", host)
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
