package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFeatureEnabled(t *testing.T) {
	// Default-ON posture (cc-clip parity): a missing toggle file => enabled.
	tests := []struct {
		name     string
		contents *string // nil => no file written
		want     bool
	}{
		{"missing file defaults on", nil, true},
		{"empty file is on", strptr(""), true},
		{"whitespace is on", strptr("  \n\t"), true},
		{"off disables", strptr("off"), false},
		{"OFF case-insensitive", strptr("OFF\n"), false},
		{"false disables", strptr("false"), false},
		{"0 disables", strptr("0"), false},
		{"no disables", strptr("no"), false},
		{"disabled disables", strptr("disabled"), false},
		{"on enables", strptr("on"), true},
		{"garbage is on", strptr("yes please"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			s := New(dir)
			if tt.contents != nil {
				if err := os.WriteFile(filepath.Join(dir, "feature."+FeatureClipText), []byte(*tt.contents), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := s.FeatureEnabled(FeatureClipText); got != tt.want {
				t.Errorf("FeatureEnabled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetFeature_RoundTrip(t *testing.T) {
	s := New(t.TempDir())
	for _, f := range []string{FeatureClipImage, FeatureClipText, FeatureNotify} {
		if !s.FeatureEnabled(f) {
			t.Errorf("%s should default ON", f)
		}
		if err := s.SetFeature(f, false); err != nil {
			t.Fatal(err)
		}
		if s.FeatureEnabled(f) {
			t.Errorf("%s should be OFF after SetFeature(false)", f)
		}
		if err := s.SetFeature(f, true); err != nil {
			t.Fatal(err)
		}
		if !s.FeatureEnabled(f) {
			t.Errorf("%s should be ON after SetFeature(true)", f)
		}
	}
}

func strptr(s string) *string { return &s }

func TestReadHost_Whitespace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "host"), []byte("  clementine \n\t"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(dir)
	got, err := s.ReadHost()
	if err != nil {
		t.Fatal(err)
	}
	if got != "clementine" {
		t.Errorf("ReadHost = %q, want %q", got, "clementine")
	}
}

func TestReadHost_Missing(t *testing.T) {
	s := New(t.TempDir())
	got, err := s.ReadHost()
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("ReadHost missing = %q, want empty", got)
	}
}

func TestAllowedPorts_CommentsAndDupesAndJunk(t *testing.T) {
	dir := t.TempDir()
	contents := `# header comment
40085
40085
# inline above blank line below

8081 # trailing comment ignored
not-a-port
0
9999
`
	if err := os.WriteFile(filepath.Join(dir, "allow"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(dir)
	got, err := s.AllowedPorts()
	if err != nil {
		t.Fatal(err)
	}
	want := []int{8081, 9999, 40085}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllowedPorts = %v, want %v", got, want)
	}
}

func TestAllowUnallow_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	added, err := s.Allow([]int{40085, 8081, 40085})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, []int{40085, 8081}) {
		t.Errorf("Allow added = %v, want [40085 8081]", added)
	}
	// Re-allow same: nothing added.
	added2, _ := s.Allow([]int{40085})
	if len(added2) != 0 {
		t.Errorf("re-Allow added = %v, want []", added2)
	}
	// Unallow drops one.
	if err := s.Unallow([]int{8081}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.AllowedPorts()
	want := []int{40085}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after Unallow: %v, want %v", got, want)
	}
}

func TestWriteHost_Trims(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.WriteHost("  clementine\n"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "host"))
	if string(b) != "clementine\n" {
		t.Errorf("on-disk host = %q, want %q", string(b), "clementine\n")
	}
}
