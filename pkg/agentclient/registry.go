package agentclient

import (
	"errors"
	"io"
	"log/slog"
	"sync"

	"github.com/fxamacker/cbor/v2"

	"github.com/VikashLoomba/Portal/pkg/protocol"
)

// ErrNotConnected is returned by send when no pipe is up — identical in intent
// to today's SendClipResponse "not connected" contract.
var ErrNotConnected = errors.New("agentclient: not connected")

// HandlerSpec declares a client-side service handler. Decode turns the
// registry-stamped Msg.Seq (DESIGN S3 — the per-service monotonic correlation
// counter, which the agent NEVER duplicates into the payload) plus the raw
// payload into a typed EngineEvent; Deliver is the NON-BLOCKING sink (clip ⇒
// publishClip cap-8, notify ⇒ publishNotify cap-16 preserving DESIGN S10 QoS;
// openurl ⇒ publish shared events). Channel capacity + drop policy are thus
// DECLARED per handler via its Deliver target (the dedicated channels created in
// Client.New). Handlers that do not correlate on seq (openurl/clip) ignore it.
type HandlerSpec struct {
	Service    string
	Version    uint32
	MaxPayload int
	Decode     func(seq uint64, payload cbor.RawMessage) (EngineEvent, error)
	Deliver    func(EngineEvent)
}

// handlerEntry is the registry's per-service bookkeeping. dormant is set when
// the agent lacks the service (or advertises a different version) — DESIGN S4.
type handlerEntry struct {
	spec    HandlerSpec
	dormant bool
}

// registry is the client-side service registry: Msg demux routing (per-service
// decode, payload cap, recover, non-blocking QoS delivery), the client→agent
// send path, and dormant-handler negotiation.
type registry struct {
	log *slog.Logger

	mu       sync.Mutex
	handlers map[string]*handlerEntry
}

func newRegistry(log *slog.Logger) *registry {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &registry{log: log, handlers: map[string]*handlerEntry{}}
}

// register records a handler by its service name.
func (r *registry) register(spec HandlerSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[spec.Service] = &handlerEntry{spec: spec}
}

// services returns the client's registered service→version map, for Hello.
func (r *registry) services() map[string]uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[string]uint32, len(r.handlers))
	for name, h := range r.handlers {
		m[name] = h.spec.Version
	}
	return m
}

// setAgentServices records the agent's advertised services (from HelloAck). Any
// registered handler whose service is absent OR at a different version is marked
// dormant with ONE warning (DESIGN S4 exact-equality).
func (r *registry) setAgentServices(m map[string]uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, h := range r.handlers {
		av, ok := m[name]
		if !ok || av != h.spec.Version {
			h.dormant = true
			r.log.Warn("agent lacks service or advertises mismatched version; handler dormant",
				"service", name, "client_version", h.spec.Version, "agent_version", av, "present", ok)
			continue
		}
		h.dormant = false
	}
}

// dispatch routes an inbound agent→client Msg to its handler. An unknown or
// dormant service is dropped (session lives); an oversized payload is dropped
// with a warning (S6); a decode error is a logged drop (session lives). Decode
// and the non-blocking Deliver run under recover so a panic drops only that
// frame (S7). The demux never blocks (Deliver is non-blocking — S10).
func (r *registry) dispatch(m *protocol.Msg) {
	r.mu.Lock()
	h, ok := r.handlers[m.Service]
	dormant := ok && h.dormant
	r.mu.Unlock()
	if !ok || dormant {
		r.log.Warn("dropping Msg for unknown/dormant service", "service", m.Service, "kind", m.Kind)
		return
	}
	if len(m.Payload) > h.spec.MaxPayload {
		r.log.Warn("dropping oversized service payload; session lives",
			"service", m.Service, "kind", m.Kind, "size", len(m.Payload), "max", h.spec.MaxPayload)
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("client service handler panicked; dropping frame",
				"service", m.Service, "kind", m.Kind, "panic", rec)
		}
	}()
	ev, err := h.spec.Decode(m.Seq, m.Payload)
	if err != nil {
		r.log.Warn("dropping Msg: payload decode failed; session lives",
			"service", m.Service, "kind", m.Kind, "err", err)
		return
	}
	h.spec.Deliver(ev)
}

// send writes a client→agent service frame. Returns ErrNotConnected when enc is
// nil (matches today's SendClipResponse "not connected" contract).
func (r *registry) send(enc *protocol.Encoder, service, kind string, payload cbor.RawMessage) error {
	if enc == nil {
		return ErrNotConnected
	}
	return enc.Write(&protocol.Envelope{Msg: &protocol.Msg{
		Service: service, Kind: kind, Payload: payload,
	}})
}
