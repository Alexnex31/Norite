package httpx

import (
	"errors"
	"net/http"
	"runtime/debug"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
)

// Recoverer converts a panic in any downstream handler into a logged 500 rather than a killed process.
//
// It replaces chi's own Recoverer for two reasons. First, chi's writes the stack trace to the standard
// library logger — unstructured text on stderr, which is exactly the line an operator most needs to find
// in a JSON log pipeline. Second, chi's responds with a bare plain-text body, while every other rejection
// this API produces uses the JSON error envelope; clients should not have to special-case the one
// response shape that shows up when something has already gone wrong.
//
// It sits above the request-logging middleware in the chain (docs/architecture.md §2), so it logs through
// the root logger rather than a request-scoped one. The request ID is still recorded, which is what ties
// this line back to the rest of the request's trail.
func Recoverer(logger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}

				// http.ErrAbortHandler is the standard library's documented way to abandon a response
				// deliberately. Swallowing it would turn an intentional abort into a spurious 500, so
				// it is passed straight through to net/http, which expects to handle it.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}

				logger.Error().
					Str("request_id", middleware.GetReqID(r.Context())).
					Str("method", r.Method).
					Str("path", r.URL.Path).
					Interface("panic", rec).
					Bytes("stack", debug.Stack()).
					Msg("recovered from panic in handler")

				// The panic value itself never reaches the client: it routinely carries internal state
				// (nil-pointer detail, SQL fragments, file paths).
				WriteError(w, r, errors.New("handler panicked"))
			}()

			next.ServeHTTP(w, r)
		})
	}
}
