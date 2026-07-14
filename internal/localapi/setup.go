package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/VikashLoomba/Portal/pkg/api"
)

const setupCloseTimeout = 2 * time.Second

// handleSetup runs one connection-scoped setup and streams its phase events.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.setupInFlight.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "setup_in_progress", "another setup is already running")
		return
	}
	defer s.setupInFlight.Store(false)

	dec := json.NewDecoder(r.Body)
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return
	}
	var req api.SetupRequest
	if raw, ok := fields["host"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &req.Host) != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "host must be a string")
			return
		}
	}
	if raw, ok := fields["force"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &req.Force) != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "force must be a boolean")
			return
		}
	}

	oldHost, _ := s.deps.Host()
	host := s.deps.NormalizeHost(req.Host)
	if host == "" {
		host = oldHost
	}
	if host == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "host is required when portal is not configured")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var sinkMu sync.Mutex
	var summary []string
	var writeErr error
	failed := false
	aborted := false
	activation := ""
	states := make(map[string]string)

	defer func() {
		sinkMu.Lock()
		steps := strings.Join(summary, " ")
		we := writeErr
		sinkMu.Unlock()

		if we != nil || ctx.Err() != nil {
			aborted = true
		}
		verdict := "ok"
		if failed {
			verdict = "fail"
		}
		if aborted {
			verdict = "canceled"
		}
		s.deps.Audit.Setup(host, req.Force, steps, activation, verdict)
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	rc := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		aborted = true
		return
	}

	sink := func(ev api.SetupEvent) {
		sinkMu.Lock()
		defer sinkMu.Unlock()

		if ev.Step != "done" && ev.Line == "" {
			states[ev.Step] = ev.Status
		}
		if ev.Step != "done" && isSetupTerminal(ev.Status) {
			summary = append(summary, ev.Step+"="+ev.Status)
		}
		if writeErr != nil {
			return
		}
		if err := writeSetupLine(rc, w, ev); err != nil {
			writeErr = err
			cancel()
		}
	}

	finished := false
	finish := func() {
		if finished {
			return
		}
		finished = true
		status := "ok"
		if failed {
			status = "fail"
		}
		sink(api.SetupEvent{Step: "done", Status: status})
	}
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("localapi setup panic", "err", rec)
			failed = true
			if finished {
				return
			}
			for _, step := range []string{"validate", "configure", "xdg-open", "clip-shims", "agent-symlink", "activate", "doctor"} {
				sinkMu.Lock()
				state := states[step]
				sinkMu.Unlock()
				if isSetupTerminal(state) {
					continue
				}
				if state != "running" {
					sink(api.SetupEvent{Step: step, Status: "running"})
				}
				sink(api.SetupEvent{
					Step:   step,
					Status: "fail",
					Error:  &api.ErrorDetail{Code: "internal", Message: "internal server error"},
				})
				break
			}
			finish()
		}
	}()

	runner := s.deps.NewSetup(sink)
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), setupCloseTimeout)
		defer closeCancel()
		runner.Close(closeCtx)
	}()
	canceled := func() bool {
		if ctx.Err() == nil {
			return false
		}
		aborted = true
		return true
	}

	proceed := runner.Validate(ctx, host, req.Force)
	if canceled() {
		return
	}
	if !proceed {
		failed = true
		finish()
		return
	}

	if err := runner.Configure(ctx, host); err != nil {
		if canceled() {
			return
		}
		failed = true
		finish()
		return
	}
	if canceled() {
		return
	}

	runner.DeployRemote(ctx, host)
	if canceled() {
		return
	}

	activation = oldHost + "→" + host
	sink(api.SetupEvent{Step: "activate", Status: "running"})
	if canceled() {
		return
	}
	if err := s.deps.Activate(ctx, host); err != nil {
		activation += " (failed)"
		failed = true
		sink(api.SetupEvent{
			Step:   "activate",
			Status: "fail",
			Error:  &api.ErrorDetail{Code: "activate_failed", Message: err.Error()},
		})
		if canceled() {
			return
		}
		finish()
		return
	}
	sink(api.SetupEvent{Step: "activate", Status: "ok"})
	if canceled() {
		return
	}

	_ = runner.Verify(ctx, host)
	if canceled() {
		return
	}
	finish()
}

func isSetupTerminal(status string) bool {
	switch status {
	case "ok", "warn", "fail":
		return true
	default:
		return false
	}
}

// writeSetupLine writes one compact SetupEvent under the stream deadline.
func writeSetupLine(rc *http.ResponseController, w http.ResponseWriter, ev api.SetupEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := rc.SetWriteDeadline(time.Now().Add(eventsWriteTimeout)); err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	return rc.Flush()
}
