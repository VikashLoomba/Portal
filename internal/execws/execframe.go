package execws

import "github.com/fxamacker/cbor/v2"

// ExecFrame is the typed envelope carried in one binary WebSocket message.
type ExecFrame struct {
	Stream string `cbor:"s"`
	Data   []byte `cbor:"d,omitempty"`
	Code   int    `cbor:"c,omitempty"`
	Rows   uint16 `cbor:"rs,omitempty"`
	Cols   uint16 `cbor:"cs,omitempty"`
}

const (
	ExecStreamStdin  = "stdin"
	ExecStreamStdout = "stdout"
	ExecStreamStderr = "stderr"
	ExecStreamExit   = "exit"
	ExecStreamError  = "error"
	ExecStreamWinch  = "winch"
)

func EncodeExecFrame(f ExecFrame) ([]byte, error) {
	return cbor.Marshal(f)
}

func DecodeExecFrame(b []byte) (ExecFrame, error) {
	// DecodeExecFrame MUST use the default decode mode and NEVER enable
	// ExtraDecErrorUnknownField; additive winch fields depend on unknown-key
	// tolerance.
	var f ExecFrame
	if err := cbor.Unmarshal(b, &f); err != nil {
		return ExecFrame{}, err
	}
	return f, nil
}
