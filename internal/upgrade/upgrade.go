// Package upgrade resolves and downloads the newest published portal release.
// It owns only the network + version-comparison half of `portal upgrade`; the
// binary swap and daemon reload stay in cmd/portal so this package needs no
// launchd or filesystem-layout knowledge and stays hermetically testable.
package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultRepo is the GitHub repository releases are published to.
const DefaultRepo = "VikashLoomba/Portal"

// DefaultAPIBase is the GitHub REST endpoint the release lookup targets. Tests
// point this at an httptest server; nothing else overrides it.
const DefaultAPIBase = "https://api.github.com"

// AssetName is the only published binary asset. Releases ship an Apple Silicon
// build exclusively (see the release workflow), so a mismatched host is a clean
// refusal rather than a download that cannot run — Supported() gates that.
const AssetName = "portal-darwin-arm64"

// maxAssetBytes caps the download. The binary is ~24 MiB; the ceiling exists so
// a hostile or truncated response cannot exhaust the disk.
const maxAssetBytes = 200 << 20

// Release is the subset of a GitHub release the upgrade path consumes.
type Release struct {
	// Tag is the release tag, e.g. "v0.7.0".
	Tag string
	// AssetURL is the direct download URL for AssetName.
	AssetURL string
}

// Client fetches release metadata and asset bytes over HTTP.
type Client struct {
	// HTTP is the transport; nil selects a client with a bounded timeout.
	HTTP *http.Client
	// APIBase overrides DefaultAPIBase (tests only).
	APIBase string
	// Repo overrides DefaultRepo (tests only).
	Repo string
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *Client) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimSuffix(c.APIBase, "/")
	}
	return DefaultAPIBase
}

func (c *Client) repo() string {
	if c.Repo != "" {
		return c.Repo
	}
	return DefaultRepo
}

// Latest resolves the newest published release and the download URL of its
// Apple Silicon asset. A release without that asset is an error rather than a
// partial result the caller would have to re-check.
func (c *Client) Latest(ctx context.Context) (Release, error) {
	url := c.apiBase() + "/repos/" + c.repo() + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("query latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("query latest release: unexpected status %s", resp.Status)
	}
	var payload struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	// Bound the metadata read: a release payload is a few KiB.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return Release{}, fmt.Errorf("decode latest release: %w", err)
	}
	if payload.TagName == "" {
		return Release{}, fmt.Errorf("latest release has no tag")
	}
	for _, a := range payload.Assets {
		if a.Name == AssetName && a.URL != "" {
			return Release{Tag: payload.TagName, AssetURL: a.URL}, nil
		}
	}
	return Release{}, fmt.Errorf("release %s publishes no %s asset", payload.TagName, AssetName)
}

// Download writes the release asset to dst with mode 0755. dst is created and
// truncated; the caller is responsible for choosing a temporary path and for
// moving it into place only after verifying it.
func (c *Client) Download(ctx context.Context, rel Release, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.AssetURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", AssetName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: unexpected status %s", AssetName, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxAssetBytes+1))
	if err != nil {
		f.Close()
		return fmt.Errorf("download %s: %w", AssetName, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if n > maxAssetBytes {
		return fmt.Errorf("download %s: asset exceeds %d bytes", AssetName, maxAssetBytes)
	}
	if n == 0 {
		return fmt.Errorf("download %s: empty asset", AssetName)
	}
	// O_CREATE honors the umask, so an existing-file or restrictive-umask case
	// could leave the temp non-executable; make the mode explicit.
	return os.Chmod(dst, 0o755)
}

// Supported reports whether the running host matches the only published asset.
// A release ships darwin/arm64 alone, so every other host is refused up front.
func Supported(goos, goarch string) bool {
	return goos == "darwin" && goarch == "arm64"
}

// Compare orders two portal version strings by their leading vMAJOR.MINOR.PATCH
// triple, returning -1 when current precedes latest, 0 when they name the same
// release, and +1 when current is ahead.
//
// A `git describe` build ("v0.7.0-3-gabc1234") carries commits made AFTER its
// base tag, so an equal base with a commit suffix sorts AHEAD of the plain tag —
// upgrading such a build would move it backwards. An unparseable current
// version (notably the "dev" default of an un-stamped build) sorts BEFORE every
// release so `upgrade` still offers the newest binary rather than refusing.
func Compare(current, latest string) int {
	curBase, curExtra, curOK := parseVersion(current)
	latBase, _, latOK := parseVersion(latest)
	if !latOK {
		// An unparseable remote tag cannot be ordered; treat it as newer than
		// nothing so the caller's up-to-date branch does not swallow it.
		return 0
	}
	if !curOK {
		return -1
	}
	for i := 0; i < 3; i++ {
		if curBase[i] != latBase[i] {
			if curBase[i] < latBase[i] {
				return -1
			}
			return 1
		}
	}
	if curExtra {
		return 1
	}
	return 0
}

// parseVersion extracts the leading numeric triple from a version string,
// reporting whether a pre-release/commit suffix followed it.
func parseVersion(v string) (triple [3]int, extra, ok bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return triple, false, false
	}
	// Split the numeric head from any "-<n>-g<sha>" / "-rc1" / "+meta" tail.
	end := len(v)
	for i, r := range v {
		if (r < '0' || r > '9') && r != '.' {
			end = i
			break
		}
	}
	head, tail := v[:end], v[end:]
	parts := strings.Split(head, ".")
	if len(parts) != 3 {
		return triple, false, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return triple, false, false
		}
		triple[i] = n
	}
	return triple, tail != "", true
}
