package main

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/osa"
	"github.com/VikashLoomba/Portal/pkg/agentclient"
)

// notifyDeliverTimeout bounds a single osascript/terminal-notifier invocation
// so a wedged notification subsystem can't hang the handler goroutine. A
// notification is non-critical; if it can't be raised in this window, drop it.
const notifyDeliverTimeout = 5 * time.Second

// runNotifyHandler services KindNotify events from the agent: a remote event (a
// Claude Code hook firing `portald notify --hook`, or a generic `portald
// notify`) was relayed up the pipe. It runs on its OWN goroutine fed by a
// DEDICATED channel (sibling to runClipHandler / runOpenURLHandler) so a burst
// of port events can never evict a pending notification. Each notification is
// raised via osascript (cgo-free, exactly like internal/clip shells osascript)
// — or terminal-notifier when present, which gives a richer banner. Title/body
// are sanitized for AppleScript injection; an unverified event gets an
// "[unverified] " title prefix (mirroring cc-clip's trust model).
func runNotifyHandler(ctx context.Context, ch <-chan agentclient.EngineEvent, a *app.App) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Notify == nil {
				continue
			}
			n := ev.Notify
			// Capability gate (SPEC C): a user can disable notifications by
			// writing "off" into ~/.config/portal/feature.notify. Drop the
			// relayed event before raising anything when the feature is off.
			if !a.Cfg.FeatureEnabled(config.FeatureNotify) {
				a.Audit.NotifyDenied(a.Transport.Describe().Host, "disabled")
				continue
			}
			title := n.Title
			if !n.Verified {
				// Mark events that did NOT arrive via the structured hook
				// entrypoint, so an arbitrary `portald notify` cannot perfectly
				// impersonate a real Claude Code hook (cc-clip's trust model).
				title = "[unverified] " + title
			}
			a.Log.Logf("notify from %s: %q (verified=%v urgency=%d seq=%d)",
				a.Transport.Describe().Host, n.Title, n.Verified, n.Urgency, n.Seq)
			a.Audit.Notify(a.Transport.Describe().Host, n.Title, n.Verified, n.Urgency)
			subtitle := n.Subtitle
			if subtitle == "" {
				// Default subtitle to the host for context, consistent with the
				// agent binding Host on the envelope (recon recommendation).
				subtitle = a.Transport.Describe().Host
			}
			sound := n.Sound
			if sound == "" {
				sound = defaultSoundForUrgency(n.Urgency)
			}
			raiseNotification(ctx, title, n.Body, subtitle, sound)
		}
	}
}

// raiseNotification shells out to terminal-notifier (preferred, richer) or
// osascript (always present on macOS) to raise a native notification. All
// string fields are sanitized for AppleScript injection before they reach the
// osascript literal. Best-effort: failures are swallowed (a missed notification
// is non-fatal).
func raiseNotification(ctx context.Context, title, body, subtitle, sound string) {
	dctx, cancel := context.WithTimeout(ctx, notifyDeliverTimeout)
	defer cancel()

	// terminal-notifier takes its strings as argv (no shell/AppleScript
	// interpolation), so it needs no escaping — but we still strip control bytes
	// for tidy banners.
	if path, err := exec.LookPath("terminal-notifier"); err == nil {
		args := []string{
			"-title", stripControl(title),
			"-subtitle", stripControl(subtitle),
			"-message", stripControl(body),
			"-group", "portal",
		}
		if sound != "" {
			args = append(args, "-sound", stripControl(sound))
		}
		_ = exec.CommandContext(dctx, path, args...).Run()
		return
	}

	// osascript fallback. `display notification` cannot take an argv vector
	// (unlike `on run argv` scripts), so the strings are interpolated into the
	// AppleScript source — they MUST be escaped against injection.
	script := "display notification " + osa.StringLiteral(body) +
		" with title " + osa.StringLiteral(title) +
		" subtitle " + osa.StringLiteral(subtitle)
	if sound != "" {
		script += " sound name " + osa.StringLiteral(sound)
	}
	_ = exec.CommandContext(dctx, "osascript", "-e", script).Run()
}

// stripControl removes ASCII control bytes (including newline, carriage return,
// tab, NUL) from s. A notification banner is single-line; control bytes only
// serve to break the osascript literal or the argv boundary.
func stripControl(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// defaultSoundForUrgency maps a notification urgency tier to a default macOS
// system sound name when the event did not specify one. Kept minimal and
// non-intrusive: only the critical tier (tool approval) gets an audible cue by
// default so a passive notification stream doesn't constantly chime.
func defaultSoundForUrgency(urgency uint8) string {
	switch urgency {
	case 2: // critical: tool approval needed — worth an audible cue
		return "Glass"
	default:
		return ""
	}
}
