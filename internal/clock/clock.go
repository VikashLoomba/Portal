// Package clock abstracts time so the reconcile loop and launchd poll waits
// are deterministic in tests.
package clock

import (
	"context"
	"time"
)

type Clock interface {
	Now() time.Time
	// Sleep blocks for d, returning early if ctx is cancelled.
	Sleep(ctx context.Context, d time.Duration)
	// NewTicker returns a tick channel and a stop function.
	NewTicker(d time.Duration) (<-chan time.Time, func())
}

type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) Sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func (Real) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Fake is a manually-driven clock for tests.
type Fake struct {
	now    time.Time
	ticker chan time.Time
}

func NewFake(start time.Time) *Fake { return &Fake{now: start, ticker: make(chan time.Time, 1)} }

func (f *Fake) Now() time.Time                           { return f.now }
func (f *Fake) Sleep(_ context.Context, d time.Duration) { f.now = f.now.Add(d) }
func (f *Fake) NewTicker(_ time.Duration) (<-chan time.Time, func()) {
	return f.ticker, func() {}
}
func (f *Fake) Tick() { f.now = f.now.Add(time.Second); f.ticker <- f.now }

var _ Clock = Real{}
var _ Clock = (*Fake)(nil)
