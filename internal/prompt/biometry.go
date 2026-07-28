package prompt

import (
	"context"
	"sync"
	"time"
)

// BiometryOutcome is the result of a biometric consent attempt.
type BiometryOutcome uint8

const (
	// BiometryApproved records a successful fingerprint or watch approval.
	BiometryApproved BiometryOutcome = iota + 1
	// BiometryCanceled records an explicit user cancellation.
	BiometryCanceled
	// BiometryTimeout records that the approval deadline elapsed.
	BiometryTimeout
	// BiometryFallback asks the caller to use its non-biometric consent path.
	BiometryFallback
)

// Biometry gates a consent decision without receiving or releasing a secret.
type Biometry interface {
	Available(ctx context.Context) bool
	Approve(ctx context.Context, reason string, deadline time.Time) (BiometryOutcome, error)
}

// NewBiometry returns the platform biometric consent implementation.
func NewBiometry() Biometry {
	return newPlatformBiometry()
}

// BiometryRequest records the display reason and deadline passed to Approve.
type BiometryRequest struct {
	// Reason is the display-only consent text.
	Reason string
	// Deadline is the absolute end of the shared dialog budget.
	Deadline time.Time
}

// BiometryFake is a concurrency-safe Biometry for handler tests. Function
// fields, when set, take precedence over the corresponding configured result.
type BiometryFake struct {
	// AvailableFunc optionally computes each availability result.
	AvailableFunc func(context.Context) bool
	// ApproveFunc optionally computes each approval result.
	ApproveFunc func(context.Context, string, time.Time) (BiometryOutcome, error)
	// AvailableResult is returned when AvailableFunc is nil.
	AvailableResult bool
	// Outcome is returned when ApproveFunc is nil.
	Outcome BiometryOutcome
	// Err is returned when ApproveFunc is nil.
	Err error

	mu                 sync.Mutex
	availabilityChecks int
	requests           []BiometryRequest
}

// Available records an availability check and returns the configured result.
func (f *BiometryFake) Available(ctx context.Context) bool {
	f.mu.Lock()
	f.availabilityChecks++
	fn := f.AvailableFunc
	available := f.AvailableResult
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return available
}

// Approve records the request and returns the configured fake result.
func (f *BiometryFake) Approve(ctx context.Context, reason string, deadline time.Time) (BiometryOutcome, error) {
	f.mu.Lock()
	f.requests = append(f.requests, BiometryRequest{Reason: reason, Deadline: deadline})
	fn := f.ApproveFunc
	outcome := f.Outcome
	err := f.Err
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, reason, deadline)
	}
	return outcome, err
}

// AvailabilityChecks returns the number of calls made to Available.
func (f *BiometryFake) AvailabilityChecks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.availabilityChecks
}

// Requests returns a snapshot of all approval requests seen by the fake.
func (f *BiometryFake) Requests() []BiometryRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]BiometryRequest(nil), f.requests...)
}
