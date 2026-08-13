package auth

import (
	"errors"
	"html/template"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
)

// The server-rendered half of password reset: the page the emailed link lands on.
//
// # Why the backend serves HTML at all
//
// The link has to go somewhere a person can use without the CLI installed — they are, by definition, locked
// out. The web SPA does not exist until Phase O and a self-hosted instance may never deploy one, so a link
// pointing at it would be broken for most instances for most of the project's life. Two small pages here
// cost less than that, and M9's device-code flow needs the same server-rendered-page seam.
//
// # Why a form post rather than fetch()
//
// The page submits a plain form to this same origin, so it needs no JavaScript — which means the CSP can
// forbid scripts outright rather than carrying a nonce for one. It also means the page works with
// JavaScript disabled, in a text browser, and in whatever a locked-out user happens to have to hand.
//
// # CSRF
//
// There is none to defend against. The token *is* the authority: an attacker who can forge this request
// already has the token, and with it could simply submit the form themselves. No ambient credential
// participates, which is the same reason the rest of this API needs no CSRF middleware (rule 4).

// pageStyle is the stylesheet every server-rendered page shares.
//
// One definition rather than one per page: these are the only HTML this backend serves, they are seen by
// someone who is locked out or half-signed-up, and two copies drifting would mean the reset page and the
// OAuth pages slowly stop looking like the same product. It is inlined under a per-request nonce rather
// than served as a file, because a stylesheet at its own URL would need a route, a cache policy, and a
// CSP source — three things to get right for one screenful of CSS.
const pageStyle = `
  body { font-family: system-ui, sans-serif; max-width: 32rem; margin: 4rem auto; padding: 0 1.5rem;
         line-height: 1.5; color: #1a1a1a; background: #fafafa; }
  h1 { font-size: 1.5rem; }
  label { display: block; margin-top: 1.5rem; font-weight: 600; }
  input { width: 100%; padding: 0.6rem; margin-top: 0.4rem; font-size: 1rem;
          border: 1px solid #bbb; border-radius: 4px; box-sizing: border-box; }
  button { margin-top: 1.5rem; padding: 0.6rem 1.2rem; font-size: 1rem; border: 0; border-radius: 4px;
           background: #2b5eaa; color: #fff; cursor: pointer; }
  code.code { display: inline-block; padding: 0.4rem 0.6rem; background: #eee; border-radius: 4px;
              font-size: 1.1rem; word-break: break-all; }
  .note { color: #555; font-size: 0.9rem; }
  .error { color: #a12; font-weight: 600; }
`

// resetPageData is everything the templates render. Deliberately tiny, and deliberately containing no
// user-controlled text: nothing about the account is echoed, so there is nothing on this page to escape.
type resetPageData struct {
	Nonce string
	Token string
	// Error is one of a fixed set of messages this file owns, never a service error or user input.
	Error string
}

// The page is one file with two states so the styling is written once. html/template escapes by
// construction — the token is the only interpolated value, and it lands in an attribute, which is exactly
// the context html/template's contextual escaping exists for.
var resetPageTemplate = template.Must(template.New("reset").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Reset your Norite password</title>
<style nonce="{{ .Nonce }}">
` + pageStyle + `</style>
</head>
<body>
<h1>Choose a new password</h1>
{{ if .Error }}<p class="error">{{ .Error }}</p>{{ end }}
<form method="post" action="/reset">
  <input type="hidden" name="token" value="{{ .Token }}">
  <label for="password">New password</label>
  <input id="password" name="password" type="password" autocomplete="new-password" required
         minlength="12" autofocus>
  <p class="note">At least 12 characters. Length is the only rule — a passphrase is a good choice.</p>
  <button type="submit">Set new password</button>
</form>
<p class="note">Signing in again everywhere is required after this, and any API tokens on the account are
revoked, so bots and scripts will need new ones.</p>
</body>
</html>
`))

var resetDoneTemplate = template.Must(template.New("reset-done").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Password changed</title>
<style nonce="{{ .Nonce }}">
` + pageStyle + `</style>
</head>
<body>
<h1>Your password has been changed</h1>
<p>You can sign in with it now.</p>
<p class="note">Every device has been signed out, and any API tokens on the account were revoked — bots
and scripts will need new ones.</p>
</body>
</html>
`))

// PageRoutes mounts the server-rendered reset pages.
//
// Outside /api/v1 on purpose: this is a page a person opens, not an API a client codegens against, and
// putting it under the versioned API prefix would imply it moves when that version does.
func (h *Handler) PageRoutes(r chi.Router) {
	r.Get("/reset", h.resetPage)
	r.Post("/reset", h.resetPageSubmit)
	// The OAuth signup form's target. At the root beside /reset rather than under /api/v1, for the same
	// reason: it is a form a person submits, not an API a client codegens against.
	r.Post("/oauth/signup", h.oauthSignupSubmit)
}

// resetPage renders the form the emailed link opens.
//
// The token is not verified here, only carried. Checking it on GET would mean reporting its validity
// before anyone submits anything, which turns the page into an oracle for whether a token is live —
// and a link that has already been used would then be distinguishable from one that never existed.
func (h *Handler) resetPage(w http.ResponseWriter, r *http.Request) {
	h.renderReset(w, r, http.StatusOK, resetPageData{
		Nonce: httpx.NonceFrom(r.Context()),
		Token: r.URL.Query().Get("token"),
	})
}

// resetPageSubmit performs the reset and renders the outcome.
func (h *Handler) resetPageSubmit(w http.ResponseWriter, r *http.Request) {
	// 64 KiB: a form with two fields cannot legitimately be larger, and the body is read into memory.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		h.renderReset(w, r, http.StatusBadRequest, resetPageData{
			Nonce: httpx.NonceFrom(r.Context()),
			Error: "That form could not be read. Please try the link from your email again.",
		})
		return
	}

	token := r.PostFormValue("token")
	err := h.svc.ConfirmPasswordReset(r.Context(), token, r.PostFormValue("password"))
	if err == nil {
		h.render(w, r, resetDoneTemplate, http.StatusOK, resetPageData{Nonce: httpx.NonceFrom(r.Context())})
		return
	}

	// One message for every way a token can be bad, matching the JSON endpoint: distinguishing "expired"
	// from "already used" from "never existed" tells whoever is holding a stolen link which of those it is.
	message := "That reset link is no longer valid. Request a new one and use the most recent email."
	switch {
	case errors.Is(err, ErrPasswordTooShort), errors.Is(err, ErrPasswordTooLong):
		message = err.Error()
	case !errors.Is(err, ErrInvalidResetToken):
		logging.FromContext(r.Context()).Error().Err(err).Msg("password reset page failed")
		message = "Something went wrong on our end. Please try again."
	}

	// The token is echoed back into the form so a rejected password can be retried without reopening the
	// email — it is the value that arrived, and html/template escapes it into the attribute.
	h.renderReset(w, r, http.StatusBadRequest, resetPageData{
		Nonce: httpx.NonceFrom(r.Context()),
		Token: token,
		Error: message,
	})
}

func (h *Handler) renderReset(w http.ResponseWriter, r *http.Request, status int, data resetPageData) {
	h.render(w, r, resetPageTemplate, status, data)
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, tmpl *template.Template, status int, data resetPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.Execute(w, data); err != nil {
		// The header is already written, so this cannot become an error response — logged and left for
		// the client to notice as a truncated page, exactly as httpx.WriteJSON does.
		logging.FromContext(r.Context()).Error().Err(err).Msg("rendering the reset page failed")
	}
}
