package httpx

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecovererTurnsAPanicIntoTheErrorEnvelope(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	h := middleware.RequestID(Recoverer(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("connection string postgres://norite:hunter2@db.internal/norite blew up")
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/guilds", nil))

	require.Equal(t, http.StatusInternalServerError, rec.Code)

	var body errorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "internal_error", body.Error.Code)
	assert.Equal(t, "internal server error", body.Error.Message)
	// The panic value routinely carries internal state; none of it may reach the client.
	assert.NotContains(t, rec.Body.String(), "hunter2")
	assert.NotContains(t, rec.Body.String(), "db.internal")

	// Server-side, though, the operator needs everything: the panic value, a stack, and the ID that
	// ties this to the rest of the request's trail.
	out := buf.String()
	assert.Contains(t, out, "recovered from panic in handler")
	assert.Contains(t, out, "hunter2")
	assert.Contains(t, out, "/api/v1/guilds")
	assert.Contains(t, out, "\"stack\"")
	assert.NotEmpty(t, body.Error.RequestID)
	assert.Contains(t, out, body.Error.RequestID)
}

// http.ErrAbortHandler is the standard library's documented way to abandon a response deliberately;
// swallowing it would turn an intentional abort into a spurious 500.
func TestRecovererPassesThroughErrAbortHandler(t *testing.T) {
	h := Recoverer(zerolog.Nop())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	assert.PanicsWithError(t, http.ErrAbortHandler.Error(), func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
}

func TestRecovererLetsNormalResponsesThrough(t *testing.T) {
	h := Recoverer(zerolog.Nop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusTeapot, rec.Code)
}

func TestEchoRequestID(t *testing.T) {
	h := middleware.RequestID(EchoRequestID(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.NotEmpty(t, rec.Header().Get(RequestIDHeaderName))
}

// Without chi's RequestID above it there is nothing to echo; the middleware must stay out of the way
// rather than emit an empty header.
func TestEchoRequestIDWithoutAnIDIsANoop(t *testing.T) {
	rec := httptest.NewRecorder()
	EchoRequestID(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	_, present := rec.Header()[RequestIDHeaderName]
	assert.False(t, present)
}
