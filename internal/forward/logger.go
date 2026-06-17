package forward

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Logger emits one line per call, prefixed with a timestamp matching the bash
// `log()` format ("2006-01-02 15:04:05"). The launchd plist redirects the
// daemon's stdout/stderr to ~/Library/Logs/portal.log so this writes there.
type Logger interface {
	Logf(format string, args ...any)
}

// LineLogger writes timestamped lines to W. Concurrency-safe.
type LineLogger struct {
	W  io.Writer
	mu sync.Mutex
}

func StdoutLogger() *LineLogger { return &LineLogger{W: os.Stdout} }

func (l *LineLogger) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.W, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// MemLogger is a Logger that captures lines for tests.
type MemLogger struct {
	mu    sync.Mutex
	Lines []string
}

func (m *MemLogger) Logf(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Lines = append(m.Lines, fmt.Sprintf(format, args...))
}

// Has returns true if any line contains substr.
func (m *MemLogger) Has(substr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.Lines {
		if containsString(l, substr) {
			return true
		}
	}
	return false
}

func containsString(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
