package localapi

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

type specScan struct {
	ops              map[string]bool
	missingResponses []string
}

// scanSpecOps parses a hand-formatted OpenAPI document into "METHOD /path" keys.
// It only scans inside the column-0 paths: section and records operations whose
// method body lacks the required six-space responses: key.
func scanSpecOps(doc []byte) (specScan, error) {
	out := specScan{ops: map[string]bool{}}
	inPaths := false
	var cur string
	var curOp string
	hasResponses := false

	finishOp := func() {
		if curOp != "" && !hasResponses {
			out.missingResponses = append(out.missingResponses, curOp)
		}
		curOp = ""
		hasResponses = false
	}

	sc := bufio.NewScanner(bytes.NewReader(doc))
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if columnZeroKey(line) {
			finishOp()
			inPaths = line == "paths:"
			if !inPaths {
				cur = ""
			}
			continue
		}
		if !inPaths {
			continue
		}
		// Path key: exactly 2-space indent, starts with '/', ends with ':'.
		if strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":") {
			finishOp()
			body := strings.TrimPrefix(line, "  ")
			if !strings.HasPrefix(body, " ") {
				cur = strings.TrimSuffix(body, ":")
				continue
			}
		}
		// Method key: exactly 4-space indent, ends with ':'.
		if strings.HasPrefix(line, "    ") && strings.HasSuffix(line, ":") {
			body := strings.TrimPrefix(line, "    ")
			if !strings.HasPrefix(body, " ") {
				m := strings.TrimSuffix(body, ":")
				if isSpecVerb(m) && cur != "" {
					finishOp()
					curOp = strings.ToUpper(m) + " " + cur
					out.ops[curOp] = true
				}
				continue
			}
		}
		if curOp != "" && line == "      responses:" {
			hasResponses = true
		}
	}
	if err := sc.Err(); err != nil {
		return specScan{}, err
	}
	finishOp()
	return out, nil
}

func columnZeroKey(line string) bool {
	return line != "" && line[0] != ' ' && strings.HasSuffix(line, ":")
}

func isSpecVerb(s string) bool {
	switch s {
	case "get", "put", "post", "delete":
		return true
	default:
		return false
	}
}

func specOps(t *testing.T, doc []byte) map[string]bool {
	t.Helper()
	scan, err := scanSpecOps(doc)
	if err != nil {
		t.Fatal(err)
	}
	return scan.ops
}

// TestSpecMuxConformance is the D2 conformance check: every spec operation has a
// registered route and every registered route is documented in the spec. It
// fails on drift in either direction.
func TestSpecMuxConformance(t *testing.T) {
	s := New(Deps{})

	routes := map[string]bool{}
	for _, r := range s.routes {
		routes[r.Method+" "+r.Pattern] = true
	}
	spec := specOps(t, openapiSpec)

	if len(spec) == 0 {
		t.Fatal("parsed zero operations from openapi.yaml — parser or spec broken")
	}

	for op := range spec {
		if !routes[op] {
			t.Errorf("spec documents %q but no route is registered", op)
		}
	}
	for op := range routes {
		if !spec[op] {
			t.Errorf("route %q is registered but not documented in openapi.yaml", op)
		}
	}
}

func TestSpecOperationsHaveResponses(t *testing.T) {
	scan, err := scanSpecOps(openapiSpec)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.missingResponses) > 0 {
		t.Fatalf("operations missing responses: %v", scan.missingResponses)
	}
}

func TestSpecScannerIgnoresComponentKeys(t *testing.T) {
	doc := []byte(`openapi: 3.0.3
paths:
  /v1/version:
    get:
      summary: ok
      responses:
        "200":
          description: ok
components:
  schemas:
    post:
      type: object
  /v1/fake:
    put:
      summary: fake
`)
	scan, err := scanSpecOps(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"GET /v1/version": true}
	if got := len(scan.ops); got != len(want) {
		t.Fatalf("operation count = %d, want %d (%v)", got, len(want), scan.ops)
	}
	for op := range want {
		if !scan.ops[op] {
			t.Fatalf("%s not parsed from paths section: %v", op, scan.ops)
		}
	}
	if scan.ops["POST /v1/version"] || scan.ops["PUT /v1/fake"] {
		t.Fatalf("component keys were parsed as operations: %v", scan.ops)
	}
	if len(scan.missingResponses) > 0 {
		t.Fatalf("synthetic operations missing responses: %v", scan.missingResponses)
	}
}

func TestSpecScannerReportsMissingResponses(t *testing.T) {
	doc := []byte(`openapi: 3.0.3
paths:
  /v1/status:
    get:
      summary: missing
`)
	scan, err := scanSpecOps(doc)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(scan.missingResponses), "[GET /v1/status]"; got != want {
		t.Fatalf("missing responses = %s, want %s", got, want)
	}
}
