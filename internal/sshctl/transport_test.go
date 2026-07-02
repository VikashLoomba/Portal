package sshctl

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/VikashLoomba/Portal/internal/run"
	"github.com/VikashLoomba/Portal/internal/transport"
)

// Real OpenSSH writes "Master running (pid=12345)\r\n" to STDERR. The
// transport MUST parse stderr (not stdout) — this test enforces that.
func TestMasterPID_StderrSource(t *testing.T) {
	fake := &run.Fake{}
	fake.AddReply(run.FakeReply{
		Match:  "ssh",
		Stdout: "",
		Stderr: "Master running (pid=12345)\r\n",
	})
	s := New("/tmp/sock", "clementine", nil, fake)
	pid, err := s.masterPID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pid != 12345 {
		t.Errorf("masterPID = %d, want 12345", pid)
	}
}

func TestMasterPID_NoMaster(t *testing.T) {
	fake := &run.Fake{}
	fake.AddReply(run.FakeReply{
		Match:    "ssh",
		Stderr:   "Control socket connect(/tmp/sock): No such file or directory\r\n",
		ExitCode: 255,
	})
	s := New("/tmp/sock", "clementine", nil, fake)
	pid, _ := s.masterPID(context.Background())
	if pid != 0 {
		t.Errorf("masterPID = %d, want 0", pid)
	}
}

// Forward exit codes are unreliable; success/failure is decided by parsing
// stderr for the substring "request failed". A successful forward exits with
// non-zero in some OpenSSH versions yet has clean stderr — that must be
// treated as success. This test enforces that.
func TestForward_Success_NonzeroExit(t *testing.T) {
	fake := &run.Fake{}
	fake.AddReply(run.FakeReply{
		Match:    "ssh",
		Stderr:   "Forwarding request added.\r\n",
		ExitCode: 1, // unreliable — must NOT be treated as failure
	})
	s := New("/tmp/sock", "clementine", nil, fake)
	if err := s.Forward(context.Background(), 8081, 8081); err != nil {
		t.Errorf("Forward returned error on clean stderr: %v", err)
	}
}

func TestForward_Failure_StderrSubstring(t *testing.T) {
	fake := &run.Fake{}
	fake.AddReply(run.FakeReply{
		Match:    "ssh",
		Stderr:   "Forwarding request to /tmp/sock failed: open: bind: Address already in use\r\nrequest failed\r\n",
		ExitCode: 0, // exit code says "ok" but stderr says otherwise
	})
	s := New("/tmp/sock", "clementine", nil, fake)
	err := s.Forward(context.Background(), 8081, 8081)
	if err == nil {
		t.Fatal("expected ForwardError")
	}
	var fe *transport.ForwardError
	if !errors.As(err, &fe) {
		t.Errorf("got %T, want *transport.ForwardError", err)
	}
	if fe.Port != 8081 {
		t.Errorf("ForwardError.Port = %d, want 8081", fe.Port)
	}
}

// Forward should send the canonical -L "<p>:localhost:<p>" spec so both
// IPv4 and IPv6 binds on the remote are reachable.
func TestForward_LocalhostSpec(t *testing.T) {
	fake := &run.Fake{}
	fake.Default = run.FakeReply{}
	s := New("/tmp/sock", "clementine", nil, fake)
	_ = s.Forward(context.Background(), 8082, 8082)
	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fake.Calls))
	}
	args := fake.Calls[0].Args
	// Look for the -L arg.
	want := "8082:localhost:8082"
	found := false
	for _, a := range args {
		if a == want {
			found = true
		}
	}
	if !found {
		t.Errorf("-L spec not found in args %v", args)
	}
}

// TestExec_ArgvByteCompat locks T9 for the Exec argv path: Exec appends argv
// VERBATIM as trailing args to the ssh invocation (the ssh binary performs the
// space-join + remote re-shell). It MUST NOT adopt localexec's `sh -c <joined>`
// wrapping. A nil-stdin Exec routes through the injected Runner, so the fake
// observes the exact arg list ending in `... <host> a b c`.
func TestExec_ArgvByteCompat(t *testing.T) {
	fake := &run.Fake{}
	fake.Default = run.FakeReply{}
	s := New("/tmp/sock", "clementine", nil, fake)
	if _, _, err := s.Exec(context.Background(), nil, "a", "b", "c"); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fake.Calls))
	}
	args := fake.Calls[0].Args
	want := []string{"-S", "/tmp/sock", "clementine", "a", "b", "c"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("Exec args = %v, want %v (argv appended verbatim, no sh -c)", args, want)
	}
	// Guard against any sh -c drift.
	for _, a := range args {
		if a == "sh" || a == "-c" {
			t.Errorf("Exec argv must not wrap in sh -c; args = %v", args)
		}
	}
}

