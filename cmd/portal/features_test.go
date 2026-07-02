package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
)

// setFeatures pre-seeds the shared config.Store so both the daemon and the CLI
// read the same gate files.
func setFeatures(t *testing.T, cfg *config.Store, states map[string]bool) {
	t.Helper()
	for name, on := range states {
		if err := cfg.SetFeature(name, on); err != nil {
			t.Fatalf("SetFeature(%s): %v", name, err)
		}
	}
}

// EC (features list, daemon up): with the daemon serving GET /v1/features over
// the shared config.Store, `features` (no args) renders every gate in the fixed
// featureNames order.
func TestFeatures_DaemonUp_ListDeterministicOrder(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	setFeatures(t, cfg, map[string]bool{
		config.FeatureClipImage: true,
		config.FeatureClipText:  false,
		config.FeatureNotify:    true,
	})
	d := startFakeDaemon(t, cfg)
	a := newDaemonTestApp(t, d.path, cfg)

	var out, errw bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runFeatures(ctx, &out, &errw, a, nil); err != nil {
		t.Fatalf("runFeatures list: %v", err)
	}

	want := "clip-image: on\nclip-text: off\nnotify: on\n"
	if out.String() != want {
		t.Errorf("list output:\n--- got ---\n%s--- want ---\n%s", out.String(), want)
	}
	if errw.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errw.String())
	}
}

// EC (features set, daemon up): `features clip-text off` PUTs through the daemon
// (writing the shared config.Store) and echoes only the changed line.
func TestFeatures_DaemonUp_SetWritesThroughDaemon(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	setFeatures(t, cfg, map[string]bool{config.FeatureClipText: true})
	d := startFakeDaemon(t, cfg)
	a := newDaemonTestApp(t, d.path, cfg)

	var out, errw bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runFeatures(ctx, &out, &errw, a, []string{"clip-text", "off"}); err != nil {
		t.Fatalf("runFeatures set: %v", err)
	}

	if a.Cfg.FeatureEnabled(config.FeatureClipText) {
		t.Error("PUT /v1/features did not disable clip-text in the shared config.Store")
	}
	if want := "clip-text: off\n"; out.String() != want {
		t.Errorf("set output = %q, want %q", out.String(), want)
	}
	if errw.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errw.String())
	}
}

// EC (features unknown): an unknown name fails with the unknown-feature line on
// stderr and a usageErr, before any config write — the same failure up or down.
func TestFeatures_UnknownName(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	d := startFakeDaemon(t, cfg)
	a := newDaemonTestApp(t, d.path, cfg)

	var out, errw bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runFeatures(ctx, &out, &errw, a, []string{"bogus", "on"})

	var ue usageErr
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want usageErr", err)
	}
	if want := "unknown feature: bogus (known: clip-image, clip-text, notify)\n"; errw.String() != want {
		t.Errorf("stderr = %q, want %q", errw.String(), want)
	}
	if out.Len() != 0 {
		t.Errorf("unexpected stdout: %q", out.String())
	}
}

// EC (features fallback, daemon down): with a dead socket, list reads config.Store
// directly and set writes config.Store directly, with no stderr spam. Proves the
// down path is byte-for-byte the up path from the user's view.
func TestFeatures_DaemonDown_Fallback(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	setFeatures(t, cfg, map[string]bool{
		config.FeatureClipImage: false,
		config.FeatureClipText:  true,
		config.FeatureNotify:    true,
	})
	// Point APISock at a nonexistent path so localclient dials fail fast.
	a := newDaemonTestApp(t, filepath.Join(t.TempDir(), "nope.sock"), cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// LIST falls back to reading a.Cfg directly.
	var out, errw bytes.Buffer
	if err := runFeatures(ctx, &out, &errw, a, nil); err != nil {
		t.Fatalf("runFeatures list (down): %v", err)
	}
	if want := "clip-image: off\nclip-text: on\nnotify: on\n"; out.String() != want {
		t.Errorf("fallback list:\n--- got ---\n%s--- want ---\n%s", out.String(), want)
	}
	if errw.Len() != 0 {
		t.Errorf("unexpected stderr on fallback list: %q", errw.String())
	}

	// SET falls back to writing a.Cfg directly.
	out.Reset()
	errw.Reset()
	if err := runFeatures(ctx, &out, &errw, a, []string{"notify", "off"}); err != nil {
		t.Fatalf("runFeatures set (down): %v", err)
	}
	if a.Cfg.FeatureEnabled(config.FeatureNotify) {
		t.Error("fallback set did not disable notify in config.Store")
	}
	if want := "notify: off\n"; out.String() != want {
		t.Errorf("fallback set output = %q, want %q", out.String(), want)
	}
	if errw.Len() != 0 {
		t.Errorf("unexpected stderr on fallback set: %q", errw.String())
	}
}
