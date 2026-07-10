package clipshim

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/pkg/transport"
)

// recordTransport implements transport.Transport, capturing every script Ensure
// / Remove issue so a test can assert the deploy wiring WITHOUT a live dev box:
// which files get written, that the notify hook + PATH block + settings.json
// merge run, and that Remove strips them. Nil-stdin calls (probes) land in
// execScripts and are answered from reply by substring; byte-stdin calls
// (file writes) land in execBytesScripts paired with their payload.
type recordTransport struct {
	// execScripts is every nil-stdin script passed to Exec, in order.
	execScripts []string
	// execBytesScripts is every byte-stdin script passed to Exec, in order,
	// paired with the stdin payload (the shim/script text being written).
	execBytesScripts []string
	execBytesStdin   [][]byte
	// reply maps a substring of the Exec script to the stdout to return.
	reply map[string]string
	// err maps a substring of the Exec script to the error to return.
	err map[string]error
}

func (r *recordTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (r *recordTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1, Detail: "pid=1"}, nil
}
func (r *recordTransport) Close(context.Context) (bool, error) { return true, nil }
func (r *recordTransport) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: "fakehost", Endpoint: "/tmp/sock"}
}
func (r *recordTransport) Stream(_ context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, nil, nil
}

// Exec folds the old Exec (nil stdin) and ExecBytes (byte stdin) recorders:
// byte-stdin calls capture the payload; nil-stdin probes are answered by reply.
func (r *recordTransport) Exec(_ context.Context, stdin []byte, argv ...string) (string, string, error) {
	joined := strings.Join(argv, " ")
	if len(stdin) > 0 {
		r.execBytesScripts = append(r.execBytesScripts, joined)
		r.execBytesStdin = append(r.execBytesStdin, stdin)
	} else {
		r.execScripts = append(r.execScripts, joined)
	}
	for key, err := range r.err {
		if strings.Contains(joined, key) {
			return "", "", err
		}
	}
	if len(stdin) > 0 {
		return "", "", nil
	}
	for key, out := range r.reply {
		if strings.Contains(joined, key) {
			return out, "", nil
		}
	}
	return "", "", nil
}

var _ transport.Transport = (*recordTransport)(nil)

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
// reports "stale") and asserts the full deploy fires: all versioned shim
// scripts are written, the notify hook script is written, the Claude
// settings.json merge runs, and both shell environment blocks are appended.
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
	// Every versioned shim is written with its exact script payload.
	if !strings.Contains(stdin, Marker) {
		t.Error("expected shim Marker in a written payload")
	}
	for _, sh := range shims {
		if !strings.Contains(stdin, sh.script) {
			t.Errorf("expected %s shim script to be written", sh.name)
		}
	}
	// The notify-hook script is written (recognized by its portald notify --hook line).
	if !strings.Contains(stdin, "portald notify --hook") {
		t.Error("expected the notify-hook script to be written")
	}
	// The PATH-prepend block is written.
	if !strings.Contains(stdin, PathMarkerStart) {
		t.Error("expected the PATH-prepend block to be written")
	}
	if !strings.Contains(stdin, AskpassMarkerStart) {
		t.Error("expected the SUDO_ASKPASS block to be written")
	}

	scripts := tr.allScripts()
	// The Claude settings.json merge runs (python3-guarded).
	if !strings.Contains(scripts, ".claude/settings.json") {
		t.Error("expected the Claude settings.json merge to run")
	}
	// Every table entry targets its ~/.local/bin path.
	for _, sh := range shims {
		if !strings.Contains(scripts, "~/.local/bin/"+sh.name) {
			t.Errorf("expected %s shim to be deployed", sh.name)
		}
	}
	if !strings.Contains(scripts, `grep -qF "Installed by portal" ~/.local/bin/xdg-open`) {
		t.Error("v6 convergence must recognize the legacy portal-owned xdg-open wrapper")
	}
}

// TestEnsure_FastPathWhenCurrent: when all shims already carry the current
// Marker, Ensure must NOT rewrite them (no shim ExecBytes), but must still
// ensure the notify hook plus both environment blocks (idempotent convergence).
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
	// No exact shim payload may be re-written (fast path skips deployShim).
	for _, sh := range shims {
		if strings.Contains(stdin, sh.script) {
			t.Errorf("fast path should NOT rewrite the %s shim", sh.name)
		}
	}
	// But both environment blocks + notify hook still run.
	if !strings.Contains(stdin, PathMarkerStart) {
		t.Error("fast path should still ensure the PATH-prepend block")
	}
	if !strings.Contains(stdin, AskpassMarkerStart) {
		t.Error("fast path should still ensure the SUDO_ASKPASS block")
	}
	if !strings.Contains(stdin, "portald notify --hook") {
		t.Error("fast path should still ensure the notify hook")
	}
}

func TestCurrentShimsProbeCoversDeploymentTable(t *testing.T) {
	probe := currentShimsProbe()
	for _, sh := range shims {
		if !strings.Contains(probe, "~/.local/bin/"+sh.name) {
			t.Errorf("current marker probe missing %s", sh.name)
		}
	}
	if got, want := strings.Count(probe, Marker), len(shims); got != want {
		t.Fatalf("current marker probe contains Marker %d times, want %d", got, want)
	}
	if strings.Contains(probe, "clip-shim v5") {
		t.Fatal("current marker probe still accepts the pre-hardening v5 shims")
	}
}

// TestRemove_StripsEverything asserts Remove issues a script that removes the
// shims, the notify hook, the portald symlink, the env snippet, strips the
// PATH/SUDO_ASKPASS marker blocks, and strips the portal-managed settings.json
// hooks.
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
		AskpassMarkerStart,
		AskpassMarkerEnd,
		"PORTAL_MANAGED=1",
		"xdg-open xclip wl-paste portal portal-askpass sudo",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Remove script missing %q\nscript:\n%s", want, got)
		}
	}
}

// TestEnsure_NotifyHookFailureDoesNotBlockPath: a failed settings.json merge
// must not abort Ensure (the headline clip PATH convergence takes priority). We
// inject a merge error and assert both environment blocks still converge.
func TestEnsure_NotifyHookFailureDoesNotBlockPath(t *testing.T) {
	tr := &recordTransport{
		reply: map[string]string{"echo current || echo stale": "current"},
		err:   map[string]error{".claude/settings.json": errors.New("merge failed")},
	}
	if err := Ensure(context.Background(), tr); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// PATH-prepend must have run (it is the last, non-skippable step).
	if !strings.Contains(tr.allStdin(), PathMarkerStart) {
		t.Fatal("PATH-prepend must run even though the notify hook is best-effort")
	}
	if !strings.Contains(tr.allStdin(), AskpassMarkerStart) {
		t.Fatal("SUDO_ASKPASS must run even though the notify hook is best-effort")
	}
}

func TestEnsure_AskpassEnvFailurePropagates(t *testing.T) {
	writeErr := errors.New("rc file unavailable")
	tr := &recordTransport{
		reply: map[string]string{"echo current || echo stale": "current"},
		err:   map[string]error{AskpassMarkerStart: writeErr},
	}
	err := Ensure(context.Background(), tr)
	if !errors.Is(err, writeErr) {
		t.Fatalf("Ensure error = %v, want wrapped askpass write error", err)
	}
	if !strings.Contains(tr.allStdin(), PathMarkerStart) {
		t.Fatal("PATH-prepend must run before the SUDO_ASKPASS block")
	}
}
