package auth

import (
	"errors"
	"html/template"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
)

// The page a verification link lands on.
//
// Follows resetpage.go exactly — same style, same CSP override, same no-JavaScript form post — and the
// reasoning there applies unchanged: the link has to go somewhere a person can use without the CLI, the
// web SPA does not exist until Phase O, and a self-hosted instance may never deploy one.

// verifyPageData is everything the templates render. Deliberately carries no user-controlled text: nothing
// about the account is echoed, so there is nothing on this page to escape.
type verifyPageData struct {
	Nonce string
	Token string
	Error string
}

// The confirm form is a POST rather than the link itself doing the work, and that is not ceremony.
//
// A GET that verified on sight would be triggered by anything that follows links in mail — scanners in
// corporate gateways, link previewers in chat clients, some antivirus products — so an address could be
// marked verified by software the person never saw, and the link would be spent before they clicked. It is
// also rule 4: a GET must stay side-effect-free.
var verifyPageTemplate = template.Must(template.New("verify").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Confirm your Norite address</title>
<style nonce="{{ .Nonce }}">
` + pageStyle + `</style>
</head>
<body>
<h1>Confirm your address</h1>
{{ if .Error }}<p class="error">{{ .Error }}</p>{{ end }}
<p>Confirming finishes creating your account and lets you sign in.</p>
<form method="post" action="/verify">
  <input type="hidden" name="token" value="{{ .Token }}">
  <button type="submit">Confirm this address</button>
</form>
<p class="note">If you did not create a Norite account, close this page — the account stays unusable and
nobody can sign in to it.</p>
</body>
</html>
`))

var verifyDoneTemplate = template.Must(template.New("verify-done").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Address confirmed</title>
<style nonce="{{ .Nonce }}">
` + pageStyle + `</style>
</head>
<body>
<h1>Your address is confirmed</h1>
<p>You can sign in now.</p>
<p class="note">In a terminal, that is <code class="code">norite login</code>.</p>
</body>
</html>
`))

// VerifyPageRoutes mounts the server-rendered verification pages.
//
// At the root beside /reset, outside /api/v1, for the reason PageRoutes gives: a person opens these from an
// email, they are not an API a client codegens against, and putting them under the versioned prefix would
// imply they move when that version does.
func (h *Handler) VerifyPageRoutes(r chi.Router) {
	r.Get("/verify", h.verifyPage)
	r.Post("/verify", h.verifyPageSubmit)
}

// verifyPage renders the confirm button.
//
// The token is carried, not checked — the same decision resetPage documents. Checking it here would report
// its validity before anyone submits anything, turning the page into an oracle for whether a link is live,
// and would make an already-followed link distinguishable from one that never existed.
func (h *Handler) verifyPage(w http.ResponseWriter, r *http.Request) {
	h.renderVerify(w, r, verifyPageTemplate, http.StatusOK, verifyPageData{
		Nonce: httpx.NonceFrom(r.Context()),
		Token: r.URL.Query().Get("token"),
	})
}

// verifyPageSubmit performs the verification and renders the outcome.
func (h *Handler) verifyPageSubmit(w http.ResponseWriter, r *http.Request) {
	// 64 KiB: a form with one field cannot legitimately be larger, and the body is read into memory.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		h.renderVerify(w, r, verifyPageTemplate, http.StatusBadRequest, verifyPageData{
			Nonce: httpx.NonceFrom(r.Context()),
			Error: "That form could not be read. Please try the link from your email again.",
		})
		return
	}

	token := r.PostFormValue("token")
	err := h.svc.ConfirmEmailVerification(r.Context(), token)
	if err == nil {
		h.renderVerify(w, r, verifyDoneTemplate, http.StatusOK, verifyPageData{
			Nonce: httpx.NonceFrom(r.Context()),
		})
		return
	}

	// One message for every way a link can fail — unknown, expired, already used, or issued to an address
	// the account no longer has. Distinguishing them tells whoever holds a stolen link which it is.
	message := "That confirmation link is no longer valid. Sign in to have a new one sent, or register again."
	if !errors.Is(err, ErrInvalidVerificationToken) {
		logging.FromContext(r.Context()).Error().Err(err).Msg("email verification page failed")
		message = "Something went wrong on our end. Please try again."
	}

	h.renderVerify(w, r, verifyPageTemplate, http.StatusBadRequest, verifyPageData{
		Nonce: httpx.NonceFrom(r.Context()),
		Token: token,
		Error: message,
	})
}

func (h *Handler) renderVerify(w http.ResponseWriter, r *http.Request, tmpl *template.Template,
	status int, data verifyPageData,
) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.Execute(w, data); err != nil {
		// The header is already written, so this cannot become an error response — logged and left for the
		// client to notice as a truncated page, exactly as httpx.WriteJSON does.
		logging.FromContext(r.Context()).Error().Err(err).Msg("rendering the verification page failed")
	}
}
