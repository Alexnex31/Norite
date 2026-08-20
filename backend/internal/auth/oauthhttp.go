package auth

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
)

// The OAuth HTTP surface: two redirects a browser follows, two JSON endpoints a client calls, and one
// form the person fills in when they are new here.
//
// # Why the callback renders HTML rather than redirecting
//
// It has an exchange code to hand over, and a redirect would put it in a URL — in history, in a Referer,
// in every proxy log on the way. So the callback ends the browser's journey on a page, and the code
// crosses to the client from there. That is the same reason the code exists at all.

// ---------- request payloads ----------

type oauthExchangeRequest struct {
	Code string `json:"code" validate:"required"`
	// FlowVerifier is the secret whose hash this flow was started with. Required: it is what makes the code
	// redeemable only by the client that began the sign-in (see GenerateOAuthFlowVerifier).
	FlowVerifier string `json:"flow_verifier" validate:"required"`
	// DeviceID scopes the refresh-token family, exactly as it does for a password login: this is the point
	// where a client that has one finally says what it is (ADR 0011).
	DeviceID   string `json:"device_id" validate:"required,max=128"`
	DeviceName string `json:"device_name" validate:"omitempty,max=64"`
}

type oauthCompleteRequest struct {
	SignupToken string `json:"signup_token" validate:"required"`
	Username    string `json:"username" validate:"required,min=2,max=32"`
}

type oauthExchangeCodeResponse struct {
	// Code is the one-time value traded at /auth/oauth/exchange. Not a credential for anything else, and
	// worthless a second time.
	Code string `json:"code"`
}

// ---------- routes ----------

// OAuthRoutes mounts the provider endpoints inside the versioned API.
//
// The callback carries httpx.HTMLPage because it answers a browser: the JSON API's CSP forbids a page from
// doing anything at all, including submitting the form the signup case renders.
func (h *Handler) OAuthRoutes(r chi.Router) {
	r.Get("/oauth/{provider}/authorize", h.oauthAuthorize)
	r.With(httpx.HTMLPage).Get("/oauth/{provider}/callback", h.oauthCallback)
	r.Post("/oauth/exchange", h.oauthExchange)
	r.Post("/oauth/complete", h.oauthComplete)
}

