package agent

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/VikashLoomba/Portal/internal/protocol"
)

// Sentinel errors returned by the registry's Call helper (the lifted clip
// request/response machinery, DESIGN S9).
var (
	// ErrNoWaiterCapacity is returned by call when the outstanding-waiter count
	// has already hit maxInflight (the DoS guard generalizing maxInflightClip).
	ErrNoWaiterCapacity = errors.New("agent: call waiter capacity exceeded")
	// ErrCallTimeout is returned by call when no matching completeCall arrives
	// before the per-call timeout (or the request could not be admitted to the
	// outbox). Adverse-path callers (clip) map it to "none\n".
	ErrCallTimeout = errors.New("agent: call timed out")
)

// Verb is a cmd-socket verb claim (DESIGN S8): a verb name, its per-verb socket
// deadline, and the handler. The Deadline is READ LIVE by routeVerb at
// connection time (never snapshotted at registration), so a service that stores
// its socket deadline in a field can shorten it and have the shortened value
// actually applied. Handle receives the Serve ctx threaded from handleCmdConn:
// clip's handler needs it for reg.call and its select on ctx.Done(); the
// openurl/notify handlers ignore it. routeVerb is the single source of the
// per-verb socket deadline — handlers do not set their own.
type Verb struct {
	Name     string
	Deadline time.Duration
	Handle   func(ctx context.Context, conn net.Conn, rest string)
}

// Service is a compiled-in agent-side feature (openurl/notify/clip). HandleMsg
// processes inbound client→agent Msgs for this service (e.g. clip "resp").
// Verbs() MUST construct its []Verb from the service's current fields on each
// call so Deadline reflects live field values; it is invoked at registration
// (for the verb-name claims) and again per cmd-socket connection (by routeVerb,
// for the live Deadline/Handle). OutboxCap declares the service's bounded
// agent→client outbox admission budget (DESIGN S5).
type Service interface {
	Name() string
	Version() uint32
	MaxPayload() int
	OutboxCap() int
	Verbs() []Verb
	HandleMsg(kind string, payload cbor.RawMessage)
}

// registryBinder is the optional hook a Service implements to receive its
// registry back at registration, so its cmd-verb handlers can emit()/call()
// without the registry ever handing out an *Encoder (structural sole-writer).
type registryBinder interface {
	bindRegistry(r *registry)
}

// serviceEntry is the registry's per-service bookkeeping: the Service itself,
// its declared version and outbox capacity, the per-service admission budget
// (slots; occupancy == in-flight agent→client frame count), and the
// per-(service, agent→client) monotonic Seq counter (NEVER s.seq — DESIGN S3).
type serviceEntry struct {
	svc     Service
	version uint32
	cap     int
	slots   chan struct{}
	seq     uint64 // atomic
}

// registry is the agent-side service registry. It owns verb claims, inbound Msg
// dispatch, the bounded per-service outboxes merged into a single drain the
// Serve select reads, per-service Seq stamping, and the generalized clip
// Call/epoch/nonce machinery. It hands out NO *Encoder — only channels — so it
// can never become a second writer of agent→client frames (DESIGN S5).
type registry struct {
	log *slog.Logger

	mu         sync.Mutex
	svcs       map[string]*serviceEntry // service name → entry
	claims     map[string]*serviceEntry // verb name → owning entry
	clientSvcs map[string]uint32        // client's advertised service→version

	// hasClientFn is the Server's guarded subscription reader, bound by
	// bindHasClient (called by agent.New in u3). nil until bound; hasClient()
	// returns false when unbound (safe standalone default for fakes/u2 tests).
	hasClientFn func() bool

	// outboxCh is the single merged drain the Serve select reads. Its capacity
	// is (re)sized on each register to the SUM of all registered services'
	// outbox capacities, so an admitted emit()'s push is always non-blocking.
	outboxCh chan *protocol.Envelope

	// ep is this process's clip identity, seeded in newRegistry via newEpoch.
	// nonce is the dedicated atomic call-nonce counter (the old clipSeq,
	// separate from every Seq).
	ep    uint64
	nonce uint64 // atomic

	waiterMu sync.Mutex
	waiters  map[uint64]chan cbor.RawMessage
}

// newRegistry constructs an empty registry. hasClientFn is left nil (bound
// later by bindHasClient); the clip epoch is seeded here.
func newRegistry(log *slog.Logger) *registry {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &registry{
		log:      log,
		svcs:     map[string]*serviceEntry{},
		claims:   map[string]*serviceEntry{},
		waiters:  map[uint64]chan cbor.RawMessage{},
		outboxCh: make(chan *protocol.Envelope), // resized on register
		ep:       newEpoch(),
	}
}

// newEpoch returns a non-zero random clip epoch — the permanent home of the
// per-process clip identity (server.go's randEpoch is deleted with the legacy
// clip machinery in u5). Semantics are identical to randEpoch: a zero draw is
// indistinguishable from an unset field, so on the astronomically unlikely
// all-zero draw we fall back to 1.
func newEpoch() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 1
	}
	e := binary.LittleEndian.Uint64(b[:])
	if e == 0 {
		return 1
	}
	return e
}

