//go:build !linux

package watcher

import (
	"context"
)

// NetlinkConfig is referenced by cmd/portald/main.go on every OS so the
// !linux build is still valid; it is otherwise unused outside Linux.
type NetlinkConfig struct {
	PollInterval        int // ms
	UseDestroyMulticast bool
}

// NewNetlink returns ErrUnsupported on non-Linux. The portald binary is
// only ever cross-compiled GOOS=linux GOARCH=amd64, so this code path is
// solely for darwin compile checks.
func NewNetlink(_ NetlinkConfig) (Watcher, error) { return nil, ErrUnsupported }

// stub keeps the unused-context import quiet.
var _ = context.Background
