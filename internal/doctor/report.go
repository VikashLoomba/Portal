// Package doctor holds the JSON-serializable data types for the clipboard +
// notification self-test report. The types live here (not in package main) so
// the local API's POST /v1/doctor can return a report as JSON; presentation
// (CLI rendering) stays in package main. This package imports nothing from
// internal/app — it is a leaf so there is no layering cycle.
package doctor

import (
	"encoding/json"
	"strconv"
)

// Status is the outcome of one doctor probe.
type Status uint8

const (
	Pass Status = iota
	Warn        // non-fatal: a degraded-but-usable condition
	Fail        // fatal: the clip/notify path will NOT work
)

// Tag returns the fixed-width label rendered in the report ("PASS"/"WARN"/"FAIL").
// An unrecognized Status defaults to "FAIL" so an uninitialized value never
// reads as a pass.
func (s Status) Tag() string {
	switch s {
	case Pass:
		return "PASS"
	case Warn:
		return "WARN"
	default:
		return "FAIL"
	}
}

// MarshalJSON emits the quoted Tag() string so the JSON report is human-usable
// (fixing the previously unserializable int-enum status).
func (s Status) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(s.Tag())), nil
}

// UnmarshalJSON parses the quoted tag emitted by MarshalJSON back into a Status,
// so *Report decodes cleanly over the wire (localclient.Doctor, u5's renderDoctor
// path). It is symmetric with Tag()/MarshalJSON: "PASS"->Pass, "WARN"->Warn,
// "FAIL"->Fail; any other token or a malformed value -> Fail, so an unknown or
// corrupt status can never read as a pass. Pointer receiver: it mutates *s.
func (s *Status) UnmarshalJSON(b []byte) error {
	var tag string
	if err := json.Unmarshal(b, &tag); err != nil {
		*s = Fail
		return nil
	}
	switch tag {
	case "PASS":
		*s = Pass
	case "WARN":
		*s = Warn
	default:
		*s = Fail
	}
	return nil
}

// Check is one line of the report.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Report accumulates the per-probe results for one host.
type Report struct {
	Host   string  `json:"host"`
	Checks []Check `json:"checks"`
}

// Add appends a Check to the report.
func (r *Report) Add(name string, status Status, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: status, Detail: detail})
}

// OK reports whether the report has zero Fail checks (Warn is tolerated).
func (r *Report) OK() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return false
		}
	}
	return true
}