// bindHasClient binds the Server's guarded subscription reader so service
// handlers can observe subscription state (called by agent.New in u3).
func (r *registry) bindHasClient(fn func() bool) {
	r.hasClientFn = fn
}

// hasClient reports whether a Mac client is currently subscribed. Returns false
// when unbound (safe standalone default). Service handlers gate on
// `reg.hasClient() && reg.clientHas(service)`.
func (r *registry) hasClient() bool {
	if r.hasClientFn == nil {
		return false
	}
	return r.hasClientFn()
}

// register records svc by Name, claims its verbs (a duplicate claim across
// services PANICS — programmer error, DESIGN S8), allocates its (initially
// empty) outbox admission budget, binds the registry back into the service, and
// resizes the merged outbox to the sum of all services' capacities. It calls
// svc.Verbs() ONCE HERE ONLY to extract verb names — it does NOT snapshot
// Verb.Deadline/Handle (those are read live in routeVerb).
func (r *registry) register(svc Service) {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := svc.Name()
	if _, dup := r.svcs[name]; dup {
		panic("agent: duplicate service registration: " + name)
	}
	entry := &serviceEntry{
		svc:     svc,
		version: svc.Version(),
		cap:     svc.OutboxCap(),
	}
	entry.slots = make(chan struct{}, entry.cap)

	// Claim verb names; a duplicate claim across services is a programmer error.
	for _, v := range svc.Verbs() {
		if _, dup := r.claims[v.Name]; dup {
			panic("agent: duplicate cmd-socket verb claim: " + v.Name)
		}
		r.claims[v.Name] = entry
	}
	r.svcs[name] = entry

	// Bind the registry back so the service can emit()/call() (never an encoder).
	if b, ok := svc.(registryBinder); ok {
		b.bindRegistry(r)
	}

	// The merged outbox capacity == the SUM of per-service caps, so an admitted
	// emit() always has a free slot to push into.
	total := 0
	for _, e := range r.svcs {
		total += e.cap
	}
	r.outboxCh = make(chan *protocol.Envelope, total)
}

// services returns the agent's registered service→version map, for HelloAck.
func (r *registry) services() map[string]uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[string]uint32, len(r.svcs))
	for name, e := range r.svcs {
		m[name] = e.version
	}
	return m
}

// setClientServices records the client's advertised services (from Hello). A
// registered service the client advertises at a DIFFERENT version is treated as
// absent (DESIGN S4 exact-equality), with one warning emitted here at set time.
func (r *registry) setClientServices(m map[string]uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clientSvcs = make(map[string]uint32, len(m))
	for name, v := range m {
		r.clientSvcs[name] = v
	}
	for name, e := range r.svcs {
		if cv, ok := m[name]; ok && cv != e.version {
			r.log.Warn("client advertised service at mismatched version; treating as absent",
				"service", name, "agent_version", e.version, "client_version", cv)
		}
	}
}

// clientHas reports whether the client advertised service at the EXACT SAME
// version this agent registered it (DESIGN S4). Version inequality ⇒ absent.
func (r *registry) clientHas(service string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.svcs[service]
	if !ok {
		return false
	}
	cv, ok := r.clientSvcs[service]
	return ok && cv == e.version
}

// dispatch routes an inbound client→agent Msg to its service's HandleMsg. An
// unknown service (S4) or an oversized payload (S6) is dropped with a warning —
// the session LIVES (distinct from the frame-level MaxFrameBytes, still fatal in
// the codec). HandleMsg runs under recover so a panicking handler drops only
// that frame (S7).
func (r *registry) dispatch(m *protocol.Msg) {
	r.mu.Lock()
	e, ok := r.svcs[m.Service]
	r.mu.Unlock()
	if !ok {
		r.log.Warn("dropping Msg for unknown service", "service", m.Service, "kind", m.Kind)
		return
	}
	if len(m.Payload) > e.svc.MaxPayload() {
		r.log.Warn("dropping oversized service payload; session lives",
			"service", m.Service, "kind", m.Kind, "size", len(m.Payload), "max", e.svc.MaxPayload())
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("service HandleMsg panicked; dropping frame",
				"service", m.Service, "kind", m.Kind, "panic", rec)
		}
	}()
	e.svc.HandleMsg(m.Kind, m.Payload)
}

// emit stamps a per-(service, agent→client) Seq (NEVER s.seq — DESIGN S3),
// builds the envelope, then admits it non-blocking against the service's slots
// channel. A full slots channel is DropNewest overflow (S5): emit returns false
// and the cmd-verb handler writes its own grammar-preserving overflow reply. On
// admission the push onto the merged outbox is guaranteed non-blocking (merged
// capacity == sum of per-service caps), and emit returns true.
func (r *registry) emit(service, kind string, payload cbor.RawMessage) bool {
	r.mu.Lock()
	e, ok := r.svcs[service]
	ob := r.outboxCh
	r.mu.Unlock()
	if !ok {
		r.log.Warn("emit for unknown service dropped", "service", service, "kind", kind)
		return false
	}
	env := &protocol.Envelope{Msg: &protocol.Msg{
		Service: service,
		Kind:    kind,
		Seq:     atomic.AddUint64(&e.seq, 1),
		Payload: payload,
	}}
	select {
	case e.slots <- struct{}{}:
	default:
		return false // DropNewest: full outbox
	}
	// Guaranteed non-blocking: a slot is always free because merged cap == sum
	// of per-service caps and total admitted ≤ that sum.
	ob <- env
	return true
}

