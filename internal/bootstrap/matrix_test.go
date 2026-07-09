package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/VikashLoomba/Portal/pkg/transport"
)

type execRecord struct {
	stdin []byte
	argv  []string
}

type recordingTransport struct {
	mu        sync.Mutex
	unameOut  string
	unameErr  error
	probeOuts []string
	records   []execRecord
}

func (r *recordingTransport) Ensure(context.Context) (bool, error) { return false, nil }

func (r *recordingTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1, Detail: "pid=1"}, nil
}

func (r *recordingTransport) Exec(_ context.Context, stdin []byte, argv ...string) (string, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var stdinCopy []byte
	if stdin != nil {
		stdinCopy = append([]byte(nil), stdin...)
	}
	r.records = append(r.records, execRecord{
		stdin: stdinCopy,
		argv:  append([]string(nil), argv...),
	})

	if len(argv) > 0 && argv[0] == "uname" {
		if r.unameErr != nil {
			return "", "", r.unameErr
		}
		if r.unameOut != "" {
			return r.unameOut, "", nil
		}
		return "Linux x86_64\n", "", nil
	}

	if stdin == nil && strings.Contains(strings.Join(argv, " "), "test -x ") {
		if len(r.probeOuts) == 0 {
			return "MISSING\n", "", nil
		}
		out := r.probeOuts[0]
		r.probeOuts = r.probeOuts[1:]
		return out, "", nil
	}

	return "", "", nil
}

func (r *recordingTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, func() error { return nil }, errors.New("recording transport does not stream")
}

func (r *recordingTransport) Close(context.Context) (bool, error) { return false, nil }

func (r *recordingTransport) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: "recording", Endpoint: "/tmp/recording"}
}

func (r *recordingTransport) unameCount() int {
	return r.count(func(rec execRecord) bool {
		return len(rec.argv) > 0 && rec.argv[0] == "uname"
	})
}

func (r *recordingTransport) uploadStdin() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	var uploads [][]byte
	for _, rec := range r.records {
		if rec.stdin != nil {
			uploads = append(uploads, append([]byte(nil), rec.stdin...))
		}
	}
	return uploads
}

func (r *recordingTransport) scriptCount(substr string) int {
	return r.count(func(rec execRecord) bool {
		return strings.Contains(strings.Join(rec.argv, " "), substr)
	})
}

func (r *recordingTransport) count(match func(execRecord) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	var n int
	for _, rec := range r.records {
		if match(rec) {
			n++
		}
	}
	return n
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var _ transport.Transport = (*recordingTransport)(nil)

func TestMapUname(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantOS   string
		wantArch string
		wantErr  bool
	}{
		{name: "amd64", input: "Linux x86_64", wantOS: "linux", wantArch: "amd64"},
		{name: "arm64 aarch64", input: "Linux aarch64", wantOS: "linux", wantArch: "arm64"},
		{name: "arm64 literal", input: "Linux arm64", wantOS: "linux", wantArch: "arm64"},
		{name: "trailing newline", input: "Linux x86_64\n", wantOS: "linux", wantArch: "amd64"},
		{name: "darwin unsupported", input: "Darwin arm64", wantErr: true},
		{name: "linux i686 unsupported", input: "Linux i686", wantErr: true},
		{name: "empty unsupported", input: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotOS, gotArch, err := mapUname(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("mapUname returned nil error")
				}
				msg := err.Error()
				if !strings.Contains(msg, strconv.Quote(strings.TrimSpace(tt.input))) {
					t.Fatalf("error %q does not name observed input %q", msg, strings.TrimSpace(tt.input))
				}
				if !strings.Contains(msg, "supported") {
					t.Fatalf("error %q does not name supported set", msg)
				}
				return
			}
			if err != nil {
				t.Fatalf("mapUname: %v", err)
			}
			if gotOS != tt.wantOS || gotArch != tt.wantArch {
				t.Fatalf("mapUname = %s/%s, want %s/%s", gotOS, gotArch, tt.wantOS, tt.wantArch)
			}
		})
	}
}

