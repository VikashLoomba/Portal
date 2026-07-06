package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/localapi"
	"github.com/VikashLoomba/Portal/internal/localclient"
	"github.com/VikashLoomba/Portal/internal/protocol"
	"github.com/VikashLoomba/Portal/internal/transport/localexec"
)

func TestExecCmd(t *testing.T) {
	path := serveExecDaemon(t)

	tests := []struct {
		name     string
		args     []string
		wantOut  string
		wantCode int
		wantUse  bool
	}{
		{name: "true", args: []string{"--", "true"}},
		{name: "false", args: []string{"--", "false"}, wantCode: 1},
		{name: "stdout", args: []string{"--", "printf", "ok"}, wantOut: "ok"},
		{name: "missing dash", args: []string{"true"}, wantUse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &app.App{Paths: app.Paths{APISock: path}}
			cmd := newExecCmd(a)
			var out, errb bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errb)
			cmd.SetArgs(tt.args)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := cmd.ExecuteContext(ctx)

			switch {
			case tt.wantUse:
				var ue usageErr
				if !errors.As(err, &ue) {
					t.Fatalf("error = %v, want usageErr", err)
				}
				return
			case tt.wantCode != 0:
				var ece exitCodeErr
				if !errors.As(err, &ece) {
					t.Fatalf("error = %v, want exitCodeErr", err)
				}
				if ece.code != tt.wantCode {
					t.Fatalf("exit code = %d, want %d", ece.code, tt.wantCode)
				}
			default:
				if err != nil {
					t.Fatalf("ExecuteContext returned %v", err)
				}
			}
			if out.String() != tt.wantOut {
				t.Fatalf("stdout = %q, want %q", out.String(), tt.wantOut)
			}
		})
	}
}

func serveExecDaemon(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "portal-exec-api-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "api.sock")

	srv := localapi.New(localapi.Deps{
		Version:    localapi.VersionInfo{Version: "test", GitSHA: "exec", ProtoVersion: protocol.ProtoVersion},
		Config:     config.New(t.TempDir()),
		ExecStream: localexec.New(),
		Audit:      audit.New(t.TempDir()),
	})
	ln, err := localapi.Listen(path)
	if err != nil {
		t.Fatalf("localapi.Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("exec daemon did not stop")
		}
	})

	lc := localclient.New(path)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lc.Available(context.Background()) {
			return path
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("exec daemon did not come up")
	return path
}
