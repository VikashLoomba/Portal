package agent

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/VikashLoomba/Portal/pkg/protocol"
)

// Clip timeout budget (DESIGN §4.5). These keep the whole paste round trip well
// under the client's HeartbeatTimeout (12s) so a paste never trips a reconnect.
// The three constants are the PRODUCTION DEFAULTS; the clipService copies them
// into overridable per-instance fields at construction so a white-box test can
// shorten any of them on a specific instance and have the change actually take
// effect (production NEVER mutates the fields).
//
// The ordering is: agent clipTimeout (9s) < clipSockDeadline (11s) < the shim's
// clipReadTimeout (13s). The 13s lives in cmd/portald/main.go (package main), is
// remote-side, and is NOT importable from package agent — it is the OUTER bound
// documented here, never referenced in an agent-package assertion.
const (
	// defaultClipTimeout bounds how long handleClip waits on the Mac client for
	// a ClipResponse before answering the shim with "none\n".
	defaultClipTimeout = 9 * time.Second
	// defaultClipSockDeadline is the cmd-socket read/write deadline applied to
	// clip verbs only (open/notify keep their tighter 5s). > clipTimeout so the
	// agent's own "none\n" write always wins the race against the socket deadline.
	defaultClipSockDeadline = 11 * time.Second
	// defaultMaxInflightClip bounds concurrent clip waiters as a DoS guard
	// (DESIGN §7.1): a same-uid process spamming the socket cannot fork unbounded
	// waiters / pending ClipRequest writes on the Serve loop.
	defaultMaxInflightClip = 4
)

// clipService is the compiled-in clip service (DESIGN §4.1). It is the ONLY
// remaining agent→client request/response pair, so it exercises the registry's
// generalized Call helper (DESIGN S9): a `clip <kind> [fmt]` cmd-socket verb
// mints a nonce, emits a ClipRequest up the pipe, and waits for the correlated
// ClipResponse (delivered by HandleMsg → completeCall). Image/text bytes never
// traverse the pipe inline — this frame carries only a SHA — so MaxPayload is
// small.
//
// clipTimeout, clipSockDeadline, and maxInflight are FIELDS (initialized from
// the package const defaults at newClipService), NOT consts, and all three are
// read LIVE at request time: clipTimeout and maxInflight inside handleClip's
// reg.call, clipSockDeadline via Verbs()→routeVerb (which reads Verb.Deadline
// live per connection). Production never mutates them; a white-box test may
// shorten them on a specific instance to exercise the timeout budget (EC9).
type clipService struct {
	reg              *registry
	log              *slog.Logger
	clipTimeout      time.Duration
	clipSockDeadline time.Duration
	maxInflight      int
}

// newClipService constructs the service bound to reg with the production default
// timeout budget. reg.call()/hasClient()/epoch()/nextNonce() give it the
// correlation machinery and subscription view without ever handing it an
// *Encoder (structural sole-writer, DESIGN S5).
func newClipService(reg *registry, log *slog.Logger) *clipService {
	return &clipService{
		reg:              reg,
		log:              log,
		clipTimeout:      defaultClipTimeout,
		clipSockDeadline: defaultClipSockDeadline,
		maxInflight:      defaultMaxInflightClip,
	}
}

func (c *clipService) Name() string    { return "clip" }
func (c *clipService) Version() uint32 { return 1 }
func (c *clipService) MaxPayload() int { return 4096 } // ClipResponse carries only a SHA
func (c *clipService) OutboxCap() int  { return 8 }    // mirrors legacy clipReqCh

// Verbs claims the `clip` cmd-socket verb, building the Verb from the LIVE
// c.clipSockDeadline field on each call. routeVerb (u2) reads Verbs() live per
// connection, so the applied socket deadline always reflects the current field
// value — this is what makes clipSockDeadline genuinely shortenable at request
// time (routeVerb, not handleClip, is the single source of the socket deadline).
func (c *clipService) Verbs() []Verb {
	return []Verb{{Name: "clip", Deadline: c.clipSockDeadline, Handle: c.handleClip}}
}

// HandleMsg processes an inbound client→agent clip Msg. Only "resp" is expected
// (the client answering a ClipRequest). It decodes the ClipResponse and hands it
// to reg.completeCall, where the stale-epoch drop and non-blocking waiter
// delivery live (DESIGN S9). The raw payload is forwarded to the waiter verbatim
// so handleClip re-decodes it into a typed ClipResponse.
func (c *clipService) HandleMsg(kind string, payload cbor.RawMessage) {
	if kind != "resp" {
		return
	}
	resp, err := protocol.UnmarshalPayload[protocol.ClipResponse](payload)
	if err != nil {
		c.log.Warn("clip response decode failed; dropping", "err", err)
		return
	}
	c.reg.completeCall(resp.Nonce, resp.Epoch, payload)
}

