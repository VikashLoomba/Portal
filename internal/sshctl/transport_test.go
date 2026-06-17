package sshctl

import (
	"context"
	"errors"
	"testing"

	"github.com/vikashl/portal/internal/run"
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
	pid, err := s.MasterPID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pid != 12345 {
		t.Errorf("MasterPID = %d, want 12345", pid)
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
	pid, _ := s.MasterPID(context.Background())
	if pid != 0 {
		t.Errorf("MasterPID = %d, want 0", pid)
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
	var fe *ForwardError
	if !errors.As(err, &fe) {
		t.Errorf("got %T, want *ForwardError", err)
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