// oauthAuthorize sends the user to the provider.
func (h *Handler) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if !ValidOAuthProvider(provider) {
		// Rejected before the service sees it: the value reaches a database column and an error message,
		// and "whatever the client sent" is not a provider name.
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrNotFound, "no such sign-in provider"))
		return
	}

	// The binding the client keeps: it publishes the hash here and presents the secret at /exchange, so
	// the code this flow produces is redeemable by this client and by nobody who merely opens the link.
	authURL, err := h.svc.StartOAuth(r.Context(), StartOAuthInput{
		Provider:      provider,
		FlowChallenge: r.URL.Query().Get("flow_challenge"),
		// Where a command-line client asked to be returned to. Absent for a browser, which is the only
		// caller that has somewhere to render.
		ClientRedirectURI: r.URL.Query().Get("client_redirect_uri"),
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	// 302 rather than 307: this is a GET with no body to preserve, and 302 is what every provider's
	// documentation and every browser expects here.
	http.Redirect(w, r, authURL, http.StatusFound)
}

// oauthCallback is where the provider sends the user back.
func (h *Handler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	nonce := httpx.NonceFrom(r.Context())
	provider := chi.URLParam(r, "provider")

	outcome, err := h.svc.CompleteOAuth(r.Context(), OAuthCallbackInput{
		Provider: provider,
		State:    r.URL.Query().Get("state"),
		Code:     r.URL.Query().Get("code"),
		// A provider that reports its own failure — someone who pressed "cancel", most often — sends this
		// instead of a code. The service decides what it means, because only the service has consumed the
		// state row and therefore knows whether a client is waiting to be told.
		ProviderError: r.URL.Query().Get("error"),
	})
	if err != nil {
		h.renderOAuthFailure(w, r, nonce, err)
		return
	}

	if outcome.SignedIn() {
		// A client waiting on a loopback listener gets the code delivered rather than displayed. Note what
		// travels: a single-use code with a two-minute life that is worthless without the flow verifier,
		// over a hop that never leaves the machine — not the token pair ADR 0024 refused to put in a URL.
		//
		// The destination came out of the consumed state row, not out of this request. Nothing here reads
		// client_redirect_uri from r.URL, and a test asserts that a callback carrying one is ignored.
		if outcome.ClientRedirectURI != "" {
			http.Redirect(w, r,
				oauthReturnURL(outcome.ClientRedirectURI, url.Values{"code": {outcome.ExchangeCode}}),
				http.StatusFound)
			return
		}
		h.renderPage(w, r, oauthDoneTemplate, http.StatusOK, oauthPageData{
			Nonce: nonce,
			Code:  outcome.ExchangeCode,
		})
		return
	}

	h.renderPage(w, r, oauthSignupTemplate, http.StatusOK, oauthPageData{
		Nonce:       nonce,
		SignupToken: outcome.SignupToken,
		Username:    outcome.SuggestedUsername,
		Email:       outcome.Email,
	})
}

// oauthSignupSubmit completes a signup from the rendered form.
func (h *Handler) oauthSignupSubmit(w http.ResponseWriter, r *http.Request) {
	nonce := httpx.NonceFrom(r.Context())

	// 64 KiB: a form with two fields cannot legitimately be larger, and the body is read into memory.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		h.renderOAuthError(w, r, nonce, "That form could not be read. Please start the sign-in again.")
		return
	}

	token := r.PostFormValue("signup_token")
	username := r.PostFormValue("username")

	result, err := h.svc.CompleteOAuthSignup(r.Context(), token, username)
	if err == nil {
		// Where the code goes was decided when the flow started and traveled here inside the signed
		// continuation token, not in this form. That distinction is the point: this request body is
		// written by whoever is looking at the page, and a hidden redirect field would let them choose
		// where somebody else's exchange code is delivered.
		if result.ClientRedirectURI != "" {
			http.Redirect(w, r,
				oauthReturnURL(result.ClientRedirectURI, url.Values{"code": {result.ExchangeCode}}),
				http.StatusFound)
			return
		}
		h.renderPage(w, r, oauthDoneTemplate, http.StatusOK,
			oauthPageData{Nonce: nonce, Code: result.ExchangeCode})
		return
	}

	// A failure that ends the flow, on a sign-up that named a listener, is reported there — otherwise the
	// browser shows a page and the client waits out its whole timeout for a decision already made. The
	// username errors below are deliberately not routed here: the form re-renders and the listener is
	// still waiting for the eventual success.
	var callback *OAuthCallbackError
	if errors.As(err, &callback) && callback.ClientRedirectURI != "" {
		h.reportOAuthFailureToListener(w, r, callback)
		return
	}

	// A rejected username is worth re-rendering the form for: the person can fix it without starting the
	// whole flow again. Anything else means the continuation token is gone, and there is nothing to
	// re-render with.
	var message string
	switch {
	case errors.Is(err, ErrInvalidUsername), errors.Is(err, ErrUsernameTaken):
		message = err.Error()
	case errors.Is(err, ErrOAuthSignupToken):
		h.renderOAuthError(w, r, nonce, "This sign-up has expired. Please start again.")
		return
	case errors.Is(err, ErrOAuthRegistrationClosed):
		h.renderOAuthError(w, r, nonce, "This instance requires an invite code to create an account.")
		return
	case errors.Is(err, ErrEmailTaken):
		// Reachable, and it was landing in the default branch: an account deleted since the callback — or
		// deleted long ago, since a soft-deleted row keeps its address — leaves the address claimed while
		// being invisible to every lookup that would have refused the flow earlier. The person reached
		// "choose your username" legitimately and cannot get past it, so they are told what actually
		// happened rather than "something went wrong on our end", which was both untrue and unactionable.
		//
		// Discloses no more than registration, which answers 409 on a taken address by design.
		h.renderOAuthError(w, r, nonce,
			"An account already uses that email address. Sign in with your password instead.")
		return
	default:
		logging.FromContext(r.Context()).Error().Err(err).Msg("oauth signup page failed")
		h.renderOAuthError(w, r, nonce, "Something went wrong on our end. Please try again.")
		return
	}

	// The address is re-derived from the token rather than carried in a hidden field. It is only displayed,
	// but a form value is whatever the client posted, and a page of ours captioning attacker-chosen text
	// with "Creating an account for…" is a phishing surface handed over for free. The token parses here by
	// construction — the switch above sent every other outcome to an error page.
	var email string
	if continuation, err := h.svc.parseOAuthSignupToken(token); err == nil {
		email = continuation.Identity.Email
	}

	h.renderPage(w, r, oauthSignupTemplate, http.StatusBadRequest, oauthPageData{
		Nonce:       nonce,
		SignupToken: token,
		Username:    username,
		Email:       email,
		Error:       message,
	})
}

