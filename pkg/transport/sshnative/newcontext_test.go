package sshnative

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewContextHonorsResolverCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resolver := func(ctx context.Context, _ string) (ResolvedHost, error) {
		<-ctx.Done()
		return ResolvedHost{}, ctx.Err()
	}

	started := time.Now()
	_, err := NewContext(ctx, "box", WithConfigResolver(resolver))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("NewContext error = %v, want context canceled", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("NewContext returned after %s, want prompt cancellation", elapsed)
	}
}
