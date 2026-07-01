package localapi

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// specOps parses the embedded openapi.yaml into a set of "METHOD /path" keys
// using the documented formatting contract: a path key is 2-space-indented and
// starts with '/'; a method key is 4-space-indented and is one of the HTTP
// verbs. This is a deliberately minimal stdlib line parser, not a YAML library.
func specOps(t *testing.T) map[string]bool {
	t.Helper()
	verbs := map[string]bool{"get": true, "put": true, "post": true, "delete": true}
	ops := map[string]bool{}
	var cur string
	sc := bufio.NewScanner(bytes.NewReader(openapiSpec))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		// Path key: exactly 2-space indent, starts with '/', ends with ':'.
		if strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":") {
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
				if verbs[m] && cur != "" {
					ops[strings.ToUpper(m)+" "+cur] = true
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return ops
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
	spec := specOps(t)

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
