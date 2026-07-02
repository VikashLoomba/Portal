package agent

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/VikashLoomba/Portal/internal/protocol"
)

// handledMsg records one HandleMsg delivery.
type handledMsg struct {
	kind    string
	payload cbor.RawMessage
}

// fakeService is a Service driven entirely by test-mutable fields. Its single
// verb's Deadline is read LIVE from sockDeadline (a mutable field) so a test can
// shorten it after registration; HandleMsg can be told to panic.
type fakeService struct {
	name       string
	version    uint32
	maxPayload int
	outboxCap  int
	verbName   string

	mu            sync.Mutex
	sockDeadline  time.Duration // read live by Verbs()
	panicOnMsg    bool
	handled       []handledMsg
	lastHandleCtx context.Context
	lastRest      string
	boundReg      *registry
}

func (f *fakeService) Name() string             { return f.name }
func (f *fakeService) Version() uint32          { return f.version }
func (f *fakeService) MaxPayload() int          { return f.maxPayload }
func (f *fakeService) OutboxCap() int           { return f.outboxCap }
func (f *fakeService) bindRegistry(r *registry) { f.boundReg = r }

// Verbs constructs the verb from CURRENT fields on each call, so Deadline
// reflects the live sockDeadline value (never a registration snapshot).
func (f *fakeService) Verbs() []Verb {
	f.mu.Lock()
	dl := f.sockDeadline
	name := f.verbName
	f.mu.Unlock()
	return []Verb{{Name: name, Deadline: dl, Handle: f.handle}}
}

func (f *fakeService) handle(ctx context.Context, _ net.Conn, rest string) {
	f.mu.Lock()
	f.lastHandleCtx = ctx
	f.lastRest = rest
	f.mu.Unlock()
}

func (f *fakeService) HandleMsg(kind string, payload cbor.RawMessage) {
	f.mu.Lock()
	p := f.panicOnMsg
	if !p {
		f.handled = append(f.handled, handledMsg{
			kind:    kind,
			payload: append(cbor.RawMessage(nil), payload...),
		})
	}
	f.mu.Unlock()
	if p {
		panic("fakeService: deliberate HandleMsg panic")
	}
}

func (f *fakeService) handledKinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.handled))
	for i, h := range f.handled {
		out[i] = h.kind
	}
	return out
}

// fakeAddr satisfies net.Addr for fakeConn.
type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// fakeConn is a net.Conn that records the last SetDeadline argument.
type fakeConn struct {
	mu       sync.Mutex
	deadline time.Time
}

func (c *fakeConn) Read([]byte) (int, error)    { return 0, nil }
func (c *fakeConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *fakeConn) Close() error                { return nil }
func (c *fakeConn) LocalAddr() net.Addr         { return fakeAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr        { return fakeAddr{} }
func (c *fakeConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func (c *fakeConn) lastDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline
}

func rawN(n int) cbor.RawMessage { return cbor.RawMessage(make([]byte, n)) }

// waitWaiters blocks until the registry has at least n outstanding call waiters.
func waitWaiters(t *testing.T, r *registry, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.waiterMu.Lock()
		got := len(r.waiters)
		r.waiterMu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiter count never reached %d (got %d)", n, got)
		}
		time.Sleep(time.Millisecond)
	}
}

// (a) A duplicate verb claim across two services panics at registration.
func TestRegistry_DupVerbPanics(t *testing.T) {
	r := newRegistry(nil)
	s1 := &fakeService{name: "s1", version: 1, verbName: "dup", outboxCap: 2}
	s2 := &fakeService{name: "s2", version: 1, verbName: "dup", outboxCap: 2}
	r.register(s1)
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected panic on duplicate verb claim across services")
		}
	}()
	r.register(s2)
}

// (b) dispatch decodes/routes a well-formed Msg to HandleMsg. Also proves the
// registry binds itself back into the service at registration.
func TestRegistry_DispatchRoutes(t *testing.T) {
	r := newRegistry(nil)
	s := &fakeService{name: "svc", version: 1, maxPayload: 1024, outboxCap: 2, verbName: "v"}
	r.register(s)
	if s.boundReg != r {
		t.Fatal("register did not bind the registry back into the service")
	}
	pl, err := protocol.MarshalPayload(protocol.OpenURL{URL: "http://x"})
	if err != nil {
		t.Fatal(err)
	}
	r.dispatch(&protocol.Msg{Service: "svc", Kind: "k", Payload: pl})
	if kinds := s.handledKinds(); len(kinds) != 1 || kinds[0] != "k" {
		t.Fatalf("want [k] handled, got %v", kinds)
	}
}

