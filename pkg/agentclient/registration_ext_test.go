package agentclient_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/VikashLoomba/Portal/internal/bootstrap"
	"github.com/VikashLoomba/Portal/pkg/agent"
	"github.com/VikashLoomba/Portal/pkg/agent/watcher"
	"github.com/VikashLoomba/Portal/pkg/agentclient"
	"github.com/VikashLoomba/Portal/pkg/protocol"
	"github.com/VikashLoomba/Portal/pkg/transport"
)

type echoPayload struct {
	Text string `cbor:"text"`
}

type echoService struct {
	host    agent.ServiceHost
	version uint32
}

func (e *echoService) Name() string    { return "echo" }
func (e *echoService) Version() uint32 { return e.version }
func (e *echoService) MaxPayload() int { return 4096 }
func (e *echoService) OutboxCap() int  { return 8 }
func (e *echoService) Verbs() []agent.Verb {
	return nil
}
func (e *echoService) HandleMsg(kind string, payload cbor.RawMessage) {
	if kind == "ping" {
		e.host.Emit("echo", "pong", payload)
	}
}

func echoFactory(version uint32) agent.ServiceFactory {
	return func(host agent.ServiceHost, log *slog.Logger) agent.Service {
		return &echoService{host: host, version: version}
	}
}

type publicPipeTransport struct {
	sha      string
	w        *watcher.Fake
	services []agent.ServiceFactory
}

func newPublicPipeTransport(sha string, services []agent.ServiceFactory) *publicPipeTransport {
	w := watcher.NewFake()
	w.SetSnapshot(nil)
	return &publicPipeTransport{sha: sha, w: w, services: services}
}

func (p *publicPipeTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (p *publicPipeTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1, Detail: "pipe"}, nil
}
func (p *publicPipeTransport) Exec(context.Context, []byte, ...string) (string, string, error) {
	return "", "", nil
}
func (p *publicPipeTransport) Close(context.Context) (bool, error) { return false, nil }
func (p *publicPipeTransport) Describe() transport.Desc {
	return transport.Desc{Impl: "pipe", Host: "test", Endpoint: "io.Pipe"}
}
func (p *publicPipeTransport) Stream(ctx context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go func() { _ = stderrW.Close() }()
	srv := agent.New(agent.Config{
		In:                c2aR,
		Out:               a2cW,
		Watcher:           p.w,
		AgentSHA:          p.sha,
		HeartbeatInterval: 100 * time.Millisecond,
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		Services:          p.services,
	})
	go func() {
		_ = srv.Serve(ctx)
		_ = a2cW.Close()
	}()
	wait := func() error {
		_ = c2aR.Close()
		return nil
	}
	return c2aW, a2cR, stderrR, wait, nil
}

var _ transport.Transport = (*publicPipeTransport)(nil)

type publicBootstrapper struct {
	sha string
}

func (b publicBootstrapper) EnsureUploaded(context.Context) (string, error) {
	return "/agent-" + b.sha, nil
}
func (b publicBootstrapper) SetBootID(string) {}
func (b publicBootstrapper) EmbeddedSHA() string {
	return b.sha
}

type publicSession struct {
	c      *agentclient.Client
	cancel context.CancelFunc
	done   chan struct{}
}

func newPublicSession(t *testing.T, sha string, agentServices []agent.ServiceFactory, handlers []agentclient.HandlerSpec) *publicSession {
	t.Helper()
	tr := newPublicPipeTransport(sha, agentServices)
	c := agentclient.New(agentclient.Config{
		Transport:        tr,
		Bootstrap:        publicBootstrapper{sha: sha},
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		StderrSink:       io.Discard,
		HeartbeatTimeout: 2 * time.Second,
		ReconnectMin:     time.Second,
		ReconnectMax:     time.Second,
		Handlers:         handlers,
	})
	if err := c.Subscribe(nil, nil, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = c.Run(ctx)
		close(done)
	}()
	s := &publicSession{c: c, cancel: cancel, done: done}
	s.waitConnected(t)
	return s
}

func (s *publicSession) close() {
	s.cancel()
	<-s.done
}

func (s *publicSession) waitConnected(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := s.c.Snapshot(); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("client never reached connected+snapshot state")
}

func echoHandler(version uint32, delivered chan<- string) agentclient.HandlerSpec {
	return agentclient.HandlerSpec{
		Service:    "echo",
		Version:    version,
		MaxPayload: 4096,
		Decode: func(_ uint64, payload cbor.RawMessage) (agentclient.EngineEvent, error) {
			got, err := protocol.UnmarshalPayload[echoPayload](payload)
			if err != nil {
				return agentclient.EngineEvent{}, err
			}
			return agentclient.EngineEvent{Kind: agentclient.KindOpenURL, URL: got.Text}, nil
		},
		Deliver: func(ev agentclient.EngineEvent) {
			select {
			case delivered <- ev.URL:
			default:
			}
		},
	}
}

func TestPublicCustomServiceEchoRoundTrip(t *testing.T) {
	sha := bootstrap.EmbeddedSHA()
	if sha == "" {
		t.Skip("embedded SHA empty; run `make agent` first")
	}
	delivered := make(chan string, 1)
	sess := newPublicSession(t, sha,
		[]agent.ServiceFactory{echoFactory(1)},
		[]agentclient.HandlerSpec{echoHandler(1, delivered)},
	)
	defer sess.close()

	const want = "hello through public echo"
	payload, err := protocol.MarshalPayload(echoPayload{Text: want})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.c.Send("echo", "ping", payload); err != nil {
		t.Fatalf("Send echo ping: %v", err)
	}
	select {
	case got := <-delivered:
		if got != want {
			t.Fatalf("echo payload = %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no echo payload delivered")
	}
}

func TestPublicCustomServiceVersionMismatchDormant(t *testing.T) {
	sha := bootstrap.EmbeddedSHA()
	if sha == "" {
		t.Skip("embedded SHA empty; run `make agent` first")
	}
	delivered := make(chan string, 1)
	sess := newPublicSession(t, sha,
		[]agent.ServiceFactory{echoFactory(1)},
		[]agentclient.HandlerSpec{echoHandler(2, delivered)},
	)
	defer sess.close()

	payload, err := protocol.MarshalPayload(echoPayload{Text: "must stay dormant"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.c.Send("echo", "ping", payload); err != nil {
		t.Fatalf("Send echo ping: %v", err)
	}
	select {
	case got := <-delivered:
		t.Fatalf("dormant echo handler delivered %q", got)
	case <-time.After(300 * time.Millisecond):
	}
	if _, _, ok := sess.c.Snapshot(); !ok {
		t.Fatal("session lost snapshot after dormant echo frame")
	}
	if got := sess.c.LastDisconnectErr(); got != "" {
		t.Fatalf("session disconnected after dormant echo frame: %q", got)
	}
}
