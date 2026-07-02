// Package localclient is the thin typed HTTP/1.1+JSON client the CLI (and any
// future in-process tool) uses to talk to the daemon's internal/localapi server
// over the Unix socket at app.Paths.APISock. It mirrors §4.5's endpoint contract
// with typed getters/mutators and an ndjson events stream; it is stdlib-only and
// never decodes into an any-typed payload — every response has a concrete struct
// (localapi.Status, localapi.PortStatus, hub.Notify, doctor.Report). See
// DESIGN-local-core-api.md §5.1.
package localclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/VikashLoomba/Portal/internal/doctor"
	"github.com/VikashLoomba/Portal/internal/hub"
	"github.com/VikashLoomba/Portal/internal/localapi"
)

// Per-call default timeouts, exposed as vars so tests can shrink them. Status/
// ports/allow/features share StatusTimeout; version-probe and reconcile use the
// quick ProbeTimeout. Doctor deliberately has NO per-call cap: POST /v1/doctor is
// long-running and rides the caller's ctx (§4.5 "honors client disconnect") — see
// Doctor. There is likewise NO global http.Client.Timeout — an events stream is
// long-lived and a client timeout would tear it down mid-stream (§5.1).
var (
	StatusTimeout = 2 * time.Second
	ProbeTimeout  = 1 * time.Second
)

// Sentinels for the two endpoint-specific non-2xx codes callers branch on. Every
// other non-2xx yields an *APIError.
var (
	// ErrNotConnected maps GET /v1/ports 503 not_connected (no cached Snapshot yet).
	ErrNotConnected = errors.New("localclient: agent not connected")
	// ErrFeatureUnknown maps PUT /v1/features/{name} 404 feature_unknown.
	ErrFeatureUnknown = errors.New("localclient: unknown feature")
)

// APIError is a decoded D9 error envelope for a non-2xx response the caller does
// not special-case (everything but Ports 503 and Features 404).
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("localapi %d %s: %s", e.Status, e.Code, e.Message)
}

// Event is the decode envelope for one ndjson line of GET /v1/events. It mirrors
// localapi's unexported eventLine and the §4.6 contract: exactly one of Status/
// Notify is populated per Type (snapshot/state carry Status; notify carries
// Notify; tick carries neither). It reuses the exported localapi.Status and
// hub.Notify so nothing is any-typed.
type Event struct {
	Type   string           `json:"type"`
	Status *localapi.Status `json:"status,omitempty"`
	Notify *hub.Notify      `json:"notify,omitempty"`
}

// Client is a typed client bound to one API socket path. The zero value is not
// usable; call New.
type Client struct {
	sock string
	hc   *http.Client
}

// New builds a Client dialing the Unix socket at sock. The transport's
// DialContext ignores the HTTP address argument and always dials the socket, so
// every request URL uses the placeholder host "unix" (e.g. http://unix/v1/status).
// No global hc.Timeout is set — per-call deadlines come from context so the
// long-lived Events stream is never torn down.
func New(sock string) *Client {
	c := &Client{sock: sock}
	c.hc = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", c.sock)
			},
		},
	}
	return c
}

// do issues req and returns the response. The caller owns closing resp.Body.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	return c.hc.Do(req)
}

// newReq builds a request bound to the socket host under a fresh timeout derived
// from ctx. body is nil for GET/DELETE and the marshaled payload for PUT/POST.
// Every method routes through here so the per-call timeout is applied uniformly
// (there is no bespoke per-method timeout to drift out of coverage). The
// returned cancel MUST be deferred by the caller (it releases the timeout even
// on the happy path).
func (c *Client) newReq(ctx context.Context, method, path string, timeout time.Duration, body io.Reader) (*http.Request, context.CancelFunc, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(cctx, method, "http://unix"+path, body)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return req, cancel, nil
}

// apiError reads a non-2xx body as a D9 error envelope and returns an *APIError.
// A body that is not the envelope still yields an *APIError carrying the status,
// so a non-2xx is NEVER reported as success.
func apiError(resp *http.Response) *APIError {
	var eb struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	return &APIError{Status: resp.StatusCode, Code: eb.Error.Code, Message: eb.Error.Message}
}

// decodeJSON decodes the response body into v (a concrete type — no any).
func decodeJSON[T any](resp *http.Response, v *T) error {
	return json.NewDecoder(resp.Body).Decode(v)
}

// Available reports whether the daemon answers GET /v1/version within
// ProbeTimeout. Any HTTP response (even non-2xx) counts as available; only a
// dial/transport error counts as down. It mirrors localapi.probeAlive but is
// instance-scoped (uses this Client's socket + transport).
func (c *Client) Available(ctx context.Context) bool {
	req, cancel, err := c.newReq(ctx, http.MethodGet, "/v1/version", ProbeTimeout, nil)
	if err != nil {
		return false
	}
	defer cancel()
	resp, err := c.do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// Status returns the full Status aggregate from GET /v1/status.
func (c *Client) Status(ctx context.Context) (localapi.Status, error) {
	var st localapi.Status
	req, cancel, err := c.newReq(ctx, http.MethodGet, "/v1/status", StatusTimeout, nil)
	if err != nil {
		return st, err
	}
	defer cancel()
	resp, err := c.do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, apiError(resp)
	}
	if err := decodeJSON(resp, &st); err != nil {
		return st, err
	}
	return st, nil
}

