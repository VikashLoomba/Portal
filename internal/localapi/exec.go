package localapi

import "github.com/fxamacker/cbor/v2"

// ExecFrame is the X7 typed envelope carried in binary WebSocket frames.
// stdin/stdout/stderr/error carry Data; exit carries Code.
type ExecFrame struct {
	Stream string `cbor:"s"`
	Data   []byte `cbor:"d,omitempty"`
	Code   int    `cbor:"c,omitempty"`
}

// ExecStream* names are the stable X7 stream vocabulary. Clients send only
// stdin; servers send stdout, stderr, exit, and error.
const (
	ExecStreamStdin  = "stdin"
	ExecStreamStdout = "stdout"
	ExecStreamStderr = "stderr"
	ExecStreamExit   = "exit"
	ExecStreamError  = "error"
)

// EncodeExecFrame returns the CBOR payload for one ExecFrame.
func EncodeExecFrame(f ExecFrame) ([]byte, error) {
	return cbor.Marshal(f)
}

// DecodeExecFrame decodes one CBOR ExecFrame and returns an error for malformed
// input.
func DecodeExecFrame(b []byte) (ExecFrame, error) {
	var f ExecFrame
	if err := cbor.Unmarshal(b, &f); err != nil {
		return ExecFrame{}, err
	}
	return f, nil
}
