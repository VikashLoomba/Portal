package clipshim

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
// settings.json merge runs, and all three shell blocks are converged.
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
	if !strings.Contains(stdin, EarlyPathMarkerStart) {
		t.Error("expected the early PATH-prepend block to be written")
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
	// Every backup decision greps the version-INDEPENDENT ownership prefix so
	// an upgrade never mistakes an older portal shim for a user binary.
	for _, sh := range shims {
		if !strings.Contains(scripts, `grep -qF "Installed by portal" ~/.local/bin/`+sh.name) {
			t.Errorf("backup ownership grep for %s must use the unversioned prefix", sh.name)
		}
	}
	if strings.Contains(scripts, `! grep -qF "`+Marker) {
		t.Error("backup ownership grep must never key on the versioned Marker")
	}
	if !strings.HasPrefix(Marker, ownershipMarker) {
		t.Fatalf("ownershipMarker %q is not a prefix of Marker %q", ownershipMarker, Marker)
	}
}

// TestEnsure_FastPathWhenCurrent: when all shims already carry the current
// Marker, Ensure must NOT rewrite them (no shim ExecBytes), but must still
// ensure the notify hook plus all three shell blocks (idempotent convergence).
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
	// But all shell blocks + notify hook still run.
	if !strings.Contains(stdin, PathMarkerStart) {
		t.Error("fast path should still ensure the PATH-prepend block")
	}
	if !strings.Contains(stdin, EarlyPathMarkerStart) {
		t.Error("fast path should still ensure the early PATH-prepend block")
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
	if strings.Contains(probe, "clip-shim v6") {
		t.Fatal("current marker probe still accepts the pre-u10 v6 shims")
	}
}

// TestRemove_StripsEverything asserts Remove issues a script that removes the
// shims, the notify hook, the portald symlink, the env snippet, strips the
// early-PATH/PATH/SUDO_ASKPASS marker blocks, and strips the portal-managed
// settings.json hooks.
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
		EarlyPathMarkerStart,
		EarlyPathMarkerEnd,
		AskpassMarkerStart,
		AskpassMarkerEnd,
		"~/.bash_profile",
		"~/.bash_login",
		"PORTAL_MANAGED=1",
		"xdg-open xclip wl-paste portal portal-askpass sudo",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Remove script missing %q\nscript:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `grep -qF "Installed by portal" ~/.local/bin/"$bin".portal-backup`) {
		t.Error("Remove must classify backups with the unversioned ownership prefix")
	}
	if strings.Contains(got, Marker) {
		t.Error("Remove backup classification must not key on the versioned Marker")
	}
}

