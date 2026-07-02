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

// TestStatusUnmarshalJSON proves the symmetric decode added for localclient.Doctor
// and u5's renderDoctor: a known tag maps to its enum value, while an unknown
// token or a malformed value defaults to Fail so a corrupt status never reads as
// a pass.
func TestStatusUnmarshalJSON(t *testing.T) {
	tests := []struct {
		in   string
		want Status
	}{
		{`"PASS"`, Pass},
		{`"WARN"`, Warn},
		{`"FAIL"`, Fail},
		{`"NOPE"`, Fail}, // unknown token -> Fail
		{`123`, Fail},    // malformed (not a string) -> Fail
		{`"pass"`, Fail}, // case-sensitive: lowercase is not a known tag
	}
	for _, tt := range tests {
		var s Status
		if err := json.Unmarshal([]byte(tt.in), &s); err != nil {
			t.Fatalf("Unmarshal(%s): unexpected error %v", tt.in, err)
		}
		if s != tt.want {
			t.Errorf("Unmarshal(%s) = %v (tag %q), want %v (tag %q)", tt.in, s, s.Tag(), tt.want, tt.want.Tag())
		}
	}
}

// TestReportRoundTrip proves a full Report survives Marshal->Unmarshal with every
// Check.Status preserved, so renderDoctor's Tag() reads correctly after a decode
// over the socket.
func TestReportRoundTrip(t *testing.T) {
	want := Report{
		Host: "devbox",
		Checks: []Check{
			{Name: "ssh master", Status: Pass, Detail: "UP (pid=1)"},
			{Name: "agent verb: clip", Status: Warn},
			{Name: "notify", Status: Fail, Detail: "no path"},
		},
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Host != want.Host {
		t.Errorf("Host = %q, want %q", got.Host, want.Host)
	}
	if len(got.Checks) != len(want.Checks) {
		t.Fatalf("got %d checks, want %d", len(got.Checks), len(want.Checks))
	}
	for i, c := range got.Checks {
		if c != want.Checks[i] {
			t.Errorf("check %d = %+v, want %+v", i, c, want.Checks[i])
		}
	}
}
