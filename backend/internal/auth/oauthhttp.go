package auth

import (
	"errors"
	"html/template"
	"net/http"

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
	Username    string `json:"username" validate:"required,min=2,max=64"`
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
	url, err := h.svc.StartOAuth(r.Context(), provider, r.URL.Query().Get("flow_challenge"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	// 302 rather than 307: this is a GET with no body to preserve, and 302 is what every provider's
	// documentation and every browser expects here.
	http.Redirect(w, r, url, http.StatusFound)
}

// oauthCallback is where the provider sends the user back.
func (h *Handler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	nonce := httpx.NonceFrom(r.Context())
	provider := chi.URLParam(r, "provider")

	// A provider that reports its own failure — a user who pressed "cancel" most often — sends error
	// rather than code. Treated as an ordinary abandonment, not a fault.
	if reason := r.URL.Query().Get("error"); reason != "" {
		logging.FromContext(r.Context()).Debug().
			Str("provider", provider).
			Msg("oauth callback reported an error from the provider")
		h.renderOAuthError(w, r, nonce,
			"The sign-in was not completed. You can close this page and try again.")
		return
	}

	outcome, err := h.svc.CompleteOAuth(r.Context(), provider,
		r.URL.Query().Get("state"), r.URL.Query().Get("code"))
	if err != nil {
		h.renderOAuthFailure(w, r, nonce, err)
		return
	}

	if outcome.SignedIn() {
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

	code, err := h.svc.CompleteOAuthSignup(r.Context(), token, username)
	if err == nil {
		h.renderPage(w, r, oauthDoneTemplate, http.StatusOK, oauthPageData{Nonce: nonce, Code: code})
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
	if identity, _, err := h.svc.parseOAuthSignupToken(token); err == nil {
		email = identity.Email
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

	code, err := h.svc.CompleteOAuthSignup(r.Context(), req.SignupToken, req.Username)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	// An exchange code rather than a token pair, so both completion paths end the same way and a client
	// has one place to turn a code into a session.
	httpx.WriteJSON(w, r, http.StatusOK, oauthExchangeCodeResponse{Code: code})
}

// renderOAuthFailure maps a service error onto a page.
func (h *Handler) renderOAuthFailure(w http.ResponseWriter, r *http.Request, nonce string, err error) {
	switch {
	case errors.Is(err, ErrOAuthLinkRequired):
		// The one failure worth explaining at length: the person owns both accounts and needs to be told
		// what to do, or they will keep pressing a button that never works.
		h.renderOAuthError(w, r, nonce, ErrOAuthLinkRequired.Error())
	case errors.Is(err, ErrOAuthEmailUnverified):
		// Worth explaining for the same reason as ErrOAuthLinkRequired: the person can fix this, and a
		// generic failure would leave them pressing a button that never works.
		h.renderOAuthError(w, r, nonce, ErrOAuthEmailUnverified.Error())
	case errors.Is(err, ErrOAuthIdentityLinkedElsewhere), errors.Is(err, ErrOAuthAccountAlreadyLinked):
		h.renderOAuthError(w, r, nonce, err.Error())
	case errors.Is(err, ErrOAuthRegistrationClosed):
		h.renderOAuthError(w, r, nonce, "This instance requires an invite code to create an account.")
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
