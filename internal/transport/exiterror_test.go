package transport

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantOK   bool
	}{
		{name: "nil", err: nil, wantCode: 0, wantOK: false},
		{name: "exit_error", err: &ExitError{Code: 3}, wantCode: 3, wantOK: true},
		{name: "wrapped", err: fmt.Errorf("x: %w", &ExitError{Code: 7}), wantCode: 7, wantOK: true},
		{name: "plain", err: errors.New("plain"), wantCode: 0, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, ok := ExitCode(tt.err)
			if code != tt.wantCode || ok != tt.wantOK {
				t.Errorf("ExitCode() = (%d, %v), want (%d, %v)", code, ok, tt.wantCode, tt.wantOK)
			}
		})
	}
}

func TestExitErrorError(t *testing.T) {
	msg := (&ExitError{Code: 3}).Error()
	if !strings.Contains(msg, "3") || !strings.Contains(msg, "status") {
		t.Errorf("Error() = %q, want code and status", msg)
	}

	signalMsg := (&ExitError{Code: 2, Signal: "KILL"}).Error()
	if !strings.Contains(signalMsg, "signal KILL") {
		t.Errorf("Error() = %q, want signal KILL", signalMsg)
	}
}
