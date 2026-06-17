// Package watcher abstracts the listen-socket detector. The production impl
// (netlink_linux.go) does NETLINK_SOCK_DIAG dumps every 75ms plus
// SKNLGRP_INET_TCP_DESTROY multicast for instant removes; the FakeWatcher
// (fake.go) drives agent.Server tests cross-platform.
package watcher

import (
	"context"
	"errors"
	"time"
)

// ErrUnsupported is returned by stub watchers (non-Linux).
var ErrUnsupported = errors.New("watcher: unsupported on this OS")

// Listen describes a single TCP listening socket discovered by the watcher.
// Source is filled by the producer (dump-diff vs DESTROY multicast); the
// agent passes it through to PortRemoved.Source on the wire so clients can
// distinguish polled-loss from actual close events for diagnostics.
type Listen struct {
	Port    uint16
	Family  uint8 // 4 or 6
	Addr    string
	InodeNS uint32
}

// EventKind discriminates Add vs Remove.
type EventKind uint8

const (
	KindAdd EventKind = 1 + iota
	KindRemove
)

// Event is a single watcher transition.
type Event struct {
	Kind   EventKind
	Listen Listen
	At     time.Time
	// Source is opaque (1 = dump-diff, 2 = destroy-multicast). Mirrored
	// onto PortRemoved.Source.
	Source uint8
}

// Watcher is what agent.Server consumes. Start returns a channel of events
// that closes on shutdown; SnapshotNow returns the current full set on
// demand (used to fulfill SubscribeAck → Snapshot exchanges).
type Watcher interface {
	// Start begins watching. The returned channel emits Events and is
	// closed when ctx is cancelled or the watcher errors permanently.
	Start(ctx context.Context) (<-chan Event, error)
	// SnapshotNow returns the current full set of LISTEN sockets at the
	// moment of call. Cheap: re-uses the netlink dump path.
	SnapshotNow(ctx context.Context) ([]Listen, error)
}
