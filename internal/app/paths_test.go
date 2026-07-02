package app

import (
	"path/filepath"
	"testing"
)

// TestDerivePaths_APISock covers the three APISock derivations: the default
// under ConfigDir, the PORTAL_API_SOCK override, and PORTAL_CONFIG_DIR
// propagating into the default (D5 — the staging harness isolates the API
// socket for free). t.Setenv with "" behaves as unset for DerivePaths, which
// treats an empty env value as absent.
func TestDerivePaths_APISock(t *testing.T) {
	const home = "/home/tester"
	const uid = 501

	tests := []struct {
		name      string
		configDir string // PORTAL_CONFIG_DIR ("" = unset)
		apiSock   string // PORTAL_API_SOCK ("" = unset)
		want      string
	}{
		{
			name:      "default under config dir",
			configDir: "",
			apiSock:   "",
			want:      filepath.Join(home, ".config", Tool, "api.sock"),
		},
		{
			name:      "PORTAL_API_SOCK override wins",
			configDir: "",
			apiSock:   "/custom/place/api.sock",
			want:      "/custom/place/api.sock",
		},
		{
			name:      "override wins even with a custom config dir",
			configDir: "/staging/cfg",
			apiSock:   "/custom/place/api.sock",
			want:      "/custom/place/api.sock",
		},
		{
			name:      "PORTAL_CONFIG_DIR propagates into the default",
			configDir: "/staging/cfg",
			apiSock:   "",
			want:      filepath.Join("/staging/cfg", "api.sock"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PORTAL_CONFIG_DIR", tt.configDir)
			t.Setenv("PORTAL_API_SOCK", tt.apiSock)
			p := DerivePaths(home, uid)
			if p.APISock != tt.want {
				t.Errorf("APISock = %q, want %q", p.APISock, tt.want)
			}
		})
	}
}
