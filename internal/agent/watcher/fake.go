package watcher

import (
	"context"
	"sync"
	"time"
)

// Fake is a manual-driven Watcher for tests. Push events with Emit;
// SetSnapshot replaces what SnapshotNow returns.
type Fake struct {
	mu       sync.Mutex
	snapshot []Listen
	events   chan Event
}

// NewFake returns a Fake with a 64-buffered event channel.
func NewFake() *Fake {
	return &Fake{events: make(chan Event, 64)}
}

// Start returns the event channel. ctx cancellation closes the channel.
func (f *Fake) Start(ctx context.Context) (<-chan Event, error) {
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.events != nil {
			close(f.events)
			f.events = nil
		}
	}()
	return f.events, nil
}

// SnapshotNow returns the configured snapshot.
func (f *Fake) SnapshotNow(ctx context.Context) ([]Listen, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Listen, len(f.snapshot))
	copy(out, f.snapshot)
	return out, nil
}

// SetSnapshot replaces the current full set.
func (f *Fake) SetSnapshot(ls []Listen) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshot = append([]Listen(nil), ls...)
}

// Emit sends an event. Blocks if the consumer is slow and buffer is full
// (matches the real watcher's backpressure behavior). Safe to call after
// ctx cancellation — it recovers from the send-on-closed-channel panic.
func (f *Fake) Emit(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	f.mu.Lock()
	ch := f.events
	f.mu.Unlock()
	if ch == nil {
		return
	}
	defer func() { recover() }()
	ch <- ev
}

var _ Watcher = (*Fake)(nil)
