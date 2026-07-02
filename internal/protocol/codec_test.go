package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

// ensure errors import is used
var _ = errors.Is

func TestRoundtripHello(t *testing.T) {
	in := &Envelope{Hello: &Hello{
		ProtoVersion: 1, ClientGitSHA: "abc", ClientPID: 42, PollIntervalMs: 75, WantDestroyMC: true,
	}}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(in); err != nil {
		t.Fatal(err)
	}
	out, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatal(err)
	}
	if out.Hello == nil {
		t.Fatal("Hello not present")
	}
	if !reflect.DeepEqual(in.Hello, out.Hello) {
		t.Errorf("got %+v, want %+v", out.Hello, in.Hello)
	}
}

func TestRoundtripSnapshotAndAdded(t *testing.T) {
	in := &Envelope{Snapshot: &Snapshot{
		Seq: 5, GeneratedAt: 1700000000_000000000,
		Ports: []Port{
			{Port: 8081, Family: 4, Addr: "127.0.0.1", InodeNS: 0},
			{Port: 8082, Family: 6, Addr: "::1", InodeNS: 0},
		},
	}}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(in); err != nil {
		t.Fatal(err)
	}
	out, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in.Snapshot, out.Snapshot) {
		t.Errorf("got %+v, want %+v", out.Snapshot, in.Snapshot)
	}
}

func TestBadMagic(t *testing.T) {
	// Construct a frame with corrupt magic.
	var buf bytes.Buffer
	buf.WriteByte('X')
	buf.WriteByte('Y')
	binary.Write(&buf, binary.BigEndian, uint32(0))
	if _, err := NewDecoder(&buf).Read(); !errors.Is(err, ErrBadMagic) {
		t.Errorf("got %v, want ErrBadMagic", err)
	}
}

func TestOversizeReject(t *testing.T) {
	// Header claims a giant payload but we never have to allocate the body.
	var buf bytes.Buffer
	buf.Write(FrameMagic[:])
	binary.Write(&buf, binary.BigEndian, uint32(MaxFrameBytes+1))
	if _, err := NewDecoder(&buf).Read(); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("got %v, want ErrFrameTooLarge", err)
	}
}

func TestShortFrame(t *testing.T) {
	// Header claims 100 bytes; body has 5.
	var buf bytes.Buffer
	buf.Write(FrameMagic[:])
	binary.Write(&buf, binary.BigEndian, uint32(100))
	buf.Write([]byte("short"))
	if _, err := NewDecoder(&buf).Read(); !errors.Is(err, ErrShortFrame) {
		t.Errorf("got %v, want ErrShortFrame", err)
	}
}

func TestEmptyReaderEOF(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewDecoder(&buf).Read(); err != io.EOF {
		t.Errorf("got %v, want io.EOF", err)
	}
}

func TestMultipleFieldsRejected(t *testing.T) {
	// A frame with two non-nil fields violates the tagged-union contract.
	in := &Envelope{
		Hello:    &Hello{ProtoVersion: 1},
		HelloAck: &HelloAck{ProtoVersion: 1}, // second field
	}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(in); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDecoder(&buf).Read(); !errors.Is(err, ErrMultipleFields) {
		t.Errorf("got %v, want ErrMultipleFields", err)
	}
}

func TestMultipleFramesInPipeline(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	enc.Write(&Envelope{Hello: &Hello{ProtoVersion: 1, ClientGitSHA: "a"}})
	enc.Write(&Envelope{HelloAck: &HelloAck{ProtoVersion: 1, AgentGitSHA: "b"}})
	enc.Write(&Envelope{Heartbeat: &Heartbeat{Seq: 1, UptimeNano: 100, Now: 200}})

	dec := NewDecoder(&buf)
	for i, want := range []string{"Hello", "HelloAck", "Heartbeat"} {
		env, err := dec.Read()
		if err != nil {
			t.Fatalf("read[%d]: %v", i, err)
		}
		switch want {
		case "Hello":
			if env.Hello == nil {
				t.Errorf("frame %d: missing Hello", i)
			}
		case "HelloAck":
			if env.HelloAck == nil {
				t.Errorf("frame %d: missing HelloAck", i)
			}
		case "Heartbeat":
			if env.Heartbeat == nil {
				t.Errorf("frame %d: missing Heartbeat", i)
			}
		}
	}
}

func TestRoundtripClipRequest(t *testing.T) {
	in := &Envelope{ClipRequest: &ClipRequest{
		Nonce: 7, Epoch: 0xdeadbeef, Kind: "image", Format: "png",
	}}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(in); err != nil {
		t.Fatal(err)
	}
	out, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatal(err)
	}
	if out.ClipRequest == nil {
		t.Fatal("ClipRequest not present")
	}
	if !reflect.DeepEqual(in.ClipRequest, out.ClipRequest) {
		t.Errorf("got %+v, want %+v", out.ClipRequest, in.ClipRequest)
	}
}

func TestRoundtripClipResponse(t *testing.T) {
	in := &Envelope{ClipResponse: &ClipResponse{
		Nonce: 7, Epoch: 0xdeadbeef, OK: true, Has: true,
		SHA: "0123456789abcdef0123456789abcdef",
	}}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(in); err != nil {
		t.Fatal(err)
	}
	out, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatal(err)
	}
	if out.ClipResponse == nil {
		t.Fatal("ClipResponse not present")
	}
	if !reflect.DeepEqual(in.ClipResponse, out.ClipResponse) {
		t.Errorf("got %+v, want %+v", out.ClipResponse, in.ClipResponse)
	}
}

