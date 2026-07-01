package localapi

import (
	"encoding/json"
	"net/http"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/hub"
)

// eventLine is the typed envelope for one ndjson line of GET /v1/events. Exactly
// one shape is populated per Type: snapshot/state carry Status; notify carries
// Notify; tick carries neither. A typed envelope keeps line encoding off any.
type eventLine struct {
	Type   string      `json:"type"`
	Status *Status     `json:"status,omitempty"`
	Notify *hub.Notify `json:"notify,omitempty"`
}

// eventsWriteTimeout bounds each ndjson line write. A slow or dead client that
// cannot drain a line within this deadline is dropped (the handler returns,
// cancelling its subscriptions) rather than backpressuring the hub — whose
// per-subscriber buffers already guarantee a non-blocking Publish. The deadline
// is the handler's only backpressure defense; the hub owns the rest (§4.7).
const eventsWriteTimeout = 5 * time.Second

// handleEvents streams the snapshot-as-reset ndjson event log (D3): a full
// Status snapshot first, then a full-Status "state" line on every Coalesced hub
// signal, a "notify" line on every Queued hub Event, and a "tick" line every
// TickInterval for hung-daemon detection. It subscribes to BOTH hub classes,
// writes each line under a deadline, and returns on client disconnect, any write
// error, or a passed deadline — never blocking the hub.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.subCount.Add(1)
	defer s.subCount.Add(-1)

	coalesced, cancelCoalesced := s.deps.Hub.Subscribe(hub.Coalesced)
	defer cancelCoalesced()
	queued, cancelQueued := s.deps.Hub.Subscribe(hub.Queued)
	defer cancelQueued()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	rc := http.NewResponseController(w)

	ctx := r.Context()

	// Snapshot is ALWAYS the first line: a reconnecting client is coherent
	// before any delta.
	snap := s.buildStatus(ctx)
	if s.writeEventLine(rc, w, eventLine{Type: "snapshot", Status: &snap}) != nil {
		return
	}

	ticker := time.NewTicker(s.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-coalesced:
			// Latest-wins coalescing: re-read the full current Status so the
			// client needs no merge implementation.
			st := s.buildStatus(ctx)
			if s.writeEventLine(rc, w, eventLine{Type: "state", Status: &st}) != nil {
				return
			}
		case ev := <-queued:
			if s.writeEventLine(rc, w, eventLine{Type: "notify", Notify: ev.Notify}) != nil {
				return
			}
		case <-ticker.C:
			if s.writeEventLine(rc, w, eventLine{Type: "tick"}) != nil {
				return
			}
		}
	}
}

// writeEventLine marshals line as one compact JSON object plus a trailing
// newline, writing it under eventsWriteTimeout and flushing. Any marshal/write
// error or a passed deadline is returned so the caller can drop the client.
func (s *Server) writeEventLine(rc *http.ResponseController, w http.ResponseWriter, line eventLine) error {
	b, err := json.Marshal(line)
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
