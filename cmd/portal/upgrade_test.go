package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/internal/upgrade"
)

// upgradeFixture stands up a release server plus an installed-binary path so
// each test exercises the real runUpgrade flow end to end.
type upgradeFixture struct {
	deps      upgradeDeps
	binPath   string
	restarted int
	out       *bytes.Buffer
}

const installedMarker = "portal v0.4.1 (commit installed)"

func newUpgradeFixture(t *testing.T, tag string, assetBody string, withAsset bool) *upgradeFixture {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		assets := "[]"
		if withAsset {
			assets = `[{"name":"` + upgrade.AssetName + `","browser_download_url":"` + srv.URL + `/asset"}]`
		}
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `","assets":` + assets + `}`))
	})
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(assetBody))
	})

	dir := t.TempDir()
	binPath := filepath.Join(dir, "portal")
	if err := os.WriteFile(binPath, []byte(installedMarker), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &upgradeFixture{binPath: binPath, out: &bytes.Buffer{}}
	f.deps = upgradeDeps{
		Releases: &upgrade.Client{APIBase: srv.URL, Repo: "owner/repo"},
		BinPath:  binPath,
		Current:  "v0.4.1",
		GOOS:     "darwin",
		GOARCH:   "arm64",
		Restart: func(context.Context) error {
			f.restarted++
			return nil
		},
		// Default verifier: the downloaded bytes ARE the reported version line,
		// so a test controls the self-test purely through the asset body.
		Verify: func(_ context.Context, path string) (string, error) {
			b, err := os.ReadFile(path)
			return string(b), err
		},
	}
	return f
}

// setInstalled rewrites the installed binary so it reports `version`, and
// returns the exact bytes written so a test can assert it stayed untouched.
func (f *upgradeFixture) setInstalled(t *testing.T, version string) string {
	t.Helper()
	line := "portal " + version + " (commit installed)"
	if err := os.WriteFile(f.binPath, []byte(line), 0o755); err != nil {
		t.Fatal(err)
	}
	return line
}

// installedContents reports what currently sits at the installed binary path,
// so every failure test can prove the working binary survived.
func (f *upgradeFixture) installedContents(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.binPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertNoTempLeftBehind proves the staging file never outlives the run.
func (f *upgradeFixture) assertNoTempLeftBehind(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(f.binPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".portal") {
			t.Fatalf("staging file %q left behind", e.Name())
		}
	}
}

func TestUpgrade_HappyPath(t *testing.T) {
	f := newUpgradeFixture(t, "v0.7.0", "portal v0.7.0 (commit abc)", true)

	if err := runUpgrade(context.Background(), f.out, f.deps, false, false); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if got := f.installedContents(t); got != "portal v0.7.0 (commit abc)" {
		t.Fatalf("installed binary = %q, want the downloaded asset", got)
	}
	if f.restarted != 1 {
		t.Fatalf("restarts = %d, want exactly 1", f.restarted)
	}
	out := f.out.String()
	for _, want := range []string{"downloading", "v0.7.0", "daemon reloaded"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q missing %q", out, want)
		}
	}
	f.assertNoTempLeftBehind(t)
}

func TestUpgrade_UpToDateDoesNotDownload(t *testing.T) {
	f := newUpgradeFixture(t, "v0.7.0", "portal v0.7.0", true)
	current := f.setInstalled(t, "v0.7.0")

	if err := runUpgrade(context.Background(), f.out, f.deps, false, false); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if got := f.installedContents(t); got != current {
		t.Fatalf("installed binary = %q, want it untouched", got)
	}
	if f.restarted != 0 {
		t.Fatalf("restarts = %d, want 0 when already current", f.restarted)
	}
	if !strings.Contains(f.out.String(), "up to date") {
		t.Fatalf("output = %q, want an up-to-date line", f.out.String())
	}
}

// A git-describe build sits AFTER its base tag; upgrading it would move the
// binary backwards, so it must be reported as current.
func TestUpgrade_DescribeBuildAheadOfTagIsCurrent(t *testing.T) {
	f := newUpgradeFixture(t, "v0.7.0", "portal v0.7.0", true)
	current := f.setInstalled(t, "v0.7.0-4-gdeadbee")

	if err := runUpgrade(context.Background(), f.out, f.deps, false, false); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if got := f.installedContents(t); got != current {
		t.Fatalf("installed binary = %q, want it untouched", got)
	}
	if f.restarted != 0 {
		t.Fatalf("restarts = %d, want 0", f.restarted)
	}
}

func TestUpgrade_ForceReinstallsCurrentVersion(t *testing.T) {
	f := newUpgradeFixture(t, "v0.7.0", "portal v0.7.0 fresh", true)
	f.setInstalled(t, "v0.7.0")

	if err := runUpgrade(context.Background(), f.out, f.deps, false, true); err != nil {
		t.Fatalf("runUpgrade --force: %v", err)
	}
	if got := f.installedContents(t); got != "portal v0.7.0 fresh" {
		t.Fatalf("installed binary = %q, want the re-downloaded asset", got)
	}
	if f.restarted != 1 {
		t.Fatalf("restarts = %d, want 1", f.restarted)
	}
}

