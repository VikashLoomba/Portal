package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/config"
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
	// The daemon and the CLI read DISTINCT config.Stores seeded to opposite
	// postures. A real GET /v1/features renders the daemon's posture; a silent
	// fall-through to a.Cfg would render the CLI store's (inverted) posture, so
	// the output assertion alone now distinguishes the two paths — they are no
	// longer byte-identical over one shared store.
	daemonCfg := newTestConfig(t, "devbox")
	setFeatures(t, daemonCfg, map[string]bool{
		config.FeatureClipImage: true,
		config.FeatureClipText:  false,
		config.FeatureNotify:    true,
		config.FeatureExec:      false,
	})
	cliCfg := newTestConfig(t, "devbox")
	setFeatures(t, cliCfg, map[string]bool{
		config.FeatureClipImage: false,
		config.FeatureClipText:  true,
		config.FeatureNotify:    false,
		config.FeatureExec:      true,
	})
	d := startFakeDaemon(t, daemonCfg)
	a := newDaemonTestApp(t, d.path, cliCfg)

	var out, errw bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	before := d.featureReads()
	if err := runFeatures(ctx, &out, &errw, a, nil); err != nil {
		t.Fatalf("runFeatures list: %v", err)
	}

	// The daemon's posture, NOT the CLI store's inverted one.
	want := "clip-image: on\nclip-text: off\nnotify: on\nexec: off\n"
	if out.String() != want {
		t.Errorf("list output:\n--- got ---\n%s--- want ---\n%s", out.String(), want)
	}
	if errw.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errw.String())
	}
	// Direct proof the GET was served by the daemon: only a daemon round-trip
	// advances the wrapper's read count (the fallback reads a.Cfg, unwrapped).
	if d.featureReads() <= before {
		t.Errorf("daemon feature reads did not advance (%d -> %d); list fell through to the config.Store fallback instead of GET /v1/features", before, d.featureReads())
	}
}

// EC (features set, daemon up): `features clip-text off` PUTs through the daemon
// (writing the shared config.Store) and echoes only the changed line.
func TestFeatures_DaemonUp_SetWritesThroughDaemon(t *testing.T) {
	// The daemon writes daemonCfg; the CLI's fallback would write the SEPARATE
	// cliCfg (a.Cfg). Both start with clip-text on. This split is what makes the
	// two paths distinguishable: a real PUT mutates daemonCfg and leaves cliCfg
	// untouched, whereas any fall-through — even one where lc.SetFeature still
	// fired the PUT but the branch also ran a.Cfg.SetFeature — mutates cliCfg.
	daemonCfg := newTestConfig(t, "devbox")
	setFeatures(t, daemonCfg, map[string]bool{config.FeatureClipText: true})
	cliCfg := newTestConfig(t, "devbox")
	setFeatures(t, cliCfg, map[string]bool{config.FeatureClipText: true})
	d := startFakeDaemon(t, daemonCfg)
	a := newDaemonTestApp(t, d.path, cliCfg)

	var out, errw bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	before := d.featureWrites()
	if err := runFeatures(ctx, &out, &errw, a, []string{"clip-text", "off"}); err != nil {
		t.Fatalf("runFeatures set: %v", err)
	}

	// The daemon received exactly one PUT.
	if got := d.featureWrites() - before; got != 1 {
		t.Errorf("daemon feature writes advanced by %d, want 1; the set never reached PUT /v1/features", got)
	}
	// The daemon's store reflects the change...
	if daemonCfg.FeatureEnabled(config.FeatureClipText) {
		t.Error("PUT /v1/features did not disable clip-text in the daemon's config.Store")
	}
	// ...and the CLI's fallback store was NOT written. This is the decisive proof
	// the write went THROUGH the daemon rather than falling through to a.Cfg: any
	// fall-through would have flipped clip-text off here too.
	if !a.Cfg.FeatureEnabled(config.FeatureClipText) {
		t.Error("fallback config.Store was written; the set fell through to a.Cfg instead of PUT /v1/features")
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
	if want := "unknown feature: bogus (known: clip-image, clip-text, notify, exec)\n"; errw.String() != want {
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
		config.FeatureExec:      true,
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
	if want := "clip-image: off\nclip-text: on\nnotify: on\nexec: on\n"; out.String() != want {
		t.Errorf("fallback list:\n--- got ---\n%s--- want ---\n%s", out.String(), want)
	}
	if errw.Len() != 0 {
		t.Errorf("unexpected stderr on fallback list: %q", errw.String())
	}

	// SET falls back to writing a.Cfg directly.
	out.Reset()
	errw.Reset()
	if err := runFeatures(ctx, &out, &errw, a, []string{"exec", "off"}); err != nil {
		t.Fatalf("runFeatures set (down): %v", err)
	}
	if a.Cfg.FeatureEnabled(config.FeatureExec) {
		t.Error("fallback set did not disable exec in config.Store")
	}
	if want := "exec: off\n"; out.String() != want {
		t.Errorf("fallback set output = %q, want %q", out.String(), want)
	}
	if errw.Len() != 0 {
		t.Errorf("unexpected stderr on fallback set: %q", errw.String())
	}
}