// release returns ONE unit of admission budget to service's slots channel. The
// Serve drain arm (u3) MUST call this for EVERY frame drained from outbox(),
// UNCONDITIONALLY (even when the frame is discarded), keyed by the drained
// env.Msg.Service — WITHOUT it a service's slots channel fills permanently and
// emit() returns false forever. Non-blocking/safe: the token is guaranteed
// present because every merged item corresponds to a held token.
func (r *registry) release(service string) {
	r.mu.Lock()
	e, ok := r.svcs[service]
	r.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-e.slots:
	default:
		// Guaranteed present in correct operation; guarded to stay non-blocking.
	}
}

// outbox returns the merged drain the Serve select reads. Structural
// sole-writer: the registry hands out channels only, never an *Encoder.
func (r *registry) outbox() <-chan *protocol.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outboxCh
}

// epoch returns this process's clip identity (echoed in requests, checked in
// completeCall). Immutable after newRegistry.
func (r *registry) epoch() uint64 { return r.ep }

// nextNonce mints a fresh call nonce (the old clipSeq; separate from every Seq).
func (r *registry) nextNonce() uint64 { return atomic.AddUint64(&r.nonce, 1) }

// call is the lifted clip request/response machinery (DESIGN S9). ctx is the
// Serve ctx threaded through Verb.Handle. If outstanding waiters already hit
// maxInflight it returns ErrNoWaiterCapacity; otherwise it registers a
// buffered-1 waiter keyed by nonce, emits the request, and waits for the waiter,
// ctx, or timeout (ErrCallTimeout / ctx.Err on the adverse paths). The waiter is
// always deleted on exit so a late/duplicate completeCall is dropped.
func (r *registry) call(ctx context.Context, service, kind string, timeout time.Duration, maxInflight int, nonce uint64, payload cbor.RawMessage) (cbor.RawMessage, error) {
	r.waiterMu.Lock()
	if len(r.waiters) >= maxInflight {
		r.waiterMu.Unlock()
		return nil, ErrNoWaiterCapacity
	}
	ch := make(chan cbor.RawMessage, 1)
	r.waiters[nonce] = ch
	r.waiterMu.Unlock()

	defer func() {
		r.waiterMu.Lock()
		delete(r.waiters, nonce)
		r.waiterMu.Unlock()
	}()

	if !r.emit(service, kind, payload) {
		// The request could not be admitted (full outbox). Treat as an adverse
		// path — the caller (clip) answers "none\n" — without burning the whole
		// timeout waiting for a response that will never come.
		return nil, ErrCallTimeout
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		return nil, ErrCallTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// completeCall delivers a response to its waiting call. A response whose epoch
// does not match this registry's epoch is a stale/cross-generation frame and is
// dropped with a warning (DESIGN S9). Delivery is non-blocking (the waiter
// channel is buffered 1, so it never stalls; a duplicate is dropped).
func (r *registry) completeCall(nonce, epoch uint64, payload cbor.RawMessage) {
	if epoch != r.epoch() {
		r.log.Warn("dropping call response with stale epoch",
			"got", epoch, "want", r.epoch(), "nonce", nonce)
		return
	}
	r.waiterMu.Lock()
	ch, ok := r.waiters[nonce]
	r.waiterMu.Unlock()
	if !ok {
		return // no waiter (timed out / duplicate) — drop
	}
	select {
	case ch <- payload:
	default:
		// Waiter already satisfied — drop the duplicate.
	}
}

// routeVerb dispatches a claimed cmd-socket verb (DESIGN S8). An unknown verb
// returns false so the cmd dispatcher writes "rejected\n" (default-deny). For a
// claimed verb it obtains the matching Verb LIVE by scanning svc.Verbs() (the
// verb SET is immutable, but the Deadline/Handle values are read fresh — this is
// what makes a service's socket-deadline field authoritative and genuinely
// shortenable at connection time), applies the per-verb deadline, and threads
// the ctx it was given into Handle. routeVerb is the single source of the
// per-verb socket deadline.
func (r *registry) routeVerb(ctx context.Context, conn net.Conn, verb, rest string) bool {
	r.mu.Lock()
	e, ok := r.claims[verb]
	r.mu.Unlock()
	if !ok {
		return false
	}
	for _, v := range e.svc.Verbs() {
		if v.Name != verb {
			continue
		}
		conn.SetDeadline(time.Now().Add(v.Deadline))
		v.Handle(ctx, conn, rest)
		return true
	}
	// Claimed at registration but no longer advertised — the verb set is
	// immutable, so this is unreachable in correct operation; default-deny.
	return false
}
