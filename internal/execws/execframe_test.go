package execws

import (
	"bytes"
	"testing"

	"github.com/fxamacker/cbor/v2"
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
		{name: "winch size", in: ExecFrame{Stream: ExecStreamWinch, Rows: 24, Cols: 80}},
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
			if got.Rows != tt.in.Rows {
				t.Fatalf("Rows = %d, want %d", got.Rows, tt.in.Rows)
			}
			if got.Cols != tt.in.Cols {
				t.Fatalf("Cols = %d, want %d", got.Cols, tt.in.Cols)
			}
		})
	}
}

func TestDecodeExecFrameIgnoresUnknownFields(t *testing.T) {
	encoded, err := cbor.Marshal(map[string]any{
		"s":       ExecStreamStdout,
		"d":       []byte("ok"),
		"unknown": "ignored",
	})
	if err != nil {
		t.Fatalf("marshal cbor: %v", err)
	}
	got, err := DecodeExecFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeExecFrame: %v", err)
	}
	if got.Stream != ExecStreamStdout || !bytes.Equal(got.Data, []byte("ok")) {
		t.Fatalf("decoded frame = %+v, want stdout ok", got)
	}
}

func TestExecFrameRowsColsOmitEmpty(t *testing.T) {
	tests := []struct {
		name     string
		in       ExecFrame
		wantRows bool
		wantCols bool
	}{
		{name: "zero omitted", in: ExecFrame{Stream: ExecStreamWinch}, wantRows: false, wantCols: false},
		{name: "nonzero present", in: ExecFrame{Stream: ExecStreamWinch, Rows: 24, Cols: 80}, wantRows: true, wantCols: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := EncodeExecFrame(tt.in)
			if err != nil {
				t.Fatalf("EncodeExecFrame: %v", err)
			}
			var got map[string]any
			if err := cbor.Unmarshal(encoded, &got); err != nil {
				t.Fatalf("unmarshal map: %v", err)
			}
			if _, ok := got["rs"]; ok != tt.wantRows {
				t.Fatalf("rs present = %v, want %v in %v", ok, tt.wantRows, got)
			}
			if _, ok := got["cs"]; ok != tt.wantCols {
				t.Fatalf("cs present = %v, want %v in %v", ok, tt.wantCols, got)
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
