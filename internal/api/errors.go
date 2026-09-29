package api

import (
	"encoding/json"
	"net/http"
)

// Error codes. They are part of the public contract (openapi/openapi.yaml).
const (
	CodeInvalidParameter = "invalid_parameter"
	CodeUnknownParameter = "unknown_parameter"
	CodeInvalidQuery     = "invalid_query"
	CodeUnknownRelease   = "unknown_release"
	CodeReleaseExpired   = "release_expired"
	CodeUnknownStyle     = "unknown_style"
	CodeNoActiveRelease  = "no_active_release"
	CodeInvalidTile      = "invalid_tile_coordinates"
	CodeZoomOutOfRange   = "tile_zoom_out_of_range"
	CodeUnknownFontstack = "unknown_fontstack"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeBodyNotAllowed   = "request_body_not_allowed"
	CodeTimeout          = "timeout"
	CodeUnavailable      = "service_unavailable"
	CodeInternal         = "internal_error"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Parameter string `json:"parameter,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// writeError sends a JSON error body. Errors are never cached.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message, parameter string) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Del("ETag")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{
		Code: code, Message: message, Parameter: parameter, RequestID: requestID(r.Context()),
	}})
}