// --- dual-stack (u1) new-method tests: prove behavior-identity with the
// legacy counterparts before any consumer migrates. ---

// fakeForwards records the pid it is asked about and returns scripted values.
type fakeForwards struct {
	gotPID int
	ports  []int
	lines  []string
}

func (f *fakeForwards) MasterForwards(_ context.Context, pid int) ([]int, error) {
	f.gotPID = pid
	return f.ports, nil
}

func (f *fakeForwards) MasterForwardLines(_ context.Context, pid int) ([]string, error) {
	f.gotPID = pid
	return f.lines, nil
}

func masterUpFake() *run.Fake {
	f := &run.Fake{}
	f.Default = run.FakeReply{Stderr: "Master running (pid=4242)\r\n"}
	return f
}

func TestHealth_Up(t *testing.T) {
	s := New("/tmp/sock", "clementine", nil, masterUpFake())
	h, err := s.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := transport.Health{Up: true, Pid: 4242, Detail: "pid=4242"}
	if h != want {
		t.Errorf("Health = %+v, want %+v", h, want)
	}
}

func TestHealth_Down(t *testing.T) {
	fake := &run.Fake{}
	fake.Default = run.FakeReply{
		Stderr:   "Control socket connect(/tmp/sock): No such file or directory\r\n",
		ExitCode: 255,
	}
	s := New("/tmp/sock", "clementine", nil, fake)
	h, err := s.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := transport.Health{Up: false, Pid: 0, Detail: ""}
	if h != want {
		t.Errorf("Health = %+v, want %+v", h, want)
	}
}

func TestDescribe(t *testing.T) {
	s := New("/tmp/sock", "clementine", nil, &run.Fake{})
	got := s.Describe()
	want := transport.Desc{Impl: "system-ssh", Host: "clementine", Endpoint: "/tmp/sock"}
	if got != want {
		t.Errorf("Describe = %+v, want %+v", got, want)
	}
}

// Ensure mirrors EnsureMaster's rebuilt bool: an already-running master yields
// rebuilt=false (no rebuild performed).
func TestEnsure_MirrorsRebuilt(t *testing.T) {
	s := New("/tmp/sock", "clementine", nil, masterUpFake())
	rebuilt, err := s.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt {
		t.Error("Ensure rebuilt = true for already-running master, want false")
	}
}

func TestListForwards_DelegatesWithMasterPID(t *testing.T) {
	fwd := &fakeForwards{ports: []int{8080, 9090}}
	s := New("/tmp/sock", "clementine", nil, masterUpFake())
	s.Forwards = fwd
	ports, err := s.ListForwards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fwd.gotPID != 4242 {
		t.Errorf("MasterForwards called with pid %d, want 4242", fwd.gotPID)
	}
	if !reflect.DeepEqual(ports, []int{8080, 9090}) {
		t.Errorf("ListForwards = %v, want [8080 9090]", ports)
	}
}

func TestForwardLines_DelegatesWithMasterPID(t *testing.T) {
	fwd := &fakeForwards{lines: []string{"127.0.0.1:8080", "[::1]:8080"}}
	s := New("/tmp/sock", "clementine", nil, masterUpFake())
	s.Forwards = fwd
	lines, err := s.ForwardLines(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fwd.gotPID != 4242 {
		t.Errorf("MasterForwardLines called with pid %d, want 4242", fwd.gotPID)
	}
	if !reflect.DeepEqual(lines, []string{"127.0.0.1:8080", "[::1]:8080"}) {
		t.Errorf("ForwardLines = %v, want [127.0.0.1:8080 [::1]:8080]", lines)
	}
}

// When the master is down, List/Lines return nil WITHOUT touching the source.
func TestListForwards_NilWhenMasterDown(t *testing.T) {
	fwd := &fakeForwards{ports: []int{8080}}
	fake := &run.Fake{}
	fake.Default = run.FakeReply{ExitCode: 255}
	s := New("/tmp/sock", "clementine", nil, fake)
	s.Forwards = fwd
	ports, err := s.ListForwards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ports != nil {
		t.Errorf("ListForwards = %v, want nil when master down", ports)
	}
	if fwd.gotPID != 0 {
		t.Errorf("MasterForwards called (pid=%d) despite down master", fwd.gotPID)
	}
	lines, err := s.ForwardLines(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lines != nil {
		t.Errorf("ForwardLines = %v, want nil when master down", lines)
	}
}

// Close mirrors Exit: stopped=true iff the master responded (exit code 0).
func TestClose_MirrorsExit(t *testing.T) {
	fake := &run.Fake{}
	fake.Default = run.FakeReply{} // exit 0 => stopped
	s := New("/tmp/sock", "clementine", nil, fake)
	stopped, err := s.Close(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Error("Close stopped = false, want true when master responded")
	}
}
