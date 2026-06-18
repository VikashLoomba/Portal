//go:build darwin

package service

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/clock"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/run"
)

//go:embed plist.tmpl
var plistTmpl string

var plistTemplate = template.Must(template.New("plist").Parse(plistTmpl))

// Launchd is the macOS Manager implementation. Every launchctl/plutil call
// goes through a Runner so the bootout-poll behavior can be unit-tested.
type Launchd struct {
	S      Spec
	Runner run.Runner
	Clk    clock.Clock
}

func New(s Spec, r run.Runner, c clock.Clock) *Launchd {
	return &Launchd{S: s, Runner: r, Clk: c}
}

func (m *Launchd) domainLabel() string { return m.S.Domain + "/" + m.S.Label }

// IsLoaded returns true iff `launchctl print <domain>/<label>` exits 0.
func (m *Launchd) IsLoaded(ctx context.Context) (bool, error) {
	_, _, code, err := m.Runner.Run(ctx, "launchctl", []string{"print", m.domainLabel()}, "")
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

// Status: same launchctl print, then grep the 4 indented lines starting with
// state/pid/runs/last exit code, indenting them with two spaces to match the
// bash formatter (so the `status` command output is byte-identical).
func (m *Launchd) Status(ctx context.Context) (Status, error) {
	stdout, _, code, err := m.Runner.Run(ctx, "launchctl", []string{"print", m.domainLabel()}, "")
	if err != nil {
		return Status{}, err
	}
	if code != 0 {
		return Status{Loaded: false}, nil
	}
	var lines []string
	for _, line := range strings.Split(stdout, "\n") {
		trim := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
			continue
		}
		switch {
		case strings.HasPrefix(trim, "state ="),
			strings.HasPrefix(trim, "pid ="),
			strings.HasPrefix(trim, "runs ="),
			strings.HasPrefix(trim, "last exit code ="):
			lines = append(lines, "  "+trim)
		}
		if len(lines) == 4 {
			break
		}
	}
	return Status{Loaded: true, StateLines: lines}, nil
}

// writePlist renders the embedded template into PlistPath and validates it.
// Bash explicitly `chmod 644` after every write, so we do the same — Go's
// O_CREATE|O_TRUNC only honors the mode arg on file CREATION, leaving stale
// 0o600 perms in place if the plist already exists.
func (m *Launchd) writePlist(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(m.S.Plist), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(m.S.Plist, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := plistTemplate.Execute(f, m.S); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(m.S.Plist, 0o644); err != nil {
		return err
	}
	_, _, code, err := m.Runner.Run(ctx, "plutil", []string{"-lint", m.S.Plist}, "")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("plutil -lint failed for %s", m.S.Plist)
	}
	return nil
}

// bootoutWait is the async-race fix: bootout returns BEFORE launchd actually
// drops the label, so an immediate bootstrap fails with "Input/output error".
// Poll up to 10×300ms = ~3s for the label to be gone, then proceed regardless
// (matches bash: best-effort, return 0).
func (m *Launchd) bootoutWait(ctx context.Context) {
	_, _, _, _ = m.Runner.Run(ctx, "launchctl", []string{"bootout", m.domainLabel()}, "")
	for i := 0; i < 10; i++ {
		loaded, _ := m.IsLoaded(ctx)
		if !loaded {
			return
		}
		m.Clk.Sleep(ctx, 300*time.Millisecond)
	}
}

// bootstrapWait rides out a still-finishing teardown by retrying up to 5
// times at 500ms intervals, then a final attempt that surfaces the error.
func (m *Launchd) bootstrapWait(ctx context.Context) error {
	var lastStderr string
	for i := 0; i < 5; i++ {
		_, stderr, code, err := m.Runner.Run(ctx, "launchctl",
			[]string{"bootstrap", m.S.Domain, m.S.Plist}, "")
		if err == nil && code == 0 {
			return nil
		}
		lastStderr = stderr
		m.Clk.Sleep(ctx, 500*time.Millisecond)
	}
	_, stderr, code, err := m.Runner.Run(ctx, "launchctl",
		[]string{"bootstrap", m.S.Domain, m.S.Plist}, "")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("bootstrap failed: %s", strings.TrimSpace(firstNonEmpty(stderr, lastStderr)))
	}
	return nil
}

func firstNonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}

func (m *Launchd) Install(ctx context.Context) error {
	if err := m.writePlist(ctx); err != nil {
		return err
	}
	m.bootoutWait(ctx)
	if err := m.bootstrapWait(ctx); err != nil {
		return err
	}
	_, _, _, _ = m.Runner.Run(ctx, "launchctl", []string{"enable", m.domainLabel()}, "")
	_, _, _, _ = m.Runner.Run(ctx, "launchctl", []string{"kickstart", m.domainLabel()}, "")
	return nil
}

func (m *Launchd) Uninstall(ctx context.Context) error {
	_, _, _, _ = m.Runner.Run(ctx, "launchctl", []string{"bootout", m.domainLabel()}, "")
	_ = os.Remove(m.S.Plist)
	return nil
}

func (m *Launchd) Reload(ctx context.Context) error {
	if err := m.writePlist(ctx); err != nil {
		return err
	}
	m.bootoutWait(ctx)
	if err := m.bootstrapWait(ctx); err != nil {
		return err
	}
	_, _, _, _ = m.Runner.Run(ctx, "launchctl", []string{"kickstart", "-k", m.domainLabel()}, "")
	return nil
}

func (m *Launchd) Start(ctx context.Context) error {
	loaded, _ := m.IsLoaded(ctx)
	if loaded {
		_, _, _, _ = m.Runner.Run(ctx, "launchctl", []string{"kickstart", m.domainLabel()}, "")
		return nil
	}
	if _, err := os.Stat(m.S.Plist); err != nil {
		return fmt.Errorf("not installed; run install first")
	}
	// Bash runs bootstrap → enable → kickstart unconditionally (set -e is
	// OFF); a transient bootstrap failure (e.g. "Input/output error" during
	// a still-finishing teardown) does NOT abort the sequence and the user
	// still sees "started (LABEL)". Match that.
	_, _, _, _ = m.Runner.Run(ctx, "launchctl", []string{"bootstrap", m.S.Domain, m.S.Plist}, "")
	_, _, _, _ = m.Runner.Run(ctx, "launchctl", []string{"enable", m.domainLabel()}, "")
	_, _, _, _ = m.Runner.Run(ctx, "launchctl", []string{"kickstart", m.domainLabel()}, "")
	return nil
}

func (m *Launchd) Stop(ctx context.Context) error {
	m.bootoutWait(ctx)
	return nil
}

func (m *Launchd) Restart(ctx context.Context) error {
	loaded, _ := m.IsLoaded(ctx)
	if loaded {
		_, _, _, _ = m.Runner.Run(ctx, "launchctl", []string{"kickstart", "-k", m.domainLabel()}, "")
		return nil
	}
	return m.Start(ctx)
}

var _ Manager = (*Launchd)(nil)