// Ports returns remote loopback listeners from GET /v1/ports. A 503 (no cached
// Snapshot yet) maps to ErrNotConnected; any other non-2xx to *APIError.
func (c *Client) Ports(ctx context.Context) ([]localapi.PortStatus, error) {
	req, cancel, err := c.newReq(ctx, http.MethodGet, "/v1/ports", StatusTimeout, nil)
	if err != nil {
		return nil, err
	}
	defer cancel()
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, ErrNotConnected
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var ports []localapi.PortStatus
	if err := decodeJSON(resp, &ports); err != nil {
		return nil, err
	}
	return ports, nil
}

// allowlistResponse mirrors localapi's {"allowed":[...]} body.
type allowlistResponse struct {
	Allowed []int `json:"allowed"`
}

// Allow adds port to the allowlist via PUT /v1/allow/{port} and returns the new
// allowlist. An invalid port yields the server's 400 as an *APIError.
func (c *Client) Allow(ctx context.Context, port int) ([]int, error) {
	return c.mutateAllow(ctx, http.MethodPut, port)
}

// Unallow removes port from the allowlist via DELETE /v1/allow/{port}, decoding
// like Allow.
func (c *Client) Unallow(ctx context.Context, port int) ([]int, error) {
	return c.mutateAllow(ctx, http.MethodDelete, port)
}

func (c *Client) mutateAllow(ctx context.Context, method string, port int) ([]int, error) {
	req, cancel, err := c.newReq(ctx, method, "/v1/allow/"+strconv.Itoa(port), StatusTimeout, nil)
	if err != nil {
		return nil, err
	}
	defer cancel()
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var out allowlistResponse
	if err := decodeJSON(resp, &out); err != nil {
		return nil, err
	}
	return out.Allowed, nil
}

// Features returns the capability gates as a name->enabled map from GET
// /v1/features.
func (c *Client) Features(ctx context.Context) (map[string]bool, error) {
	req, cancel, err := c.newReq(ctx, http.MethodGet, "/v1/features", StatusTimeout, nil)
	if err != nil {
		return nil, err
	}
	defer cancel()
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var m map[string]bool
	if err := decodeJSON(resp, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// SetFeature toggles one capability gate via PUT /v1/features/{name} with a
// typed {"enabled":on} body and returns the updated map. An unknown name (404)
// maps to ErrFeatureUnknown; any other non-2xx to *APIError.
func (c *Client) SetFeature(ctx context.Context, name string, on bool) (map[string]bool, error) {
	body, err := json.Marshal(struct {
		Enabled bool `json:"enabled"`
	}{Enabled: on})
	if err != nil {
		return nil, err
	}
	// Routes through newReq so the per-call StatusTimeout is applied by the same
	// mechanism as every sibling method — no bespoke inline timeout to drift out
	// of the hung-server coverage.
	req, cancel, err := c.newReq(ctx, http.MethodPut, "/v1/features/"+name, StatusTimeout, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer cancel()
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrFeatureUnknown
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var m map[string]bool
	if err := decodeJSON(resp, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Reconcile kicks the daemon's forward-engine reconcile via POST /v1/reconcile.
// It returns nil iff the daemon accepted it (202).
func (c *Client) Reconcile(ctx context.Context) error {
	req, cancel, err := c.newReq(ctx, http.MethodPost, "/v1/reconcile", ProbeTimeout, nil)
	if err != nil {
		return err
	}
	defer cancel()
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return apiError(resp)
	}
	return nil
}

// Doctor runs the daemon-side self-test via POST /v1/doctor and decodes the
// structured report. This decode only works because this package adds
// doctor.Status.UnmarshalJSON — without it {"status":"PASS"} fails to unmarshal
// into the doctor.Status uint8 and Doctor would always error (§5.1 STAGE-1 fixup).
//
// Unlike the status-class getters, Doctor imposes NO artificial per-call deadline
// and rides the caller's ctx directly (like Events). POST /v1/doctor is
// long-running (§4.5): the daemon runs a full clip/notify smoke test over its
// live ssh transport, which on a high-latency link legitimately exceeds any small
// fixed budget — a fixed cap here would abort a healthy-but-slow run and force the
// caller into a redundant local fallback. The caller owns the deadline; cancelling
// ctx (e.g. the user interrupting the CLI) closes the connection and the daemon
// honors the disconnect.
func (c *Client) Doctor(ctx context.Context) (*doctor.Report, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v1/doctor", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var rep doctor.Report
	if err := decodeJSON(resp, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// eventsBufCap bumps the ndjson scanner's max token size well past a single
// Status line (matching events_test.go's 1<<20), so a large snapshot never
// trips bufio.ErrTooLong.
const eventsBufCap = 1 << 20

// Events opens GET /v1/events with the raw ctx (NO per-call timeout — the stream
// is long-lived) and returns a channel of decoded Events plus a cap-1 error
// channel. A dial/HTTP error is returned as the third value. On success a single
// goroutine scans the ndjson body, unmarshals each line into an Event, and sends
// it on the buffered events chan; on scanner end it sends the terminal error to
// errc (nil on a clean EOF — including the daemon closing the stream on its own
// shutdown, produced by the localapi BaseContext fixup) and closes events. The
// caller cancels ctx to stop; the goroutine is race-clean under -race.
func (c *Client) Events(ctx context.Context) (<-chan Event, <-chan error, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/v1/events", nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, nil, apiError(resp)
	}

	events := make(chan Event, 16)
	errc := make(chan error, 1)
	go func() {
		defer resp.Body.Close()
		defer close(events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), eventsBufCap)
		for sc.Scan() {
			var ev Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				errc <- err
				return
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			}
		}
		// Scanner ended: nil on a clean EOF (daemon shutdown finalizes the chunked
		// body), or the read error (a benign closed-conn read on cancel is fine).
		errc <- sc.Err()
	}()
	return events, errc, nil
}
