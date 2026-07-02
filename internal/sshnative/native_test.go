package sshnative

import (
	"testing"
)

// TestDescribe pins the Desc shape; Impl is always native-ssh and Pid semantics
// (Health) are covered by the conformance suite. The passthrough resolver splits
// the literal target verbatim, so the resolved endpoint equals the input.
func TestDescribe(t *testing.T) {
	c, err := New("alice@box:2222",
		WithConfigResolver(passthroughResolver),
		WithHostKeyCallback(insecureIgnoreHostKey))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := c.Describe()
	if d.Impl != "native-ssh" {
		t.Errorf("Impl = %q, want native-ssh", d.Impl)
	}
	if d.Host != "box" {
		t.Errorf("Host = %q, want box", d.Host)
	}
	if d.Endpoint != "alice@box:2222" {
		t.Errorf("Endpoint = %q, want alice@box:2222", d.Endpoint)
	}
}

// TestNewNoDial proves New does not dial: constructing a Client for an
// unreachable target succeeds and reports Health.Up == false.
func TestNewNoDial(t *testing.T) {
	c, err := New("alice@203.0.113.1:22",
		WithConfigResolver(passthroughResolver),
		WithHostKeyCallback(insecureIgnoreHostKey))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := c.Health(nil)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Up {
		t.Error("Health.Up = true before Ensure, want false")
	}
	if h.Pid != 0 {
		t.Errorf("Health.Pid = %d, want 0 (native has no pid)", h.Pid)
	}
}
