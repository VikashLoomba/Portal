package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/fxamacker/cbor/v2"
)

// ErrBadMagic, ErrFrameTooLarge, ErrShortFrame are sentinel errors. The
// reader closes the connection on any of them — there is no in-band
// recovery; the client reconnects via the bootstrap manager.
var (
	ErrBadMagic      = errors.New("protocol: bad frame magic")
	ErrFrameTooLarge = errors.New("protocol: frame exceeds MaxFrameBytes")
	ErrShortFrame    = errors.New("protocol: short frame")
)

// encMode encodes with deterministic-but-cheap settings. Sort:None — we
// don't need canonical CBOR (the wire is point-to-point), just stable.
var encMode cbor.EncMode = func() cbor.EncMode {
	m, err := cbor.EncOptions{Sort: cbor.SortNone}.EncMode()
	if err != nil {
		panic(err)
	}
	return m
}()

// decMode rejects duplicate map keys (DupMapKeyEnforcedAPF — "as per fxamacker"),
// so a tampered/desynced frame fails closed instead of silently masking fields.
var decMode cbor.DecMode = func() cbor.DecMode {
	m, err := cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF}.DecMode()
	if err != nil {
		panic(err)
	}
	return m
}()

// Encoder writes framed CBOR Envelopes to an io.Writer. Concurrency-safe.
// The agent's stdout has a single writer goroutine, so the mutex is mostly
// for safety on the client side where multiple commands may race.
type Encoder struct {
	w  io.Writer
	mu sync.Mutex
}

func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

// Write serializes env into a single frame: magic + uint32 BE length + CBOR.
// Returns an error if the payload exceeds MaxFrameBytes or the underlying
// Writer fails. Partial writes are not retried — the caller's reconnect
// path must close the stream and start fresh.
func (e *Encoder) Write(env *Envelope) error {
	payload, err := encMode.Marshal(env)
	if err != nil {
		return fmt.Errorf("cbor marshal: %w", err)
	}
	if len(payload) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	header := make([]byte, 6)
	header[0] = FrameMagic[0]
	header[1] = FrameMagic[1]
	binary.BigEndian.PutUint32(header[2:6], uint32(len(payload)))

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.w.Write(header); err != nil {
		return err
	}
	if _, err := e.w.Write(payload); err != nil {
		return err
	}
	return nil
}

// Decoder reads framed CBOR Envelopes from an io.Reader. Single-reader.
type Decoder struct {
	r      io.Reader
	header [6]byte
	buf    []byte // grows up to MaxFrameBytes
}

func NewDecoder(r io.Reader) *Decoder { return &Decoder{r: r} }

// Read blocks until a complete frame is available or the underlying Reader
// errors. Returns the decoded Envelope. On any framing error it returns the
// sentinel and the caller must close the stream.
func (d *Decoder) Read() (*Envelope, error) {
	if _, err := io.ReadFull(d.r, d.header[:]); err != nil {
		return nil, err
	}
	if d.header[0] != FrameMagic[0] || d.header[1] != FrameMagic[1] {
		return nil, ErrBadMagic
	}
	n := binary.BigEndian.Uint32(d.header[2:6])
	if n > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	if cap(d.buf) < int(n) {
		d.buf = make([]byte, n)
	} else {
		d.buf = d.buf[:n]
	}
	if _, err := io.ReadFull(d.r, d.buf); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrShortFrame
		}
		return nil, err
	}
	var env Envelope
	if err := decMode.Unmarshal(d.buf, &env); err != nil {
		return nil, fmt.Errorf("cbor unmarshal: %w", err)
	}
	return &env, nil
}
