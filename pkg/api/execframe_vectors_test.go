package api_test

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

	"github.com/VikashLoomba/Portal/pkg/api"
)

var updateExecVectors = flag.Bool("update", false, "rewrite exec frame golden vectors")

func TestExecFrameVectors(t *testing.T) {
	dir := execVectorDir(t)
	tests := execVectorCases()

	if *updateExecVectors {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir vectors: %v", err)
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if *updateExecVectors {
				writeExecVector(t, dir, tt.name, tt.want)
				return
			}

			encoded := readHexVector(t, filepath.Join(dir, tt.name+".hex"))
			decoded, err := api.DecodeExecFrame(encoded)
			if err != nil {
				t.Fatalf("DecodeExecFrame: %v", err)
			}
			if !reflect.DeepEqual(decoded, tt.want) {
				t.Fatalf("decoded vector = %+v, want %+v", decoded, tt.want)
			}

			reencoded := encodeExecVector(t, tt.want)
			if !bytes.Equal(reencoded, encoded) {
				t.Fatalf("re-encoded bytes differ from %s.hex; rerun go test ./pkg/api -run Vector -update after an INTENTIONAL wire change. If this was unintentional, this is silent codec drift from a field/tag add, remove, or rename", tt.name)
			}

			sidecar := readExecJSONVector(t, filepath.Join(dir, tt.name+".json"))
			if !reflect.DeepEqual(sidecar, tt.want) {
				t.Fatalf("json sidecar = %+v, want %+v", sidecar, tt.want)
			}
		})
	}
}

type execVectorCase struct {
	name string
	want api.ExecFrame
}

func execVectorCases() []execVectorCase {
	return []execVectorCase{
		{
			name: "exec_stdin",
			want: api.ExecFrame{Stream: api.ExecStreamStdin, Data: []byte("input\n")},
		},
		{
			name: "exec_stdout",
			want: api.ExecFrame{Stream: api.ExecStreamStdout, Data: []byte("output\n")},
		},
		{
			name: "exec_stderr",
			want: api.ExecFrame{Stream: api.ExecStreamStderr, Data: []byte("warning\n")},
		},
		{
			name: "exec_exit",
			want: api.ExecFrame{Stream: api.ExecStreamExit, Code: 7},
		},
		{
			name: "exec_error",
			want: api.ExecFrame{Stream: api.ExecStreamError, Data: []byte("remote exec failed")},
		},
		{
			name: "exec_winch",
			want: api.ExecFrame{Stream: api.ExecStreamWinch, Rows: 40, Cols: 120},
		},
		{
			name: "exec_pty_stdout",
			want: api.ExecFrame{Stream: api.ExecStreamStdout, Data: []byte("pty output\r\n")},
		},
	}
}

func execVectorDir(t *testing.T) string {
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

func writeExecVector(t *testing.T, dir, name string, want api.ExecFrame) {
	t.Helper()
	encoded := encodeExecVector(t, want)
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

func encodeExecVector(t *testing.T, want api.ExecFrame) []byte {
	t.Helper()
	encoded, err := api.EncodeExecFrame(want)
	if err != nil {
		t.Fatalf("EncodeExecFrame: %v", err)
	}
	return encoded
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

func readExecJSONVector(t *testing.T, path string) api.ExecFrame {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read json %s: %v", path, err)
	}
	var got api.ExecFrame
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal json %s: %v", path, err)
	}
	return got
}
