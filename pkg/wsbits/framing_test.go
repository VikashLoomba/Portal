package wsbits

import (
	"bytes"
	"encoding/binary"
	"io"
	"strconv"
	"testing"
)

func TestAcceptKeyRFC6455Vector(t *testing.T) {
	got := AcceptKey("dGhlIHNhbXBsZSBub25jZQ==")
	if got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("AcceptKey = %q, want RFC 6455 vector", got)
	}
}

func TestFrameRoundTripPayloadBoundaries(t *testing.T) {
	tests := []struct {
		name          string
		mask          bool
		requireMasked bool
	}{
		{name: "client masked", mask: true, requireMasked: true},
		{name: "server unmasked", mask: false, requireMasked: false},
	}
	sizes := []int{0, 1, 125, 126, 127, 65535, 65536}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, size := range sizes {
				t.Run(payloadSizeName(size), func(t *testing.T) {
					want := patternedPayload(size)
					var out bytes.Buffer
					if err := WriteFrame(&out, OpBinary, want, tt.mask); err != nil {
						t.Fatalf("WriteFrame: %v", err)
					}
					op, got, err := ReadFrame(&out, tt.requireMasked)
					if err != nil {
						t.Fatalf("ReadFrame: %v", err)
					}
					if op != OpBinary {
						t.Fatalf("opcode = 0x%x, want binary", byte(op))
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("payload mismatch for size %d", size)
					}
				})
			}
		})
	}
}

func TestReadFrameRejectsWrongMaskDirection(t *testing.T) {
	tests := []struct {
		name          string
		frame         []byte
		requireMasked bool
	}{
		{
			name:          "unmasked client frame",
			frame:         []byte{0x80 | byte(OpBinary), 0x00},
			requireMasked: true,
		},
		{
			name:          "masked server frame",
			frame:         maskedFrame(0x80|byte(OpBinary), nil),
			requireMasked: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := ReadFrame(bytes.NewReader(tt.frame), tt.requireMasked); err == nil {
				t.Fatal("ReadFrame returned nil error")
			}
		})
	}
}

func TestReadFrameRejectsMalformedFrames(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
	}{
		{
			name:  "declared length exceeds max",
			frame: oversizedMaskedFrameHeader(MaxPayload + 1),
		},
		{
			name:  "invalid opcode",
			frame: maskedFrame(0x80|0x3, nil),
		},
		{
			name:  "continuation frame",
			frame: maskedFrame(0x80|byte(OpContinuation), nil),
		},
		{
			name:  "truncated frame",
			frame: maskedFrame(0x80|byte(OpBinary), []byte("hello"))[:4],
		},
		{
			name:  "fragmented data frame",
			frame: maskedFrame(byte(OpBinary), []byte("hello")),
		},
		{
			name:  "reserved bits",
			frame: maskedFrame(0x80|0x40|byte(OpBinary), nil),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := ReadFrame(bytes.NewReader(tt.frame), true); err == nil {
				t.Fatal("ReadFrame returned nil error")
			}
		})
	}
}

func TestWriteClose(t *testing.T) {
	var out bytes.Buffer
	if err := WriteClose(&out, false, 1000, "done"); err != nil {
		t.Fatalf("WriteClose: %v", err)
	}
	op, payload, err := ReadFrame(&out, false)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if op != OpClose {
		t.Fatalf("opcode = 0x%x, want close", byte(op))
	}
	if got := binary.BigEndian.Uint16(payload[:2]); got != 1000 {
		t.Fatalf("close code = %d, want 1000", got)
	}
	if got := string(payload[2:]); got != "done" {
		t.Fatalf("close reason = %q, want done", got)
	}
}

func TestWritePong(t *testing.T) {
	var out bytes.Buffer
	if err := WriteFrame(&out, OpPong, []byte("ping"), false); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	op, payload, err := ReadFrame(&out, false)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if op != OpPong {
		t.Fatalf("opcode = 0x%x, want pong", byte(op))
	}
	if !bytes.Equal(payload, []byte("ping")) {
		t.Fatalf("pong payload = %q, want ping", payload)
	}
}

func TestWriteFullShortWrites(t *testing.T) {
	w := &shortWriter{limit: 2}
	if err := WriteFull(w, []byte("hello")); err != nil {
		t.Fatalf("WriteFull: %v", err)
	}
	if got := w.buf.String(); got != "hello" {
		t.Fatalf("written = %q, want hello", got)
	}
}

type shortWriter struct {
	limit int
	buf   bytes.Buffer
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit {
		p = p[:w.limit]
	}
	if len(p) == 0 {
		return 0, io.ErrShortWrite
	}
	return w.buf.Write(p)
}

func maskedFrame(first byte, payload []byte) []byte {
	const maskBit = 0x80
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	frame := []byte{first}
	n := len(payload)
	switch {
	case n <= 125:
		frame = append(frame, maskBit|byte(n))
	case n <= 0xffff:
		frame = append(frame, maskBit|126, 0, 0)
		binary.BigEndian.PutUint16(frame[2:4], uint16(n))
	default:
		frame = append(frame, maskBit|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(frame[2:10], uint64(n))
	}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	return frame
}

func oversizedMaskedFrameHeader(n uint64) []byte {
	frame := []byte{0x80 | byte(OpBinary), 0x80 | 127, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint64(frame[2:10], n)
	frame = append(frame, 0, 0, 0, 0)
	return frame
}

func patternedPayload(size int) []byte {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	return payload
}

func payloadSizeName(size int) string {
	return "size_" + strconv.Itoa(size)
}
