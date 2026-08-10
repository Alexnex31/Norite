package httpx

import (
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// RequestIDHeaderName is the response header carrying the request's correlation ID.
const RequestIDHeaderName = "X-Request-Id"

// EchoRequestID copies the request ID chi's RequestID middleware generated into the response headers.
//
// chi keeps that ID in the context only, which is enough for server-side logging but leaves a client with
// no way to name the request it is complaining about. The error envelope already carries it for failures;
// this covers successful responses too, so "the request that returned the wrong data" is just as
// reportable as one that errored.
//
// Must be mounted below chi's RequestID middleware, which is what generates the value.
func EchoRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := middleware.GetReqID(r.Context()); id != "" {
			w.Header().Set(RequestIDHeaderName, id)
		}
		next.ServeHTTP(w, r)
	})
}
