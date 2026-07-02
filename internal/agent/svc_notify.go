package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/VikashLoomba/Portal/internal/protocol"
)

// notifyBodyMax bounds the inbound `notify` body on the cmd socket. The notify
// verb crosses the 1 MiB CBOR MaxFrameBytes downstream, but the notification
// surface (title/body) is tiny — cap it well under the 4096 socket read so a
// malformed/oversized body is rejected before relay.
const notifyBodyMax = 3072

// notifyWire is the JSON shape `portald notify` writes on the cmd socket. It is
// already classified on the remote side (the structured-hook vs generic split
// happens in `portald notify`), so the agent just validates/bounds it and
// relays it up the pipe — it does NOT re-interpret the payload. Verified
// distinguishes a real Claude Code hook (true) from an arbitrary caller (false,
// rendered "[unverified]" on the Mac). Fields beyond these are ignored.
type notifyWire struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Subtitle string `json:"subtitle"`
	Urgency  uint8  `json:"urgency"`
	Verified bool   `json:"verified"`
	Source   string `json:"source"`
	Sound    string `json:"sound"`
}

// notifyService is the compiled-in notify service (DESIGN §4.1): it relays a
// remote notification (a Claude Code hook firing `portald notify --hook`, or a
// generic `portald notify`) up to the Mac client, which raises it natively. It
// is agent→client ONLY, so HandleMsg is a no-op — the client never sends a
// notify Msg down the pipe. The notification surface is tiny, so MaxPayload is
// small; OutboxCap mirrors the legacy notifyCh buffer (8).
type notifyService struct {
	reg *registry
	log *slog.Logger
}

// newNotifyService constructs the service bound to reg. reg.emit()/hasClient()
// give it a frame sink and subscription view without ever handing it an
// *Encoder (structural sole-writer, DESIGN S5).
func newNotifyService(reg *registry, log *slog.Logger) *notifyService {
	return &notifyService{reg: reg, log: log}
}

func (n *notifyService) Name() string    { return "notify" }
func (n *notifyService) Version() uint32 { return 1 }
func (n *notifyService) MaxPayload() int { return 4096 } // title/body only
func (n *notifyService) OutboxCap() int  { return 8 }    // mirrors legacy notifyCh

// Verbs claims the `notify` cmd-socket verb with a 5s socket deadline (byte-for-byte
// the legacy notify path, DESIGN §4.5). routeVerb applies the deadline before
// calling handleNotify, so handleNotify never sets its own.
func (n *notifyService) Verbs() []Verb {
	return []Verb{{Name: "notify", Deadline: 5 * time.Second, Handle: n.handleNotify}}
}

// HandleMsg is a no-op: notify is agent→client only.
func (n *notifyService) HandleMsg(kind string, payload cbor.RawMessage) {}

// handleNotify carries the legacy handleNotifyReq semantics VERBATIM: trim;
// reject an empty or oversized body with "rejected\n"; parse the bounded JSON
// (malformed ⇒ "rejected\n"); reject a blank title with "rejected\n"; gate on
// hasClient()&&clientHas("notify") (both must hold, else "no-client\n");
// otherwise relay the classified Notify (classify/[unverified] semantics are
// decided upstream in `portald notify` and carried verbatim in Verified —
// passthrough only, DESIGN §7.2) as a notify Msg and answer "ok\n" on admission
// or "dropped\n" on a full outbox (a missed notification is non-fatal, so the
// drop is reported rather than falsely claiming success). ctx is unused — notify
// is fire-and-forget with no Call/cancel path — and handleNotify does NOT set
// the socket deadline (routeVerb already applied the verb's 5s deadline).
func (n *notifyService) handleNotify(ctx context.Context, conn net.Conn, rest string) {
	body := strings.TrimSpace(rest)
	if body == "" || len(body) > notifyBodyMax {
		_, _ = conn.Write([]byte("rejected\n"))
		return
	}
	var w notifyWire
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		_, _ = conn.Write([]byte("rejected\n"))
		return
	}
	// A notification with no title is unusable — reject rather than relay an
	// empty frame the Mac would render as a blank notification.
	if strings.TrimSpace(w.Title) == "" {
		_, _ = conn.Write([]byte("rejected\n"))
		return
	}
	// Gate on both a subscribed client AND that client advertising notify@1
	// (DESIGN S4). Either missing ⇒ answer exactly as the legacy !hasClient path.
	if !(n.reg.hasClient() && n.reg.clientHas("notify")) {
		_, _ = conn.Write([]byte("no-client\n"))
		return
	}
	// Copy all fields verbatim; Verified carries the upstream classify decision.
	// Seq is stamped by the registry (never s.seq — DESIGN S3).
	payload, err := protocol.MarshalPayload(protocol.Notify{
		Title:    w.Title,
		Body:     w.Body,
		Subtitle: w.Subtitle,
		Urgency:  w.Urgency,
		Verified: w.Verified,
		Source:   w.Source,
		Sound:    w.Sound,
	})
	if err != nil {
		// A tiny title/body struct never fails to marshal; treat the impossible
		// as a drop rather than falsely claiming success.
		n.log.Warn("notify payload marshal failed", "err", err)
		_, _ = conn.Write([]byte("dropped\n"))
		return
	}
	if n.reg.emit("notify", "event", payload) {
		_, _ = conn.Write([]byte("ok\n"))
	} else {
		// Full outbox — DropNewest (S5). Report the drop rather than claim success.
		_, _ = conn.Write([]byte("dropped\n"))
	}
}