// oauthExchange trades the one-time code for a token pair.
func (h *Handler) oauthExchange(w http.ResponseWriter, r *http.Request) {
	var req oauthExchangeRequest
	if !h.decode(w, r, &req) {
		return
	}

	pair, err := h.svc.ExchangeOAuthCode(r.Context(), req.Code, req.FlowVerifier, LoginInput{
		DeviceID:   req.DeviceID,
		DeviceName: req.DeviceName,
		IP:         clientAddr(r),
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, newTokenPairResponse(pair))
}

// oauthComplete is the JSON equivalent of the signup form, for a client driving the flow itself.
func (h *Handler) oauthComplete(w http.ResponseWriter, r *http.Request) {
	var req oauthCompleteRequest
	if !h.decode(w, r, &req) {
		return
	}

	result, err := h.svc.CompleteOAuthSignup(r.Context(), req.SignupToken, req.Username)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	// An exchange code rather than a token pair, so both completion paths end the same way and a client
	// has one place to turn a code into a session.
	//
	// result.ClientRedirectURI is deliberately ignored. A client driving the flow through this endpoint
	// rather than the form is already holding the answer in its own process — it has no browser to send
	// anywhere, and handing it back a URL to visit would be answering a question it did not ask.
	httpx.WriteJSON(w, r, http.StatusOK, oauthExchangeCodeResponse{Code: result.ExchangeCode})
}

// renderOAuthFailure reports a callback failure — to the client's listener when the flow named one, and
// on a page otherwise.
func (h *Handler) renderOAuthFailure(w http.ResponseWriter, r *http.Request, nonce string, err error) {
	// A failure that happened after the state was consumed knows where the client is waiting. Failures
	// before it — an unknown, expired or replayed state — genuinely do not, and rendering is the right
	// answer there rather than a gap: the alternative is trusting a destination supplied on the callback's
	// own URL, which is the one thing this design refuses to do.
	var callback *OAuthCallbackError
	if errors.As(err, &callback) && callback.ClientRedirectURI != "" {
		h.reportOAuthFailureToListener(w, r, callback)
		return
	}

	switch {
	case errors.Is(err, ErrOAuthEmailUnverified):
		// The one failure worth explaining at length: the person can act on it, and a generic message would
		// leave them pressing a button that never works. Identical whether or not an account owns the
		// address — see the sentinel's own comment.
		h.renderOAuthError(w, r, nonce, ErrOAuthEmailUnverified.Error())
	case errors.Is(err, ErrOAuthIdentityLinkedElsewhere), errors.Is(err, ErrOAuthAccountAlreadyLinked):
		h.renderOAuthError(w, r, nonce, err.Error())
	case errors.Is(err, ErrOAuthRegistrationClosed):
		h.renderOAuthError(w, r, nonce, "This instance requires an invite code to create an account.")
	case errors.Is(err, ErrOAuthProviderDeclined):
		h.renderOAuthError(w, r, nonce,
			"The sign-in was not completed. You can close this page and try again.")
	case errors.Is(err, ErrOAuthState):
		h.renderOAuthError(w, r, nonce, "This sign-in link is no longer valid. Please start again.")
	case errors.Is(err, ErrUnknownProvider):
		h.renderOAuthError(w, r, nonce, "This instance does not offer that sign-in provider.")
	case errors.Is(err, ErrOAuthNoEmail):
		h.renderOAuthError(w, r, nonce,
			"That provider did not share an email address, which this instance needs to create an account.")
	default:
		logging.FromContext(r.Context()).Error().Err(err).Msg("oauth callback failed")
		h.renderOAuthError(w, r, nonce, "Something went wrong on our end. Please try again.")
	}
}

// reportOAuthFailureToListener sends a failure code to the client's loopback listener.
func (h *Handler) reportOAuthFailureToListener(w http.ResponseWriter, r *http.Request,
	callback *OAuthCallbackError,
) {
	log := logging.FromContext(r.Context())
	if callback.Code == oauthErrServer {
		// The one failure the vocabulary cannot describe, so it has to be recorded here. Redirecting skips
		// renderOAuthFailure's switch, whose default case is otherwise the only place an unclassified
		// failure is logged at all — a pool exhausted mid-flow, or a failed token exchange, would answer
		// the client and leave nothing behind at the default level. Once M8 ships this is the common login
		// path, which is exactly the wrong thing to make unobservable.
		log.Error().Err(callback.Err).Msg("oauth flow failed; reporting server_error to a listener")
	} else {
		log.Debug().Str("oauth_error", callback.Code).
			Msg("returning an oauth failure to a client's listener")
	}
	http.Redirect(w, r,
		oauthReturnURL(callback.ClientRedirectURI, url.Values{"error": {callback.Code}}),
		http.StatusFound)
}

func (h *Handler) renderOAuthError(w http.ResponseWriter, r *http.Request, nonce, message string) {
	h.renderPage(w, r, oauthErrorTemplate, http.StatusBadRequest, oauthPageData{Nonce: nonce, Error: message})
}

// ---------- pages ----------

// oauthPageData is everything the OAuth templates render.
//
// Every string here is either a fixed message this file owns or a value html/template escapes into an
// attribute. Nothing a provider supplied is rendered as markup (CLAUDE.md rule 9).
type oauthPageData struct {
	Nonce       string
	SignupToken string
	Username    string
	Email       string
	Code        string
	Error       string
}

func (h *Handler) renderPage(w http.ResponseWriter, r *http.Request, tmpl *template.Template,
	status int, data oauthPageData,
) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.Execute(w, data); err != nil {
		// The header is already out, so this cannot become an error response — logged and left for the
		// client to notice as a truncated page, exactly as httpx.WriteJSON does.
		logging.FromContext(r.Context()).Error().Err(err).Msg("rendering an oauth page failed")
	}
}

