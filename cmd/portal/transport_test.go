package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
)

// The no-arg form prints the ACTIVE transport's Describe().Impl unconditionally
// (the canonical way to see the live transport) — not the config file value.
func TestRunTransport_NoArgPrintsActiveImpl(t *testing.T) {
	a := &app.App{
		Cfg:       config.New(t.TempDir()),
		Transport: nativeHealthTransport{up: true, pid: 0}, // Describe().Impl == native-ssh
	}
	var buf bytes.Buffer
	if err := runTransport(&buf, a, nil); err != nil {
		t.Fatalf("runTransport: %v", err)
	}
	if got := buf.String(); got != "native-ssh\n" {
		t.Errorf("no-arg transport = %q, want %q", got, "native-ssh\n")
	}
}

// `portal transport native`/`system` round-trips through SetTransport and notes
// the required daemon restart.
func TestRunTransport_SetRoundTrip(t *testing.T) {
	cfg := config.New(t.TempDir())
	a := &app.App{Cfg: cfg, Transport: nativeHealthTransport{up: true, pid: 0}}

	for _, name := range []string{"native", "system"} {
		var buf bytes.Buffer
		if err := runTransport(&buf, a, []string{name}); err != nil {
			t.Fatalf("runTransport(%q): %v", name, err)
		}
		got, err := cfg.Transport()
		if err != nil {
			t.Fatal(err)
		}
		if got != name {
			t.Errorf("after set %q, config Transport = %q", name, got)
		}
		if !strings.Contains(buf.String(), "restart") {
			t.Errorf("set %q output should note a daemon restart, got %q", name, buf.String())
		}
	}
}

// An invalid name is a usage error (and never touches the config file).
func TestRunTransport_InvalidNameIsUsageError(t *testing.T) {
	cfg := config.New(t.TempDir())
	a := &app.App{Cfg: cfg, Transport: nativeHealthTransport{up: true, pid: 0}}

	err := runTransport(io.Discard, a, []string{"localexec"})
	if err == nil {
		t.Fatal("invalid transport name should return a usage error")
	}
	if _, ok := err.(usageErr); !ok {
		t.Errorf("error type = %T, want usageErr", err)
	}
	// Config must remain the default (unwritten).
	if got, _ := cfg.Transport(); got != "system" {
		t.Errorf("config Transport = %q after rejected set, want system (unchanged)", got)
	}
}
