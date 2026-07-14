package app

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/clock"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/forward"
	"github.com/VikashLoomba/Portal/pkg/agentclient"
	"github.com/VikashLoomba/Portal/pkg/hub"
	"github.com/VikashLoomba/Portal/pkg/run"
)

func TestNewStackLeavesAgentEventPumpUnstarted(t *testing.T) {
	s, err := NewStack(context.Background(), Paths{Sock: t.TempDir() + "/cm.sock"},
		config.New(t.TempDir()), hub.New(), "box", &run.Fake{}, clock.Real{},
		&forward.MemLogger{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if s.Engine.AgentEvents == nil {
		t.Fatal("Engine.AgentEvents is nil; engine would use interval mode")
	}
	select {
	case _, ok := <-s.Engine.AgentEvents:
		t.Fatalf("agent event channel changed before stack start (open=%v)", ok)
	default:
	}
}

func TestNewStackRejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The system branch constructs without I/O, so only this guard stops a
	// canceled activation request from proceeding into old-stack teardown.
	if _, err := NewStack(ctx, Paths{Sock: t.TempDir() + "/cm.sock"},
		config.New(t.TempDir()), hub.New(), "box", &run.Fake{}, clock.Real{},
		&forward.MemLogger{}, io.Discard); err == nil {
		t.Fatal("NewStack succeeded with a canceled context")
	}
}

func TestPumpAgentEventsCancellationAndMapping(t *testing.T) {
	in := make(chan agentclient.EngineEvent)
	out := make(chan forward.EngineEvent)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pumpAgentEvents(ctx, in, out)
		close(done)
	}()

	in <- agentclient.EngineEvent{Kind: agentclient.KindDelta, Added: []uint16{8080}}
	got := <-out
	if got.Kind != forward.EvDelta || len(got.Added) != 1 || got.Added[0] != 8080 {
		t.Fatalf("mapped event = %+v", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pump did not stop when its context was canceled")
	}
	if _, ok := <-out; ok {
		t.Fatal("pump output remains open after cancellation")
	}
}

func TestPumpAgentEventsCancelWhileSendBlocked(t *testing.T) {
	in := make(chan agentclient.EngineEvent, 1)
	in <- agentclient.EngineEvent{Kind: agentclient.KindConnected}
	out := make(chan forward.EngineEvent)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pumpAgentEvents(ctx, in, out)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pump did not stop while its output send was blocked")
	}
}

func TestStackAgentEventPumpsStopAcrossSwaps(t *testing.T) {
	const swaps = 5
	var wg sync.WaitGroup
	cancels := make([]context.CancelFunc, 0, swaps)
	for range swaps {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		s := &Stack{
			agentIn:  make(chan agentclient.EngineEvent),
			agentOut: make(chan forward.EngineEvent),
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RunAgentEventPump(ctx)
		}()
	}
	for _, cancel := range cancels {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("agent event pumps leaked across canceled stacks")
	}
}
