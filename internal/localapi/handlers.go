package localapi

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

// openapiSpec is the committed spec, served verbatim at GET /v1/openapi.yaml
// and walked by the conformance test. Its formatting contract is documented at
// the top of openapi.yaml.
//
//go:embed openapi.yaml
var openapiSpec []byte

// errorBody is the D9 error envelope: {"error":{"code":..,"message":..}}.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON encodes v as a JSON response with the given status code. Generic so
// every call site passes a concrete type (no any-typed payloads).
func writeJSON[T any](w http.ResponseWriter, code int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the D9 error shape with a stable machine code.
func writeError(w http.ResponseWriter, code int, machineCode, msg string) {
	writeJSON(w, code, errorBody{Error: errorDetail{Code: machineCode, Message: msg}})
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
