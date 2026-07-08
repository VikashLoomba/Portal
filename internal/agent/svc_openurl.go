package agent

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/VikashLoomba/Portal/pkg/protocol"
)

// openURLService is the compiled-in openurl service (DESIGN §4.1): it relays
// http/https URLs from `portald open <url>` (typically the ~/.local/bin/xdg-open
// wrapper) up to the Mac client, which opens them in the default browser. It is
// agent→client ONLY, so HandleMsg is a no-op — the client never sends an openurl
// Msg down the pipe. URLs are tiny, so MaxPayload is small; OutboxCap mirrors the
// legacy openURLCh buffer (8).
type openURLService struct {
	reg *registry
	log *slog.Logger
}

// newOpenURLService constructs the service bound to reg. reg.emit()/hasClient()
// give it a frame sink and subscription view without ever handing it an
// *Encoder (structural sole-writer, DESIGN S5).
func newOpenURLService(reg *registry, log *slog.Logger) *openURLService {
	return &openURLService{reg: reg, log: log}
}

func (o *openURLService) Name() string    { return "openurl" }
func (o *openURLService) Version() uint32 { return 1 }
func (o *openURLService) MaxPayload() int { return 4096 } // URLs are tiny
func (o *openURLService) OutboxCap() int  { return 8 }    // mirrors legacy openURLCh

// Verbs claims the `open` cmd-socket verb with a 5s socket deadline (byte-for-byte
// the legacy open path, DESIGN §4.5). routeVerb applies the deadline before
// calling handleOpen, so handleOpen never sets its own.
func (o *openURLService) Verbs() []Verb {
	return []Verb{{Name: "open", Deadline: 5 * time.Second, Handle: o.handleOpen}}
}

// HandleMsg is a no-op: openurl is agent→client only.
func (o *openURLService) HandleMsg(kind string, payload cbor.RawMessage) {}

// handleOpen carries the legacy handleOpenReq semantics VERBATIM: trim; reject a
// non-http/https URL with "rejected\n"; gate on hasClient()&&clientHas("openurl")
// (both must hold, else "no-client\n"); otherwise emit the URL as an openurl
// Msg and answer "ok\n" on admission or "dropped\n" on a full outbox. ctx is
// unused — openurl has no Call/cancel path — and handleOpen does NOT set the
// socket deadline (routeVerb already applied the verb's 5s deadline).
func (o *openURLService) handleOpen(ctx context.Context, conn net.Conn, rest string) {
	rawURL := strings.TrimSpace(rest)
	if rawURL == "" {
		return
	}
	// Only relay http/https URLs. Defense-in-depth: the Mac validates too, but
	// rejecting here keeps non-http URLs off the wire entirely.
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		_, _ = conn.Write([]byte("rejected\n"))
		return
	}
	// Gate on both a subscribed client AND that client advertising openurl@1
	// (DESIGN S4). Either missing ⇒ answer exactly as the legacy !hasClient path.
	if !(o.reg.hasClient() && o.reg.clientHas("openurl")) {
		_, _ = conn.Write([]byte("no-client\n"))
		return
	}
	payload, err := protocol.MarshalPayload(protocol.OpenURL{URL: rawURL})
	if err != nil {
		// A URL string never fails to marshal; treat the impossible as a drop
		// rather than falsely claiming success.
		o.log.Warn("openurl payload marshal failed", "err", err)
		_, _ = conn.Write([]byte("dropped\n"))
		return
	}
	if o.reg.emit("openurl", "open", payload) {
		_, _ = conn.Write([]byte("ok\n"))
	} else {
		// Full outbox — DropNewest (S5). Report the drop rather than claim success.
		_, _ = conn.Write([]byte("dropped\n"))
	}
}
