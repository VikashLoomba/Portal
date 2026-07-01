// Package hub is a read-only, in-memory fan-out tee for local API observers.
//
// It is deliberately NOT an event-ordering authority: it spawns no goroutines,
// owns no timers, and never sits between agentclient and its existing
// consumers (the engine, clip handler, and notify handler keep their dedicated
// channels). agentclient tees copies of state-change signals and notification
// events into the hub; the hub fans them out to per-subscriber buffered
// channels with a per-class drop policy so a slow or dead API observer can
// never backpressure the demux/heartbeat path. The risk register in
// DESIGN-local-core-api.md §10 calls this out explicitly: the hub tees, it
// never feeds the engine.
//
// Clip events are unrepresentable here by construction (there is no Event
// variant for them): pastes are answered by the daemon itself and must never
// be observable by, or raced with, API clients.
package hub

import (
	"sync"
	"sync/atomic"
)

// Class is a QoS discriminator selecting a subscriber's buffer and drop policy.
type Class uint8

const (
	// Coalesced carries pure state-change SIGNALs (connect/disconnect/
	// snapshot/delta). Buffer cap 1, latest-wins: a slow subscriber always
	// sees current truth, never a backlog. Notify is nil for this class.
	Coalesced Class = iota + 1
	// Queued carries notification events. Buffer cap 16, drop-oldest with the
	// dropped count recorded (surfaced in Status.Health). Notify is non-nil.
	Queued
)

// buffer capacities per class (see Class docs).
const (
	coalescedCap = 1
	queuedCap    = 16
)

// Notify mirrors agentclient.NotifyEvent field-for-field. It is duplicated
// rather than imported so the hub imports nothing from agentclient (the tee
// dependency points one way only: agentclient → hub).
type Notify struct {
	Title    string
	Body     string
	Subtitle string
	Urgency  uint8
	Verified bool
	Source   string
	Sound    string
	Seq      uint64
}

// Event is the tagged-union unit of fan-out. For Coalesced, Notify is nil (a
// pure state-change signal; subscribers re-read full state themselves). For
// Queued, Notify is non-nil. Clip events are not representable here by design.
type Event struct {
	Class  Class
	Notify *Notify
}

// subscriber is one registered observer. mu guards drain+resend so those two
// steps are atomic with respect to concurrent Publishers (multiple Publishers
// may hold the hub's RLock at once). once makes cancel idempotent.
type subscriber struct {
	class Class
	ch    chan Event
	mu    sync.Mutex
	once  sync.Once
}

// Hub fans Events out to subscribers of a matching Class. The zero value is
// not usable; call New.
type Hub struct {
	mu      sync.RWMutex             // guards subs; Publish RLocks, Subscribe/cancel Lock
	subs    map[*subscriber]struct{} // registered observers
	dropped uint64                   // atomic: Queued events dropped
}

// New returns a ready Hub with no subscribers.
func New() *Hub {
	return &Hub{subs: make(map[*subscriber]struct{})}
}

// Subscribe registers an observer for one Class and returns its receive channel
// plus a cancel func. The channel is buffered per class (Coalesced: cap 1,
// Queued: cap 16). cancel is idempotent (guarded by sync.Once) and safe to call
// during a concurrent Publish: it removes the subscriber under the hub write
// lock — which cannot be acquired while any Publish holds the read lock — then
// closes the channel, so no send can ever observe a closed channel. A
// subscriber added while others exist receives no backlog; its buffer starts
// empty.
func (h *Hub) Subscribe(class Class) (<-chan Event, func()) {
	capacity := coalescedCap
	if class == Queued {
		capacity = queuedCap
	}
	s := &subscriber{class: class, ch: make(chan Event, capacity)}

	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()

	cancel := func() {
		s.once.Do(func() {
			h.mu.Lock()
			delete(h.subs, s)
			h.mu.Unlock()
			// Safe outside the lock: delete under Lock drained all
			// in-flight Publishers (they hold RLock), and no new Publish
			// can find s now, so no send races this close.
			close(s.ch)
		})
	}
	return s.ch, cancel
}

// Publish routes ev only to subscribers whose Class == ev.Class. It never
// blocks, never errors, and never panics on a closed subscriber: it holds the
// read lock across all non-blocking sends, so cancel (which needs the write
// lock to remove+close) can never close a channel mid-send. Publishing to a
// class with zero subscribers is a no-op.
func (h *Hub) Publish(ev Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if s.class != ev.Class {
			continue
		}
		switch s.class {
		case Coalesced:
			s.sendCoalesced(ev)
		case Queued:
			s.sendQueued(ev, &h.dropped)
		}
	}
}

// DroppedNotify returns the running count of Queued events dropped by the
// drop-oldest policy (for Status.Health).
func (h *Hub) DroppedNotify() uint64 {
	return atomic.LoadUint64(&h.dropped)
}

// sendCoalesced delivers ev with latest-wins semantics on a cap-1 buffer: if
// the buffer is full, drain the one pending item then send the new one, all
// under s.mu so the drain+resend is atomic w.r.t. other Publishers. The final
// send cannot block: s.mu excludes other senders and the buffer was just
// emptied (a concurrent receiver only ever frees space).
func (s *subscriber) sendCoalesced(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.ch <- ev:
	default:
		// Buffer full: drop the stale pending item, then send current truth.
		select {
		case <-s.ch:
		default:
		}
		select {
		case s.ch <- ev:
		default:
		}
	}
}

// sendQueued delivers ev with drop-oldest semantics on a cap-16 buffer: if the
// buffer is full, pop the oldest item (incrementing dropped) and append the new
// one, preserving the order of the retained 16. All under s.mu so the pop+send
// is atomic w.r.t. other Publishers.
func (s *subscriber) sendQueued(ev Event, dropped *uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.ch <- ev:
	default:
		// Buffer full: drop the oldest, count it, then append the newest.
		select {
		case <-s.ch:
			atomic.AddUint64(dropped, 1)
		default:
		}
		select {
		case s.ch <- ev:
		default:
		}
	}
}