var oauthSignupTemplate = template.Must(template.New("oauth-signup").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Choose your username</title>
<style nonce="{{ .Nonce }}">` + pageStyle + `</style>
</head>
<body>
<h1>Choose your username</h1>
{{ if .Email }}<p class="note">Creating an account for {{ .Email }}.</p>{{ end }}
{{ if .Error }}<p class="error">{{ .Error }}</p>{{ end }}
<form method="post" action="/oauth/signup">
  <input type="hidden" name="signup_token" value="{{ .SignupToken }}">
  <label for="username">Username</label>
  <input id="username" name="username" type="text" value="{{ .Username }}" required
         minlength="2" maxlength="32" autofocus>
  <p class="note">Letters and digits in any script, plus <code>_</code>, <code>.</code> and
  <code>-</code>. This is how other people will find and mention you.</p>
  <button type="submit">Create account</button>
</form>
</body>
</html>
`))

var oauthDoneTemplate = template.Must(template.New("oauth-done").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Signed in</title>
<style nonce="{{ .Nonce }}">` + pageStyle + `</style>
</head>
<body>
<h1>You're signed in</h1>
<p>Return to Norite to finish. If it asks for a sign-in code, this is it:</p>
<p><code class="code">{{ .Code }}</code></p>
<p class="note">The code works once and expires in a couple of minutes.</p>
</body>
</html>
`))

var oauthErrorTemplate = template.Must(template.New("oauth-error").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign-in did not complete</title>
<style nonce="{{ .Nonce }}">` + pageStyle + `</style>
</head>
<body>
<h1>Sign-in did not complete</h1>
<p class="error">{{ .Error }}</p>
</body>
</html>
`))
