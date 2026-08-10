package logging

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
)

// RequestLogger returns middleware that attaches a request-scoped logger to the context and emits one
// structured line per completed request.
//
// Deliberately *not* logged, per CLAUDE.md rule 8:
//
//   - the query string — it is user-controlled and, on OAuth callback and password-reset routes
//     specifically, carries authorization codes and reset tokens. Only the routed path is recorded.
//   - any request header — Authorization and Cookie both carry live credentials, so the whole header map
//     is left out rather than maintaining a deny-list that a future header could slip past.
//   - request and response bodies — passwords, tokens, and message content all travel there.
//
// Panics are not handled here; chi's Recoverer sits above this middleware in the chain and converts them
// before they unwind past it.
func RequestLogger(logger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			reqLogger := logger.With().
				Str("request_id", middleware.GetReqID(r.Context())).
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Logger()

			next.ServeHTTP(ww, r.WithContext(WithContext(r.Context(), reqLogger)))

			status := ww.Status()
			// A handler that writes nothing leaves the wrapper's status at 0; net/http sends 200 in that
			// case, so record what the client actually saw.
			if status == 0 {
				status = http.StatusOK
			}

			event := reqLogger.Info()
			if status >= http.StatusInternalServerError {
				event = reqLogger.Error()
			} else if status >= http.StatusBadRequest {
				event = reqLogger.Warn()
			}

			event.
				Int("status", status).
				Int("bytes", ww.BytesWritten()).
				Dur("duration", time.Since(start)).
				Msg("request completed")
		})
	}
}
