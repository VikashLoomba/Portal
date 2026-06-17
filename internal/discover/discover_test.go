package discover

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeTransport struct {
	stdout string
	err    error
	argv   []string
	stdin  string
}

func (f *fakeTransport) MasterPID(ctx context.Context) (int, error)             { return 0, nil }
func (f *fakeTransport) EnsureMaster(ctx context.Context) (int, bool, error)    { return 1, false, nil }
func (f *fakeTransport) Forward(ctx context.Context, l, r int) error            { return nil }
func (f *fakeTransport) Cancel(ctx context.Context, l, r int) error             { return nil }
func (f *fakeTransport) Exit(ctx context.Context) (bool, error)                 { return false, nil }
func (f *fakeTransport) Host() string                                           { return "x" }
func (f *fakeTransport) Sock() string                                           { return "/tmp/sock-x" }
func (f *fakeTransport) Exec(_ context.Context, stdin string, argv ...string) (string, error) {
	f.argv = append([]string(nil), argv...)
	f.stdin = stdin
	return f.stdout, f.err
}

func TestDesiredPorts_Parse(t *testing.T) {
	t.Helper()
	ft := &fakeTransport{stdout: "8081\n8082\n3000\n8081\n\n"}
	d := New(ft)
	got, err := d.DesiredPorts(context.Background(), []int{22, 25}, []int{40085})
	if err != nil {
		t.Fatal(err)
	}
	want := []int{3000, 8081, 8082}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DesiredPorts = %v, want %v", got, want)
	}

	// Argv must include "bash -s 22 25 -- 40085" verbatim.
	joined := strings.Join(ft.argv, " ")
	if !strings.Contains(joined, "bash -s 22 25 -- 40085") {
		t.Errorf("argv = %v; want contains 'bash -s 22 25 -- 40085'", ft.argv)
	}
	// Stdin should be the embedded remote.sh.
	if !strings.Contains(ft.stdin, "ss -Htln") {
		t.Errorf("stdin missing ss -Htln; got len=%d", len(ft.stdin))
	}
}

func TestDesiredPorts_TransportErrorPropagates(t *testing.T) {
	ft := &fakeTransport{err: errors.New("boom")}
	d := New(ft)
	if _, err := d.DesiredPorts(context.Background(), nil, nil); err == nil {
		t.Errorf("expected error, got nil")
	}
}
