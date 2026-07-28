package agentclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/VikashLoomba/Portal/pkg/protocol"
)

func clipWritePayload(t *testing.T, req protocol.ClipWriteRequest) cbor.RawMessage {
	t.Helper()
	payload, err := protocol.MarshalPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestClientClipWrite_DecodePublish(t *testing.T) {
	tests := []struct {
		name string
		req  protocol.ClipWriteRequest
	}{
		{
			name: "image",
			req: protocol.ClipWriteRequest{
				Nonce: 11, Epoch: 12, Kind: "image", Format: "png",
				SHA: "0123456789abcdef0123456789abcdef", Size: 4242,
			},
		},
		{
			name: "clear",
			req:  protocol.ClipWriteRequest{Nonce: 13, Epoch: 14, Kind: "clear"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(Config{})
			c.registry.dispatch(&protocol.Msg{
				Service: "clipwrite", Kind: "req", Payload: clipWritePayload(t, tt.req),
			})
			select {
			case ev := <-c.ClipWriteEvents():
				if ev.Kind != KindClipWriteRequest || ev.ClipWrite == nil {
					t.Fatal("clipboard-write request was not published on its dedicated channel")
				}
				got := ev.ClipWrite
				if got.Nonce != tt.req.Nonce || got.Epoch != tt.req.Epoch ||
					got.Kind != tt.req.Kind || got.Format != tt.req.Format ||
					got.SHA != tt.req.SHA || got.Size != tt.req.Size {
					t.Fatalf("clipboard-write event = %+v, want %+v", got, tt.req)
				}
			case <-time.After(time.Second):
				t.Fatal("no clipboard-write event delivered")
			}
		})
	}
}

func TestClientClipWrite_DedicatedChannelNotEvicted(t *testing.T) {
	c := New(Config{})
	if cap(c.clipWriteEvents) != 8 {
		t.Fatalf("clipWriteEvents cap = %d, want 8", cap(c.clipWriteEvents))
	}
	for len(c.events) < cap(c.events) {
		c.events <- EngineEvent{Kind: KindDelta}
	}

	req := protocol.ClipWriteRequest{Nonce: 21, Epoch: 22, Kind: "clear"}
	c.registry.dispatch(&protocol.Msg{
		Service: "clipwrite", Kind: "req", Payload: clipWritePayload(t, req),
	})
	select {
	case ev := <-c.ClipWriteEvents():
		if ev.ClipWrite == nil || ev.ClipWrite.Nonce != req.Nonce {
			t.Fatalf("wrong clipboard-write event: %+v", ev)
		}
	default:
		t.Fatal("clipboard-write event was evicted despite a full shared events channel")
	}
}

func TestClientClipWrite_ChannelIsDistinctFromClip(t *testing.T) {
	c := New(Config{})
	if c.ClipWriteEvents() == c.ClipEvents() {
		t.Fatal("ClipWriteEvents and ClipEvents returned the same channel")
	}

	req := protocol.ClipWriteRequest{Nonce: 31, Epoch: 32, Kind: "clear"}
	c.registry.dispatch(&protocol.Msg{
		Service: "clipwrite", Kind: "req", Payload: clipWritePayload(t, req),
	})
	select {
	case ev := <-c.ClipEvents():
		t.Fatalf("clipboard-write request was misrouted to ClipEvents: %+v", ev)
	default:
	}
	select {
	case ev := <-c.ClipWriteEvents():
		if ev.ClipWrite == nil || ev.ClipWrite.Nonce != req.Nonce {
			t.Fatalf("wrong clipboard-write event: %+v", ev)
		}
	default:
		t.Fatal("clipboard-write request was not delivered to ClipWriteEvents")
	}
}

func TestClientClipWrite_DropsWhenFull(t *testing.T) {
	c := New(Config{})
	for len(c.clipWriteEvents) < cap(c.clipWriteEvents) {
		c.clipWriteEvents <- EngineEvent{Kind: KindClipWriteRequest}
	}

	req := protocol.ClipWriteRequest{Nonce: 41, Epoch: 42, Kind: "clear"}
	payload := clipWritePayload(t, req)
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		c.registry.dispatch(&protocol.Msg{
			Service: "clipwrite", Kind: "req", Payload: payload,
		})
	}()
	select {
	case <-dispatchDone:
	case <-time.After(time.Second):
		t.Fatal("clipboard-write dispatch blocked on a full dedicated channel")
	}
	if len(c.clipWriteEvents) != cap(c.clipWriteEvents) {
		t.Fatalf("clipWriteEvents len = %d, want full capacity %d", len(c.clipWriteEvents), cap(c.clipWriteEvents))
	}

	clipPayload, err := protocol.MarshalPayload(protocol.ClipRequest{
		Nonce: 43, Epoch: 44, Kind: "text",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.registry.dispatch(&protocol.Msg{Service: "clip", Kind: "req", Payload: clipPayload})
	select {
	case ev := <-c.ClipEvents():
		if ev.Clip == nil || ev.Clip.Nonce != 43 {
			t.Fatalf("wrong clip event: %+v", ev)
		}
	default:
		t.Fatal("full clipboard-write channel interfered with ClipEvents delivery")
	}
}

func TestClientClipWrite_DropsUnexpectedMessageKinds(t *testing.T) {
	c := New(Config{})
	req := protocol.ClipWriteRequest{Nonce: 45, Epoch: 46, Kind: "clear"}
	payload := clipWritePayload(t, req)
	for _, kind := range []string{"", "event", "resp"} {
		c.registry.dispatch(&protocol.Msg{
			Service: "clipwrite", Kind: kind, Payload: payload,
		})
	}
	select {
	case ev := <-c.ClipWriteEvents():
		t.Fatalf("unexpected-kind clipboard-write frame was delivered: %+v", ev)
	default:
	}

	c.registry.dispatch(&protocol.Msg{
		Service: "clipwrite", Kind: "req", Payload: payload,
	})
	select {
	case ev := <-c.ClipWriteEvents():
		if ev.ClipWrite == nil || ev.ClipWrite.Nonce != req.Nonce {
			t.Fatalf("valid clipboard-write frame after drops = %+v", ev)
		}
	default:
		t.Fatal("valid clipboard-write frame was not delivered after unexpected-kind drops")
	}
}

func TestClientClipWrite_LateDeliveryAfterRunTeardown(t *testing.T) {
	c := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not tear down")
	}

	req := protocol.ClipWriteRequest{Nonce: 47, Epoch: 48, Kind: "clear"}
	c.registry.dispatch(&protocol.Msg{
		Service: "clipwrite", Kind: "req", Payload: clipWritePayload(t, req),
	})
	select {
	case ev := <-c.ClipWriteEvents():
		if ev.ClipWrite == nil || ev.ClipWrite.Nonce != req.Nonce {
			t.Fatalf("late clipboard-write event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("late clipboard-write frame was not delivered after Run teardown")
	}
}

func TestClientClipWrite_OversizedPayloadDrop(t *testing.T) {
	c := New(Config{})
	c.registry.dispatch(&protocol.Msg{
		Service: "clipwrite", Kind: "req", Payload: cbor.RawMessage(make([]byte, 4097)),
	})
	select {
	case ev := <-c.ClipWriteEvents():
		t.Fatalf("oversized clipboard-write payload was delivered: %+v", ev)
	default:
	}

	req := protocol.ClipWriteRequest{Nonce: 51, Epoch: 52, Kind: "clear"}
	c.registry.dispatch(&protocol.Msg{
		Service: "clipwrite", Kind: "req", Payload: clipWritePayload(t, req),
	})
	select {
	case ev := <-c.ClipWriteEvents():
		if ev.ClipWrite == nil || ev.ClipWrite.Nonce != req.Nonce {
			t.Fatalf("wrong clipboard-write event after oversized drop: %+v", ev)
		}
	default:
		t.Fatal("valid clipboard-write request was not delivered after oversized drop")
	}
}

func TestClientClipWrite_AdvertisedInHello(t *testing.T) {
	c := New(Config{})
	if got := c.registry.services()["clipwrite"]; got != 1 {
		t.Fatalf("clipwrite service version = %d, want 1", got)
	}
}

func TestClientClipWrite_SendBeforeConnect(t *testing.T) {
	c := New(Config{})
	err := c.SendClipWriteResponse(&protocol.ClipWriteResponse{
		Nonce: 1, Epoch: 2, Err: "disabled",
	})
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("SendClipWriteResponse before connect: want ErrNotConnected, got %v", err)
	}
}
