// Package wsbits is not a general WebSocket library — exactly what the exec subprotocol needs.
package wsbits

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

type Opcode byte

const (
	OpContinuation Opcode = 0x0
	OpText         Opcode = 0x1
	OpBinary       Opcode = 0x2
	OpClose        Opcode = 0x8
	OpPing         Opcode = 0x9
	OpPong         Opcode = 0xA
)

const GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const MaxPayload = 16 << 20

func AcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + GUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// ReadFrame accepts one complete frame only; fragmentation is rejected because
// exec frames are already message-sized.
func ReadFrame(r io.Reader, requireMasked bool) (Opcode, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}

	fin := header[0]&0x80 != 0
	if header[0]&0x70 != 0 {
		return 0, nil, errors.New("websocket: reserved bits set")
	}
	op := Opcode(header[0] & 0x0f)
	if !validOpcode(op) {
		return 0, nil, fmt.Errorf("websocket: reserved opcode 0x%x", byte(op))
	}
	if op == OpContinuation {
		return 0, nil, errors.New("websocket: continuation frames are unsupported")
	}
	if !fin {
		return 0, nil, errors.New("websocket: fragmented messages are unsupported")
	}

	masked := header[1]&0x80 != 0
	switch {
	case requireMasked && !masked:
		return 0, nil, errors.New("websocket: client frame is unmasked")
	case !requireMasked && masked:
		return 0, nil, errors.New("websocket: server frame is masked")
	}

	n, err := readPayloadLen(r, header[1]&0x7f)
	if err != nil {
		return 0, nil, err
	}
	if n > MaxPayload {
		return 0, nil, fmt.Errorf("websocket: payload length %d exceeds limit", n)
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, int(n))
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

func WriteFrame(w io.Writer, op Opcode, payload []byte, mask bool) error {
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

	if !mask {
		if err := WriteFull(w, header); err != nil {
			return err
		}
		return WriteFull(w, payload)
	}

	header[1] |= 0x80
	var key [4]byte
	if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
		return err
	}

	frame := make([]byte, 0, len(header)+len(key)+len(payload))
	frame = append(frame, header...)
	frame = append(frame, key[:]...)
	for i, b := range payload {
		frame = append(frame, b^key[i%4])
	}
	return WriteFull(w, frame)
}

func WriteClose(w io.Writer, mask bool, code uint16, reason string) error {
	payload := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(payload, code)
	payload = append(payload, reason...)
	return WriteFrame(w, OpClose, payload, mask)
}

func WriteFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

func validOpcode(op Opcode) bool {
	switch op {
	case OpContinuation, OpText, OpBinary, OpClose, OpPing, OpPong:
		return true
	default:
		return false
	}
}

func readPayloadLen(r io.Reader, len7 byte) (uint64, error) {
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
