package doctorprobe

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/pkg/transport"
)

type cancelTransport struct {
	cancel context.CancelFunc
	execs  []string
}

func (t *cancelTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (t *cancelTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1}, nil
}
func (t *cancelTransport) Exec(ctx context.Context, _ []byte, argv ...string) (string, string, error) {
	t.execs = append(t.execs, strings.Join(argv, " "))
	t.cancel()
	return "", "", ctx.Err()
}
func (t *cancelTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, nil, nil
}
func (t *cancelTransport) Close(context.Context) (bool, error) { return false, nil }
func (t *cancelTransport) Describe() transport.Desc {
	return transport.Desc{Impl: transport.ImplSystemSSH, Host: "box"}
}

func TestRunStopsBetweenRemoteProbesOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tr := &cancelTransport{cancel: cancel}
	rep := Run(ctx, "box", tr)
	if len(tr.execs) != 1 || !strings.Contains(tr.execs[0], "command -v xclip") {
		t.Fatalf("Exec calls = %v, want only xclip PATH probe", tr.execs)
	}
	if len(rep.Checks) != 2 || rep.Checks[1].Name != "PATH winner: xclip" {
		t.Fatalf("partial report = %#v", rep.Checks)
	}
}
