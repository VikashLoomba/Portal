package localapi

import (
	"context"
	"encoding/json"
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
		{http.MethodGet, "/v1/events", s.handleEvents},
		{http.MethodGet, "/v1/ports", s.handlePorts},
		{http.MethodPut, "/v1/allow/{port}", s.handleAllowPut},
		{http.MethodDelete, "/v1/allow/{port}", s.handleAllowDelete},
		{http.MethodGet, "/v1/features", s.handleFeatures},
		{http.MethodPut, "/v1/features/{name}", s.handleFeaturePut},
		{http.MethodPost, "/v1/reconcile", s.handleReconcile},
		{http.MethodPost, "/v1/doctor", s.handleDoctor},
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
	return &peerCredListener{UnixListener: ul, selfUID: os.Getuid(), uidOf: peerUID}, nil
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
	// uidOf resolves an accepted conn's peer uid; it defaults to peerUID and is a
	// field only so a test can drive Accept with a faked mismatched/erroring peer
	// (the real peerUID needs a genuine same-uid socket, which can't exercise the
	// close-and-skip branches — the actual §4.7 trust boundary).
	uidOf func(*net.UnixConn) (int, error)
}

// Accept returns the next connection from a same-uid peer, transparently
// closing and skipping mismatched or unreadable peers.
func (l *peerCredListener) Accept() (net.Conn, error) {
	uidOf := l.uidOf
	if uidOf == nil {
		uidOf = peerUID
	}
	for {
		c, err := l.UnixListener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := uidOf(c)
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
	srv := &http.Server{
		Handler: s.middleware(s.mux),
		// Derive every in-flight request context from the Serve ctx so cancelling
		// it cancels each handler's r.Context() — including GET /v1/events'. Graceful
		// Shutdown alone never force-closes an attached streaming conn, so without
		// this the events handler's `case <-ctx.Done()` never fires and the stream
		// never EOFs on daemon shutdown (it also stops Shutdown blocking the full 2s
		// while a status --watch stream is attached). With it wired, the handler
		// returns, net/http finalizes the chunked body (clean EOF to the client),
		// and Shutdown then reaps the now-idle conn.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
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

// middleware wraps the mux with panic recovery (recover → 500 error JSON), a
// debug request log, and D9 envelope enforcement for the framework's own 404/405
// responses. Peer-cred is enforced at the listener, not here.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ew := &envelopeWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("localapi handler panic", "err", rec, "method", r.Method, "path", r.URL.Path)
				writeError(ew, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		s.log.Debug("localapi request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(ew, r)
	})
}

// envelopeWriter rewrites the ServeMux's default plain-text 404/405 bodies into
// the D9 error envelope. Go 1.22 method patterns answer an unknown path with 404
// and a wrong verb on a known path with 405, both as text/plain; a typed client
// decoding non-2xx bodies as {"error":{...}} would otherwise fail (§ D9). Our own
// handlers already set Content-Type application/json before WriteHeader, so those
// (e.g. 404 feature_unknown) pass through untouched. Unwrap keeps
// http.ResponseController (events streaming's Flush/SetWriteDeadline) working.
type envelopeWriter struct {
	http.ResponseWriter
	swallow bool // true once we've substituted our own JSON body for the default
}

func (w *envelopeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *envelopeWriter) WriteHeader(code int) {
	if (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) &&
		w.Header().Get("Content-Type") != "application/json" {
		machineCode, msg := "not_found", "no such endpoint"
		if code == http.StatusMethodNotAllowed {
			machineCode, msg = "method_not_allowed", "method not allowed for this endpoint"
		}
		w.Header().Set("Content-Type", "application/json")
		w.ResponseWriter.WriteHeader(code)
		_ = json.NewEncoder(w.ResponseWriter).Encode(errorBody{Error: errorDetail{Code: machineCode, Message: msg}})
		w.swallow = true
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *envelopeWriter) Write(b []byte) (int, error) {
	if w.swallow {
		// Discard the framework's plain-text body; we already wrote the envelope.
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// buildStatus assembles the Status aggregate from deps. Missing/erroring
// sources degrade to zero values rather than failing the whole status.
func (s *Server) buildStatus(ctx context.Context) Status {
	// Ports/Forwards/Allowed are initialized to empty non-nil slices so the JSON
	// fields are ALWAYS arrays, never null — matching GET /v1/ports and letting a
	// polyglot client iterate them safely even in the disconnected state (§4.4).
	st := Status{
		Version:  s.deps.Version,
		Features: map[string]bool{},
		Ports:    []PortStatus{},
		Forwards: []ForwardStatus{},
		Allowed:  []int{},
	}

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

	var masterUp bool
	if s.deps.Master != nil {
		h, _ := s.deps.Master.Health(ctx)
		d := s.deps.Master.Describe()
		masterUp = h.Up
		st.Master = MasterStatus{Up: h.Up, Pid: h.Pid, Transport: d.Impl, Detail: h.Detail}
	}

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

	if s.deps.Ports != nil && masterUp {
		if lines, err := s.deps.Ports.ForwardLines(ctx); err == nil {
			for _, name := range lines {
				st.Forwards = append(st.Forwards, ForwardStatus{Name: name})
			}
		}
	}

	if s.deps.Config != nil {
		// AllowedPorts returns nil for a missing file; keep the initialized empty
		// slice in that case so Allowed never marshals to null.
		if allowed, err := s.deps.Config.AllowedPorts(); err == nil && allowed != nil {
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
	if s.deps.ReconcileGen != nil {
		st.Health.ReconcileCount = s.deps.ReconcileGen()
	}

	return st
}
