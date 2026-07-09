// In-tree WebSocket framing for DESIGN-exec-bootstrap.md §6.

package localapi

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/VikashLoomba/Portal/pkg/wsbits"
)

// wsUpgrade validates all WebSocket headers before hijack so callers can still
// write HTTP errors. It uses ResponseController because localapi's
// envelopeWriter only promotes http.ResponseWriter's interface method set;
// direct http.Hijacker assertions fail there, while ResponseController follows
// Unwrap like events.go's Flush path.
func wsUpgrade(w http.ResponseWriter, r *http.Request, extraHeaders ...string) (net.Conn, *bufio.ReadWriter, error) {
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if !headerContainsToken(r.Header.Values("Connection"), "upgrade") {
		return nil, nil, errors.New("websocket: missing Connection upgrade")
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return nil, nil, errors.New("websocket: missing Upgrade websocket")
	}
	if strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")) != "13" {
		return nil, nil, errors.New("websocket: unsupported version")
	}
	if key == "" {
		return nil, nil, errors.New("websocket: missing Sec-WebSocket-Key")
	}

	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, nil, err
	}
	if _, err := fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n", wsbits.AcceptKey(key)); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	for _, hdr := range extraHeaders {
		if _, err := fmt.Fprintf(brw, "%s\r\n", hdr); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
	}
	if _, err := fmt.Fprint(brw, "\r\n"); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := brw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, brw, nil
}

func headerContainsToken(values []string, want string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}
