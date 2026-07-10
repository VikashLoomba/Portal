package agentclient

import (
	"errors"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/pkg/hub"
)

// recvTimeout is generous: the tee is a synchronous, in-process channel send,
// so a real event lands immediately; the timeout only bounds the failure case.
const recvTimeout = 200 * time.Millisecond

// newTeeFixture builds a Client whose Config.Hub is a real hub with a
// Coalesced and a Queued subscription already registered.
func newTeeFixture(t *testing.T) (c *Client, coal, queued <-chan hub.Event) {
	t.Helper()
	h := hub.New()
	coalCh, cancelCoal := h.Subscribe(hub.Coalesced)
	t.Cleanup(cancelCoal)
	queuedCh, cancelQueued := h.Subscribe(hub.Queued)
	t.Cleanup(cancelQueued)
	c = New(Config{Hub: h})
	return c, coalCh, queuedCh
}

// expectCoalesced fails unless one Coalesced Event arrives.
func expectCoalesced(t *testing.T, ch <-chan hub.Event) {
	t.Helper()
	select {
	case ev := <-ch:
		if ev.Class != hub.Coalesced {
			t.Fatalf("got class %d, want Coalesced", ev.Class)
		}
		if ev.Notify != nil {
			t.Fatalf("Coalesced event must carry no Notify, got %+v", ev.Notify)
		}
	case <-time.After(recvTimeout):
		t.Fatal("expected a Coalesced event, none arrived")
	}
}

// expectNoEvent fails if any event arrives on ch within a short window.
func expectNoEvent(t *testing.T, ch <-chan hub.Event, what string) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("expected no event on %s, got %+v", what, ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHubTee_CoalescedOnStateEvents asserts the state-signal enumeration tees a
// Coalesced event for KindConnected/KindDisconnected/KindSnapshotReplaced/
// KindDelta (draining between each because the cap-1 buffer is latest-wins).
func TestHubTee_CoalescedOnStateEvents(t *testing.T) {
	c, coal, _ := newTeeFixture(t)

	c.publish(EngineEvent{Kind: KindConnected})
	expectCoalesced(t, coal)

	c.publish(EngineEvent{Kind: KindSnapshotReplaced})
	expectCoalesced(t, coal)

	c.publish(EngineEvent{Kind: KindDelta, Added: []uint16{8082}})
	expectCoalesced(t, coal)

	c.publish(EngineEvent{Kind: KindDisconnected, Err: errors.New("blip")})
	expectCoalesced(t, coal)
}

// TestHubTee_QueuedOnNotify asserts publishNotify tees a Queued event carrying
// all eight Notify fields verbatim.
func TestHubTee_QueuedOnNotify(t *testing.T) {
	c, _, queued := newTeeFixture(t)

	want := &NotifyEvent{
		Title:    "Build done",
		Body:     "exit 0",
		Subtitle: "claude-code",
		Urgency:  2,
		Verified: true,
		Source:   "hook",
		Sound:    "Glass",
		Seq:      42,
	}
	c.publishNotify(EngineEvent{Kind: KindNotify, Notify: want})

	select {
	case ev := <-queued:
		if ev.Class != hub.Queued {
			t.Fatalf("got class %d, want Queued", ev.Class)
		}
		if ev.Notify == nil {
			t.Fatal("Queued event must carry a Notify, got nil")
		}
		got := *ev.Notify
		wantHub := hub.Notify{
			Title: want.Title, Body: want.Body, Subtitle: want.Subtitle,
			Urgency: want.Urgency, Verified: want.Verified, Source: want.Source,
			Sound: want.Sound, Seq: want.Seq,
		}
		if got != wantHub {
			t.Fatalf("Notify = %+v, want %+v", got, wantHub)
		}
	case <-time.After(recvTimeout):
		t.Fatal("expected a Queued Notify event, none arrived")
	}
}

// TestHubTee_NoCoalescedOnOpenURL asserts KindOpenURL is NOT mapped into the
// hub — the URL relay stays daemon-internal in v1.
func TestHubTee_NoCoalescedOnOpenURL(t *testing.T) {
	c, coal, _ := newTeeFixture(t)

	c.publish(EngineEvent{Kind: KindOpenURL, URL: "https://example.test"})
	expectNoEvent(t, coal, "Coalesced (KindOpenURL)")
}

// TestHubTee_NoEventOnClip asserts a clip publish produces NO hub event of any
// class — clip is excluded by TYPE (not representable in hub.Event).
func TestHubTee_NoEventOnClip(t *testing.T) {
	c, coal, queued := newTeeFixture(t)

	c.publishClip(EngineEvent{Kind: KindClipRequest, Clip: &ClipEvent{
		Nonce: 1, Epoch: 1, Kind: "text",
	}})
	expectNoEvent(t, coal, "Coalesced (clip)")
	expectNoEvent(t, queued, "Queued (clip)")
}

// TestHubTee_NoEventOnCred asserts a credential publish remains daemon-local
// and produces no hub event of either class.
func TestHubTee_NoEventOnCred(t *testing.T) {
	c, coal, queued := newTeeFixture(t)

	c.publishCred(EngineEvent{Kind: KindCredRequest, Cred: &CredEvent{
		Nonce: 2, Epoch: 3, Label: "database", Mode: "env", Target: "PW",
	}})
	expectNoEvent(t, coal, "Coalesced (cred)")
	expectNoEvent(t, queued, "Queued (cred)")
}

// TestHubTee_LastDisconnectErr asserts LastDisconnectErr tracks the most recent
// KindDisconnected error, and reports "" for a disconnect with no error.
func TestHubTee_LastDisconnectErr(t *testing.T) {
	c, _, _ := newTeeFixture(t)

	if got := c.LastDisconnectErr(); got != "" {
		t.Fatalf("LastDisconnectErr before any disconnect = %q, want %q", got, "")
	}

	c.publish(EngineEvent{Kind: KindDisconnected, Err: errors.New("heartbeat timeout")})
	if got := c.LastDisconnectErr(); got != "heartbeat timeout" {
		t.Fatalf("LastDisconnectErr = %q, want %q", got, "heartbeat timeout")
	}

	c.publish(EngineEvent{Kind: KindDisconnected, Err: nil})
	if got := c.LastDisconnectErr(); got != "" {
		t.Fatalf("LastDisconnectErr after nil-err disconnect = %q, want %q", got, "")
	}
}