// (c) An oversized Msg.Payload is dropped and the registry stays usable — a
// subsequent in-cap Msg dispatches (EC5, session lives).
func TestRegistry_MaxPayloadDropSessionLives(t *testing.T) {
	r := newRegistry(nil)
	s := &fakeService{name: "svc", version: 1, maxPayload: 4, outboxCap: 2, verbName: "v"}
	r.register(s)
	r.dispatch(&protocol.Msg{Service: "svc", Kind: "big", Payload: rawN(8)})
	r.dispatch(&protocol.Msg{Service: "svc", Kind: "ok", Payload: rawN(2)})
	if kinds := s.handledKinds(); len(kinds) != 1 || kinds[0] != "ok" {
		t.Fatalf("want only [ok] handled (oversized dropped), got %v", kinds)
	}
}

// (d) A panicking HandleMsg drops the frame, the registry survives, and the next
// dispatch works (EC6 agent side).
func TestRegistry_PanicIsolation(t *testing.T) {
	r := newRegistry(nil)
	s := &fakeService{name: "svc", version: 1, maxPayload: 64, outboxCap: 2, verbName: "v", panicOnMsg: true}
	r.register(s)
	r.dispatch(&protocol.Msg{Service: "svc", Kind: "boom", Payload: rawN(1)})

	s.mu.Lock()
	s.panicOnMsg = false
	s.mu.Unlock()
	r.dispatch(&protocol.Msg{Service: "svc", Kind: "ok", Payload: rawN(1)})
	if kinds := s.handledKinds(); len(kinds) != 1 || kinds[0] != "ok" {
		t.Fatalf("registry did not survive a panicking handler, handled=%v", kinds)
	}
}

// (e) Filling the outbox to capacity makes emit return false (DropNewest) with
// no panic, without any drain (S5).
func TestRegistry_OutboxOverflow(t *testing.T) {
	r := newRegistry(nil)
	s := &fakeService{name: "svc", version: 1, maxPayload: 64, outboxCap: 2, verbName: "v"}
	r.register(s)
	for i := 0; i < 2; i++ {
		if !r.emit("svc", "k", nil) {
			t.Fatalf("emit %d within capacity should succeed", i)
		}
	}
	if r.emit("svc", "k", nil) {
		t.Fatal("emit past capacity must return false (DropNewest)")
	}
}

// (f) The release guard: after filling to cap, draining one frame and releasing
// its budget lets the next emit succeed; interleaving (emit; drain+release) for
// 3×cap iterations keeps every emit accepted. A missing/incorrect release fails
// HERE at unit level.
func TestRegistry_BudgetRecycle(t *testing.T) {
	r := newRegistry(nil)
	const capN = 3
	s := &fakeService{name: "svc", version: 1, maxPayload: 64, outboxCap: capN, verbName: "v"}
	r.register(s)

	for i := 0; i < capN; i++ {
		if !r.emit("svc", "k", nil) {
			t.Fatalf("emit %d within capacity should succeed", i)
		}
	}
	if r.emit("svc", "k", nil) {
		t.Fatal("emit past capacity must fail before any release")
	}
	env := <-r.outbox()
	r.release(env.Msg.Service)
	if !r.emit("svc", "k", nil) {
		t.Fatal("emit after one release should be accepted")
	}

	for i := 0; i < 3*capN; i++ {
		env := <-r.outbox()
		r.release(env.Msg.Service)
		if !r.emit("svc", "k", nil) {
			t.Fatalf("interleaved emit %d should be accepted (release recycles budget)", i)
		}
	}
}

// (g) N emit()s advance ONLY the per-service Seq (1..N). There is no port-event
// staleness counter in the registry at all (that counter lives in Server), so
// the isolation is structural — this asserts the per-service Seq progression.
func TestRegistry_SeqIsolation(t *testing.T) {
	r := newRegistry(nil)
	const n = 5
	s := &fakeService{name: "svc", version: 1, maxPayload: 64, outboxCap: n, verbName: "v"}
	r.register(s)
	for i := 0; i < n; i++ {
		if !r.emit("svc", "k", nil) {
			t.Fatalf("emit %d should succeed", i)
		}
	}
	for want := uint64(1); want <= n; want++ {
		env := <-r.outbox()
		if env.Msg.Seq != want {
			t.Fatalf("per-service Seq: want %d, got %d", want, env.Msg.Seq)
		}
	}
}

