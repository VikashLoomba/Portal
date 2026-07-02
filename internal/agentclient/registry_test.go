package agentclient

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/protocol"
)

// countHandler is a slog.Handler that counts warn-level records, for asserting
// exactly-once dormancy warnings.
type countHandler struct {
	mu    sync.Mutex
	warns int
}

func (h *countHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.mu.Lock()
		h.warns++
		h.mu.Unlock()
	}
	return nil
}
func (h *countHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countHandler) WithGroup(string) slog.Handler      { return h }

func (h *countHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.warns
}

// raw1 is a minimal valid payload (never decoded when the handler's Decode
// ignores it).
func raw1() cbor.RawMessage { return cbor.RawMessage([]byte{0x01}) }

// routing to Deliver + a session that lives across a decode failure.
func TestClientRegistry_Routing(t *testing.T) {
	r := newRegistry(nil)
	var got EngineEvent
	r.register(HandlerSpec{
		Service: "notify", Version: 1, MaxPayload: 1024,
		Decode:  func(uint64, cbor.RawMessage) (EngineEvent, error) { return EngineEvent{Kind: KindNotify}, nil },
		Deliver: func(ev EngineEvent) { got = ev },
	})
	r.dispatch(&protocol.Msg{Service: "notify", Kind: "event", Payload: raw1()})
	if got.Kind != KindNotify {
		t.Fatalf("want KindNotify delivered, got %v", got.Kind)
	}
}

func TestClientRegistry_DecodeFailureDrops(t *testing.T) {
	r := newRegistry(nil)
	delivered := false
	r.register(HandlerSpec{
		Service: "bad", Version: 1, MaxPayload: 1024,
		Decode:  func(uint64, cbor.RawMessage) (EngineEvent, error) { return EngineEvent{}, errors.New("boom") },
		Deliver: func(EngineEvent) { delivered = true },
	})
	goodDelivered := false
	r.register(HandlerSpec{
		Service: "good", Version: 1, MaxPayload: 1024,
		Decode:  func(uint64, cbor.RawMessage) (EngineEvent, error) { return EngineEvent{Kind: KindNotify}, nil },
		Deliver: func(EngineEvent) { goodDelivered = true },
	})
	r.dispatch(&protocol.Msg{Service: "bad", Kind: "x", Payload: raw1()})
	if delivered {
		t.Fatal("decode failure must not reach Deliver")
	}
	// Session lives: a subsequent well-formed dispatch still routes.
	r.dispatch(&protocol.Msg{Service: "good", Kind: "x", Payload: raw1()})
	if !goodDelivered {
		t.Fatal("registry did not survive a decode failure")
	}
}

func TestClientRegistry_UnknownAndDormantDrop(t *testing.T) {
	r := newRegistry(nil)
	delivered := false
	r.register(HandlerSpec{
		Service: "clip", Version: 1, MaxPayload: 1024,
		Decode:  func(uint64, cbor.RawMessage) (EngineEvent, error) { return EngineEvent{Kind: KindClipRequest}, nil },
		Deliver: func(EngineEvent) { delivered = true },
	})
	// Unknown service: dropped, no panic.
	r.dispatch(&protocol.Msg{Service: "nope", Kind: "x", Payload: raw1()})
	if delivered {
		t.Fatal("unknown service must not deliver")
	}
	// Mark clip dormant (agent lacks it), then dispatch → dropped.
	r.setAgentServices(map[string]uint32{})
	r.dispatch(&protocol.Msg{Service: "clip", Kind: "x", Payload: raw1()})
	if delivered {
		t.Fatal("dormant service must not deliver")
	}
}

func TestClientRegistry_MaxPayloadDrop(t *testing.T) {
	r := newRegistry(nil)
	delivered := 0
	r.register(HandlerSpec{
		Service: "svc", Version: 1, MaxPayload: 4,
		Decode:  func(uint64, cbor.RawMessage) (EngineEvent, error) { return EngineEvent{Kind: KindNotify}, nil },
		Deliver: func(EngineEvent) { delivered++ },
	})
	r.dispatch(&protocol.Msg{Service: "svc", Kind: "big", Payload: cbor.RawMessage(make([]byte, 8))})
	r.dispatch(&protocol.Msg{Service: "svc", Kind: "ok", Payload: cbor.RawMessage(make([]byte, 2))})
	if delivered != 1 {
		t.Fatalf("want 1 delivery (oversized dropped, in-cap delivered), got %d", delivered)
	}
}

