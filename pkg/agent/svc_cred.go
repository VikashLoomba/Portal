package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/VikashLoomba/Portal/pkg/protocol"
)

// Credential timeout budget (DESIGN-cred C10). These constants are production
// defaults copied into overridable credService fields at construction. The
// fields are read live so white-box tests can shorten one service instance;
// production never mutates them.
//
// The ordering is: agent credTimeout (130s) < credSockDeadline (135s) < the
// portald keychain socket-read deadline (140s, introduced with the u3 caller).
const (
	defaultCredTimeout      = 130 * time.Second
	defaultCredSockDeadline = 135 * time.Second
	defaultMaxInflightCred  = 1

	maxCredLabelBytes   = 200
	maxCredContextBytes = 300
)

// CredShimReq is the CBOR payload inside the box-local
// `cred\t<base64(CBOR)>` cmd-socket request. It is IPC between portald and the
// agent on the same box; it is not a Mac↔box protocol frame and never rides
// protocol.Msg.Payload. Base64 keeps label, target, and requester bytes from
// corrupting the cmd socket's tab/newline framing.
type CredShimReq struct {
	Label     string `cbor:"l"`
	Mode      string `cbor:"m"`
	Target    string `cbor:"t,omitempty"`
	Requester string `cbor:"r,omitempty"`
}

// credService is the compiled-in credential request/response service. A valid
// box-local request is translated into a protocol.CredRequest, emitted through
// ServiceHost.Call, and correlated with a protocol.CredResponse by nonce and
// epoch. maxInflight is one because only one human approval dialog may pend;
// the second caller receives an immediate busy denial instead of a queue.
// Secrets return inline over the authenticated pipe and cmd socket, never via
// a side-channel file.
//
// credTimeout, credSockDeadline, and maxInflight are fields initialized from
// production defaults and read live at request time, matching clipService's
// white-box timeout seam.
type credService struct {
	host             ServiceHost
	log              *slog.Logger
	credTimeout      time.Duration
	credSockDeadline time.Duration
	maxInflight      int
}

// newCredService constructs a cred service with the production timeout and
// waiter-capacity defaults.
func newCredService(host ServiceHost, log *slog.Logger) *credService {
	return &credService{
		host:             host,
		log:              log,
		credTimeout:      defaultCredTimeout,
		credSockDeadline: defaultCredSockDeadline,
		maxInflight:      defaultMaxInflightCred,
	}
}

func (c *credService) Name() string    { return "cred" }
func (c *credService) Version() uint32 { return 1 }
func (c *credService) MaxPayload() int { return 8192 }
func (c *credService) OutboxCap() int  { return 2 }

// Verbs claims the `cred` cmd-socket verb using the live socket-deadline field.
func (c *credService) Verbs() []Verb {
	return []Verb{{Name: "cred", Deadline: c.credSockDeadline, Handle: c.handleCred}}
}

// HandleMsg processes client→agent credential responses. Malformed or
// unexpected frames are dropped; a valid response is handed to Complete, which
// enforces the registry epoch and delivers it without blocking the Serve loop.
func (c *credService) HandleMsg(kind string, payload cbor.RawMessage) {
	if kind != "resp" {
		return
	}
	resp, err := protocol.UnmarshalPayload[protocol.CredResponse](payload)
	if err != nil {
		c.log.Warn("cred response decode failed; dropping", "err", err)
		return
	}
	c.host.Complete(resp.Nonce, resp.Epoch, payload)
}

// handleCred services one base64-wrapped CredShimReq. Every recognized adverse
// path returns a grammar-preserving deny line: invalid local input is
// label-invalid, a missing cred-capable client is no-client, waiter saturation
// is busy, and every other Call failure is timeout.
func (c *credService) handleCred(ctx context.Context, conn net.Conn, rest string) {
	req, ok := decodeCredShimReq(rest)
	if !ok {
		writeCredDeny(conn, "label-invalid")
		return
	}

	if !(c.host.HasClient() && c.host.ClientHas("cred")) {
		writeCredDeny(conn, "no-client")
		return
	}

	respRaw, err := c.host.Call(ctx, "cred", "req", c.credTimeout, c.maxInflight, func(nonce, epoch uint64) cbor.RawMessage {
		payload, err := protocol.MarshalPayload(protocol.CredRequest{
			Nonce: nonce, Epoch: epoch, Label: req.Label, Requester: req.Requester,
			Mode: req.Mode, Target: req.Target,
		})
		if err != nil {
			c.log.Warn("cred request marshal failed", "err", err)
			return nil
		}
		return payload
	})
	if err != nil {
		if errors.Is(err, ErrNoWaiterCapacity) {
			writeCredDeny(conn, "busy")
		} else {
			writeCredDeny(conn, "timeout")
		}
		return
	}

	resp, err := protocol.UnmarshalPayload[protocol.CredResponse](respRaw)
	if err != nil {
		writeCredDeny(conn, "timeout")
		return
	}
	c.writeCredReply(conn, &resp)
}

func decodeCredShimReq(encoded string) (CredShimReq, bool) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return CredShimReq{}, false
	}
	req, err := protocol.UnmarshalPayload[CredShimReq](raw)
	if err != nil || !validCredShimReq(req) {
		return CredShimReq{}, false
	}
	return req, true
}

func validCredShimReq(req CredShimReq) bool {
	if req.Label == "" || len(req.Label) > maxCredLabelBytes {
		return false
	}
	if len(req.Requester) > maxCredContextBytes || len(req.Target) > maxCredContextBytes {
		return false
	}
	switch req.Mode {
	case "env", "stdin", "askpass":
		return true
	default:
		return false
	}
}

// writeCredReply writes the binary-safe cmd-socket response. A denied response
// with no reason falls back to the stable "denied" token.
func (c *credService) writeCredReply(conn net.Conn, resp *protocol.CredResponse) {
	if resp == nil || !resp.OK {
		reason := "denied"
		if resp != nil && resp.Err != "" {
			reason = resp.Err
		}
		writeCredDeny(conn, reason)
		return
	}
	secret := base64.StdEncoding.EncodeToString(resp.Secret)
	_, _ = conn.Write([]byte("ok\t" + secret + "\n"))
}

func writeCredDeny(conn net.Conn, reason string) {
	if reason == "" {
		reason = "denied"
	}
	_, _ = conn.Write([]byte("deny\t" + reason + "\n"))
}
