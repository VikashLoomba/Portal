// Package audit is the append-only security audit log for the Mac-side daemon.
// It records every clipboard read served to a remote, every notification
// raised, and every URL opened — the user-inspectable trail of what the remote
// dev box asked this Mac to do (SPEC C). cc-clip keeps session/notification
// logs; this is the equivalent for portal's transport.
//
// The log is intentionally simple: one line per event, RFC3339-timestamped,
// tab-separated, appended under the portal config dir as `audit.log`. It is
// best-effort — a logging failure never blocks the action it records, since an
// un-served paste because of a full disk would be a worse failure mode than a
// missed audit line. Concurrent writers are serialized by a mutex so lines
// never interleave (the Mac daemon has several handler goroutines: clip,
// notify, open-URL).
package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Log is an append-only audit sink. The zero value is unusable; construct with
// New. A nil *Log is safe to call (every method no-ops) so callers can wire it
// unconditionally without nil checks on the hot path.
type Log struct {
	mu   sync.Mutex
	path string
	now  func() time.Time // injectable clock for tests
}

// New returns an audit log that appends to <dir>/audit.log. The directory is
// created lazily on first write so constructing a Log is cheap and never fails.
func New(dir string) *Log {
	return &Log{path: filepath.Join(dir, "audit.log"), now: time.Now}
}

// Path returns the audit log file path (for `portal status`/`doctor` to point
// the user at it).
func (l *Log) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// ClipServed records that a clipboard read was served to host. kind is
// "image"/"text"; detail is the short SHA (image) or byte length (text). A
// SKIPPED/denied serve should be recorded via ClipDenied instead.
func (l *Log) ClipServed(host, kind, detail string) {
	l.write("clip-served", "host="+host, "kind="+kind, detail)
}

// ClipDenied records that a clipboard read was refused — because the capability
// was disabled, the text was concealed, or no content was available. reason is
// a short token ("disabled", "concealed", "none").
func (l *Log) ClipDenied(host, kind, reason string) {
	l.write("clip-denied", "host="+host, "kind="+kind, "reason="+reason)
}

// Notify records that a native notification was raised (or denied). verified
// distinguishes a structured Claude-hook event from a generic/unverified one.
func (l *Log) Notify(host, title string, verified bool, urgency uint8) {
	l.write("notify", "host="+host,
		fmt.Sprintf("verified=%v", verified),
		fmt.Sprintf("urgency=%d", urgency),
		"title="+oneLine(title))
}

// NotifyDenied records that a relayed notification was dropped before delivery
// (e.g. the notify capability was disabled).
func (l *Log) NotifyDenied(host, reason string) {
	l.write("notify-denied", "host="+host, "reason="+reason)
}

// OpenURL records that a URL was opened on the Mac at the remote's request.
func (l *Log) OpenURL(host, url string) {
	l.write("open-url", "host="+host, "url="+oneLine(url))
}

// ExecOpen records that an exec WebSocket session was opened by peer uid.
func (l *Log) ExecOpen(host, sid, argv string, uid int, pty bool) {
	fields := []string{"host=" + host, "sid=" + sid, "uid=" + strconv.Itoa(uid), "argv=" + oneLine(argv)}
	if pty {
		fields = append(fields, "pty=1")
	}
	l.write("exec-open", fields...)
}

// ExecClose records the exec session result exactly once at stream teardown.
func (l *Log) ExecClose(host, sid string, code int, errStr string, dur time.Duration) {
	l.write("exec-close", "host="+host, "sid="+sid, fmt.Sprintf("code=%d", code), "err="+oneLine(errStr), "dur="+dur.String())
}

// write appends one tab-separated, timestamped line. Best-effort: on any error
// it silently no-ops (the action it records must not depend on the log).
func (l *Log) write(event string, fields ...string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	ts := l.now().UTC().Format(time.RFC3339)
	line := ts + "\t" + event
	for _, fld := range fields {
		if fld == "" {
			continue
		}
		line += "\t" + fld
	}
	_, _ = f.WriteString(line + "\n")
}

// oneLine collapses a value to a single audit-safe line: control bytes
// (newlines, tabs, NULs) are stripped so a crafted URL/title can't forge extra
// log lines or columns. It does NOT truncate — the audit trail keeps full
// values — but a pathological length is the caller's concern.
func oneLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
