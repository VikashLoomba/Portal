package app

import (
	"context"
	"errors"
	"testing"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/transport"
)

// errTransport is the graceful-degradation placeholder NewProd installs when the
// selection-aware factory cannot build the configured transport (e.g. `native`
// selected but the host is an ssh alias native cannot resolve). It must surface
// the construction error on every box-touching operation — so the failure is
// LOUD, never a silent fallback — while Health degrades to a clean DOWN (no
// error) so `portal status`/doctor render instead of crashing, and Close is a
// harmless no-op. This is what keeps the CLI's own recovery commands
// (`transport system`, `host`) usable after a bad selection.
func TestErrTransport_SurfacesErrorButStaysConstructable(t *testing.T) {
	sentinel := errors.New("sshnative: target \"mybox\" missing user; expected user@host[:port]")
	et := errTransport{err: sentinel}
	ctx := context.Background()

	if _, err := et.Ensure(ctx); !errors.Is(err, sentinel) {
		t.Errorf("Ensure err = %v, want the construction error surfaced", err)
	}
	if _, _, err := et.Exec(ctx, nil, "echo", "hi"); !errors.Is(err, sentinel) {
		t.Errorf("Exec err = %v, want the construction error surfaced", err)
	}
	if _, _, _, _, err := et.Stream(ctx, "echo"); !errors.Is(err, sentinel) {
		t.Errorf("Stream err = %v, want the construction error surfaced", err)
	}
	if err := et.Forward(ctx, 1, 2); !errors.Is(err, sentinel) {
		t.Errorf("Forward err = %v, want the construction error surfaced", err)
	}
	if err := et.Cancel(ctx, 1, 2); !errors.Is(err, sentinel) {
		t.Errorf("Cancel err = %v, want the construction error surfaced", err)
	}
	if _, err := et.ListForwards(ctx); !errors.Is(err, sentinel) {
		t.Errorf("ListForwards err = %v, want the construction error surfaced", err)
	}
	if _, err := et.ForwardLines(ctx); !errors.Is(err, sentinel) {
		t.Errorf("ForwardLines err = %v, want the construction error surfaced", err)
	}

	// Health must NOT itself error (status/doctor ignore its error and read Up):
	// it reports DOWN with the cause as Detail so those commands render cleanly.
	h, err := et.Health(ctx)
	if err != nil {
		t.Errorf("Health err = %v, want nil (status/doctor must render, not crash)", err)
	}
	if h.Up {
		t.Error("Health.Up = true, want false for an unavailable transport")
	}
	if h.Detail != sentinel.Error() {
		t.Errorf("Health.Detail = %q, want the construction error as the cause", h.Detail)
	}

	// Close is a harmless no-op (nothing was ever brought up).
	if stopped, err := et.Close(ctx); err != nil || stopped {
		t.Errorf("Close = (%v, %v), want (false, nil)", stopped, err)
	}

	// Describe surfaces an "unavailable" impl so `portal transport` (no-arg) and
	// status show the degraded state rather than a bogus live transport.
	if got := et.Describe().Impl; got != "unavailable" {
		t.Errorf("Describe().Impl = %q, want unavailable", got)
	}
}

// A construction failure must not stop the App from carrying a working transport
// value: errTransport satisfies BOTH interfaces, so the NewProd wiring
// (tr, pf = et, et) type-checks and every consumer keeps a non-nil transport.
func TestErrTransport_SatisfiesBothInterfaces(t *testing.T) {
	var _ transport.Transport = errTransport{err: errors.New("x")}
	var _ transport.PortForwarder = errTransport{err: errors.New("x")}
}
