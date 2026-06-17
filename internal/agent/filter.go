// Package agent is the portald RPC server. The filter sub-component decides
// which raw watcher.Listen sockets reach the wire.
package agent

import (
	"sync"

	"github.com/vikashl/portal/internal/agent/watcher"
)

// Filter applies the deny / ephemeral-range / allow-override pipeline to a
// raw set of watcher.Listen records. It is concurrency-safe: SetAllowDeny
// can be called from the command-read goroutine while Apply runs from the
// watcher-event goroutine.
//
// Decision order (allow always wins; matches the bash original):
//  1. loopback predicate (caller-supplied — netlink filter does this)
//  2. allow → KEEP
//  3. deny  → DROP
//  4. ExcludeEphemeral && port in [EphemMin,EphemMax] → DROP
//  5. otherwise KEEP
type Filter struct {
	mu               sync.RWMutex
	deny             map[uint16]struct{}
	allow            map[uint16]struct{}
	excludeEphemeral bool
	EphemMin         uint16 // immutable after New()
	EphemMax         uint16
}

// NewFilter constructs a Filter with the given ephemeral range. Allow/deny
// sets are empty until SetAllowDeny.
func NewFilter(ephemMin, ephemMax uint16) *Filter {
	return &Filter{
		deny:     map[uint16]struct{}{},
		allow:    map[uint16]struct{}{},
		EphemMin: ephemMin,
		EphemMax: ephemMax,
	}
}

// SetAllowDeny replaces both sets and the ExcludeEphemeral flag atomically.
func (f *Filter) SetAllowDeny(deny, allow []uint16, excludeEphemeral bool) {
	d := make(map[uint16]struct{}, len(deny))
	for _, p := range deny {
		d[p] = struct{}{}
	}
	a := make(map[uint16]struct{}, len(allow))
	for _, p := range allow {
		a[p] = struct{}{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deny = d
	f.allow = a
	f.excludeEphemeral = excludeEphemeral
}

// Accept reports whether a single Listen passes the filter. Used in the
// streaming-event hot path (no slice allocation).
func (f *Filter) Accept(l watcher.Listen) bool {
	if !isLoopback(l) {
		return false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if _, ok := f.allow[l.Port]; ok {
		return true
	}
	if _, ok := f.deny[l.Port]; ok {
		return false
	}
	if f.excludeEphemeral && l.Port >= f.EphemMin && l.Port <= f.EphemMax {
		return false
	}
	return true
}

// Apply filters a slice and returns the kept entries (sorted ascending by
// port, deterministic for testing).
func (f *Filter) Apply(in []watcher.Listen) []watcher.Listen {
	out := make([]watcher.Listen, 0, len(in))
	for _, l := range in {
		if f.Accept(l) {
			out = append(out, l)
		}
	}
	sortListenByPort(out)
	return out
}

// isLoopback returns true iff the listener is bound to 127.0.0.0/8 or ::1.
// 0.0.0.0 / :: ARE loopback-reachable but match every interface; we choose
// to forward only EXPLICIT loopback binds — the bash original made the same
// choice (the `ss -Htln` filter row was checked against 127. or ::1).
func isLoopback(l watcher.Listen) bool {
	if l.Family == 4 {
		return len(l.Addr) >= 4 && l.Addr[:4] == "127."
	}
	if l.Family == 6 {
		return l.Addr == "::1"
	}
	return false
}

// sortListenByPort is a small no-allocation helper instead of pulling sort.
func sortListenByPort(s []watcher.Listen) {
	// Insertion sort — typical N is <30 listeners on a dev box.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].Port > s[j].Port; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
