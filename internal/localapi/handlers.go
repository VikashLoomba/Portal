package localapi

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/VikashLoomba/Portal/pkg/api"
)

// openapiSpec is the committed spec, served verbatim at GET /v1/openapi.yaml
// and walked by the conformance test. Its formatting contract is documented at
// the top of openapi.yaml.
//
//go:embed openapi.yaml
var openapiSpec []byte

// writeJSON encodes v as a JSON response with the given status code. Generic so
// every call site passes a concrete type (no any-typed payloads).
func writeJSON[T any](w http.ResponseWriter, code int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the D9 error shape with a stable machine code.
func writeError(w http.ResponseWriter, code int, machineCode, msg string) {
	writeJSON(w, code, api.ErrorBody{Error: api.ErrorDetail{Code: machineCode, Message: msg}})
}

// handleVersion serves the injected VersionInfo. Works before a host is
// configured (D6).
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deps.Version)
}

// handleOpenAPI serves the embedded spec bytes verbatim.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openapiSpec)
}

// handleStatus serves the full Status aggregate.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildStatus(r.Context()))
}

// allowlistResponse is the body of the allow mutation endpoints: the full
// allowlist after the change (allow mutations return it inline; there is no
// separate GET /v1/allowed — §4.5).
type allowlistResponse struct {
	Allowed []int `json:"allowed"`
}

// featureUpdate is the PUT /v1/features/{name} body: a typed toggle, never an
// any-shaped payload.
type featureUpdate struct {
	Enabled bool `json:"enabled"`
}

// parsePort parses a port path segment and reports whether it is a valid TCP
// port in 1..65535.
func parsePort(raw string) (int, bool) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// handlePorts lists remote loopback listeners from the cached Snapshot. Before
// the first cached Snapshot (ok==false) it is 503 not_connected — a later
// boundary than the handshake (§4.5).
func (s *Server) handlePorts(w http.ResponseWriter, r *http.Request) {
	_, ports, ok := s.deps.Agent.Snapshot()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "not_connected", "no cached port snapshot from the agent yet")
		return
	}
	out := make([]api.PortStatus, 0, len(ports))
	for _, p := range ports {
		out = append(out, api.PortStatus{Port: int(p)})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAllowPut adds a port to the allowlist and pushes the new list to the
// in-process AgentClient (best-effort — this is what makes "~100ms" true),
// returning the resulting allowlist.
func (s *Server) handleAllowPut(w http.ResponseWriter, r *http.Request) {
	port, ok := parsePort(r.PathValue("port"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_port", "port must be an integer in 1..65535")
		return
	}
	if _, err := s.deps.Config.Allow([]int{port}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.writeAllowlist(w)
}

// handleAllowDelete removes a port from the allowlist and pushes the new list,
// mirroring handleAllowPut.
func (s *Server) handleAllowDelete(w http.ResponseWriter, r *http.Request) {
	port, ok := parsePort(r.PathValue("port"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_port", "port must be an integer in 1..65535")
		return
	}
	if err := s.deps.Config.Unallow([]int{port}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.writeAllowlist(w)
}

// writeAllowlist reads back the allowlist, pushes it to the AgentClient
// best-effort (deny/ephemeral policy lives in the run.go closure, not here),
// and writes it as the 200 response.
func (s *Server) writeAllowlist(w http.ResponseWriter) {
	allowed, err := s.deps.Config.AllowedPorts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if s.deps.PushAllow != nil {
		_ = s.deps.PushAllow(allowed)
	}
	writeJSON(w, http.StatusOK, allowlistResponse{Allowed: allowed})
}

// handleFeatures returns the capability gates as a name→enabled map.
func (s *Server) handleFeatures(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.featuresMap())
}

// handleFeaturePut toggles one capability gate. An unknown name is 404
// feature_unknown; a malformed/empty body is 400 invalid_request.
func (s *Server) handleFeaturePut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.knownFeature(name) {
		writeError(w, http.StatusNotFound, "feature_unknown", "unknown feature: "+name)
		return
	}
	var u featureUpdate
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be {\"enabled\":bool}")
		return
	}
	if err := s.deps.Config.SetFeature(name, u.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.featuresMap())
}

// featuresMap builds the name→enabled map over the configured FeatureNames.
func (s *Server) featuresMap() map[string]bool {
	m := make(map[string]bool, len(s.deps.FeatureNames))
	for _, name := range s.deps.FeatureNames {
		m[name] = s.deps.Config.FeatureEnabled(name)
	}
	return m
}

// knownFeature reports whether name is one of the configured gates.
func (s *Server) knownFeature(name string) bool {
	for _, n := range s.deps.FeatureNames {
		if n == name {
			return true
		}
	}
	return false
}

// handleReconcile kicks the forward engine's reconcile and returns 202.
func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if s.deps.Kick != nil {
		s.deps.Kick()
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// handleDoctor runs the doctor self-test and returns the structured report as
// JSON. It passes r.Context() so a client disconnect aborts the ssh probes.
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	rep := s.deps.Doctor(r.Context())
	writeJSON(w, http.StatusOK, rep)
}
