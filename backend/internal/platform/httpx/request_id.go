package httpx

import (
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// RequestIDHeaderName is the response header carrying the request's correlation ID.
const RequestIDHeaderName = "X-Request-Id"

// maxInboundRequestIDLength bounds an ID accepted from a trusted proxy. Long enough for a UUID, a chi ID,
// or a W3C trace ID; short enough that the value cannot become a payload in its own right.
const maxInboundRequestIDLength = 128

// SanitizeInboundRequestID decides whether the client's X-Request-Id header may be used as this request's
// correlation ID, and strips it when not.
//
// chi's RequestID middleware adopts an inbound X-Request-Id verbatim whenever one is present. That value
// then reaches the response headers (EchoRequestID), every log line for the request, and every error body
// — so on a directly-exposed process it is an attacker-controlled string in all three. Two concrete
// consequences: any caller can make unrelated requests share one ID, which defeats the "quote your request
// ID" support path this codebase invests in; and a caller can hang up to Go's whole MaxHeaderBytes budget
// off every one of their log lines.
//
// The decision is the same one httpx.RealIP makes about X-Forwarded-For, and is made in the same place and
// for the same reason: a forwarded header is only meaningful behind a proxy the operator controls, so it is
// honored when TrustProxyHeaders is on and discarded otherwise. Even when trusted the value is bounded and
// charset-restricted, because a trusted proxy generally passes through what the client sent rather than
// minting its own.
//
// Must be mounted above chi's RequestID middleware, which is what reads the header.
func SanitizeInboundRequestID(trustProxyHeaders bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id := r.Header.Get(RequestIDHeaderName); id != "" && !usableRequestID(id, trustProxyHeaders) {
				// Deleted rather than overwritten: chi generates its own when the header is absent, and
				// duplicating that generator here would be a second thing to keep in sync.
				r.Header.Del(RequestIDHeaderName)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// usableRequestID reports whether an inbound correlation ID may be adopted as-is.
func usableRequestID(id string, trustProxyHeaders bool) bool {
	if !trustProxyHeaders || len(id) > maxInboundRequestIDLength {
		return false
	}
	// Printable ASCII minus space and the quoting characters, so the value cannot break a log field, a
	// header, or a JSON string no matter where it is later rendered.
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '=', r == '+', r == '/', r == ':':
		default:
			return false
		}
	}
	return true
}

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
