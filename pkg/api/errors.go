package api

import "fmt"

// ErrorBody is the D9 error envelope: {"error":{"code":..,"message":..}}.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// APIError is a decoded D9 error envelope for a non-2xx response the caller does
// not special-case.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("localapi %d %s: %s", e.Status, e.Code, e.Message)
}