func TestUpgrade_CheckOnlyReportsWithoutInstalling(t *testing.T) {
	f := newUpgradeFixture(t, "v0.7.0", "portal v0.7.0", true)

	if err := runUpgrade(context.Background(), f.out, f.deps, true, false); err != nil {
		t.Fatalf("runUpgrade --check: %v", err)
	}
	if got := f.installedContents(t); got != installedMarker {
		t.Fatalf("installed binary = %q, want it untouched by --check", got)
	}
	if f.restarted != 0 {
		t.Fatalf("restarts = %d, want 0 for --check", f.restarted)
	}
	if !strings.Contains(f.out.String(), "update available") {
		t.Fatalf("output = %q, want an update-available line", f.out.String())
	}
}

func TestUpgrade_CheckOnlyWhenCurrent(t *testing.T) {
	f := newUpgradeFixture(t, "v0.7.0", "portal v0.7.0", true)
	f.setInstalled(t, "v0.7.0")

	if err := runUpgrade(context.Background(), f.out, f.deps, true, false); err != nil {
		t.Fatalf("runUpgrade --check: %v", err)
	}
	if !strings.Contains(f.out.String(), "up to date") {
		t.Fatalf("output = %q, want an up-to-date line", f.out.String())
	}
}

// The core safety property: a download that fails its self-test must never
// replace the working binary, and must not reload the daemon.
func TestUpgrade_FailedSelfTestKeepsInstalledBinary(t *testing.T) {
	f := newUpgradeFixture(t, "v0.7.0", "corrupt", true)
	f.deps.Verify = func(context.Context, string) (string, error) {
		return "", errors.New("exec format error")
	}

	err := runUpgrade(context.Background(), f.out, f.deps, false, false)
	if err == nil || !strings.Contains(err.Error(), "self-test") {
		t.Fatalf("err = %v, want a self-test failure", err)
	}
	if got := f.installedContents(t); got != installedMarker {
		t.Fatalf("installed binary = %q, want the working binary preserved", got)
	}
	if f.restarted != 0 {
		t.Fatalf("restarts = %d, want 0 after a failed self-test", f.restarted)
	}
	f.assertNoTempLeftBehind(t)
}

// A binary that runs but reports a different version than the release claimed
// is equally untrustworthy — a mismatched or substituted asset.
func TestUpgrade_VersionMismatchKeepsInstalledBinary(t *testing.T) {
	f := newUpgradeFixture(t, "v0.7.0", "portal v0.3.0 (wrong asset)", true)

	err := runUpgrade(context.Background(), f.out, f.deps, false, false)
	if err == nil || !strings.Contains(err.Error(), "expected v0.7.0") {
		t.Fatalf("err = %v, want a version-mismatch error", err)
	}
	if got := f.installedContents(t); got != installedMarker {
		t.Fatalf("installed binary = %q, want the working binary preserved", got)
	}
	if f.restarted != 0 {
		t.Fatalf("restarts = %d, want 0 after a mismatch", f.restarted)
	}
	f.assertNoTempLeftBehind(t)
}

// A reload failure happened AFTER the swap: the message must say the upgrade
// landed rather than implying it did not.
func TestUpgrade_RestartFailureReportsBinaryAlreadyUpgraded(t *testing.T) {
	f := newUpgradeFixture(t, "v0.7.0", "portal v0.7.0", true)
	f.deps.Restart = func(context.Context) error { return errors.New("launchctl said no") }

	err := runUpgrade(context.Background(), f.out, f.deps, false, false)
	if err == nil || !strings.Contains(err.Error(), "upgraded to v0.7.0") {
		t.Fatalf("err = %v, want an upgraded-but-reload-failed error", err)
	}
	if got := f.installedContents(t); got != "portal v0.7.0" {
		t.Fatalf("installed binary = %q, want the new binary in place", got)
	}
}

func TestUpgrade_UnsupportedHostRefusesBeforeAnyNetwork(t *testing.T) {
	f := newUpgradeFixture(t, "v0.7.0", "portal v0.7.0", true)
	// A bogus API base proves the host check short-circuits before any request.
	f.deps.Releases = &upgrade.Client{APIBase: "http://127.0.0.1:0", Repo: "owner/repo"}
	f.deps.GOOS, f.deps.GOARCH = "linux", "amd64"

	err := runUpgrade(context.Background(), f.out, f.deps, false, false)
	if err == nil || !strings.Contains(err.Error(), "linux/amd64") {
		t.Fatalf("err = %v, want a host-unsupported error", err)
	}
}

