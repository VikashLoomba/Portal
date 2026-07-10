// Package config is the file-backed mutable state: the configured dev box
// host and the allowlist. Both files are read every reconcile pass so edits
// take effect within the poll interval with no daemon restart — same contract
// as the bash original.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Store struct {
	// Dir is the config directory (e.g. ~/.config/portal).
	Dir string

	// mu serializes the allow/feature file read-modify-write mutations against
	// each other. The allow file is edited via a non-atomic RMW (AllowedPorts →
	// filter/append → rewrite), so two concurrent mutators — the daemon now
	// serves allow/unallow from per-request HTTP goroutines (D7 single owner) —
	// would otherwise interleave and silently drop one edit. A process mutex is
	// sufficient because the daemon is the single-instance owner of the socket.
	mu sync.Mutex
}

func New(dir string) *Store { return &Store{Dir: dir} }

func (s *Store) hostFile() string      { return filepath.Join(s.Dir, "host") }
func (s *Store) allowFile() string     { return filepath.Join(s.Dir, "allow") }
func (s *Store) transportFile() string { return filepath.Join(s.Dir, "transport") }

// Transport reports the configured transport selection (T8). It returns
// "system" (the default) when the file is absent, the trimmed file value when
// present, and an error naming the file and the bad value for anything other
// than "system"/"native". An invalid value is a LOUD failure — callers surface
// it at startup rather than silently falling back to system. "localexec" is
// deliberately NOT accepted here (it is test/dev only, never config-selectable).
func (s *Store) Transport() (string, error) {
	b, err := os.ReadFile(s.transportFile())
	if err != nil {
		if os.IsNotExist(err) {
			return "system", nil
		}
		return "", err
	}
	v := strings.TrimSpace(string(b))
	switch v {
	case "system", "native":
		return v, nil
	default:
		return "", fmt.Errorf("invalid transport %q in %s (want \"system\" or \"native\")", v, s.transportFile())
	}
}

// SetTransport persists the transport selection (creating Dir if needed),
// validating that name is "system" or "native" (else an error). The value is
// whitespace-trimmed before write so any SetTransport→Transport round-trip is
// idempotent. "localexec" is rejected — it is test/dev only.
func (s *Store) SetTransport(name string) error {
	name = strings.TrimSpace(name)
	switch name {
	case "system", "native":
	default:
		return fmt.Errorf("invalid transport %q (want \"system\" or \"native\")", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.transportFile(), []byte(name+"\n"), 0o644)
}

// featureFile is the per-feature on/off toggle path, e.g. ~/.config/portal/
// feature.clip-text. One file per feature (mirroring the host/allow file-per-
// setting convention) so a user can flip a capability with a plain `rm` /
// `touch` and the running daemon picks it up on the next read with no restart.
func (s *Store) featureFile(feature string) string {
	return filepath.Join(s.Dir, "feature."+feature)
}

// Feature toggle names. These are the capability-gate keys (SPEC C): the Mac
// side serves clip-read / notify / credentials only when the corresponding
// feature is enabled.
const (
	// FeatureClipImage gates serving the Mac clipboard IMAGE to a remote shim.
	FeatureClipImage = "clip-image"
	// FeatureClipText gates serving the Mac clipboard TEXT to a remote shim.
	// Text is additionally subject to the concealed-clipboard skip (a password
	// manager marking the pasteboard org.nspasteboard.ConcealedType is never
	// served even when this is on) — that check is independent of this toggle.
	FeatureClipText = "clip-text"
	// FeatureNotify gates raising a native macOS notification from a relayed
	// remote event (Claude Code hook / generic `portald notify`).
	FeatureNotify = "notify"
	// FeatureExec gates the local exec WebSocket bridge that launches commands
	// through the configured transport.
	FeatureExec = "exec"
	// FeatureCred gates credential prompts and delivery to the remote box.
	FeatureCred = "cred"
)

// FeatureEnabled reports whether the named capability is enabled. The contract
// matches cc-clip's default posture: every feature is ON by default (returns
// true when no toggle file exists). A user disables a feature by writing the
// word "off"/"false"/"0"/"no" (case-insensitive) into feature.<name>; any other
// content (or an empty/missing file) is ON. RE-READ EACH PASS — like
// AllowedPorts, an edit propagates to the running daemon with no restart.
func (s *Store) FeatureEnabled(feature string) bool {
	b, err := os.ReadFile(s.featureFile(feature))
	if err != nil {
		// Missing file (or unreadable) => default ON, matching cc-clip.
		return true
	}
	switch strings.ToLower(stripAllWhitespace(string(b))) {
	case "off", "false", "0", "no", "disabled":
		return false
	default:
		return true
	}
}

// SetFeature persists an explicit on/off toggle for the named capability,
// creating Dir if needed. Writing "on"/"off" makes the state inspectable and
// idempotent — `SetFeature(f,true)` then `FeatureEnabled(f)` round-trips.
func (s *Store) SetFeature(feature string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	val := "on\n"
	if !on {
		val = "off\n"
	}
	return os.WriteFile(s.featureFile(feature), []byte(val), 0o644)
}

// ReadHost returns the configured ssh host, or "" if no host file exists.
// All whitespace is stripped to match the bash `tr -d '[:space:]'` behavior
// (so a trailing newline or accidental spaces don't poison the alias).
func (s *Store) ReadHost() (string, error) {
	b, err := os.ReadFile(s.hostFile())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return stripAllWhitespace(string(b)), nil
}

// WriteHost persists the host (creating Dir if needed). Whitespace is
// stripped before write so any Read+Write round-trip is idempotent.
func (s *Store) WriteHost(host string) error {
	host = stripAllWhitespace(host)
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.hostFile(), []byte(host+"\n"), 0o644)
}

