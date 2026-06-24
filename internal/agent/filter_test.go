package agent

import (
	"reflect"
	"testing"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/agent/watcher"
)

func L(port uint16, fam uint8, addr string) watcher.Listen {
	return watcher.Listen{Port: port, Family: fam, Addr: addr}
}

func TestFilter_Loopback(t *testing.T) {
	f := NewFilter(32768, 60999)
	f.SetAllowDeny(nil, nil, true)

	in := []watcher.Listen{
		L(8081, 4, "127.0.0.1"),
		L(8082, 4, "0.0.0.0"),     // not loopback → drop
		L(8083, 4, "192.168.1.5"), // not loopback → drop
		L(8084, 6, "::1"),
		L(8085, 6, "::"),      // not loopback → drop
		L(8086, 6, "fe80::1"), // not loopback → drop
	}
	got := f.Apply(in)
	want := []watcher.Listen{L(8081, 4, "127.0.0.1"), L(8084, 6, "::1")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loopback: got %v, want %v", got, want)
	}
}

func TestFilter_DenyAndEphemeral(t *testing.T) {
	f := NewFilter(32768, 60999)
	f.SetAllowDeny([]uint16{22, 25}, nil, true)

	in := []watcher.Listen{
		L(8081, 4, "127.0.0.1"),  // keep
		L(22, 4, "127.0.0.1"),    // deny → drop
		L(25, 4, "127.0.0.1"),    // deny → drop
		L(40000, 4, "127.0.0.1"), // ephemeral → drop
		L(8081, 4, "127.0.0.1"),  // exact dup
	}
	got := f.Apply(in)
	if len(got) != 2 {
		t.Errorf("expected 2 kept, got %d (%v)", len(got), got)
	}
}

func TestFilter_AllowOverridesAll(t *testing.T) {
	f := NewFilter(32768, 60999)
	f.SetAllowDeny([]uint16{22}, []uint16{22, 40085}, true)

	in := []watcher.Listen{
		L(22, 4, "127.0.0.1"),    // both deny AND allow → allow wins
		L(40085, 4, "127.0.0.1"), // ephemeral AND allow → allow wins
		L(40086, 4, "127.0.0.1"), // ephemeral, not allowed → drop
	}
	got := f.Apply(in)
	want := []watcher.Listen{L(22, 4, "127.0.0.1"), L(40085, 4, "127.0.0.1")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("allow override: got %v, want %v", got, want)
	}
}

func TestFilter_SortByPort(t *testing.T) {
	f := NewFilter(32768, 60999)
	f.SetAllowDeny(nil, nil, false)

	in := []watcher.Listen{
		L(9111, 4, "127.0.0.1"),
		L(3000, 6, "::1"),
		L(8081, 4, "127.0.0.1"),
	}
	got := f.Apply(in)
	want := []uint16{3000, 8081, 9111}
	for i, l := range got {
		if l.Port != want[i] {
			t.Errorf("got[%d]=%d, want %d", i, l.Port, want[i])
		}
	}
}

func TestFilter_AcceptIPv4Subnets(t *testing.T) {
	f := NewFilter(32768, 60999)
	f.SetAllowDeny(nil, nil, false)
	if !f.Accept(L(8081, 4, "127.0.53.1")) { // RFC 5735 — entire 127.0.0.0/8 is loopback
		t.Errorf("127.0.53.1 should be loopback")
	}
	if !f.Accept(L(8081, 4, "127.255.255.255")) {
		t.Errorf("127.255.255.255 should be loopback")
	}
	if f.Accept(L(8081, 4, "126.0.0.1")) {
		t.Errorf("126.0.0.1 should NOT be loopback")
	}
}
