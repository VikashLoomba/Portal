package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
)

// downApp returns an App pointed at a nonexistent socket, sharing cfg, for the
// daemon-DOWN fallback assertions.
func downApp(t *testing.T, cfg *config.Store) *app.App {
	t.Helper()
	return newDaemonTestApp(t, filepath.Join(t.TempDir(), "nope.sock"), cfg)
}

// EC3: with the daemon up, `allow N` pushes a real PUT that reaches the agent
// (a fresh Subscribe bumps rsid) WITHOUT waiting for a reconcile, and the
// printed suffix is the (now-truthful) ~100ms line.
func TestRunAllow_DaemonUp_PushReachesAgent(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	d := startFakeDaemon(t, cfg)
	a := newDaemonTestApp(t, d.path, cfg)

	before := d.agent.RSID()
	var out, errb bytes.Buffer
	if err := runAllow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
		t.Fatalf("runAllow: %v", err)
	}
	if got := d.agent.RSID(); got != before+1 {
		t.Errorf("rsid = %d, want %d (a fresh Subscribe should have reached the agent)", got, before+1)
	}
	if want := "allowed: 40085 (takes effect within ~100ms)\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	if errb.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errb.String())
	}
}

// EC4: exact allow/unallow lines for both the up and down paths, plus the
// unchanged "already allowed" and "allowlist is empty" strings.
func TestAllowUnallow_GoldenLines(t *testing.T) {
	t.Run("allow_up", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		d := startFakeDaemon(t, cfg)
		a := newDaemonTestApp(t, d.path, cfg)

		var out, errb bytes.Buffer
		if err := runAllow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
			t.Fatalf("runAllow: %v", err)
		}
		if want := "allowed: 40085 (takes effect within ~100ms)\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
		if errb.Len() != 0 {
			t.Errorf("unexpected stderr: %q", errb.String())
		}
	})

	t.Run("allow_down", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		a := downApp(t, cfg)

		var out, errb bytes.Buffer
		if err := runAllow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
			t.Fatalf("runAllow: %v", err)
		}
		if want := "allowed: 40085 (takes effect when the daemon reconciles)\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
		if errb.Len() != 0 {
			t.Errorf("unexpected stderr: %q", errb.String())
		}
	})

	t.Run("already_allowed", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		d := startFakeDaemon(t, cfg)
		a := newDaemonTestApp(t, d.path, cfg)

		// First allow puts 40085 in the file; the second is a no-op that prints
		// the unchanged "already allowed" line with no latency suffix.
		var scratch bytes.Buffer
		if err := runAllow(context.Background(), &scratch, &scratch, a, []string{"40085"}); err != nil {
			t.Fatalf("runAllow (seed): %v", err)
		}
		var out, errb bytes.Buffer
		if err := runAllow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
			t.Fatalf("runAllow: %v", err)
		}
		if want := "already allowed: 40085\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
	})

	t.Run("unallow_up", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		if _, err := cfg.Allow([]int{40085}); err != nil {
			t.Fatalf("seed Allow: %v", err)
		}
		d := startFakeDaemon(t, cfg)
		a := newDaemonTestApp(t, d.path, cfg)

		var out, errb bytes.Buffer
		if err := runUnallow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
			t.Fatalf("runUnallow: %v", err)
		}
		if want := "unallowed: 40085 (forward drops within ~100ms if in ephemeral range)\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
		if errb.Len() != 0 {
			t.Errorf("unexpected stderr: %q", errb.String())
		}
	})

	t.Run("unallow_down", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		if _, err := cfg.Allow([]int{40085}); err != nil {
			t.Fatalf("seed Allow: %v", err)
		}
		a := downApp(t, cfg)

		var out, errb bytes.Buffer
		if err := runUnallow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
			t.Fatalf("runUnallow: %v", err)
		}
		if want := "unallowed: 40085 (forward drops when the daemon reconciles)\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
		if errb.Len() != 0 {
			t.Errorf("unexpected stderr: %q", errb.String())
		}
	})

	t.Run("allowlist_is_empty", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox") // no allow file written
		a := downApp(t, cfg)

		var out, errb bytes.Buffer
		if err := runUnallow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
			t.Fatalf("runUnallow: %v", err)
		}
		if want := "allowlist is empty\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
	})
}

// EC2: with the daemon down, allow still writes the local file (the reconcile
// path depends on it) and prints the honest latency suffix — no stderr spam.
func TestRunAllow_DaemonDown_WritesFileAndHonestSuffix(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	a := downApp(t, cfg)

	var out, errb bytes.Buffer
	if err := runAllow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
		t.Fatalf("runAllow: %v", err)
	}
	// The local write must have landed regardless of the daemon being down.
	allowed, err := cfg.AllowedPorts()
	if err != nil {
		t.Fatalf("AllowedPorts: %v", err)
	}
	if !contains(allowed, 40085) {
		t.Errorf("allowlist = %v, want to contain 40085 (local write must happen when the daemon is down)", allowed)
	}
	if want := "allowed: 40085 (takes effect when the daemon reconciles)\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	if errb.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errb.String())
	}
}
