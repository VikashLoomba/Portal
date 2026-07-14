package api

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestSetupEventMarshalOmitsEmptyFields(t *testing.T) {
	encoded, err := json.Marshal(SetupEvent{Step: "validate", Status: "running"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal map: %v", err)
	}
	if len(got) != 2 || got["step"] != "validate" || got["status"] != "running" {
		t.Fatalf("marshaled event keys = %v, want exactly step and status", got)
	}
}

func TestSetupEventReportIsOpaqueJSON(t *testing.T) {
	report := json.RawMessage(`{"passed":true}`)
	encoded, err := json.Marshal(SetupEvent{Step: "doctor", Status: "ok", Report: report})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("Unmarshal map: %v", err)
	}
	if got := wire["report"]; !bytes.Equal(got, report) {
		t.Fatalf("report JSON = %s, want nested object %s", got, report)
	}

	var got SetupEvent
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal SetupEvent: %v", err)
	}
	if !bytes.Equal(got.Report, report) {
		t.Fatalf("round-tripped report = %s, want %s", got.Report, report)
	}
}

func TestSetupEventErrorMarshalsNested(t *testing.T) {
	event := SetupEvent{
		Step:   "validate",
		Status: "fail",
		Error:  &ErrorDetail{Code: "validation_failed", Message: "ssh failed"},
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal map: %v", err)
	}
	detail, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %#v, want nested object", got["error"])
	}
	if detail["code"] != "validation_failed" || detail["message"] != "ssh failed" {
		t.Fatalf("error detail = %v, want validation_failed and ssh failed", detail)
	}
}

func TestSetupEventRoundTrip(t *testing.T) {
	want := SetupEvent{
		Step:   "doctor",
		Status: "warn",
		Line:   "doctor completed with warnings",
		Error:  &ErrorDetail{Code: "doctor_warning", Message: "one check failed"},
		Report: json.RawMessage(`{"passed":false,"checks":["ssh"]}`),
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got SetupEvent
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-tripped event = %+v, want %+v", got, want)
	}
}

func TestSetupRequestJSON(t *testing.T) {
	unmarshalTests := []struct {
		name string
		body string
		want SetupRequest
	}{
		{name: "defaults", body: `{}`, want: SetupRequest{}},
		{name: "host only", body: `{"host":"h"}`, want: SetupRequest{Host: "h"}},
		{name: "host and force", body: `{"host":"h","force":true}`, want: SetupRequest{Host: "h", Force: true}},
	}
	for _, tt := range unmarshalTests {
		t.Run("unmarshal "+tt.name, func(t *testing.T) {
			var got SetupRequest
			if err := json.Unmarshal([]byte(tt.body), &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got != tt.want {
				t.Fatalf("request = %+v, want %+v", got, tt.want)
			}
		})
	}

	marshalTests := []struct {
		name string
		in   SetupRequest
		want string
	}{
		{name: "defaults", in: SetupRequest{}, want: `{}`},
		{name: "host only", in: SetupRequest{Host: "h"}, want: `{"host":"h"}`},
		{name: "force only", in: SetupRequest{Force: true}, want: `{"force":true}`},
		{name: "host and force", in: SetupRequest{Host: "h", Force: true}, want: `{"host":"h","force":true}`},
	}
	for _, tt := range marshalTests {
		t.Run("marshal "+tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("request JSON = %s, want %s", got, tt.want)
			}
		})
	}
}