// handleClip services a `clip <kind> [fmt]` verb. It carries the legacy
// handleClipReq/writeClipReply semantics VERBATIM except (i) it does NOT call
// conn.SetDeadline — routeVerb already applied the live c.clipSockDeadline
// (single source of the socket deadline); and (ii) it reads the timeout/inflight
// budget from the clipService FIELDS instead of package consts. It answers
// "none\n" — never an error, and NEVER "dropped\n" (S5) — on every adverse path
// (no client, inflight cap hit, outbox full, timeout, ctx cancel) so the shim
// falls through cleanly to the real binary. The image/text bytes themselves
// cross out-of-band (clipupload); this socket only carries the SHA. ctx is
// threaded from handleCmdConn into reg.call for the adverse-path handling.
func (c *clipService) handleClip(ctx context.Context, conn net.Conn, rest string) {
	// Parse the kind/format off the tab-framed remainder. Reject unknown shapes
	// to preserve default-deny.
	var kind, format string
	switch rest {
	case "targets":
		kind = "targets"
	case "text":
		kind = "text"
	case "image\tpng":
		kind, format = "image", "png"
	default:
		_, _ = conn.Write([]byte("rejected\n"))
		return
	}

	// Gate on both a subscribed client AND that client advertising clip@1
	// (DESIGN S4). Either missing ⇒ answer "none\n" immediately rather than
	// making the shim eat the full timeout.
	if !(c.reg.hasClient() && c.reg.clientHas("clip")) {
		_, _ = conn.Write([]byte("none\n"))
		return
	}

	nonce := c.reg.nextNonce()
	payload, err := protocol.MarshalPayload(protocol.ClipRequest{
		Nonce: nonce, Epoch: c.reg.epoch(), Kind: kind, Format: format,
	})
	if err != nil {
		// A tiny nonce/kind struct never fails to marshal; treat the impossible
		// as an adverse path rather than a crash.
		c.log.Warn("clip request marshal failed", "err", err)
		_, _ = conn.Write([]byte("none\n"))
		return
	}

	// call mints the waiter, emits the request via the Serve loop (the sole frame
	// writer), and waits. ErrNoWaiterCapacity (cap hit), ErrCallTimeout (outbox
	// full or no response before clipTimeout), or ctx cancel ⇒ "none\n".
	respRaw, err := c.reg.call(ctx, "clip", "req", c.clipTimeout, c.maxInflight, nonce, payload)
	if err != nil {
		_, _ = conn.Write([]byte("none\n"))
		return
	}
	resp, err := protocol.UnmarshalPayload[protocol.ClipResponse](respRaw)
	if err != nil {
		_, _ = conn.Write([]byte("none\n"))
		return
	}
	c.writeClipReply(conn, kind, &resp)
}

// writeClipReply maps a ClipResponse to the byte-exact socket reply portald clip
// expects. Anything short of an affirmative answer is "none\n". Preserved
// verbatim from the legacy Server.writeClipReply.
func (c *clipService) writeClipReply(conn net.Conn, kind string, resp *protocol.ClipResponse) {
	if resp == nil || !resp.OK {
		_, _ = conn.Write([]byte("none\n"))
		return
	}
	switch kind {
	case "targets":
		if resp.Has {
			// Advertise the CANONICAL kind the Mac decided ("image" or "text").
			// portald clip targets maps this to the tool-specific target line(s)
			// its caller (xclip vs wl-paste) greps for — the agent stays
			// tool-agnostic. Default to image if the Mac left Kind empty (an
			// older Mac that only ever reported image availability).
			k := resp.Kind
			if k != "image" && k != "text" {
				k = "image"
			}
			_, _ = conn.Write([]byte("ok\t" + k + "\n"))
		} else {
			_, _ = conn.Write([]byte("none\n"))
		}
	case "image", "text":
		if resp.SHA != "" {
			_, _ = conn.Write([]byte("ok\t" + resp.SHA + "\n"))
		} else {
			_, _ = conn.Write([]byte("none\n"))
		}
	default:
		_, _ = conn.Write([]byte("none\n"))
	}
}
