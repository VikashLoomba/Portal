package api

import "encoding/json"

// SetupRequest is the body of POST /v1/setup. Host is optional when a host is
// already configured; Force defaults to false.
//
// D9 error-code inventory for setup:
//   - invalid_request (400): malformed or empty body, or no resolvable host.
//   - setup_in_progress (409): another POST /v1/setup is already running.
//   - not_configured (503): a host-bound endpoint was called while the daemon's
//     stack is nil (S5); POST /v1/setup itself does not return this code.
type SetupRequest struct {
	Host  string `json:"host,omitempty"`
	Force bool   `json:"force,omitempty"`
}

// SetupEvent is one line of the POST /v1/setup ndjson stream.
type SetupEvent struct {
	Step   string          `json:"step"`             // validate|configure|xdg-open|clip-shims|agent-symlink|activate|doctor|done
	Status string          `json:"status"`           // running|ok|warn|fail
	Line   string          `json:"line,omitempty"`   // relayed ssh stderr / human progress detail
	Error  *ErrorDetail    `json:"error,omitempty"`  // populated when status=fail (and warn where useful)
	Report json.RawMessage `json:"report,omitempty"` // doctor step only: the doctor.Report, opaque like POST /v1/doctor
}
