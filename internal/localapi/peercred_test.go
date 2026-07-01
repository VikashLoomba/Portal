package localapi

import "testing"

// TestAllowPeer is the EC5 peer-cred decision test (function-level, no
// syscalls): only a same-uid peer is authorized.
func TestAllowPeer(t *testing.T) {
	tests := []struct {
		name     string
		peerUID  int
		selfUID  int
		expected bool
	}{
		{"same uid", 501, 501, true},
		{"root peer, user self", 0, 501, false},
		{"user peer, root self", 501, 0, false},
		{"same non-default uid", 1000, 1000, true},
		{"off by one", 501, 502, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allowPeer(tt.peerUID, tt.selfUID); got != tt.expected {
				t.Errorf("allowPeer(%d, %d) = %v, want %v", tt.peerUID, tt.selfUID, got, tt.expected)
			}
		})
	}
}
