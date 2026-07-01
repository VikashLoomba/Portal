package doctor

import (
	"encoding/json"
	"testing"
)

// TestStatusTag pins the label text, including the default-to-FAIL guard for an
// out-of-range Status (an uninitialized value must never read as a pass).
func TestStatusTag(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{Pass, "PASS"},
		{Warn, "WARN"},
		{Fail, "FAIL"},
		{Status(99), "FAIL"},
	}
	for _, tt := range tests {
		if got := tt.status.Tag(); got != tt.want {
			t.Errorf("Status(%d).Tag() = %q, want %q", tt.status, got, tt.want)
		}
	}
}

// TestReportAddOK covers Add accumulation and the OK() fail-only semantics: any
// Fail makes the report not OK; Warn is tolerated; empty is OK.
func TestReportAddOK(t *testing.T) {
	tests := []struct {
		name     string
		statuses []Status
		wantOK   bool
	}{
		{"empty", nil, true},
		{"pass_only", []Status{Pass, Pass}, true},
		{"warn_tolerated", []Status{Pass, Warn}, true},
		{"fail_not_ok", []Status{Pass, Warn, Fail}, false},
		{"fail_alone", []Status{Fail}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rep Report
			for i, s := range tt.statuses {
				rep.Add("check", s, "")
				if len(rep.Checks) != i+1 {
					t.Fatalf("Add did not append: len=%d want=%d", len(rep.Checks), i+1)
				}
			}
			if got := rep.OK(); got != tt.wantOK {
				t.Errorf("OK() = %v, want %v", got, tt.wantOK)
			}
		})
	}
}

// TestStatusMarshalJSON proves each Status marshals to its quoted tag, fixing
// the previously unserializable int-enum.
func TestStatusMarshalJSON(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{Pass, `"PASS"`},
		{Warn, `"WARN"`},
		{Fail, `"FAIL"`},
	}
	for _, tt := range tests {
		got, err := json.Marshal(tt.status)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", tt.status, err)
		}
		if string(got) != tt.want {
			t.Errorf("Marshal(%v) = %s, want %s", tt.status, got, tt.want)
		}
	}
}

// TestReportMarshalJSON pins the full serialized object shape: Status renders as
// its tag string, detail is omitted when empty, and host/checks keys are stable.
func TestReportMarshalJSON(t *testing.T) {
	rep := Report{
		Host: "devbox",
		Checks: []Check{
			{Name: "ssh master", Status: Pass, Detail: "UP (pid=1)"},
			{Name: "agent verb: clip", Status: Warn},
		},
	}
	got, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"host":"devbox","checks":[` +
		`{"name":"ssh master","status":"PASS","detail":"UP (pid=1)"},` +
		`{"name":"agent verb: clip","status":"WARN"}` +
		`]}`
	if string(got) != want {
		t.Errorf("Marshal mismatch:\n got: %s\nwant: %s", got, want)
	}
}