// (h) call(): success, timeout, no-capacity, stale-epoch and late/duplicate.
func TestRegistry_CallSuccess(t *testing.T) {
	r := newRegistry(nil)
	s := &fakeService{name: "clip", version: 1, maxPayload: 64, outboxCap: 4, verbName: "clip"}
	r.register(s)
	nonce := r.nextNonce()
	want := cbor.RawMessage([]byte{0xAA})
	go func() {
		env := <-r.outbox()
		r.release(env.Msg.Service)
		r.completeCall(nonce, r.epoch(), want)
	}()
	got, err := r.call(context.Background(), "clip", "req", time.Second, 4, nonce, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(got) != 1 || got[0] != 0xAA {
		t.Fatalf("want payload 0xAA, got %v", got)
	}
}

func TestRegistry_CallTimeout(t *testing.T) {
	r := newRegistry(nil)
	s := &fakeService{name: "clip", version: 1, maxPayload: 64, outboxCap: 4, verbName: "clip"}
	r.register(s)
	nonce := r.nextNonce()
	_, err := r.call(context.Background(), "clip", "req", 20*time.Millisecond, 4, nonce, nil)
	if !errors.Is(err, ErrCallTimeout) {
		t.Fatalf("want ErrCallTimeout, got %v", err)
	}
}

func TestRegistry_CallNoWaiterCapacity(t *testing.T) {
	r := newRegistry(nil)
	s := &fakeService{name: "clip", version: 1, maxPayload: 64, outboxCap: 4, verbName: "clip"}
	r.register(s)
	n1 := r.nextNonce()
	go func() { _, _ = r.call(context.Background(), "clip", "req", time.Second, 1, n1, nil) }()
	waitWaiters(t, r, 1)

	n2 := r.nextNonce()
	_, err := r.call(context.Background(), "clip", "req", time.Second, 1, n2, nil)
	if !errors.Is(err, ErrNoWaiterCapacity) {
		t.Fatalf("want ErrNoWaiterCapacity, got %v", err)
	}
	// Unblock the first waiter so its goroutine exits cleanly.
	r.completeCall(n1, r.epoch(), nil)
}

func TestRegistry_CallStaleEpochDropped(t *testing.T) {
	r := newRegistry(nil)
	s := &fakeService{name: "clip", version: 1, maxPayload: 64, outboxCap: 4, verbName: "clip"}
	r.register(s)
	nonce := r.nextNonce()
	resCh := make(chan error, 1)
	go func() {
		_, err := r.call(context.Background(), "clip", "req", 100*time.Millisecond, 4, nonce, nil)
		resCh <- err
	}()
	waitWaiters(t, r, 1)
	// A completeCall with the wrong epoch must be dropped (waiter unsatisfied).
	r.completeCall(nonce, r.epoch()+1, cbor.RawMessage([]byte{0x09}))
	if err := <-resCh; !errors.Is(err, ErrCallTimeout) {
		t.Fatalf("stale-epoch response should NOT satisfy the waiter; want ErrCallTimeout, got %v", err)
	}
}

func TestRegistry_CallLateDuplicateDropped(t *testing.T) {
	r := newRegistry(nil)
	s := &fakeService{name: "clip", version: 1, maxPayload: 64, outboxCap: 4, verbName: "clip"}
	r.register(s)
	nonce := r.nextNonce()
	resCh := make(chan cbor.RawMessage, 1)
	go func() {
		got, _ := r.call(context.Background(), "clip", "req", time.Second, 4, nonce, nil)
		resCh <- got
	}()
	waitWaiters(t, r, 1)
	r.completeCall(nonce, r.epoch(), cbor.RawMessage([]byte{0x01}))
	if got := <-resCh; len(got) != 1 || got[0] != 0x01 {
		t.Fatalf("want first response 0x01, got %v", got)
	}
	// The waiter is gone now; a late/duplicate completeCall is a safe no-op.
	r.completeCall(nonce, r.epoch(), cbor.RawMessage([]byte{0x02}))
}

// (i) Sole-writer: emit's only observable effect is one frame on outbox(); the
// registry exposes no *Encoder (structural — asserted by the type's surface).
func TestRegistry_SoleWriterEnqueuesOnly(t *testing.T) {
	r := newRegistry(nil)
	s := &fakeService{name: "svc", version: 1, maxPayload: 64, outboxCap: 2, verbName: "v"}
	r.register(s)
	if !r.emit("svc", "k", nil) {
		t.Fatal("emit should succeed")
	}
	select {
	case env := <-r.outbox():
		if env.Msg == nil || env.Msg.Service != "svc" {
			t.Fatalf("unexpected frame: %+v", env)
		}
	default:
		t.Fatal("emit did not enqueue onto outbox()")
	}
	select {
	case <-r.outbox():
		t.Fatal("emit enqueued more than one frame")
	default:
	}
}

// (j) hasClient() is false before bindHasClient; after binding it tracks the
// bound getter.
func TestRegistry_HasClientGetter(t *testing.T) {
	r := newRegistry(nil)
	if r.hasClient() {
		t.Fatal("hasClient() must be false before bindHasClient")
	}
	b := false
	r.bindHasClient(func() bool { return b })
	if r.hasClient() {
		t.Fatal("hasClient() should track bound getter (b=false)")
	}
	b = true
	if !r.hasClient() {
		t.Fatal("hasClient() should track bound getter (b=true)")
	}
}

// (k) routeVerb reads Verb.Deadline LIVE (not a registration snapshot), threads
// its ctx into Handle, and returns false for an unknown verb.
func TestRegistry_RouteVerbLiveDeadline(t *testing.T) {
	r := newRegistry(nil)
	s := &fakeService{
		name: "clip", version: 1, maxPayload: 64, outboxCap: 4,
		verbName: "clip", sockDeadline: 11 * time.Second,
	}
	r.register(s)

	// Shorten the socket deadline AFTER registration — routeVerb must apply the
	// new value, proving the deadline is not snapshotted at register time.
	s.mu.Lock()
	s.sockDeadline = 20 * time.Millisecond
	s.mu.Unlock()

	conn := &fakeConn{}
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "threaded")
	before := time.Now()
	if !r.routeVerb(ctx, conn, "clip", "targets") {
		t.Fatal("routeVerb should return true for a claimed verb")
	}

	dl := conn.lastDeadline()
	if dl.After(before.Add(2 * time.Second)) {
		t.Fatalf("deadline was snapshotted (still ~11s): %v after start", dl.Sub(before))
	}

	s.mu.Lock()
	gotCtx, gotRest := s.lastHandleCtx, s.lastRest
	s.mu.Unlock()
	if gotCtx == nil || gotCtx.Value(ctxKey{}) != "threaded" {
		t.Fatal("routeVerb did not thread the given ctx into Handle")
	}
	if gotRest != "targets" {
		t.Fatalf("rest not passed through: %q", gotRest)
	}

	if r.routeVerb(ctx, conn, "unknown", "") {
		t.Fatal("routeVerb should return false for an unknown verb (default-deny)")
	}
}

