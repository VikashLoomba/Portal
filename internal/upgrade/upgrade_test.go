package upgrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	tests := []struct {
		name    string
		current string
		latest  string
		want    int
	}{
		{"older minor", "v0.4.1", "v0.7.0", -1},
		{"older patch", "v0.7.0", "v0.7.1", -1},
		{"older major", "v0.9.9", "v1.0.0", -1},
		{"same tag", "v0.7.0", "v0.7.0", 0},
		{"newer", "v0.8.0", "v0.7.0", 1},
		// A git-describe build carries commits made after its base tag, so it
		// is AHEAD of the plain tag — upgrading it would move it backwards.
		{"describe build ahead of its base tag", "v0.7.0-3-gabc1234", "v0.7.0", 1},
		{"describe build behind a newer tag", "v0.4.1-19-g6d9988f", "v0.7.0", -1},
		// An un-stamped build must still be offered the newest release.
		{"dev build", "dev", "v0.7.0", -1},
		{"empty current", "", "v0.7.0", -1},
		{"unparseable current", "not-a-version", "v0.7.0", -1},
		{"prefixless current", "0.4.1", "v0.7.0", -1},
		{"rc suffix ahead of base", "v0.7.0-rc1", "v0.7.0", 1},
		// An unorderable remote tag must not be reported as an upgrade.
		{"unparseable latest", "v0.7.0", "nightly", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Compare(tt.current, tt.latest); got != tt.want {
				t.Fatalf("Compare(%q, %q) = %d, want %d", tt.current, tt.latest, got, tt.want)
			}
		})
	}
}

func TestSupported(t *testing.T) {
	if !Supported("darwin", "arm64") {
		t.Fatal("darwin/arm64 must be supported — it is the published asset")
	}
	for _, tc := range [][2]string{{"darwin", "amd64"}, {"linux", "arm64"}, {"linux", "amd64"}} {
		if Supported(tc[0], tc[1]) {
			t.Fatalf("%s/%s reported as supported; releases publish %s only", tc[0], tc[1], AssetName)
		}
	}
}

// releaseServer serves a canned latest-release payload plus the asset bytes.
func releaseServer(t *testing.T, tag string, assets map[string]string, body []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		var entries []string
		for name, path := range assets {
			entries = append(entries, `{"name":`+quote(name)+`,"browser_download_url":`+quote(srv.URL+path)+`}`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":` + quote(tag) + `,"assets":[` + strings.Join(entries, ",") + `]}`))
	})
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
	return srv
}

func quote(s string) string { return `"` + s + `"` }

func TestLatest(t *testing.T) {
	srv := releaseServer(t, "v0.7.0", map[string]string{AssetName: "/asset"}, []byte("payload"))
	c := &Client{APIBase: srv.URL, Repo: "owner/repo"}

	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Tag != "v0.7.0" {
		t.Fatalf("tag = %q, want v0.7.0", rel.Tag)
	}
	if !strings.HasSuffix(rel.AssetURL, "/asset") {
		t.Fatalf("asset URL = %q, want the %s download URL", rel.AssetURL, AssetName)
	}
}

func TestLatestRejectsReleaseWithoutTheAsset(t *testing.T) {
	srv := releaseServer(t, "v0.7.0", map[string]string{"portal-linux-amd64": "/asset"}, []byte("payload"))
	c := &Client{APIBase: srv.URL, Repo: "owner/repo"}

	if _, err := c.Latest(context.Background()); err == nil ||
		!strings.Contains(err.Error(), AssetName) {
		t.Fatalf("err = %v, want a missing-%s error", err, AssetName)
	}
}

func TestLatestRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := &Client{APIBase: srv.URL, Repo: "owner/repo"}

	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("a 500 must be an error, not an empty release")
	}
}

func TestLatestRejectsTaglessPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"assets":[]}`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{APIBase: srv.URL, Repo: "owner/repo"}

	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("a payload with no tag must be an error")
	}
}

func TestDownloadWritesExecutableBytes(t *testing.T) {
	want := []byte("#!/bin/sh\necho portal v0.7.0\n")
	srv := releaseServer(t, "v0.7.0", map[string]string{AssetName: "/asset"}, want)
	c := &Client{APIBase: srv.URL, Repo: "owner/repo"}
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "portal.tmp")
	if err := c.Download(context.Background(), rel, dst); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("downloaded %q, want %q", got, want)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("mode = %v, want the executable bit set", info.Mode().Perm())
	}
}

func TestDownloadRejectsEmptyAsset(t *testing.T) {
	srv := releaseServer(t, "v0.7.0", map[string]string{AssetName: "/asset"}, nil)
	c := &Client{APIBase: srv.URL, Repo: "owner/repo"}
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}

	err = c.Download(context.Background(), rel, filepath.Join(t.TempDir(), "portal.tmp"))
	if err == nil || !strings.Contains(err.Error(), "empty asset") {
		t.Fatalf("err = %v, want an empty-asset error", err)
	}
}

func TestDownloadRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := &Client{}

	err := c.Download(context.Background(), Release{Tag: "v0.7.0", AssetURL: srv.URL},
		filepath.Join(t.TempDir(), "portal.tmp"))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want a 404 error", err)
	}
}