func TestSelectArtifact(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantOS    string
		wantArch  string
		wantBytes []byte
	}{
		{name: "amd64", input: "Linux x86_64", wantOS: "linux", wantArch: "amd64", wantBytes: agentBinaryAMD64},
		{name: "arm64 aarch64", input: "Linux aarch64", wantOS: "linux", wantArch: "arm64", wantBytes: agentBinaryARM64},
		{name: "arm64 literal", input: "Linux arm64", wantOS: "linux", wantArch: "arm64", wantBytes: agentBinaryARM64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			art, err := selectArtifact(tt.input)
			if err != nil {
				t.Fatalf("selectArtifact: %v", err)
			}
			if art.goos != tt.wantOS || art.goarch != tt.wantArch {
				t.Fatalf("artifact = %s/%s, want %s/%s", art.goos, art.goarch, tt.wantOS, tt.wantArch)
			}
			if !bytes.Equal(art.bytes, tt.wantBytes) {
				t.Fatal("artifact bytes do not match embedded binary")
			}
			sum := sha256.Sum256(tt.wantBytes)
			if art.sha != hex.EncodeToString(sum[:]) {
				t.Fatalf("artifact sha = %s, want %s", art.sha, hex.EncodeToString(sum[:]))
			}
		})
	}
}

func TestSelectArtifactCachedBootID(t *testing.T) {
	t.Run("unchanged BootID probes once", func(t *testing.T) {
		tr := &recordingTransport{unameOut: "Linux x86_64\n"}
		m := New(tr, testLogger())
		m.SetBootID("B")

		for i := 0; i < 2; i++ {
			if _, err := m.selectArtifactCached(context.Background()); err != nil {
				t.Fatalf("selectArtifactCached call %d: %v", i+1, err)
			}
		}
		if got := tr.unameCount(); got != 1 {
			t.Fatalf("uname probes = %d, want 1", got)
		}
	})

	t.Run("changed BootID re-probes", func(t *testing.T) {
		tr := &recordingTransport{unameOut: "Linux x86_64\n"}
		m := New(tr, testLogger())
		m.SetBootID("B")
		if _, err := m.selectArtifactCached(context.Background()); err != nil {
			t.Fatalf("selectArtifactCached first: %v", err)
		}
		m.SetBootID("C")
		if _, err := m.selectArtifactCached(context.Background()); err != nil {
			t.Fatalf("selectArtifactCached second: %v", err)
		}
		if got := tr.unameCount(); got != 2 {
			t.Fatalf("uname probes = %d, want 2", got)
		}
	})

	t.Run("concurrent reconnect probes once", func(t *testing.T) {
		tr := &recordingTransport{unameOut: "Linux x86_64\n"}
		m := New(tr, testLogger())
		m.SetBootID("B")

		const workers = 32
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		wg.Add(workers)
		for i := 0; i < workers; i++ {
			go func() {
				defer wg.Done()
				_, err := m.selectArtifactCached(context.Background())
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("selectArtifactCached: %v", err)
			}
		}
		if got := tr.unameCount(); got != 1 {
			t.Fatalf("uname probes = %d, want 1", got)
		}
	})

	t.Run("unsupported architecture fails closed", func(t *testing.T) {
		tr := &recordingTransport{unameOut: "Linux sparc64\n"}
		m := New(tr, testLogger())

		_, err := m.selectArtifactCached(context.Background())
		if err == nil {
			t.Fatal("selectArtifactCached returned nil error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "Linux sparc64") || !strings.Contains(msg, "supported") {
			t.Fatalf("error = %q, want observed architecture and supported set", msg)
		}
		if got := tr.unameCount(); got != 1 {
			t.Fatalf("uname probes = %d, want 1", got)
		}
		if got := len(tr.uploadStdin()); got != 0 {
			t.Fatalf("upload execs = %d, want 0", got)
		}
		if got := tr.scriptCount("mv \"$tmp\""); got != 0 {
			t.Fatalf("mv execs = %d, want 0", got)
		}
	})
}
