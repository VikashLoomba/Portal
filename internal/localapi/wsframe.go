// In-tree WebSocket framing for DESIGN-exec-bootstrap.md §6.

package localapi

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// wsUpgrade validates all WebSocket headers before hijack so callers can still
// write HTTP errors. It uses ResponseController because localapi's
// envelopeWriter only promotes http.ResponseWriter's interface method set;
// direct http.Hijacker assertions fail there, while ResponseController follows
// Unwrap like events.go's Flush path.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.ReadWriter, error) {
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
	if _, err := fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsAccept(key)); err != nil {
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

type wsOpcode byte

const (
	opContinuation wsOpcode = 0x0
	opText         wsOpcode = 0x1
	opBinary       wsOpcode = 0x2
	opClose        wsOpcode = 0x8
	opPing         wsOpcode = 0x9
	opPong         wsOpcode = 0xA
)

const wsMaxPayload = 16 << 20

// wsReadMessage accepts one complete WebSocket frame only: FIN must be set and
// continuation frames are rejected because the in-tree client never fragments.
// Client frames MUST be masked per RFC 6455; unmasked frames are protocol
// errors.
func wsReadMessage(rw *bufio.ReadWriter) (op wsOpcode, payload []byte, err error) {
	var header [2]byte
	if _, err := io.ReadFull(rw, header[:]); err != nil {
		return 0, nil, err
	}

	fin := header[0]&0x80 != 0
	if header[0]&0x70 != 0 {
		return 0, nil, errors.New("websocket: reserved bits set")
	}
	op = wsOpcode(header[0] & 0x0f)
	if !validOpcode(op) {
		return 0, nil, fmt.Errorf("websocket: reserved opcode 0x%x", byte(op))
	}
	if op == opContinuation {
		return 0, nil, errors.New("websocket: continuation frames are unsupported")
	}
	if !fin {
		return 0, nil, errors.New("websocket: fragmented messages are unsupported")
	}

	masked := header[1]&0x80 != 0
	if !masked {
		return 0, nil, errors.New("websocket: client frame is unmasked")
	}

	n, err := wsReadPayloadLen(rw, header[1]&0x7f)
	if err != nil {
		return 0, nil, err
	}
	if n > wsMaxPayload {
		return 0, nil, fmt.Errorf("websocket: payload length %d exceeds limit", n)
	}

	var mask [4]byte
	if _, err := io.ReadFull(rw, mask[:]); err != nil {
		return 0, nil, err
	}
	payload = make([]byte, int(n))
	if _, err := io.ReadFull(rw, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return op, payload, nil
}

func validOpcode(op wsOpcode) bool {
	switch op {
	case opContinuation, opText, opBinary, opClose, opPing, opPong:
		return true
	default:
		return false
	}
}

func wsReadPayloadLen(r io.Reader, len7 byte) (uint64, error) {
	switch len7 {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		return uint64(binary.BigEndian.Uint16(b[:])), nil
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		return binary.BigEndian.Uint64(b[:]), nil
	default:
		return uint64(len7), nil
	}
}

// wsWriteBinary writes one unmasked, FIN-set binary server frame for a complete
// message payload; callers must not pass fragmented message chunks.
func wsWriteBinary(w io.Writer, payload []byte) error {
	return wsWriteFrame(w, opBinary, payload)
}

// wsWritePong writes one unmasked server pong frame with the caller-supplied
// control payload.
func wsWritePong(w io.Writer, payload []byte) error {
	return wsWriteFrame(w, opPong, payload)
}

// wsWriteClose writes one unmasked server close frame; the status code is
// encoded in the two-byte RFC 6455 close payload prefix.
func wsWriteClose(w io.Writer, code uint16, reason string) error {
	payload := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(payload, code)
	payload = append(payload, reason...)
	return wsWriteFrame(w, opClose, payload)
}

// wsWriteFrame writes a single unfragmented server frame and never masks it;
// RFC 6455 masking is required only on client-to-server frames.
func wsWriteFrame(w io.Writer, op wsOpcode, payload []byte) error {
	header := []byte{0x80 | byte(op)}
	n := len(payload)
	switch {
	case n <= 125:
		header = append(header, byte(n))
	case n <= 0xffff:
		header = append(header, 126, 0, 0)
		binary.BigEndian.PutUint16(header[2:4], uint16(n))
	default:
		header = append(header, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[2:10], uint64(n))
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}
