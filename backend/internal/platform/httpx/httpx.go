// Package httpx holds the HTTP plumbing every domain package shares: the JSON response envelope, the
// domain-error-to-HTTP-status mapping, JSON encode/decode helpers, and the secure-headers middleware.
//
// Domain packages return sentinel errors from this package (or errors wrapping them) and let WriteError
// decide the status code, so the mapping lives in exactly one place instead of being re-decided per
// handler.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
)

// Sentinel errors domain services return. WriteError maps each to a status code; anything unrecognized
// becomes a 500 with its detail withheld from the client.
var (
	ErrBadRequest   = errors.New("bad request")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrNotFound     = errors.New("not found")
	// ErrMethodNotAllowed is distinct from ErrBadRequest: a client that used the wrong verb needs to know
	// the route exists and which verbs it accepts, which a generic 400 hides.
	ErrMethodNotAllowed = errors.New("method not allowed")
	ErrConflict         = errors.New("conflict")
	ErrRateLimited      = errors.New("rate limited")
	ErrUnavailable      = errors.New("service unavailable")
)

// ErrorBody is the error envelope every non-2xx JSON response carries.
//
// Shape is deliberately flat and stable: clients (CLI --json output, the GUI, the later SPA) all key off
// Code, never off the human-readable Message.
type ErrorBody struct {
	// Code is a stable, machine-readable identifier, e.g. "not_found".
	Code string `json:"code"`
	// Message is a human-readable summary, safe to show a user.
	Message string `json:"message"`
	// RequestID ties the response to its server-side log line.
	RequestID string `json:"request_id,omitempty"`
}

// errorResponse wraps ErrorBody so error payloads are distinguishable from success payloads at the top
// level of the JSON document.
type errorResponse struct {
	Error ErrorBody `json:"error"`
}

// StatusError pairs an explicit HTTP status and client-visible message with an underlying error.
//
// Use it when a handler needs to say more than a bare sentinel does; the wrapped error still reaches the
// log while only Message reaches the client.
type StatusError struct {
	Status  int
	Code    string
	Message string
	Err     error
}

func (e *StatusError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *StatusError) Unwrap() error { return e.Err }

// Errorf builds a StatusError from one of this package's sentinels, keeping the sentinel wrapped so
// errors.Is still matches it.
func Errorf(sentinel error, format string, args ...any) *StatusError {
	status, code := statusFor(sentinel)
	return &StatusError{
		Status:  status,
		Code:    code,
		Message: fmt.Sprintf(format, args...),
		Err:     sentinel,
	}
}

// WriteJSON writes v as JSON with the given status code.
//
// An encoding failure after the header is already flushed cannot be turned into an error response, so it
// is logged and the connection is left for the client to notice as a truncated body.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if v == nil || status == http.StatusNoContent {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logging.FromContext(r.Context()).Error().Err(err).Msg("failed to encode JSON response")
	}
}

// WriteError maps err to a status code and writes the error envelope.
//
// Server-side (5xx) details are logged but never sent to the client — an internal error message can carry
// query fragments, hostnames, or driver detail that has no business crossing the wire.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := classify(err)

	logger := logging.FromContext(r.Context())
	if status >= http.StatusInternalServerError {
		logger.Error().Err(err).Int("status", status).Msg("request failed")
	} else {
		logger.Debug().Err(err).Int("status", status).Msg("request rejected")
	}

	WriteJSON(w, r, status, errorResponse{Error: ErrorBody{
		Code:      code,
		Message:   message,
		RequestID: middleware.GetReqID(r.Context()),
	}})
}

// classify resolves an error to the status, code, and client-visible message to send.
func classify(err error) (status int, code string, message string) {
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		message := statusErr.Message
		if statusErr.Status >= http.StatusInternalServerError {
			message = "internal server error"
		}
		return statusErr.Status, statusErr.Code, message
	}

	for _, sentinel := range []error{
		ErrBadRequest, ErrUnauthorized, ErrForbidden, ErrNotFound, ErrMethodNotAllowed,
		ErrConflict, ErrRateLimited, ErrUnavailable,
	} {
		if errors.Is(err, sentinel) {
			status, code := statusFor(sentinel)
			return status, code, sentinel.Error()
		}
	}

	return http.StatusInternalServerError, "internal_error", "internal server error"
}

// statusFor maps a sentinel to its HTTP status and stable error code.
func statusFor(sentinel error) (int, string) {
	switch {
	case errors.Is(sentinel, ErrBadRequest):
		return http.StatusBadRequest, "bad_request"
	case errors.Is(sentinel, ErrUnauthorized):
		return http.StatusUnauthorized, "unauthorized"
	case errors.Is(sentinel, ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(sentinel, ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(sentinel, ErrMethodNotAllowed):
		return http.StatusMethodNotAllowed, "method_not_allowed"
	case errors.Is(sentinel, ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(sentinel, ErrRateLimited):
		return http.StatusTooManyRequests, "rate_limited"
	case errors.Is(sentinel, ErrUnavailable):
		return http.StatusServiceUnavailable, "service_unavailable"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

// maxRequestBody caps how much of a request body DecodeJSON will read. Generous for the JSON payloads
// this API accepts, small enough that an unbounded body can't exhaust memory. Attachment uploads
// (Milestone M50) get their own, larger, streaming path rather than raising this.
const maxRequestBody = 1 << 20 // 1 MiB

// DecodeJSON reads a JSON request body into dst.
//
// It enforces three things the standard decoder does not, all of them defensive rather than convenient:
// a size cap, rejection of unknown fields (docs/architecture.md §14.1 — a typo'd or injected field must
// fail loudly, not be silently ignored), and rejection of trailing content after the first JSON value.
// Every failure comes back as ErrBadRequest so handlers can pass it straight to WriteError.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mediaType := strings.TrimSpace(strings.Split(ct, ";")[0]); mediaType != "application/json" {
			return Errorf(ErrBadRequest, "Content-Type must be application/json")
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// A second value in the same body is a sign of a malformed or smuggled request, not a valid payload.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Errorf(ErrBadRequest, "request body must contain a single JSON object")
	}
	return nil
}

// decodeError turns a json decode failure into a client-safe ErrBadRequest.
func decodeError(err error) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var maxBytesErr *http.MaxBytesError

	switch {
	case errors.As(err, &syntaxErr):
		return Errorf(ErrBadRequest, "malformed JSON at byte %d", syntaxErr.Offset)
	case errors.Is(err, io.ErrUnexpectedEOF):
		// A body that stops mid-value: truncated upload, or a client that closed early.
		return Errorf(ErrBadRequest, "malformed JSON: unexpected end of request body")
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return Errorf(ErrBadRequest, "field %q must be of type %s", typeErr.Field, typeErr.Type)
		}
		return Errorf(ErrBadRequest, "request body has the wrong type")
	case errors.As(err, &maxBytesErr):
		return Errorf(ErrBadRequest, "request body must not exceed %d bytes", maxRequestBody)
	case errors.Is(err, io.EOF):
		return Errorf(ErrBadRequest, "request body must not be empty")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.TrimPrefix(err.Error(), "json: unknown field ")
		return Errorf(ErrBadRequest, "unknown field %s", field)
	default:
		return Errorf(ErrBadRequest, "request body could not be decoded")
	}
}
