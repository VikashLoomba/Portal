//go:build !darwin

package service

import (
	"context"

	"github.com/VikashLoomba/Portal/internal/clock"
	"github.com/VikashLoomba/Portal/pkg/run"
)

// Stub allows the module to compile on non-darwin (e.g. Linux CI). All ops
// return ErrUnsupported. A future systemd backend lands here.
type Stub struct{}

func New(_ Spec, _ run.Runner, _ clock.Clock) *Stub      { return &Stub{} }
func (*Stub) Install(ctx context.Context) error          { return ErrUnsupported }
func (*Stub) Uninstall(ctx context.Context) error        { return ErrUnsupported }
func (*Stub) Reload(ctx context.Context) error           { return ErrUnsupported }
func (*Stub) Start(ctx context.Context) error            { return ErrUnsupported }
func (*Stub) Stop(ctx context.Context) error             { return ErrUnsupported }
func (*Stub) Restart(ctx context.Context) error          { return ErrUnsupported }
func (*Stub) IsLoaded(ctx context.Context) (bool, error) { return false, nil }
func (*Stub) Status(ctx context.Context) (Status, error) { return Status{}, nil }

var _ Manager = (*Stub)(nil)
