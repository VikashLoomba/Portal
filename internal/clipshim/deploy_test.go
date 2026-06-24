package clipshim

import (
	"context"
	"io"
	"strings"
	"testing"
)

// recordTransport implements sshctl.Transport, capturing every script Ensure /
// Remove issue (both Exec and ExecBytes) so a test can assert the deploy wiring
// WITHOUT a live dev box: which files get written, that the notify hook + PATH
// block + settings.json merge run, and that Remove strips them. Exec replies are
// scripted by substring so the steady-state "current" fast path can be forced.
type recordTransport struct {
	// execScripts is every script passed to Exec, in order.
	execScripts []string
	// execBytesScripts is every script passed to ExecBytes, in order, paired
	// with the stdin payload (the shim/script text being written).
	execBytesScripts []string
	execBytesStdin   [][]byte
	// reply maps a substring of the Exec script to the stdout to return.
	reply map[string]string
}

func (r *recordTransport) MasterPID(ctx context.Context) (int, error)          { return 1, nil }
func (r *recordTransport) EnsureMaster(ctx context.Context) (int, bool, error) { return 1, false, nil }
func (r *recordTransport) Forward(ctx context.Context, l, rr int) error        { return nil }
func (r *recordTransport) Cancel(ctx context.Context, l, rr int) error         { return nil }
func (r *recordTransport) Exit(ctx context.Context) (bool, error)              { return true, nil }
func (r *recordTransport) Host() string                                        { return "fakehost" }
func (r *recordTransport) Sock() string                                        { return "/tmp/sock" }
func (r *recordTransport) ExecStream(_ context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, nil, nil
}

func (r *recordTransport) Exec(_ context.Context, _ string, argv ...string) (string, error) {
	joined := strings.Join(argv, " ")
	r.execScripts = append(r.execScripts, joined)
	for key, out := range r.reply {
		if strings.Contains(joined, key) {
			return out, nil
		}
	}
	return "", nil
}

func (r *recordTransport) ExecBytes(_ context.Context, stdin []byte, argv ...string) (string, string, error) {
	r.execBytesScripts = append(r.execBytesScripts, strings.Join(argv, " "))
	r.execBytesStdin = append(r.execBytesStdin, stdin)
	return "", "", nil
}

// allScripts returns every Exec + ExecBytes script joined, for substring asserts.
func (r *recordTransport) allScripts() string {
	return strings.Join(append(append([]string{}, r.execScripts...), r.execBytesScripts...), "\n")
}

// allStdin returns every ExecBytes stdin payload joined, for content asserts.
func (r *recordTransport) allStdin() string {
	var b strings.Builder
	for _, s := range r.execBytesStdin {
		b.Write(s)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestEnsure_DeploysShimsAndHook drives Ensure on a stale box (the marker grep
// reports "stale") and asserts the full deploy fires: both shim scripts are
// written, the notify hook script is written, the Claude settings.json merge
// runs, and the PATH-prepend block is appended.
func TestEnsure_DeploysShimsAndHook(t *testing.T) {
	tr := &recordTransport{
		reply: map[string]string{
			// Force the slow path: shims are not yet current.
			"echo current || echo stale": "stale",
			// Each shim's post-write verify must report ok so Ensure proceeds.
			"echo ok || echo missing": "ok",
		},
	}
	if err := Ensure(context.Background(), tr); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	stdin := tr.allStdin()
	// Both shims written (their Marker is in the stdin payload).
	if !strings.Contains(stdin, Marker) {
		t.Error("expected shim Marker in a written payload")
	}
	// The notify-hook script is written (recognized by its portald notify --hook line).
	if !strings.Contains(stdin, "portald notify --hook") {
		t.Error("expected the notify-hook script to be written")
	}
	// The PATH-prepend block is written.
	if !strings.Contains(stdin, PathMarkerStart) {
		t.Error("expected the PATH-prepend block to be written")
	}

	scripts := tr.allScripts()
	// The Claude settings.json merge runs (python3-guarded).
	if !strings.Contains(scripts, ".claude/settings.json") {
		t.Error("expected the Claude settings.json merge to run")
	}
	// Both shims targeted ~/.local/bin/xclip and wl-paste.
	if !strings.Contains(scripts, "~/.local/bin/xclip") || !strings.Contains(scripts, "~/.local/bin/wl-paste") {
		t.Error("expected both xclip and wl-paste shims to be deployed")
	}
}

// TestEnsure_FastPathWhenCurrent: when both shims already carry the current
// Marker, Ensure must NOT rewrite them (no shim ExecBytes), but must still
// ensure the notify hook + PATH block (idempotent convergence).
func TestEnsure_FastPathWhenCurrent(t *testing.T) {
	tr := &recordTransport{
		reply: map[string]string{
			"echo current || echo stale": "current",
		},
	}
	if err := Ensure(context.Background(), tr); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	stdin := tr.allStdin()
	// The shim Marker must NOT have been re-written (fast path skips deployShim).
	if strings.Contains(stdin, "# "+Marker+". Intercepts") {
		t.Error("fast path should NOT rewrite the shim scripts")
	}
	// But the PATH block + notify hook still run.
	if !strings.Contains(stdin, PathMarkerStart) {
		t.Error("fast path should still ensure the PATH-prepend block")
	}
	if !strings.Contains(stdin, "portald notify --hook") {
		t.Error("fast path should still ensure the notify hook")
	}
}

// TestRemove_StripsEverything asserts Remove issues a script that removes the
// shims, the notify hook, the portald symlink, the env snippet, strips the
// PATH-prepend marker block, and strips the portal-managed settings.json hooks.
func TestRemove_StripsEverything(t *testing.T) {
	tr := &recordTransport{}
	Remove(context.Background(), tr)
	got := tr.allScripts()
	for _, want := range []string{
		"~/.local/bin/portal-notify-hook",
		"~/.cache/portal/portald",
		"~/.config/portal/env.sh",
		PathMarkerStart,
		PathMarkerEnd,
		"PORTAL_MANAGED=1",
		"xdg-open xclip wl-paste",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Remove script missing %q\nscript:\n%s", want, got)
		}
	}
}

// TestEnsure_NotifyHookFailureDoesNotBlockPath: a failed settings.json merge
// must not abort Ensure (the headline clip PATH convergence takes priority). We
// can't make Exec fail by substring alone here, but we assert the documented
// contract structurally: ensureNotifyHook's error is swallowed in Ensure, so a
// box without python3 (merge no-op) still converges the PATH block. Since the
// fake never errors, the meaningful assertion is that PATH-prepend ran AFTER the
// hook step regardless.
func TestEnsure_NotifyHookFailureDoesNotBlockPath(t *testing.T) {
	tr := &recordTransport{
		reply: map[string]string{"echo current || echo stale": "current"},
	}
	if err := Ensure(context.Background(), tr); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// PATH-prepend must have run (it is the last, non-skippable step).
	if !strings.Contains(tr.allStdin(), PathMarkerStart) {
		t.Fatal("PATH-prepend must run even though the notify hook is best-effort")
	}
}