func TestUpgrade_MissingInstalledBinaryPointsAtInstall(t *testing.T) {
	f := newUpgradeFixture(t, "v0.7.0", "portal v0.7.0", true)
	if err := os.Remove(f.binPath); err != nil {
		t.Fatal(err)
	}

	err := runUpgrade(context.Background(), f.out, f.deps, false, false)
	if err == nil || !strings.Contains(err.Error(), "install") {
		t.Fatalf("err = %v, want guidance to run install first", err)
	}
	if f.restarted != 0 {
		t.Fatalf("restarts = %d, want 0", f.restarted)
	}
}

func TestUpgrade_ReleaseLookupFailurePropagates(t *testing.T) {
	f := newUpgradeFixture(t, "v0.7.0", "portal v0.7.0", false) // no asset published

	err := runUpgrade(context.Background(), f.out, f.deps, false, false)
	if err == nil || !strings.Contains(err.Error(), upgrade.AssetName) {
		t.Fatalf("err = %v, want a missing-asset error", err)
	}
	if got := f.installedContents(t); got != installedMarker {
		t.Fatalf("installed binary = %q, want it untouched", got)
	}
}

// verifyUpgradeBinary is the production self-test; prove it actually executes
// the candidate and surfaces both its output and its failures.
func TestVerifyUpgradeBinary(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("#!/bin/sh\necho \"portal v0.7.0 (commit abc)\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	line, err := verifyUpgradeBinary(context.Background(), good)
	if err != nil {
		t.Fatalf("verifyUpgradeBinary: %v", err)
	}
	if !strings.Contains(line, "v0.7.0") {
		t.Fatalf("line = %q, want the version", line)
	}

	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyUpgradeBinary(context.Background(), bad); err == nil {
		t.Fatal("a non-zero exit must be an error")
	}

	notExec := filepath.Join(dir, "not-exec")
	if err := os.WriteFile(notExec, []byte("plain data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyUpgradeBinary(context.Background(), notExec); err == nil {
		t.Fatal("a non-executable file must be an error")
	}
}

// REGRESSION: upgrade must compare the INSTALLED binary — the one the daemon
// executes — not the process invoking the command. A freshly built ./portal
// from a checkout run against a stale install previously reported "up to date"
// and left the daemon on old code.
func TestUpgrade_ComparesInstalledBinaryNotTheInvoker(t *testing.T) {
	f := newUpgradeFixture(t, "v0.8.0", "portal v0.8.0 (commit new)", true)
	f.setInstalled(t, "v0.4.1")
	// The invoking process is already newer than the release; only the stale
	// installed binary should drive the decision.
	f.deps.Current = "v0.8.0"

	if err := runUpgrade(context.Background(), f.out, f.deps, false, false); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if got := f.installedContents(t); got != "portal v0.8.0 (commit new)" {
		t.Fatalf("installed binary = %q, want the stale install replaced", got)
	}
	if f.restarted != 1 {
		t.Fatalf("restarts = %d, want 1", f.restarted)
	}
}

// --check must likewise report the installed version, not the invoker's.
func TestUpgrade_CheckReportsInstalledVersion(t *testing.T) {
	f := newUpgradeFixture(t, "v0.8.0", "portal v0.8.0", true)
	f.setInstalled(t, "v0.4.1")
	f.deps.Current = "v0.8.0"

	if err := runUpgrade(context.Background(), f.out, f.deps, true, false); err != nil {
		t.Fatalf("runUpgrade --check: %v", err)
	}
	out := f.out.String()
	if !strings.Contains(out, "update available") || !strings.Contains(out, "v0.4.1") {
		t.Fatalf("output = %q, want an update-available line naming the installed v0.4.1", out)
	}
}

// An installed binary that refuses to run must not abort the upgrade that
// would repair it: fall back to the running process's version and proceed.
func TestUpgrade_UnreadableInstalledBinaryFallsBackAndStillUpgrades(t *testing.T) {
	f := newUpgradeFixture(t, "v0.8.0", "portal v0.8.0 (commit new)", true)
	f.deps.Current = "v0.4.1"
	realVerify := f.deps.Verify
	f.deps.Verify = func(ctx context.Context, path string) (string, error) {
		if path == f.binPath {
			return "", errors.New("exec format error")
		}
		return realVerify(ctx, path)
	}

	if err := runUpgrade(context.Background(), f.out, f.deps, false, false); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if got := f.installedContents(t); got != "portal v0.8.0 (commit new)" {
		t.Fatalf("installed binary = %q, want the broken install replaced", got)
	}
}

func TestParseVersionField(t *testing.T) {
	tests := []struct{ line, want string }{
		{"portal v0.8.0 (commit abc)", "v0.8.0"},
		{"portal v0.8.0\n", "v0.8.0"},
		{"  portal v0.8.0  ", "v0.8.0"},
		{"portald v0.8.0", ""},
		{"portal", ""},
		{"", ""},
		{"garbage output", ""},
	}
	for _, tt := range tests {
		if got := parseVersionField(tt.line); got != tt.want {
			t.Errorf("parseVersionField(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}
