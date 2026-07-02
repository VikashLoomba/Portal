package main

import (
	"context"
	"io"
	"testing"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/transport"
)

// nativeHealthTransport is a transport.Transport whose Health is fully
// configurable — crucially it can report {Up:true, Pid:0}, the shape a native
// (pidless) connection produces. It is the EC10 fixture proving no caller gates
// on Pid>0: a healthy Pid==0 transport must behave exactly like a healthy
// Pid==N one.
type nativeHealthTransport struct {
	up  bool
	pid int
}

func (t nativeHealthTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (t nativeHealthTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: t.up, Pid: t.pid}, nil
}
func (t nativeHealthTransport) Exec(context.Context, []byte, ...string) (string, string, error) {
	return "", "", nil
}
func (t nativeHealthTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, func() error { return nil }, nil
}
func (t nativeHealthTransport) Close(context.Context) (bool, error) { return false, nil }
func (t nativeHealthTransport) Describe() transport.Desc {
	return transport.Desc{Impl: "native-ssh", Host: "fakehost", Endpoint: "conn"}
}

var _ transport.Transport = nativeHealthTransport{}

// recordingForwarder is a transport.PortForwarder that records Forward calls and
// returns a canned current-forward set from ListForwards. Used to prove the run
// auto-forward routes through App.PF (not App.Transport) under a native-shaped
// Health.
type recordingForwarder struct {
	current   []int
	lines     []string
	forwarded [][2]int
	cancelled [][2]int
}

func (f *recordingForwarder) Forward(_ context.Context, l, r int) error {
	f.forwarded = append(f.forwarded, [2]int{l, r})
	return nil
}
func (f *recordingForwarder) Cancel(_ context.Context, l, r int) error {
	f.cancelled = append(f.cancelled, [2]int{l, r})
	return nil
}
func (f *recordingForwarder) ListForwards(context.Context) ([]int, error) {
	return append([]int(nil), f.current...), nil
}
func (f *recordingForwarder) ForwardLines(context.Context) ([]string, error) {
	return append([]string(nil), f.lines...), nil
}

var _ transport.PortForwarder = (*recordingForwarder)(nil)

// TestClipUploadReachable proves the clip-upload gate proceeds on Health.Up and
// does NOT bail on Pid==0 (EC10a): a healthy native transport (Pid==0) is
// reachable; a down transport is not.
func TestClipUploadReachable(t *testing.T) {
	tests := []struct {
		name string
		tr   transport.Transport
		want bool
	}{
		{"healthy native transport (pid=0) is reachable", nativeHealthTransport{up: true, pid: 0}, true},
		{"healthy system transport (pid=N) is reachable", nativeHealthTransport{up: true, pid: 4242}, true},
		{"down transport is not reachable", nativeHealthTransport{up: false, pid: 0}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clipUploadReachable(context.Background(), tt.tr); got != tt.want {
				t.Errorf("clipUploadReachable = %v, want %v", got, tt.want)
			}
		})
	}
}
