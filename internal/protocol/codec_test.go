package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"
)

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