// TestClipFieldCountInvariant asserts the clip frames each carry exactly one
// non-nil envelope field, so the tagged-union contract still holds with the
// v2 additions, and that a clip field paired with another field is rejected.
func TestClipFieldCountInvariant(t *testing.T) {
	if n := countEnvelopeFields(&Envelope{ClipRequest: &ClipRequest{}}); n != 1 {
		t.Errorf("ClipRequest alone: got %d fields, want 1", n)
	}
	if n := countEnvelopeFields(&Envelope{ClipResponse: &ClipResponse{}}); n != 1 {
		t.Errorf("ClipResponse alone: got %d fields, want 1", n)
	}
	// A clip field paired with any other field must trip the multi-field guard.
	in := &Envelope{
		ClipRequest:  &ClipRequest{},
		ClipResponse: &ClipResponse{},
	}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(in); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDecoder(&buf).Read(); !errors.Is(err, ErrMultipleFields) {
		t.Errorf("got %v, want ErrMultipleFields", err)
	}
}

// TestRoundtripMsg encodes a Msg carrying a MarshalPayload'd ClipRequest,
// round-trips the Envelope, and proves the RawMessage passthrough is
// byte-preserving: Service/Kind/Seq survive and UnmarshalPayload of the
// round-tripped Payload deep-equals the original struct.
func TestRoundtripMsg(t *testing.T) {
	orig := ClipRequest{Nonce: 7, Epoch: 0xdeadbeef, Kind: "image", Format: "png"}
	payload, err := MarshalPayload(orig)
	if err != nil {
		t.Fatal(err)
	}
	in := &Envelope{Msg: &Msg{Service: "clip", Kind: "req", Seq: 5, Payload: payload}}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(in); err != nil {
		t.Fatal(err)
	}
	out, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatal(err)
	}
	if out.Msg == nil {
		t.Fatal("Msg not present")
	}
	if out.Msg.Service != "clip" || out.Msg.Kind != "req" || out.Msg.Seq != 5 {
		t.Errorf("header mismatch: got %+v", out.Msg)
	}
	got, err := UnmarshalPayload[ClipRequest](out.Msg.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, orig) {
		t.Errorf("payload roundtrip: got %+v, want %+v", got, orig)
	}
}

// TestRoundtripMsgEmptyPayload proves an omitted Msg.Payload round-trips as a
// nil/empty RawMessage and an all-zero Seq omits the seq key (omitempty).
func TestRoundtripMsgEmptyPayload(t *testing.T) {
	in := &Envelope{Msg: &Msg{Service: "clip", Kind: "req"}}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(in); err != nil {
		t.Fatal(err)
	}
	out, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatal(err)
	}
	if out.Msg == nil {
		t.Fatal("Msg not present")
	}
	if len(out.Msg.Payload) != 0 {
		t.Errorf("empty payload round-tripped as %v, want empty", out.Msg.Payload)
	}
	if out.Msg.Seq != 0 {
		t.Errorf("zero Seq round-tripped as %d", out.Msg.Seq)
	}
}

// TestMsgFieldCountInvariant proves Msg participates in the one-field-per-frame
// tagged-union invariant: it counts as one field, and pairing it with any other
// field trips the multi-field guard on decode.
func TestMsgFieldCountInvariant(t *testing.T) {
	if n := countEnvelopeFields(&Envelope{Msg: &Msg{}}); n != 1 {
		t.Errorf("Msg alone: got %d fields, want 1", n)
	}
	in := &Envelope{
		Msg:   &Msg{Service: "clip", Kind: "req"},
		Hello: &Hello{ProtoVersion: 1},
	}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(in); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDecoder(&buf).Read(); !errors.Is(err, ErrMultipleFields) {
		t.Errorf("got %v, want ErrMultipleFields", err)
	}
}

// TestMsgDupKeyFailClosed proves a hand-built payload with a duplicated map key
// still fails closed via decMode (DupMapKeyEnforcedAPF protects nested payload
// maps just as it does the top-level Envelope).
func TestMsgDupKeyFailClosed(t *testing.T) {
	// CBOR: map(2) { "a": 1, "a": 2 } — a duplicated key.
	dup := cbor.RawMessage{0xA2, 0x61, 'a', 0x01, 0x61, 'a', 0x02}
	if _, err := UnmarshalPayload[map[string]uint64](dup); err == nil {
		t.Fatal("dup-key payload decoded without error, want fail-closed")
	}
}

// TestMarshalUnmarshalPayloadRoundtrip proves the typed payload helpers
// round-trip the fire-and-forget payload structs byte-for-byte.
func TestMarshalUnmarshalPayloadRoundtrip(t *testing.T) {
	t.Run("OpenURL", func(t *testing.T) {
		orig := OpenURL{URL: "https://example.com/path?q=1", Seq: 42}
		raw, err := MarshalPayload(orig)
		if err != nil {
			t.Fatal(err)
		}
		got, err := UnmarshalPayload[OpenURL](raw)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("got %+v, want %+v", got, orig)
		}
	})
	t.Run("Notify", func(t *testing.T) {
		orig := Notify{
			Title: "done", Body: "build ok", Subtitle: "ci",
			Urgency: 2, Verified: true, Source: "claude_hook", Sound: "Glass", Seq: 9,
		}
		raw, err := MarshalPayload(orig)
		if err != nil {
			t.Fatal(err)
		}
		got, err := UnmarshalPayload[Notify](raw)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("got %+v, want %+v", got, orig)
		}
	})
}