// A panicking Deliver drops only that frame; the registry survives (S7/EC6).
func TestClientRegistry_PanicIsolation(t *testing.T) {
	r := newRegistry(nil)
	shouldPanic := true
	delivered := 0
	r.register(HandlerSpec{
		Service: "svc", Version: 1, MaxPayload: 64,
		Decode: func(uint64, cbor.RawMessage) (EngineEvent, error) { return EngineEvent{Kind: KindNotify}, nil },
		Deliver: func(EngineEvent) {
			if shouldPanic {
				panic("deliberate Deliver panic")
			}
			delivered++
		},
	})
	r.dispatch(&protocol.Msg{Service: "svc", Kind: "x", Payload: raw1()})
	shouldPanic = false
	r.dispatch(&protocol.Msg{Service: "svc", Kind: "y", Payload: raw1()})
	if delivered != 1 {
		t.Fatalf("registry did not survive a panicking Deliver, delivered=%d", delivered)
	}
}

// QoS non-eviction (S10): flooding the shared events channel does not evict a
// clip event routed to its dedicated channel.
func TestClientRegistry_QoSNonEviction(t *testing.T) {
	c := New(Config{})
	// Fill the shared events channel to capacity.
	for len(c.events) < cap(c.events) {
		c.events <- EngineEvent{Kind: KindDelta}
	}
	r := newRegistry(nil)
	r.register(HandlerSpec{
		Service: "clip", Version: 1, MaxPayload: 1024,
		Decode: func(uint64, cbor.RawMessage) (EngineEvent, error) {
			return EngineEvent{Kind: KindClipRequest, Clip: &ClipEvent{Nonce: 7}}, nil
		},
		Deliver: c.publishClip,
	})
	r.dispatch(&protocol.Msg{Service: "clip", Kind: "req", Payload: raw1()})
	select {
	case ev := <-c.clipEvents:
		if ev.Clip == nil || ev.Clip.Nonce != 7 {
			t.Fatalf("wrong clip event: %+v", ev)
		}
	default:
		t.Fatal("clip event was evicted / not delivered despite a full events channel")
	}
}

func TestClientRegistry_DormancyWarningOnce(t *testing.T) {
	// Missing service → exactly one warning.
	h := &countHandler{}
	r := newRegistry(slog.New(h))
	r.register(HandlerSpec{Service: "clip", Version: 1, MaxPayload: 64})
	r.setAgentServices(map[string]uint32{}) // agent lacks clip
	if h.count() != 1 {
		t.Fatalf("missing service: want exactly 1 dormancy warning, got %d", h.count())
	}

	// Version mismatch → exactly one warning.
	h2 := &countHandler{}
	r2 := newRegistry(slog.New(h2))
	r2.register(HandlerSpec{Service: "clip", Version: 1, MaxPayload: 64})
	r2.setAgentServices(map[string]uint32{"clip": 2})
	if h2.count() != 1 {
		t.Fatalf("version mismatch: want exactly 1 dormancy warning, got %d", h2.count())
	}

	// Exact match → no warning.
	h3 := &countHandler{}
	r3 := newRegistry(slog.New(h3))
	r3.register(HandlerSpec{Service: "clip", Version: 1, MaxPayload: 64})
	r3.setAgentServices(map[string]uint32{"clip": 1})
	if h3.count() != 0 {
		t.Fatalf("exact match: want 0 warnings, got %d", h3.count())
	}
}

func TestClientRegistry_SendBeforeConnect(t *testing.T) {
	r := newRegistry(nil)
	err := r.send(nil, "clip", "resp", raw1())
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("send before connect: want ErrNotConnected, got %v", err)
	}
}
