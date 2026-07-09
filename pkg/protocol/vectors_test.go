package protocol_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/pkg/protocol"
	"github.com/fxamacker/cbor/v2"
)

var updateProtocolVectors = flag.Bool("update", false, "rewrite protocol golden vectors")

func TestProtocolVectors(t *testing.T) {
	if protocol.ProtoVersion != 4 {
		t.Fatalf("ProtoVersion = %d, want 4", protocol.ProtoVersion)
	}
	if protocol.FrameMagic != [2]byte{'P', 'F'} {
		t.Fatalf("FrameMagic = %q, want PF", protocol.FrameMagic)
	}
	if protocol.MaxFrameBytes != 1<<20 {
		t.Fatalf("MaxFrameBytes = %d, want 1048576", protocol.MaxFrameBytes)
	}

	dir := protocolVectorDir(t)
	tests := protocolVectorCases(t)

	if *updateProtocolVectors {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir vectors: %v", err)
		}
	}

	// SortNone caveat: these hex files pin this Go encoder's byte output.
	// The JSON sidecars are the semantic contract for foreign encoders; u11's
	// TypeScript verifier checks semantics, not byte identity.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if *updateProtocolVectors {
				writeProtocolVector(t, dir, tt.name, tt.want)
				return
			}

			encoded := readHexVector(t, filepath.Join(dir, tt.name+".hex"))
			decoded := decodeProtocolVector(t, encoded)
			if !reflect.DeepEqual(decoded, tt.want) {
				t.Fatalf("decoded vector = %+v, want %+v", decoded, tt.want)
			}
			assertDecodedProtoVersion(t, tt.name, decoded)

			// EC17: each arm's byte compare fails on Envelope arm, field, or CBOR
			// tag drift, forcing a deliberate vector update.
			reencoded := encodeProtocolVector(t, tt.want)
			if !bytes.Equal(reencoded, encoded) {
				t.Fatalf("re-encoded bytes differ from %s.hex; rerun go test ./pkg/protocol -run Vector -update after an INTENTIONAL wire change. If this was unintentional, this is silent codec drift from a field/tag add, remove, or rename", tt.name)
			}

			sidecar := readProtocolJSONVector(t, filepath.Join(dir, tt.name+".json"))
			if !reflect.DeepEqual(sidecar, tt.want) {
				t.Fatalf("json sidecar = %+v, want %+v", sidecar, tt.want)
			}
		})
	}
}

type protocolVectorCase struct {
	name string
	want *protocol.Envelope
}

func protocolVectorCases(t *testing.T) []protocolVectorCase {
	notifyPayload := mustProtocolPayload(t, protocol.Notify{
		Title:    "deploy complete",
		Body:     "stage 6 vectors generated",
		Subtitle: "devportal",
		Urgency:  2,
		Verified: true,
		Source:   "claude_hook",
		Sound:    "Glass",
	})

	return []protocolVectorCase{
		{
			name: "protocol_hello",
			want: &protocol.Envelope{Hello: &protocol.Hello{
				ProtoVersion:   protocol.ProtoVersion,
				ClientGitSHA:   "client-sha-u10",
				ClientPID:      4242,
				PollIntervalMs: 75,
				WantDestroyMC:  true,
				Services:       map[string]uint32{"notify": 1},
			}},
		},
		{
			name: "protocol_subscribe",
			want: &protocol.Envelope{Subscribe: &protocol.Subscribe{
				Deny:             []uint16{22, 5432},
				Allow:            []uint16{3000, 8080},
				ExcludeEphemeral: true,
				ResubscribeID:    99,
			}},
		},
		{
			name: "protocol_ping",
			want: &protocol.Envelope{Ping: &protocol.Ping{Nonce: 0x0102030405060708}},
		},
		{
			name: "protocol_req_snap",
			want: &protocol.Envelope{ReqSnap: &protocol.ReqSnap{}},
		},
		{
			name: "protocol_shutdown",
			want: &protocol.Envelope{Shutdown: &protocol.Shutdown{Reason: "operator requested"}},
		},
		{
			name: "protocol_hello_ack",
			want: &protocol.Envelope{HelloAck: &protocol.HelloAck{
				ProtoVersion: protocol.ProtoVersion,
				AgentGitSHA:  "agent-sha-u10",
				AgentPID:     4343,
				Kernel:       "Linux 6.8.0-portal",
				BootID:       "11111111-2222-3333-4444-555555555555",
				EphemMin:     32768,
				EphemMax:     60999,
				NowUnixNano:  1720000000123456789,
				Services:     map[string]uint32{"clip": 1},
			}},
		},
		{
			name: "protocol_subscribe_ack",
			want: &protocol.Envelope{SubscribeAck: &protocol.SubscribeAck{ResubscribeID: 99}},
		},
		{
			name: "protocol_snapshot",
			want: &protocol.Envelope{Snapshot: &protocol.Snapshot{
				Seq:         1001,
				GeneratedAt: 1720000000222333444,
				Ports: []protocol.Port{
					{Port: 3000, Family: 4, Addr: "127.0.0.1", InodeNS: 123456},
					{Port: 8080, Family: 6, Addr: "::1", InodeNS: 654321},
				},
			}},
		},
		{
			name: "protocol_port_added",
			want: &protocol.Envelope{PortAdded: &protocol.PortAdded{
				Seq: 1002,
				Port: protocol.Port{
					Port: 5173, Family: 4, Addr: "127.0.0.1", InodeNS: 777888,
				},
				At: 1720000000333444555,
			}},
		},
		{
			name: "protocol_port_removed",
			want: &protocol.Envelope{PortRemoved: &protocol.PortRemoved{
				Seq:    1003,
				Port:   5173,
				Family: 4,
				At:     1720000000444555666,
				Source: protocol.SourceDumpDiff,
			}},
		},
		{
			name: "protocol_heartbeat",
			want: &protocol.Envelope{Heartbeat: &protocol.Heartbeat{
				Seq:        1004,
				UptimeNano: 9876543210,
				Now:        1720000000555666777,
				Nonce:      0x1122334455667788,
			}},
		},
		{
			name: "protocol_agent_error",
			want: &protocol.Envelope{AgentError: &protocol.AgentError{
				Code:  protocol.CodeBadSubscribe,
				Msg:   "subscribe before hello",
				Fatal: true,
			}},
		},
		{
			name: "protocol_bye",
			want: &protocol.Envelope{Bye: &protocol.Bye{Reason: "shutdown complete"}},
		},
		{
			name: "protocol_msg",
			want: &protocol.Envelope{Msg: &protocol.Msg{
				Service: "notify",
				Kind:    "event",
				Seq:     77,
				Payload: notifyPayload,
			}},
		},
	}
}

