package localapi

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
)

// route is one mux entry: a Go 1.22 method-pattern handler. Method + " " +
// Pattern is the registration key; the conformance test compares this set
// against the embedded openapi.yaml in both directions.
type route struct {
	Method  string
	Pattern string
	h       http.HandlerFunc
}

// Server is the local API HTTP server. TickInterval is declared and defaulted
// here exactly once for the whole package (u6's events streamer only consumes
// it). The zero value is not usable; call New.
type Server struct {
	deps         Deps
	mux          *http.ServeMux
	routes       []route
	subCount     atomic.Int64
	TickInterval time.Duration
	log          *slog.Logger
}

// New builds a Server, fills FeatureNames/TickInterval defaults, and registers
// the route table into the mux via Go 1.22 method patterns.
func New(deps Deps) *Server {
	if len(deps.FeatureNames) == 0 {
		deps.FeatureNames = []string{config.FeatureClipImage, config.FeatureClipText, config.FeatureNotify}
	}
	s := &Server{
		deps:         deps,
		mux:          http.NewServeMux(),
		TickInterval: 30 * time.Second,
		log:          slog.Default(),
	}
	s.routes = s.registerRoutes()
	for _, r := range s.routes {
		s.mux.HandleFunc(r.Method+" "+r.Pattern, r.h)
	}
	return s
}

// registerRoutes returns this unit's always-available endpoints. Later units
// append their routes here IN LOCKSTEP with openapi.yaml so the conformance
// test stays green (u5: ports/allow/features/reconcile/doctor; u6: events).
func (s *Server) registerRoutes() []route {
	return []route{
		{http.MethodGet, "/v1/version", s.handleVersion},
		{http.MethodGet, "/v1/openapi.yaml", s.handleOpenAPI},
		{http.MethodGet, "/v1/status", s.handleStatus},
	}
}

// allowPeer is the platform-agnostic peer-cred decision: only a same-uid peer
// is authorized (D4). It lives here (not in the build-tagged peercred files) so
// it is tested once regardless of platform.
func allowPeer(peerUID, selfUID int) bool { return peerUID == selfUID }

// Listen creates the API socket with the cmd-socket trust model and returns a
// peer-cred-gated listener. It ensures the parent dir is 0700, enforces the
// single-instance lock (D7) by probing any existing socket, and chmods the
// socket 0600.
func Listen(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		// A live HTTP responder means another daemon owns the socket (D7).
		// A dial failure (ECONNREFUSED / stale file) means it is dead: unlink
		// and take over.
		if probeAlive(path) {
			return nil, fmt.Errorf("localapi: another daemon is serving %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	ul, ok := ln.(*net.UnixListener)
	if !ok {
		ln.Close()
		return nil, fmt.Errorf("localapi: expected *net.UnixListener, got %T", ln)
	}
	return &peerCredListener{UnixListener: ul, selfUID: os.Getuid()}, nil
}

// probeAlive reports whether something answers HTTP on the unix socket at path
// within 1s. Any HTTP response (even non-2xx) counts as alive; a dial failure
// counts as dead.
func probeAlive(path string) bool {
	c := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}
	resp, err := c.Get("http://unix/v1/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// peerCredListener wraps a *net.UnixListener and drops any accepted connection
// whose peer uid does not match this process's uid — enforced at the LISTENER
// so every routed request is already authorized (§4.7).
type peerCredListener struct {
	*net.UnixListener
	selfUID int
}

// Accept returns the next connection from a same-uid peer, transparently
// closing and skipping mismatched or unreadable peers.
func (l *peerCredListener) Accept() (net.Conn, error) {
	for {
		c, err := l.UnixListener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(c)
		if err != nil {
			c.Close()
			continue
		}
		if !allowPeer(uid, l.selfUID) {
			c.Close()
			continue
		}
		return c, nil
	}
}

// Serve runs the HTTP server on ln until ctx is cancelled, then shuts down with
// a short deadline and unlinks the socket (best-effort). Bind/serve errors
// propagate to the caller (run.go makes them fatal per D10).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.middleware(s.mux)}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		if p := socketPath(ln); p != "" {
			_ = os.Remove(p)
		}
		return nil
	case err := <-errc:
		return err
	}
}

// socketPath extracts the unix socket path from a listener, or "" if it is not
// a unix listener.
func socketPath(ln net.Listener) string {
	if a, ok := ln.Addr().(*net.UnixAddr); ok {
		return a.Name
	}
	return ""
}

// middleware wraps the mux with panic recovery (recover → 500 error JSON) and a
// debug request log. Peer-cred is enforced at the listener, not here.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("localapi handler panic", "err", rec, "method", r.Method, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		s.log.Debug("localapi request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// buildStatus assembles the Status aggregate from deps. Missing/erroring
// sources degrade to zero values rather than failing the whole status.
func (s *Server) buildStatus(ctx context.Context) Status {
	st := Status{Version: s.deps.Version, Features: map[string]bool{}}

	if s.deps.Host != nil {
		if h, err := s.deps.Host(); err == nil {
			st.Host = h
		}
	}
	if s.deps.Service != nil {
		if svc, err := s.deps.Service.Status(ctx); err == nil {
			st.Service = ServiceStatus{Loaded: svc.Loaded, StateLines: svc.StateLines}
		}
	}

	var masterPID int
	if s.deps.Master != nil {
		if pid, err := s.deps.Master.MasterPID(ctx); err == nil {
			masterPID = pid
		}
	}
	st.Master = MasterStatus{Up: masterPID > 0, Pid: masterPID}

	if s.deps.Agent != nil {
		if ack := s.deps.Agent.HelloAck(); ack != nil {
			st.Agent = &AgentStatus{
				Pid:    ack.AgentPID,
				SHA:    ack.AgentGitSHA,
				Kernel: ack.Kernel,
				BootID: ack.BootID,
			}
		}
		if _, ports, ok := s.deps.Agent.Snapshot(); ok {
			for _, p := range ports {
				st.Ports = append(st.Ports, PortStatus{Port: int(p)})
			}
		}
	}

	if s.deps.Ports != nil && masterPID > 0 {
		if lines, err := s.deps.Ports.MasterForwardLines(ctx, masterPID); err == nil {
			for _, name := range lines {
				st.Forwards = append(st.Forwards, ForwardStatus{Name: name})
			}
		}
	}

	if s.deps.Config != nil {
		if allowed, err := s.deps.Config.AllowedPorts(); err == nil {
			st.Allowed = allowed
		}
		for _, name := range s.deps.FeatureNames {
			st.Features[name] = s.deps.Config.FeatureEnabled(name)
		}
	}

	if s.deps.Agent != nil {
		st.Health.LastDisconnectErr = s.deps.Agent.LastDisconnectErr()
	}
	if s.deps.Hub != nil {
		st.Health.DroppedNotifyCount = s.deps.Hub.DroppedNotify()
	}
	st.Health.EventsSubscribers = int(s.subCount.Load())

	return st
}
