package agent

import (
	"context"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/VikashLoomba/Portal/pkg/protocol"
)

// Clipboard-write timeout budget (DESIGN-clipboard-write-interception §4.4).
// These constants are production defaults copied into overridable
// clipWriteService fields at construction. The fields are read live so
// white-box tests can shorten one service instance; production never mutates
// them.
//
// The ordering is: agent clipWriteTimeout (9s) < clipWriteSockDeadline (11s) <
// portald's clipReadTimeout (13s). The 13s outer bound lives in
// cmd/portald/main.go and is not importable from package agent.
const (
	defaultClipWriteTimeout      = 9 * time.Second
	defaultClipWriteSockDeadline = 11 * time.Second
	defaultMaxInflightClipWrite  = 4

	// maxClipWriteBytes mirrors internal/clipupload.MaxUploadBytes and applies
	// to both text and image writes. It is duplicated because pkg packages must
	// not import internal packages.
	maxClipWriteBytes = 8 << 20
)

// clipWriteSHARE is the only accepted content-address shape. The Mac
// reconstructs the side-channel path from this value rather than accepting a
// path from the wire.
var clipWriteSHARE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// clipWriteService is the compiled-in clipboard-write request/response
// service. A valid box-local copy request becomes a ClipWriteRequest emitted
// through ServiceHost.Call and correlated with a ClipWriteResponse by nonce
// and epoch. Clipboard bytes remain out-of-band in a content-addressed file.
type clipWriteService struct {
	host                  ServiceHost
	log                   *slog.Logger
	clipWriteTimeout      time.Duration
	clipWriteSockDeadline time.Duration
	maxInflight           int
}

// newClipWriteService constructs a clipwrite service with the production
// timeout and waiter-capacity defaults.
func newClipWriteService(host ServiceHost, log *slog.Logger) *clipWriteService {
	return &clipWriteService{
		host:                  host,
		log:                   log,
		clipWriteTimeout:      defaultClipWriteTimeout,
		clipWriteSockDeadline: defaultClipWriteSockDeadline,
		maxInflight:           defaultMaxInflightClipWrite,
	}
}

func (c *clipWriteService) Name() string    { return "clipwrite" }
func (c *clipWriteService) Version() uint32 { return 1 }
func (c *clipWriteService) MaxPayload() int { return 4096 }
func (c *clipWriteService) OutboxCap() int  { return 8 }

// Verbs claims the `copy` cmd-socket verb using the live socket-deadline field.
func (c *clipWriteService) Verbs() []Verb {
	return []Verb{{Name: "copy", Deadline: c.clipWriteSockDeadline, Handle: c.handleCopy}}
}

// HandleMsg processes client→agent clipboard-write responses. Complete
// enforces the registry epoch and delivers the payload without blocking the
// Serve loop.
func (c *clipWriteService) HandleMsg(kind string, payload cbor.RawMessage) {
	if kind != "resp" {
		return
	}
	resp, err := protocol.UnmarshalPayload[protocol.ClipWriteResponse](payload)
	if err != nil {
		c.log.Warn("clipwrite response decode failed; dropping", "err", err)
		return
	}
	c.host.Complete(resp.Nonce, resp.Epoch, payload)
}

// handleCopy services one default-deny clipboard-write command. Invalid local
// grammar is rejected before client gating. Every recognized adverse path
// maps to the single stable "none\n" reply.
func (c *clipWriteService) handleCopy(ctx context.Context, conn net.Conn, rest string) {
	kind, format, sha, size, ok := parseCopyRequest(rest)
	if !ok {
		_, _ = conn.Write([]byte("rejected\n"))
		return
	}

	if !(c.host.HasClient() && c.host.ClientHas("clipwrite")) {
		_, _ = conn.Write([]byte("none\n"))
		return
	}

	respRaw, err := c.host.Call(ctx, "clipwrite", "req", c.clipWriteTimeout, c.maxInflight, func(nonce, epoch uint64) cbor.RawMessage {
		payload, err := protocol.MarshalPayload(protocol.ClipWriteRequest{
			Nonce: nonce, Epoch: epoch, Kind: kind, Format: format, SHA: sha, Size: size,
		})
		if err != nil {
			c.log.Warn("clipwrite request marshal failed", "err", err)
			return nil
		}
		return payload
	})
	if err != nil {
		_, _ = conn.Write([]byte("none\n"))
		return
	}

	resp, err := protocol.UnmarshalPayload[protocol.ClipWriteResponse](respRaw)
	if err != nil || !resp.OK {
		_, _ = conn.Write([]byte("none\n"))
		return
	}
	_, _ = conn.Write([]byte("ok\n"))
}

func parseCopyRequest(rest string) (kind, format, sha string, size int64, ok bool) {
	fields := strings.Split(rest, "\t")
	if len(fields) == 1 && fields[0] == "clear" {
		return "clear", "", "", 0, true
	}

	var sizeField string
	switch {
	case len(fields) == 3 && fields[0] == "text":
		kind, sha, sizeField = "text", fields[1], fields[2]
	case len(fields) == 4 && fields[0] == "image" && fields[1] == "png":
		kind, format, sha, sizeField = "image", "png", fields[2], fields[3]
	default:
		return "", "", "", 0, false
	}
	if !clipWriteSHARE.MatchString(sha) {
		return "", "", "", 0, false
	}
	size, err := strconv.ParseInt(sizeField, 10, 64)
	if err != nil || size <= 0 || size > maxClipWriteBytes || strconv.FormatInt(size, 10) != sizeField {
		return "", "", "", 0, false
	}
	return kind, format, sha, size, true
}