func mustProtocolPayload[T any](t *testing.T, v T) cbor.RawMessage {
	t.Helper()
	payload, err := protocol.MarshalPayload(v)
	if err != nil {
		t.Fatalf("MarshalPayload: %v", err)
	}
	return payload
}

func protocolVectorDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := findModuleRoot(t, dir)
	return filepath.Join(root, "docs", "vectors")
}

func findModuleRoot(t *testing.T, dir string) string {
	t.Helper()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found from %s", dir)
		}
		dir = parent
	}
}

func writeProtocolVector(t *testing.T, dir, name string, want *protocol.Envelope) {
	t.Helper()
	encoded := encodeProtocolVector(t, want)
	if err := os.WriteFile(filepath.Join(dir, name+".hex"), []byte(hex.EncodeToString(encoded)+"\n"), 0644); err != nil {
		t.Fatalf("write hex: %v", err)
	}
	js, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), append(js, '\n'), 0644); err != nil {
		t.Fatalf("write json: %v", err)
	}
}

func encodeProtocolVector(t *testing.T, want *protocol.Envelope) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := protocol.NewEncoder(&buf).Write(want); err != nil {
		t.Fatalf("encode protocol vector: %v", err)
	}
	return buf.Bytes()
}

func decodeProtocolVector(t *testing.T, encoded []byte) *protocol.Envelope {
	t.Helper()
	r := bytes.NewReader(encoded)
	got, err := protocol.NewDecoder(r).Read()
	if err != nil {
		t.Fatalf("decode protocol vector: %v", err)
	}
	if r.Len() != 0 {
		t.Fatalf("decode protocol vector left %d trailing bytes", r.Len())
	}
	return got
}

func readHexVector(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hex %s: %v", path, err)
	}
	text := strings.Join(strings.Fields(string(raw)), "")
	decoded, err := hex.DecodeString(text)
	if err != nil {
		t.Fatalf("decode hex %s: %v", path, err)
	}
	return decoded
}

func readProtocolJSONVector(t *testing.T, path string) *protocol.Envelope {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read json %s: %v", path, err)
	}
	var got protocol.Envelope
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal json %s: %v", path, err)
	}
	return &got
}

func assertDecodedProtoVersion(t *testing.T, name string, got *protocol.Envelope) {
	t.Helper()
	switch name {
	case "protocol_hello":
		if got.Hello == nil || got.Hello.ProtoVersion != 4 {
			t.Fatalf("decoded hello ProtoVersion = %+v, want 4", got.Hello)
		}
	case "protocol_hello_ack":
		if got.HelloAck == nil || got.HelloAck.ProtoVersion != 4 {
			t.Fatalf("decoded hello_ack ProtoVersion = %+v, want 4", got.HelloAck)
		}
	}
}
