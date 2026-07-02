package sshnative

import (
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		wantUser string
		wantHost string
		wantPort int
		wantErr  string // substring; "" means no error
	}{
		{name: "user_host_default_port", target: "alice@box", wantUser: "alice", wantHost: "box", wantPort: 22},
		{name: "user_host_explicit_port", target: "bob@10.0.0.5:2222", wantUser: "bob", wantHost: "10.0.0.5", wantPort: 2222},
		{name: "user_with_at_in_host_uses_last_at", target: "a@b@host", wantUser: "a@b", wantHost: "host", wantPort: 22},
		{name: "missing_user", target: "justhost", wantErr: "missing user"},
		{name: "empty_user", target: "@host", wantErr: "empty user"},
		{name: "empty_host", target: "user@", wantErr: "empty host"},
		{name: "bad_port", target: "user@host:notaport", wantErr: "invalid port"},
		{name: "port_out_of_range", target: "user@host:70000", wantErr: "invalid port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, host, port, err := parseTarget(tt.target)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseTarget(%q): want error containing %q, got nil", tt.target, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTarget(%q): unexpected error %v", tt.target, err)
			}
			if user != tt.wantUser || host != tt.wantHost || port != tt.wantPort {
				t.Errorf("parseTarget(%q) = (%q, %q, %d), want (%q, %q, %d)",
					tt.target, user, host, port, tt.wantUser, tt.wantHost, tt.wantPort)
			}
		})
	}
}

// TestDescribe pins the Desc shape; Impl is always native-ssh and Pid semantics
// (Health) are covered by the conformance suite.
func TestDescribe(t *testing.T) {
	c, err := New("alice@box:2222", WithHostKeyCallback(insecureIgnoreHostKey))
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
	c, err := New("alice@203.0.113.1:22", WithHostKeyCallback(insecureIgnoreHostKey))
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
