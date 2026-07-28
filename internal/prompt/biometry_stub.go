//go:build !darwin

package prompt

import (
	"context"
	"time"
)

type unavailableBiometry struct{}

func newPlatformBiometry() Biometry {
	return unavailableBiometry{}
}

// Available reports that this platform has no biometric consent implementation.
func (unavailableBiometry) Available(context.Context) bool {
	return false
}

// Approve keeps direct calls on the caller's non-biometric consent path.
func (unavailableBiometry) Approve(context.Context, string, time.Time) (BiometryOutcome, error) {
	return BiometryFallback, nil
}