// (l) An inbound Msg for a service the registry doesn't know is dropped with no
// panic; a subsequent Msg for a known service still dispatches (session lives).
func TestRegistry_UnknownServiceDrop(t *testing.T) {
	r := newRegistry(nil)
	s := &fakeService{name: "svc", version: 1, maxPayload: 64, outboxCap: 2, verbName: "v"}
	r.register(s)
	r.dispatch(&protocol.Msg{Service: "nope", Kind: "x", Payload: rawN(1)})
	r.dispatch(&protocol.Msg{Service: "svc", Kind: "ok", Payload: rawN(1)})
	if kinds := s.handledKinds(); len(kinds) != 1 || kinds[0] != "ok" {
		t.Fatalf("want only [ok] handled (unknown-service dropped), got %v", kinds)
	}
}

// TestDeletionInvariant_NoLegacyEnvelopeFields is the DESIGN §8 deletion grep as
// a test: the v4 hard cut removed Envelope.OpenURL/ClipRequest/ClipResponse/
// Notify, so NO Go source may reference them. The compiler enforces real
// references; this scan is belt-and-suspenders. It is scoped to .go source with
// line comments stripped — historical references survive verbatim in comments
// (e.g. client.go's "the old case env.OpenURL arm") and in the DESIGN docs, so a
// literal tree-wide grep is intentionally NOT empty; only actual code must be.
func TestDeletionInvariant_NoLegacyEnvelopeFields(t *testing.T) {
	root := repoRoot(t)
	// Pattern assembled from fragments so this file's own source can never carry a
	// literal deleted-field reference that self-trips the scan.
	pat := regexp.MustCompile(`env\.(` + "OpenURL|ClipRequest|ClipResponse|Notify" + `)\b`)
	_, self, _, _ := runtime.Caller(0)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || path == self {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx]
			}
			if pat.MatchString(code) {
				t.Errorf("%s:%d references a deleted Envelope field: %q", path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}

// clientHas is true only at exact-version equality (DESIGN S4).
func TestRegistry_ClientHasExactVersion(t *testing.T) {
	r := newRegistry(nil)
	r.register(&fakeService{name: "clip", version: 2, maxPayload: 64, outboxCap: 2, verbName: "clip"})
	if r.clientHas("clip") {
		t.Fatal("clientHas should be false before setClientServices")
	}
	r.setClientServices(map[string]uint32{"clip": 1}) // version mismatch → absent
	if r.clientHas("clip") {
		t.Fatal("version mismatch must be treated as absent")
	}
	r.setClientServices(map[string]uint32{"clip": 2}) // exact match
	if !r.clientHas("clip") {
		t.Fatal("exact-version match should be present")
	}
}