// AllowedPorts reads the allowlist, stripping `#` comments, splitting on
// whitespace, accepting only all-digit tokens, and returning sorted unique
// ints. RE-READ EACH PASS — `allow`/`unallow` edits propagate to the running
// daemon within the reconcile interval with no restart needed.
func (s *Store) AllowedPorts() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allowedPortsLocked()
}

// allowedPortsLocked is AllowedPorts without acquiring mu — the shared read used
// by Allow/Unallow, which already hold mu for the whole RMW.
func (s *Store) allowedPortsLocked() ([]int, error) {
	f, err := os.Open(s.allowFile())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	seen := make(map[int]struct{})
	out := make([]int, 0, 8)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, tok := range strings.Fields(line) {
			n, err := strconv.Atoi(tok)
			if err != nil || n <= 0 {
				continue
			}
			if _, ok := seen[n]; ok {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Ints(out)
	return out, nil
}

// Allow appends ports to the allowlist, returning the ports actually added
// (skipping duplicates). Numeric validation is the caller's job.
func (s *Store) Allow(ports []int) ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.allowedPortsLocked()
	if err != nil {
		return nil, err
	}
	have := make(map[int]struct{}, len(current))
	for _, p := range current {
		have[p] = struct{}{}
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.allowFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	added := make([]int, 0, len(ports))
	for _, p := range ports {
		if _, ok := have[p]; ok {
			continue
		}
		have[p] = struct{}{}
		if _, err := fmt.Fprintf(f, "%d\n", p); err != nil {
			return added, err
		}
		added = append(added, p)
	}
	return added, nil
}

// Unallow rewrites the allowlist minus the given ports. Missing ports are
// no-ops (matches the bash command which prints "unallowed: P" regardless).
func (s *Store) Unallow(ports []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.allowedPortsLocked()
	if err != nil {
		return err
	}
	drop := make(map[int]struct{}, len(ports))
	for _, p := range ports {
		drop[p] = struct{}{}
	}
	kept := current[:0]
	for _, p := range current {
		if _, ok := drop[p]; ok {
			continue
		}
		kept = append(kept, p)
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	tmp := s.allowFile() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	for _, p := range kept {
		if _, err := fmt.Fprintf(f, "%d\n", p); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.allowFile())
}

// AllowFilePath exposes the allowlist file path for the `allowed` command's
// "(file: ...)" hint.
func (s *Store) AllowFilePath() string { return s.allowFile() }

// HostFilePath exposes the host file path.
func (s *Store) HostFilePath() string { return s.hostFile() }

func stripAllWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
