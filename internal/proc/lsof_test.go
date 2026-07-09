package proc

import (
	"context"
	"reflect"
	"testing"

	"github.com/VikashLoomba/Portal/pkg/run"
)

func TestPortFromLsofName(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		wantOk bool
	}{
		{"127.0.0.1:7265", 7265, true},
		{"[::1]:3000", 3000, true},
		{"*:22", 22, true},
		{"127.0.0.1:7265->1.2.3.4:80", 7265, true},
		{"::1.3000", 0, false},
		{"127.0.0.1:abc", 0, false},
		{"", 0, false},
		{":80", 80, true},
		{"127.0.0.1:0", 0, false},
	}
	for _, c := range cases {
		got, ok := portFromLsofName(c.in)
		if got != c.want || ok != c.wantOk {
			t.Errorf("portFromLsofName(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.wantOk)
		}
	}
}

func TestMasterForwardsParse(t *testing.T) {
	// Realistic lsof output for a master ssh forwarding three local LISTENs,
	// one IPv6, one IPv4, plus a duplicate (ssh listens on both v4 and v6).
	stdout := `COMMAND   PID    USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
ssh     12345 vikashl    7u  IPv4 0x1                0t0  TCP 127.0.0.1:8081 (LISTEN)
ssh     12345 vikashl    8u  IPv6 0x2                0t0  TCP [::1]:8081 (LISTEN)
ssh     12345 vikashl    9u  IPv4 0x3                0t0  TCP 127.0.0.1:9111 (LISTEN)
ssh     12345 vikashl   10u  IPv4 0x4                0t0  TCP 127.0.0.1:3000 (LISTEN)
`
	fake := &run.Fake{}
	fake.AddReply(run.FakeReply{Match: "lsof", Stdout: stdout})

	l := New("/usr/sbin/lsof", fake)
	got, err := l.MasterForwards(context.Background(), 12345)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{3000, 8081, 9111}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MasterForwards = %v, want %v", got, want)
	}
}

func TestMasterForwardsZeroPID(t *testing.T) {
	l := New("/usr/sbin/lsof", &run.Fake{})
	got, _ := l.MasterForwards(context.Background(), 0)
	if got != nil {
		t.Errorf("zero pid: got %v, want nil", got)
	}
}

func TestLocalHolder(t *testing.T) {
	fake := &run.Fake{}
	fake.AddReply(run.FakeReply{Match: "lsof", Stdout: "999\n"})
	l := New("/usr/sbin/lsof", fake)
	pid, _ := l.LocalHolder(context.Background(), 8081)
	if pid != 999 {
		t.Errorf("LocalHolder = %d, want 999", pid)
	}
}

func TestLocalHolderEmpty(t *testing.T) {
	fake := &run.Fake{Default: run.FakeReply{Stdout: ""}}
	l := New("/usr/sbin/lsof", fake)
	pid, _ := l.LocalHolder(context.Background(), 8081)
	if pid != 0 {
		t.Errorf("LocalHolder empty = %d, want 0", pid)
	}
}
