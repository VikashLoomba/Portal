package hub

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// stateEvent is a Coalesced signal; Notify is nil by design.
func stateEvent() Event { return Event{Class: Coalesced} }

// notifyEvent is a Queued event tagged with seq so tests can assert order.
func notifyEvent(seq uint64) Event {
	return Event{Class: Queued, Notify: &Notify{Seq: seq}}
}

// TestCoalescedLatestWins: three Publishes with no reader return immediately;
// one receive yields exactly the latest (buffer held 1). The distinguishing
// marker rides in a Notify here purely to identify which event survived — the
// production Coalesced Notify is nil, but the hub does not inspect it.
func TestCoalescedLatestWins(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe(Coalesced)
	defer cancel()

	for seq := uint64(1); seq <= 3; seq++ {
		h.Publish(Event{Class: Coalesced, Notify: &Notify{Seq: seq}})
	}

	select {
	case ev := <-ch:
		if ev.Notify == nil || ev.Notify.Seq != 3 {
			t.Fatalf("want latest seq=3, got %+v", ev.Notify)
		}
	default:
		t.Fatal("expected one buffered event")
	}
	// Buffer coalesced to a single item.
	select {
	case ev := <-ch:
		t.Fatalf("expected empty buffer, got %+v", ev)
	default:
	}
}

// TestQueuedDropOldest: 20 Publishes with no reader drop the oldest 4; draining
// yields the 16 most-recent in order.
func TestQueuedDropOldest(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe(Queued)
	defer cancel()

	for seq := uint64(1); seq <= 20; seq++ {
		h.Publish(notifyEvent(seq))
	}

	if got := h.DroppedNotify(); got != 4 {
		t.Fatalf("DroppedNotify()=%d, want 4", got)
	}

	// The retained window is seq 5..20 in order (the 16 most recent).
	for want := uint64(5); want <= 20; want++ {
		select {
		case ev := <-ch:
			if ev.Notify == nil || ev.Notify.Seq != want {
				t.Fatalf("drain: got %+v, want seq=%d", ev.Notify, want)
			}
		default:
			t.Fatalf("drain: channel empty at seq=%d", want)
		}
	}
	select {
	case ev := <-ch:
		t.Fatalf("drain: expected empty, got %+v", ev)
	default:
	}
}

// TestSlowSubscriberNeverBlocks: a subscriber that never reads must not wedge a
// tight loop of Publishes. Proven by running the loop in a goroutine and
// requiring it to complete before a timeout.
func TestSlowSubscriberNeverBlocks(t *testing.T) {
	t.Run("coalesced", func(t *testing.T) { assertNeverBlocks(t, Coalesced) })
	t.Run("queued", func(t *testing.T) { assertNeverBlocks(t, Queued) })
}

func assertNeverBlocks(t *testing.T, class Class) {
	t.Helper()
	h := New()
	_, cancel := h.Subscribe(class) // deliberately never read
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := uint64(0); i < 1000; i++ {
			h.Publish(Event{Class: class, Notify: &Notify{Seq: i}})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a non-reading subscriber")
	}
}

// TestIndependentBuffers: a drained subscriber keeps observing the latest even
// while a same-class sibling ignores its channel entirely.
func TestIndependentBuffers(t *testing.T) {
	h := New()
	fast, cancelFast := h.Subscribe(Coalesced)
	defer cancelFast()
	_, cancelSlow := h.Subscribe(Coalesced) // never read
	defer cancelSlow()

	for seq := uint64(1); seq <= 5; seq++ {
		h.Publish(Event{Class: Coalesced, Notify: &Notify{Seq: seq}})
		// Drain fast each round; it must track the newest without being
		// throttled by the wedged sibling.
		select {
		case ev := <-fast:
			if ev.Notify == nil || ev.Notify.Seq != seq {
				t.Fatalf("fast: got %+v, want seq=%d", ev.Notify, seq)
			}
		case <-time.After(time.Second):
			t.Fatalf("fast subscriber throttled at seq=%d", seq)
		}
	}
}

// TestClassIsolation: a Publish is delivered only to same-class subscribers.
func TestClassIsolation(t *testing.T) {
	h := New()
	coalesced, cancelC := h.Subscribe(Coalesced)
	defer cancelC()
	queued, cancelQ := h.Subscribe(Queued)
	defer cancelQ()

	h.Publish(notifyEvent(7)) // Queued only
	h.Publish(stateEvent())   // Coalesced only

	select {
	case ev := <-coalesced:
		if ev.Class != Coalesced || ev.Notify != nil {
			t.Fatalf("coalesced got %+v", ev)
		}
	default:
		t.Fatal("coalesced subscriber missing its state event")
	}
	select {
	case ev := <-coalesced:
		t.Fatalf("coalesced leaked a queued event: %+v", ev)
	default:
	}

	select {
	case ev := <-queued:
		if ev.Class != Queued || ev.Notify == nil || ev.Notify.Seq != 7 {
			t.Fatalf("queued got %+v", ev)
		}
	default:
		t.Fatal("queued subscriber missing its notify event")
	}
	select {
	case ev := <-queued:
		t.Fatalf("queued leaked a coalesced event: %+v", ev)
	default:
	}
}

// TestUnsubscribeRace exercises the send-vs-close boundary under the race
// detector: publishers hammer both classes while subscribers churn.
func TestUnsubscribeRace(t *testing.T) {
	h := New()
	const publishers, churners = 8, 8

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Keep at least one stable subscriber of each class so publishes have a
	// live target throughout.
	_, cancelC := h.Subscribe(Coalesced)
	defer cancelC()
	_, cancelQ := h.Subscribe(Queued)
	defer cancelQ()

	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var seq uint64
			for {
				select {
				case <-stop:
					return
				default:
				}
				seq++
				h.Publish(stateEvent())
				h.Publish(notifyEvent(seq))
			}
		}()
	}

	for i := 0; i < churners; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			class := Coalesced
			if id%2 == 0 {
				class = Queued
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				ch, cancel := h.Subscribe(class)
				// Drain a little, then leave.
				select {
				case <-ch:
				default:
				}
				cancel()
			}
		}(i)
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestCancelIdempotent: calling cancel twice is a no-op, and a Publish after
// cancel is not delivered (the subscriber is gone from the set).
func TestCancelIdempotent(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe(Coalesced)
	cancel()
	cancel() // must not panic

	h.Publish(stateEvent())

	// Channel is closed and was never sent to post-cancel: reads yield the
	// zero value with ok=false.
	if ev, ok := <-ch; ok {
		t.Fatalf("delivered after cancel: %+v", ev)
	}
}

// TestNoGoroutineLeak: the hub spawns no goroutines, so many Subscribe/cancel
// cycles leave NumGoroutine stable.
func TestNoGoroutineLeak(t *testing.T) {
	h := New()
	// Warm up, then baseline.
	runtime.GC()
	before := runtime.NumGoroutine()

	for i := 0; i < 1000; i++ {
		class := Coalesced
		if i%2 == 0 {
			class = Queued
		}
		_, cancel := h.Subscribe(class)
		h.Publish(Event{Class: class})
		cancel()
	}

	runtime.GC()
	after := runtime.NumGoroutine()
	if after > before {
		t.Fatalf("goroutine count grew: before=%d after=%d", before, after)
	}
}
