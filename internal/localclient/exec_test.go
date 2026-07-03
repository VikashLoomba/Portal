package localclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/localapi"
	"github.com/VikashLoomba/Portal/internal/protocol"
	"github.com/VikashLoomba/Portal/internal/transport/localexec"
)

func TestExec(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		stdin      io.Reader
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "stdout exit zero",
			argv:       []string{"printf", "hi"},
			wantStdout: "hi",
		},
		{
			name:     "nonzero exit",
			argv:     []string{"sh", "-c", "'exit 5'"},
			wantCode: 5,
		},
		{
			name:       "stdin half close",
			argv:       []string{"cat"},
			stdin:      strings.NewReader("payload\n"),
			wantStdout: "payload\n",
		},
		{
			name:       "stderr",
			argv:       []string{"sh", "-c", "'echo E >&2'"},
			wantStderr: "E",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, _ := startExecClientServer(t, config.New(t.TempDir()))
			var out, errb bytes.Buffer
			code, err := New(path).Exec(context.Background(), tt.argv, tt.stdin, &out, &errb)
			if err != nil {
				t.Fatalf("Exec returned error: %v", err)
			}
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tt.wantCode)
			}
			if out.String() != tt.wantStdout {
				t.Fatalf("stdout = %q, want %q", out.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(errb.String(), tt.wantStderr) {
				t.Fatalf("stderr = %q, want substring %q", errb.String(), tt.wantStderr)
			}
			if tt.wantStderr == "" && errb.String() != "" {
				t.Fatalf("stderr = %q, want empty", errb.String())
			}
		})
	}
}

func TestExecFeatureOff(t *testing.T) {
	cfg := config.New(t.TempDir())
	if err := cfg.SetFeature(config.FeatureExec, false); err != nil {
		t.Fatalf("SetFeature(exec,false): %v", err)
	}
	path, _ := startExecClientServer(t, cfg)

	var out, errb bytes.Buffer
	code, err := New(path).Exec(context.Background(), []string{"printf", "hi"}, nil, &out, &errb)
	if err == nil {
		t.Fatal("Exec error = nil, want feature_disabled APIError")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Code != "feature_disabled" {
		t.Fatalf("APIError.Code = %q, want feature_disabled", apiErr.Code)
	}
}

func startExecClientServer(t *testing.T, cfg *config.Store) (string, *localapi.Server) {
	t.Helper()
	path := filepath.Join(shortExecClientTempDir(t), "api.sock")
	srv := localapi.New(localapi.Deps{
		Version:    localapi.VersionInfo{Version: "test", GitSHA: "exec", ProtoVersion: protocol.ProtoVersion},
		Config:     cfg,
		ExecStream: localexec.New(),
		Audit:      audit.New(t.TempDir()),
	})
	ln, err := localapi.Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("exec server did not stop")
		}
	})

	waitExecClientAvailable(t, New(path))
	return path, srv
}

func waitExecClientAvailable(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Available(context.Background()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never became available")
}

func shortExecClientTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lcli-exec-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
