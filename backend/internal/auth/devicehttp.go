package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
)

// devicePagePath is where the verification page lives.
//
// At the root rather than under /api/v1, and short on purpose: this is the one URL in the whole system
// that somebody types by hand, from one screen onto another. It is also not an API anybody codegens
// against, which is the same reasoning that put /reset there.
const devicePagePath = "/device"

// The two JSON endpoints of the device-code flow: one to ask for a code, one to ask whether anybody has
// approved it yet.
//
// # Why the poll is a POST
//
// docs/architecture.md sketched it as GET /auth/device/code/{code}, and both halves of that are wrong for
// this codebase. The poll spends the code and starts a session, which rule 4 forbids a GET from doing —
// and it is the answer to more than a rule: a GET that mutates is one a browser prefetch, a link checker
// or a retry can fire on somebody's behalf. Putting the code in the path is the second problem: request
// paths are logged, and this one is a credential (rule 8). The body is where it belongs.

// deviceCodeRequest asks for a new authorization.
type deviceCodeRequest struct {
	// DeviceID is this installation's identity, captured now because the session that eventually comes out
	// of the flow is scoped to it — the browser that approves has no idea what it is.
	DeviceID   string `json:"device_id" validate:"required,max=128"`
	DeviceName string `json:"device_name" validate:"omitempty,max=64"`
}

// deviceCodeResponse is the issued pair and everything a client needs to run its own loop.
type deviceCodeResponse struct {
	// DeviceCode is the secret half. Never displayed, never logged.
	DeviceCode string `json:"device_code"`
	// UserCode is the half a person reads and types, in its grouped display form.
	UserCode string `json:"user_code"`
	// VerificationURI is where to send them. Absolute, and on this instance's own origin.
	VerificationURI string `json:"verification_uri"`
	// ExpiresIn and Interval are seconds, the units RFC 8628 uses and the units a client's own timer wants.
	// Sent rather than assumed: a client cannot import DeviceCodeTTL, and hardcoding either would make an
	// instance that tuned them silently unpollable.
	ExpiresIn int `json:"expires_in"`
	Interval  int `json:"interval"`
}

// deviceTokenRequest is one poll.
type deviceTokenRequest struct {
	DeviceCode string `json:"device_code" validate:"required"`
}

// DeviceRoutes mounts the device-code endpoints. The caller supplies the /auth/device prefix, along with
// the rate-limit bucket these two share and the rest of /auth does not.
func (h *Handler) DeviceRoutes(r chi.Router) {
	r.Post("/code", h.deviceCode)
	r.Post("/token", h.deviceToken)
}

// deviceCode issues a device code and the user code that goes with it.
func (h *Handler) deviceCode(w http.ResponseWriter, r *http.Request) {
	var req deviceCodeRequest
	if !h.decode(w, r, &req) {
		return
	}

	// Checked here rather than inside the service, because it is a property of this instance's
	// configuration and not of the request: without a public base URL there is no address to send anybody
	// to, and a flow whose verification_uri is empty fails in the client, minutes later, as a mystery.
	// Same shape as the reset endpoint answering 503 with no relay configured.
	if h.svc.publicBaseURL == "" {
		h.writeErr(w, r, ErrDeviceFlowUnavailable)
		return
	}

	// Field-by-field rather than a struct conversion, for the reason register gives: a conversion compiles
	// only while the wire shape and the service input keep identical fields in identical order, and would
	// start silently mis-assigning the moment either gained one.
	//
	//nolint:staticcheck // S1016: the coupling a conversion introduces is not wanted here
	auth, err := h.svc.StartDeviceAuth(r.Context(), StartDeviceAuthInput{
		DeviceID:   req.DeviceID,
		DeviceName: req.DeviceName,
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, deviceCodeResponse{
		DeviceCode:      auth.DeviceCode,
		UserCode:        auth.UserCode,
		VerificationURI: DeviceVerificationURL(h.svc.publicBaseURL),
		ExpiresIn:       int(auth.ExpiresAt.Sub(h.svc.now()).Seconds()),
		Interval:        int(auth.Interval.Seconds()),
	})
}

// deviceToken is one poll from a waiting client.
//
// Answers a token pair or one member of a four-code vocabulary, and nothing else. The vocabulary is
// RFC 8628 §3.5's, which is worth adopting whole rather than inventing near-misses of: a client author
// who has implemented a device flow once already knows what each of these means.
func (h *Handler) deviceToken(w http.ResponseWriter, r *http.Request) {
	var req deviceTokenRequest
	if !h.decode(w, r, &req) {
		return
	}

	pair, err := h.svc.RedeemDeviceCode(r.Context(), req.DeviceCode, clientAddr(r))
	if err != nil {
		h.writeDeviceErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, newTokenPairResponse(pair))
}

// writeDeviceErr maps the poll's outcomes onto the wire.
//
// Separate from writeErr because these are not failures in the sense that function is about: three of the
// four are ordinary states of a flow that is going fine, and they are answered 400 with a code because
// that is what every device-flow client already parses. Anything else falls through to writeErr, which is
// what turns a pool failure into a 500 with a request ID.
func (h *Handler) writeDeviceErr(w http.ResponseWriter, r *http.Request, err error) {
	var code, message string
	switch {
	case errors.Is(err, ErrDeviceAuthorizationPending):
		code, message = "authorization_pending", "waiting for this code to be approved in a browser"
	case errors.Is(err, ErrDeviceSlowDown):
		code, message = "slow_down", "polling faster than this authorization's interval"
	case errors.Is(err, ErrDeviceAccessDenied):
		code, message = "access_denied", "this device authorization was denied"
	case errors.Is(err, ErrDeviceCodeExpired):
		code, message = "expired_token", "this device authorization has expired; start again"
	default:
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteError(w, r, &httpx.StatusError{
		Status:  http.StatusBadRequest,
		Code:    code,
		Message: message,
		Err:     err,
	})
}

// DeviceVerificationURL is the absolute address a person is sent to.
func DeviceVerificationURL(publicBaseURL string) string {
	return strings.TrimSuffix(publicBaseURL, "/") + devicePagePath
}
