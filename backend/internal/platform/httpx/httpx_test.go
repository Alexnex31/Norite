package httpx

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	WriteJSON(rec, req, http.StatusCreated, map[string]string{"id": "42"})

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"id":"42"}`, rec.Body.String())
}

func TestWriteJSONNoContentWritesNoBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/", nil)

	WriteJSON(rec, req, http.StatusNoContent, map[string]string{"ignored": "yes"})

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestWriteErrorMapsSentinels(t *testing.T) {
	tests := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{ErrBadRequest, http.StatusBadRequest, "bad_request"},
		{ErrUnauthorized, http.StatusUnauthorized, "unauthorized"},
		{ErrForbidden, http.StatusForbidden, "forbidden"},
		{ErrNotFound, http.StatusNotFound, "not_found"},
		{ErrConflict, http.StatusConflict, "conflict"},
		{ErrRateLimited, http.StatusTooManyRequests, "rate_limited"},
		{ErrUnavailable, http.StatusServiceUnavailable, "service_unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.wantCode, func(t *testing.T) {
			// Wrapped, not bare — services return errors wrapping these sentinels, and errors.Is must
			// still find them.
			body := writeErrorBody(t, fmt.Errorf("layered context: %w", tt.err), tt.wantStatus)
			assert.Equal(t, tt.wantCode, body.Error.Code)
		})
	}
}

func TestWriteErrorUnknownErrorIsOpaque500(t *testing.T) {
	// A driver or query error must not reach the client verbatim: it can carry hostnames, SQL, or
	// column names.
	body := writeErrorBody(t, errors.New("pq: relation \"users\" does not exist on host db.internal"), http.StatusInternalServerError)

	assert.Equal(t, "internal_error", body.Error.Code)
	assert.Equal(t, "internal server error", body.Error.Message)
	assert.NotContains(t, body.Error.Message, "db.internal")
}

func TestWriteErrorStatusErrorDetailReachesClientOnly4xx(t *testing.T) {
	body := writeErrorBody(t, Errorf(ErrNotFound, "guild %s does not exist", "123"), http.StatusNotFound)
	assert.Equal(t, "not_found", body.Error.Code)
	assert.Equal(t, "guild 123 does not exist", body.Error.Message)

	// A 5xx StatusError keeps its detail server-side.
	serverErr := &StatusError{
		Status:  http.StatusInternalServerError,
		Code:    "internal_error",
		Message: "snapshot job failed against replica db-3",
		Err:     errors.New("dial tcp 10.0.0.3:5432: connect: connection refused"),
	}
	body = writeErrorBody(t, serverErr, http.StatusInternalServerError)
	assert.Equal(t, "internal server error", body.Error.Message)
	assert.NotContains(t, body.Error.Message, "10.0.0.3")
}

func TestWriteErrorIncludesRequestID(t *testing.T) {
	rec := httptest.NewRecorder()

	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, ErrNotFound)
	}))
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var body errorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotEmpty(t, body.Error.RequestID, "clients need a handle to correlate with the server log line")
}

func TestStatusErrorUnwrapsToItsSentinel(t *testing.T) {
	err := Errorf(ErrForbidden, "missing MANAGE_ROLES")
	assert.True(t, errors.Is(err, ErrForbidden))
	assert.Contains(t, err.Error(), "missing MANAGE_ROLES")
}

type payload struct {
	Name string `json:"name"`
}

func TestDecodeJSON(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		contentType string
		wantErr     string
	}{
		{name: "valid", body: `{"name":"norite"}`, contentType: "application/json"},
		{name: "valid with charset", body: `{"name":"norite"}`, contentType: "application/json; charset=utf-8"},
		{name: "no content type is accepted", body: `{"name":"norite"}`},
		{name: "wrong content type", body: `{"name":"norite"}`, contentType: "text/plain", wantErr: "Content-Type must be application/json"},
		{name: "unknown field", body: `{"name":"norite","is_admin":true}`, contentType: "application/json", wantErr: `unknown field "is_admin"`},
		{name: "wrong field type", body: `{"name":42}`, contentType: "application/json", wantErr: "field \"name\" must be of type string"},
		{name: "malformed", body: `{"name":`, contentType: "application/json", wantErr: "malformed JSON"},
		{name: "empty", body: ``, contentType: "application/json", wantErr: "must not be empty"},
		{name: "trailing value", body: `{"name":"a"}{"name":"b"}`, contentType: "application/json", wantErr: "single JSON object"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}

			var dst payload
			err := DecodeJSON(httptest.NewRecorder(), req, &dst)

			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, "norite", dst.Name)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrBadRequest, "every decode failure must be client-attributable")
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestDecodeJSONEnforcesSizeCap(t *testing.T) {
	oversized := `{"name":"` + strings.Repeat("a", maxRequestBody+1) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(oversized))
	req.Header.Set("Content-Type", "application/json")

	var dst payload
	err := DecodeJSON(httptest.NewRecorder(), req, &dst)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadRequest)
	assert.Contains(t, err.Error(), "must not exceed")
}

func TestSecureHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	h := SecureHeaders(false)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
	assert.NotEmpty(t, rec.Header().Get("Permissions-Policy"))
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"), "no cross-origin access is granted by default")
}

func TestSecureHeadersHSTSOnlyOverTLS(t *testing.T) {
	call := func(trustProxy bool, tlsState *tls.ConnectionState, forwardedProto string) string {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.TLS = tlsState
		if forwardedProto != "" {
			req.Header.Set("X-Forwarded-Proto", forwardedProto)
		}
		rec := httptest.NewRecorder()
		SecureHeaders(trustProxy)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)
		return rec.Header().Get("Strict-Transport-Security")
	}

	assert.Empty(t, call(false, nil, ""), "plain HTTP must not advertise HSTS (LAN-only deployments)")
	assert.NotEmpty(t, call(false, &tls.ConnectionState{}, ""), "direct TLS advertises HSTS")
	assert.NotEmpty(t, call(true, nil, "https"), "TLS-terminating proxy advertises HSTS when trusted")

	// The whole point of the trust flag: a client-settable header must not be able to turn HSTS on for
	// a directly-exposed process.
	assert.Empty(t, call(false, nil, "https"), "untrusted X-Forwarded-Proto must be ignored")
}

// writeErrorBody runs WriteError and returns the decoded envelope, asserting the status on the way.
func writeErrorBody(t *testing.T, err error, wantStatus int) errorResponse {
	t.Helper()

	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), err)

	require.Equal(t, wantStatus, rec.Code)
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))

	var body errorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}
