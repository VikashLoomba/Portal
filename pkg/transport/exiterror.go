package transport

import (
	"errors"
	"fmt"
)

// ExitError reports a command exit status without changing Transport method
// signatures. Signal and Stderr are best-effort fields; they may be empty and
// callers must not require them. Code -1 means the implementation could not
// obtain an exit status.
type ExitError struct {
	Code   int
	Signal string
	Stderr string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("remote command exited with status %d", e.Code)
	if e.Signal != "" {
		msg += fmt.Sprintf(" (signal %s)", e.Signal)
	}
	return msg
}

// ExitCode reports the code from an ExitError. ExitCode(nil) returns
// (0, false); a non-ExitError returns (0, false); wrapped ExitError values are
// unwrapped via errors.As.
func ExitCode(err error) (code int, ok bool) {
	var e *ExitError
	if errors.As(err, &e) {
		return e.Code, true
	}
	return 0, false
}
