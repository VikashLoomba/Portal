package app

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/VikashLoomba/Portal/internal/transport"
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

// transportOrErr is the exact wiring NewProd uses to join the selection-aware
// factory to errTransport. This locks that wiring in place: a regression to
// `return nil, err` (or dropping the errTransport install) in NewProd would
// remove this helper's error branch and fail here — closing the gap where
// errTransport's behavior and the factory's error production were each tested
// but never the join that keeps config-only recovery commands usable.
func TestTransportOrErr_Wiring(t *testing.T) {
	t.Run("factory error yields an errTransport pair", func(t *testing.T) {
		sentinel := errors.New("sshnative: target \"mybox\" missing user")
		tr, pf := transportOrErr(nil, nil, sentinel)
		if tr == nil || pf == nil {
			t.Fatal("a factory error must still yield a non-nil transport/forwarder pair")
		}
		// Same errTransport value backs both — Describe reports the degraded impl.
		if got := tr.Describe().Impl; got != "unavailable" {
			t.Errorf("Describe().Impl = %q, want unavailable", got)
		}
		ctx := context.Background()
		if _, _, err := tr.Exec(ctx, nil, "echo", "hi"); !errors.Is(err, sentinel) {
			t.Errorf("Exec err = %v, want the construction error surfaced", err)
		}
		if _, err := tr.Ensure(ctx); !errors.Is(err, sentinel) {
			t.Errorf("Ensure err = %v, want the construction error surfaced", err)
		}
		if err := pf.Forward(ctx, 1, 2); !errors.Is(err, sentinel) {
			t.Errorf("Forward err = %v, want the construction error surfaced", err)
		}
		// Health stays a clean DOWN so status/doctor render rather than crash.
		h, err := tr.Health(ctx)
		if err != nil {
			t.Errorf("Health err = %v, want nil", err)
		}
		if h.Up {
			t.Error("Health.Up = true, want false for an unavailable transport")
		}
	})

	t.Run("nil error passes the real pair through untouched", func(t *testing.T) {
		realTr := okTransport{}
		tr, pf := transportOrErr(realTr, realTr, nil)
		if got := tr.Describe().Impl; got != "ok-fake" {
			t.Errorf("Describe().Impl = %q, want the passed-through transport (ok-fake)", got)
		}
		if pf == nil {
			t.Fatal("nil error must pass the forwarder through, got nil")
		}
	})
}

// okTransport is a healthy no-op transport/forwarder used to prove transportOrErr
// passes a successfully-built pair through unchanged on a nil error.
type okTransport struct{}

var (
	_ transport.Transport     = okTransport{}
	_ transport.PortForwarder = okTransport{}
)

func (okTransport) Ensure(context.Context) (bool, error) { return true, nil }
func (okTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true}, nil
}
func (okTransport) Exec(context.Context, []byte, ...string) (string, string, error) {
	return "", "", nil
}
func (okTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, nil, nil
}
func (okTransport) Close(context.Context) (bool, error)            { return false, nil }
func (okTransport) Describe() transport.Desc                       { return transport.Desc{Impl: "ok-fake"} }
func (okTransport) Forward(context.Context, int, int) error        { return nil }
func (okTransport) Cancel(context.Context, int, int) error         { return nil }
func (okTransport) ListForwards(context.Context) ([]int, error)    { return nil, nil }
func (okTransport) ForwardLines(context.Context) ([]string, error) { return nil, nil }
