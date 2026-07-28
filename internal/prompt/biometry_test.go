package prompt

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestBiometryFakeRecordsRequests(t *testing.T) {
	deadline := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	fakeErr := errors.New("fake approval failure")
	f := &BiometryFake{
		AvailableResult: true,
		Outcome:         BiometryFallback,
		Err:             fakeErr,
	}
	if !f.Available(context.Background()) {
		t.Fatal("Available = false, want true")
	}
	outcome, err := f.Approve(context.Background(), "approve database", deadline)
	if outcome != BiometryFallback || !errors.Is(err, fakeErr) {
		t.Fatalf("Approve = (%d, %v), want fallback and configured error", outcome, err)
	}
	if f.AvailabilityChecks() != 1 {
		t.Fatalf("availability checks = %d, want 1", f.AvailabilityChecks())
	}
	want := BiometryRequest{Reason: "approve database", Deadline: deadline}
	requests := f.Requests()
	if len(requests) != 1 || requests[0] != want {
		t.Fatalf("requests = %#v, want %#v", requests, []BiometryRequest{want})
	}
	requests[0].Reason = "mutated"
	if f.Requests()[0] != want {
		t.Fatal("Requests returned the fake's backing slice")
	}
}

func TestBiometryFakeFunctionsTakePrecedenceAndAreConcurrent(t *testing.T) {
	deadline := time.Date(2026, 7, 28, 13, 0, 0, 0, time.UTC)
	f := &BiometryFake{
		AvailableFunc: func(context.Context) bool {
			return true
		},
		ApproveFunc: func(_ context.Context, reason string, gotDeadline time.Time) (BiometryOutcome, error) {
			if reason == "" || !gotDeadline.Equal(deadline) {
				return BiometryFallback, errors.New("unexpected fake request")
			}
			return BiometryApproved, nil
		},
		Outcome: BiometryCanceled,
		Err:     errors.New("unused"),
	}

	const calls = 32
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !f.Available(context.Background()) {
				t.Error("Available function result was ignored")
			}
			outcome, err := f.Approve(context.Background(), "approve", deadline)
			if err != nil || outcome != BiometryApproved {
				t.Errorf("Approve = (%d, %v), want approved", outcome, err)
			}
		}()
	}
	wg.Wait()
	if f.AvailabilityChecks() != calls {
		t.Errorf("availability checks = %d, want %d", f.AvailabilityChecks(), calls)
	}
	if len(f.Requests()) != calls {
		t.Errorf("approval requests = %d, want %d", len(f.Requests()), calls)
	}
}

func TestNonDarwinBiometryIsUnavailable(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Darwin uses the osascript implementation")
	}
	b := NewBiometry()
	if b.Available(context.Background()) {
		t.Fatal("non-Darwin biometry reported available")
	}
	outcome, err := b.Approve(context.Background(), "unused", time.Now())
	if err != nil || outcome != BiometryFallback {
		t.Fatalf("Approve = (%d, %v), want fallback", outcome, err)
	}
}