// TestEnsure_NotifyHookFailureDoesNotBlockPath: a failed settings.json merge
// must not abort Ensure (the headline clip PATH convergence takes priority). We
// inject a merge error and assert all shell blocks still converge.
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
	if !strings.Contains(tr.allStdin(), EarlyPathMarkerStart) {
		t.Fatal("early PATH-prepend must run even though the notify hook is best-effort")
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

func TestConditionalBashLoginRCFiles(t *testing.T) {
	for _, name := range []string{".bash_profile", ".bash_login"} {
		states := []struct {
			name   string
			exists bool
		}{
			{name: "exists", exists: true},
			{name: "absent"},
		}
		for _, state := range states {
			t.Run(name+"/"+state.name, func(t *testing.T) {
				home := t.TempDir()
				conditionalPath := filepath.Join(home, name)
				if state.exists {
					if err := os.WriteFile(conditionalPath, []byte("user login setup\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}

				for i := 0; i < 2; i++ {
					applyRecordedWrite(t, home, ensurePathPrepend)
					applyRecordedWrite(t, home, ensureAskpassEnv)
				}

				got, err := os.ReadFile(conditionalPath)
				if !state.exists {
					if !os.IsNotExist(err) {
						t.Fatalf("conditional rc was created: err=%v content=%q", err, got)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, marker := range []string{PathMarkerStart, AskpassMarkerStart} {
					if count := strings.Count(string(got), marker); count != 1 {
						t.Fatalf("%s count = %d, want 1; content=%q", marker, count, got)
					}
				}
				if !strings.HasPrefix(string(got), "user login setup\n") {
					t.Fatalf("conditional rc pre-existing content changed: %q", got)
				}
			})
		}
	}
}

func TestStandardRCFilesAreCreated(t *testing.T) {
	home := t.TempDir()
	applyRecordedWrite(t, home, ensurePathPrepend)
	applyRecordedWrite(t, home, ensureAskpassEnv)
	for _, name := range []string{".bashrc", ".zshrc", ".zshenv", ".profile"} {
		got, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatalf("%s was not created: %v", name, err)
		}
		for _, marker := range []string{PathMarkerStart, AskpassMarkerStart} {
			if !bytes.Contains(got, []byte(marker)) {
				t.Errorf("%s missing %s", name, marker)
			}
		}
	}
}

func TestEarlyPathPrependPlacementIdempotencyAndMetadata(t *testing.T) {
	home := t.TempDir()
	rc := filepath.Join(home, ".bashrc")
	original := []byte("# distro setup\ncase $- in\n*i*) ;;\n*) return;;\nesac\n")
	if err := os.WriteFile(rc, original, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}

	applyRecordedWrite(t, home, ensureEarlyPathPrepend)
	applyRecordedWrite(t, home, ensureEarlyPathPrepend)

	after, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("early prepend replaced .bashrc instead of truncate-writing its inode")
	}
	if got, want := after.Mode().Perm(), before.Mode().Perm(); got != want {
		t.Fatalf(".bashrc permissions = %v, want preserved %v", got, want)
	}
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	want := earlyPathPrependSnippet + "\n\n" + string(original)
	if string(got) != want {
		t.Fatalf(".bashrc content = %q, want early block before original content %q", got, want)
	}
	if count := strings.Count(string(got), EarlyPathMarkerStart); count != 1 {
		t.Fatalf("early marker count = %d, want 1", count)
	}
}

func TestEarlyPathPrependCreatesMissingBashrc(t *testing.T) {
	home := t.TempDir()
	applyRecordedWrite(t, home, ensureEarlyPathPrepend)
	rc := filepath.Join(home, ".bashrc")
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if want := earlyPathPrependSnippet + "\n\n"; string(got) != want {
		t.Fatalf("new .bashrc content = %q, want %q", got, want)
	}
}

func TestEarlyPathMarkersAndDedupLogicAreStable(t *testing.T) {
	if EarlyPathMarkerStart != "# >>> portal PATH early (non-interactive) >>>" {
		t.Fatalf("EarlyPathMarkerStart = %q", EarlyPathMarkerStart)
	}
	if EarlyPathMarkerEnd != "# <<< portal PATH early (non-interactive) <<<" {
		t.Fatalf("EarlyPathMarkerEnd = %q", EarlyPathMarkerEnd)
	}
	pathLine := `PATH="$HOME/.local/bin:$(printf '%s' "$PATH" | tr ':' '\n' | grep -vxF "$HOME/.local/bin" | paste -sd: -)"`
	for name, snippet := range map[string]string{
		"bottom": pathPrependSnippet,
		"early":  earlyPathPrependSnippet,
	} {
		if !strings.Contains(snippet, pathLine) {
			t.Errorf("%s PATH block does not contain shared dedup-prepend logic", name)
		}
	}
}

func TestRemoveStripsAllBlocksFromAllRCFiles(t *testing.T) {
	home := t.TempDir()
	names := []string{".bashrc", ".zshrc", ".zshenv", ".profile", ".bash_profile", ".bash_login"}
	before := make(map[string]os.FileInfo, len(names))
	for _, name := range names {
		content := "keep before\n" + earlyPathPrependSnippet + "\nkeep early after\n" +
			pathPrependSnippet + "\nkeep path after\n" + askpassEnvSnippet +
			"\n. \"$HOME/.config/portal/env.sh\"\nkeep after\n"
		path := filepath.Join(home, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = info
	}

	tr := &recordTransport{}
	Remove(context.Background(), tr)
	if len(tr.execScripts) != 1 {
		t.Fatalf("Remove recorded %d scripts, want 1", len(tr.execScripts))
	}
	runRecordedShell(t, home, tr.execScripts[0], nil)

	for _, name := range names {
		path := filepath.Join(home, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, removed := range []string{
			EarlyPathMarkerStart, EarlyPathMarkerEnd,
			PathMarkerStart, PathMarkerEnd,
			AskpassMarkerStart, AskpassMarkerEnd,
			"portal/env.sh",
		} {
			if bytes.Contains(got, []byte(removed)) {
				t.Errorf("%s still contains removed text %q: %q", name, removed, got)
			}
		}
		for _, kept := range []string{"keep before", "keep early after", "keep path after", "keep after"} {
			if !bytes.Contains(got, []byte(kept)) {
				t.Errorf("%s lost user text %q: %q", name, kept, got)
			}
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before[name], after) {
			t.Errorf("Remove replaced %s instead of truncate-writing it", name)
		}
		if gotMode, wantMode := after.Mode().Perm(), before[name].Mode().Perm(); gotMode != wantMode {
			t.Errorf("Remove mode for %s = %v, want %v", name, gotMode, wantMode)
		}
	}
}

// TestDeployShimBackupOwnership runs the recorded backup script against a real
// HOME: a portal shim of ANY version is never backed up, a genuine user binary
// is, and an existing backup is never clobbered.
func TestDeployShimBackupOwnership(t *testing.T) {
	stale := "#!/bin/sh\n# Installed by portal clip-shim v6. stale shim\n"
	user := "#!/bin/sh\necho user sudo\n"
	type backupCase struct {
		name             string
		tool             string
		script           string
		existing         string
		backup           string
		wantBackup       string
		wantBackupExists bool
	}
	var tests []backupCase
	for _, shim := range shims {
		tests = append(tests, backupCase{
			name:     "older portal " + shim.name + " is never backed up",
			tool:     shim.name,
			script:   shim.script,
			existing: stale,
		})
	}
	tests = append(tests,
		backupCase{
			name:     "legacy unversioned xdg-open is never backed up",
			tool:     "xdg-open",
			script:   XDGOpenWrapper,
			existing: "#!/bin/sh\n# Installed by portal. legacy wrapper\n",
		},
		backupCase{
			name:             "user binary is backed up",
			tool:             "sudo",
			script:           sudoShim,
			existing:         user,
			wantBackup:       user,
			wantBackupExists: true,
		},
		backupCase{
			name:             "existing backup is not clobbered",
			tool:             "sudo",
			script:           sudoShim,
			existing:         user,
			backup:           "original",
			wantBackup:       "original",
			wantBackupExists: true,
		},
	)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			binDir := filepath.Join(home, ".local", "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(binDir, tc.tool), []byte(tc.existing), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.backup != "" {
				if err := os.WriteFile(filepath.Join(binDir, tc.tool+".portal-backup"), []byte(tc.backup), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			tr := &recordTransport{reply: map[string]string{"&& echo ok": "ok"}}
			if err := deployShim(context.Background(), tr, tc.tool, tc.script); err != nil {
				t.Fatal(err)
			}
			if len(tr.execScripts) == 0 {
				t.Fatal("no backup script recorded")
			}
			runRecordedShell(t, home, tr.execScripts[0], nil)

			got, err := os.ReadFile(filepath.Join(binDir, tc.tool+".portal-backup"))
			if !tc.wantBackupExists {
				if err == nil {
					t.Fatalf("backup created for a portal-owned shim: %q", got)
				}
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("read absent backup: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.wantBackup {
				t.Fatalf("backup content = %q, want %q", got, tc.wantBackup)
			}
		})
	}
}

// TestRemoveDeletesPortalOwnedBackups runs the real Remove script against a
// HOME whose backup slots hold a mix of a stale portal shim (polluted by an
// older release's versioned backup grep) and a genuine user binary: only the
// user binary is restored; the polluted slot is deleted with its shim.
func TestRemoveDeletesPortalOwnedBackups(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\n# " + Marker + ". current shim\n"
	stale := "#!/bin/sh\n# Installed by portal clip-shim v6. stale shim\n"
	user := "#!/bin/sh\necho user portal\n"
	files := map[string]string{
		"sudo":                 shim,
		"sudo.portal-backup":   stale,
		"portal":               shim,
		"portal.portal-backup": user,
		"xclip":                shim,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	tr := &recordTransport{}
	Remove(context.Background(), tr)
	if len(tr.execScripts) != 1 {
		t.Fatalf("Remove recorded %d scripts, want 1", len(tr.execScripts))
	}
	runRecordedShell(t, home, tr.execScripts[0], nil)

	for _, gone := range []string{"sudo", "sudo.portal-backup", "portal.portal-backup", "xclip"} {
		if _, err := os.Stat(filepath.Join(binDir, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should be gone after Remove (err=%v)", gone, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(binDir, "portal"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != user {
		t.Fatalf("portal = %q, want restored user binary %q", got, user)
	}
}

func applyRecordedWrite(t *testing.T, home string, fn func(context.Context, transport.Transport) error) {
	t.Helper()
	tr := &recordTransport{}
	if err := fn(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if len(tr.execBytesScripts) != 1 || len(tr.execBytesStdin) != 1 {
		t.Fatalf("recorded writes = %d scripts/%d payloads, want 1/1", len(tr.execBytesScripts), len(tr.execBytesStdin))
	}
	runRecordedShell(t, home, tr.execBytesScripts[0], tr.execBytesStdin[0])
}

func runRecordedShell(t *testing.T, home, joined string, stdin []byte) {
	t.Helper()
	cmd := exec.Command("sh", "-c", joined)
	cmd.Dir = home
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "HOME=") &&
			!strings.HasPrefix(entry, "BASH_ENV=") &&
			!strings.HasPrefix(entry, "ENV=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run %q: %v (out=%q)", joined, err, out)
	}
}
