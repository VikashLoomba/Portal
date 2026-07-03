package localapi

import (
	"bytes"
	"testing"
)

func TestExecFrameEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   ExecFrame
	}{
		{name: "stdin data", in: ExecFrame{Stream: ExecStreamStdin, Data: []byte("input")}},
		{name: "stdout binary data", in: ExecFrame{Stream: ExecStreamStdout, Data: []byte{0x00, 0xff, 'o', 'k'}}},
		{name: "stderr data", in: ExecFrame{Stream: ExecStreamStderr, Data: []byte("warn")}},
		{name: "exit code", in: ExecFrame{Stream: ExecStreamExit, Code: 3}},
		{name: "error data", in: ExecFrame{Stream: ExecStreamError, Data: []byte("boom")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := EncodeExecFrame(tt.in)
			if err != nil {
				t.Fatalf("EncodeExecFrame: %v", err)
			}
			got, err := DecodeExecFrame(encoded)
			if err != nil {
				t.Fatalf("DecodeExecFrame: %v", err)
			}
			if got.Stream != tt.in.Stream {
				t.Fatalf("Stream = %q, want %q", got.Stream, tt.in.Stream)
			}
			if !bytes.Equal(got.Data, tt.in.Data) {
				t.Fatalf("Data = %v, want %v", got.Data, tt.in.Data)
			}
			if got.Code != tt.in.Code {
				t.Fatalf("Code = %d, want %d", got.Code, tt.in.Code)
			}
		})
	}
}

func TestDecodeExecFrameMalformedCBOR(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("DecodeExecFrame panicked: %v", rec)
		}
	}()
	if _, err := DecodeExecFrame([]byte{0xa1, 0x61, 's'}); err == nil {
		t.Fatal("DecodeExecFrame returned nil error")
	}
}
