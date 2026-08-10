package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRejectsUnknownLevelAndFormat(t *testing.T) {
	_, err := New(Options{Level: "chatty", Format: "json"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chatty")

	_, err = New(Options{Level: "info", Format: "yaml"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "yaml")
}

func TestNewRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(Options{Level: "warn", Format: "json", Output: &buf})
	require.NoError(t, err)

	logger.Info().Msg("filtered out")
	assert.Empty(t, buf.String())

	logger.Warn().Msg("kept")
	assert.Contains(t, buf.String(), "kept")
}

func TestFromContextWithoutLoggerIsDisabled(t *testing.T) {
	// A missing logger must be inert rather than falling back to a global that writes unattributed
	// lines — that would hide the wiring bug instead of surfacing it.
	logger := FromContext(context.Background())
	assert.Equal(t, zerolog.Disabled, logger.GetLevel())
}

func TestWithContextRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(Options{Level: "info", Format: "json", Output: &buf})
	require.NoError(t, err)

	ctx := WithContext(context.Background(), logger)
	FromContext(ctx).Info().Msg("round tripped")
	assert.Contains(t, buf.String(), "round tripped")
}

func TestRequestLoggerEmitsOneLineWithRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(Options{Level: "info", Format: "json", Output: &buf})
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(RequestLogger(logger))
	r.Get("/api/v1/healthz", func(w http.ResponseWriter, req *http.Request) {
		// Handlers reach the request-scoped logger through the context, not a package global.
		FromContext(req.Context()).Info().Msg("handler ran")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	lines := decodeLines(t, buf.Bytes())
	require.Len(t, lines, 2, "one handler line plus one completion line")

	completion := lines[1]
	assert.Equal(t, "request completed", completion["message"])
	assert.Equal(t, "GET", completion["method"])
	assert.Equal(t, "/api/v1/healthz", completion["path"])
	assert.Equal(t, float64(http.StatusOK), completion["status"])
	assert.Equal(t, float64(len(`{"ok":true}`)), completion["bytes"])
	assert.NotEmpty(t, completion["request_id"])
	assert.Contains(t, completion, "duration")

	// The handler line inherits the same request-scoped fields.
	assert.Equal(t, completion["request_id"], lines[0]["request_id"])
}

func TestRequestLoggerRecordsImplicit200(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(Options{Level: "info", Format: "json", Output: &buf})
	require.NoError(t, err)

	h := RequestLogger(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	lines := decodeLines(t, buf.Bytes())
	require.Len(t, lines, 1)
	assert.Equal(t, float64(http.StatusOK), lines[0]["status"])
}

func TestRequestLoggerSeverityTracksStatus(t *testing.T) {
	tests := []struct {
		status    int
		wantLevel string
	}{
		{http.StatusOK, "info"},
		{http.StatusNotFound, "warn"},
		{http.StatusInternalServerError, "error"},
	}

	for _, tt := range tests {
		var buf bytes.Buffer
		logger, err := New(Options{Level: "info", Format: "json", Output: &buf})
		require.NoError(t, err)

		h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tt.status)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

		lines := decodeLines(t, buf.Bytes())
		require.Len(t, lines, 1)
		assert.Equal(t, tt.wantLevel, lines[0]["level"], "status %d", tt.status)
	}
}

// CLAUDE.md rule 8: credentials must never reach the log. The two realistic leak paths for a request
// logger are the Authorization header and the query string (OAuth codes, password-reset tokens), so both
// are asserted against directly.
func TestRequestLoggerNeverLogsCredentials(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(Options{Level: "info", Format: "json", Output: &buf})
	require.NoError(t, err)

	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/github/callback?code=super-secret-code&state=xyz", nil)
	req.Header.Set("Authorization", "Bearer super-secret-token")
	req.Header.Set("Cookie", "session=super-secret-cookie")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	assert.NotContains(t, out, "super-secret-code")
	assert.NotContains(t, out, "super-secret-token")
	assert.NotContains(t, out, "super-secret-cookie")
	assert.NotContains(t, out, "Authorization")

	// The routed path is still recorded — dropping the query string must not cost observability.
	assert.Contains(t, out, "/api/v1/auth/oauth/github/callback")
}

func decodeLines(t *testing.T, raw []byte) []map[string]any {
	t.Helper()

	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	for dec.More() {
		var line map[string]any
		require.NoError(t, dec.Decode(&line))
		out = append(out, line)
	}
	return out
}
