package auth

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
)

// The verification page: what a person opens on a second device to finish a device-code sign-in.
//
// Three steps and four routes, which is one more than the obvious design has and the reason is the
// attack. Enter the code, prove who you are, then *approve*. The last step is separate and explicit
// because the device-code grant's live risk is not somebody guessing a code — it is somebody being sent
// one and authorizing a stranger's machine without ever understanding that is what they did (RFC 8628
// §5.4, and a pattern with real-world campaigns behind it).
//
// Nothing cryptographic prevents that. What can be done is done here: the approval page names the device
// asking, echoes the code so it can be compared against what the terminal is actually showing, and says
// in plain words that somebody who did not start this should press Deny. There is deliberately no URL
// anywhere in this flow that carries the code and could therefore simply be clicked.
//
// # State between the steps
//
// Signed continuations, because there are no sessions here to use — see devicetoken.go.
//
// # CSRF
//
// None to defend against, the same as the reset page: no ambient credential participates in any of these
// requests. The authority at each step is a token in the form body, and an attacker able to forge the
// request already holds it.

// The messages these pages show. Owned here rather than taken from an error sentinel: those are lowercase
// by Go convention, which reads wrong as a sentence, and a service error is not written for somebody
// standing in a hallway with a phone.
//
// One message covers every reason a code cannot be acted on — never issued, expired, already approved,
// denied, spent. The differences are only useful to somebody working through codes, and the person who
// mistyped theirs is told the same actionable thing either way.
const (
	deviceUnknownCodeMessage = "That code is not valid. Check it and try again, or start again " +
		"on the device that showed it to you."
	deviceExpiredStepMessage = "That sign-in is no longer valid. Start again on the device that " +
		"showed you a code."
	deviceBadFormMessage = "That form could not be read. Please start again."
	deviceBadPasswordMsg = "That email address and password do not match."
)

// devicePageData is everything the templates render.
type devicePageData struct {
	Nonce string
	// Token is the continuation for the *next* step, never the one that got here.
	Token string
	// UserCode is shown back on the approval page for comparison with the terminal.
	UserCode string
	// DeviceName is the one attacker-influenceable value on these pages. Bounded at issuance and escaped
	// by html/template; the approval page presents it as a quoted claim rather than as its own words.
	DeviceName string
	// Error is one of a fixed set of messages this file owns, never a service error and never user input.
	Error string
}

// DevicePageRoutes mounts the verification pages, at the root beside /reset.
func (h *Handler) DevicePageRoutes(r chi.Router) {
	r.Get(devicePagePath, h.devicePage)
	r.Post(devicePagePath, h.devicePageSubmit)
	r.Post(devicePagePath+"/signin", h.devicePageSignIn)
	r.Post(devicePagePath+"/approve", h.devicePageApprove)
}

// devicePage renders the code-entry form.
//
// A `code` query parameter prefills the field and does nothing else — no lookup, no state, no step
// skipped. That is the whole distance between a convenience and a verification_uri_complete, which turns
// a phished link into one click; here a link can at most save somebody eight keystrokes they still have
// to look at.
func (h *Handler) devicePage(w http.ResponseWriter, r *http.Request) {
	h.renderDevice(w, r, deviceEntryTemplate, http.StatusOK, devicePageData{
		Nonce: httpx.NonceFrom(r.Context()),
		// Normalized rather than echoed: whatever arrives is refused unless it is a well-formed code, so
		// the value that reaches the template is one of a known alphabet or nothing at all.
		UserCode: prefillUserCode(r.URL.Query().Get("code")),
	})
}

// devicePageSubmit looks up an entered code and offers the ways to sign in.
func (h *Handler) devicePageSubmit(w http.ResponseWriter, r *http.Request) {
	form, ok := h.deviceForm(w, r)
	if !ok {
		return
	}

	code, row, err := h.svc.LookUpDeviceCode(r.Context(), form.Get("user_code"))
	if err != nil {
		// One message for never-issued, expired, already-approved, denied and spent. The differences are
		// only useful to somebody trying codes, and the person who mistyped theirs is told the same
		// actionable thing either way.
		h.renderDevice(w, r, deviceEntryTemplate, http.StatusBadRequest, devicePageData{
			Nonce:    httpx.NonceFrom(r.Context()),
			UserCode: prefillUserCode(form.Get("user_code")),
			Error:    deviceUnknownCodeMessage,
		})
		return
	}

	h.renderDeviceSignIn(w, r, http.StatusOK, row, code, "")
}

// devicePageSignIn is the password branch: verify the credentials, then ask for approval.
func (h *Handler) devicePageSignIn(w http.ResponseWriter, r *http.Request) {
	form, ok := h.deviceForm(w, r)
	if !ok {
		return
	}

	entry, err := h.svc.parseDeviceToken(form.Get("device_token"), deviceEntryTokenType)
	if err != nil {
		h.renderDeviceExpired(w, r)
		return
	}

	user, err := h.svc.verifyCredentials(r.Context(), form.Get("email"), form.Get("password"))
	if err != nil {
		if !isAuthFailure(err) {
			logging.FromContext(r.Context()).Error().Err(err).Msg("device page sign-in failed")
		}
		// The row is re-read rather than carried, so the form comes back with the device name and the
		// providers it had — and so a code that expired or was denied while somebody typed is caught here
		// rather than at the approval step.
		row, lookupErr := h.svc.deviceCodeByID(r.Context(), entry.DeviceCodeID)
		if lookupErr != nil {
			h.renderDeviceExpired(w, r)
			return
		}
		// The same message a JSON login gets, and for the same reason: "that account exists but signs in
		// with Google" turns a form into an account-discovery tool.
		h.renderDeviceSignIn(w, r, http.StatusUnauthorized, row, entry.UserCode, deviceBadPasswordMsg)
		return
	}

	h.renderDeviceApproval(w, r, entry.DeviceCodeID, entry.UserCode, user.ID)
}

// devicePageApprove is the last step, and the one this page exists to make deliberate.
func (h *Handler) devicePageApprove(w http.ResponseWriter, r *http.Request) {
	form, ok := h.deviceForm(w, r)
	if !ok {
		return
	}

	// An approval token specifically. An entry token presented here would be a browser that knows a code
	// authorizing an account it never proved anything about, which is why parseDeviceToken takes the type
	// it wants rather than reporting the type it found.
	approval, err := h.svc.parseDeviceToken(form.Get("device_token"), deviceApprovalTokenType)
	if err != nil {
		h.renderDeviceExpired(w, r)
		return
	}

	if form.Get("decision") != "approve" {
		if err := h.svc.DenyDeviceAuthorization(r.Context(), approval.DeviceCodeID); err != nil {
			logging.FromContext(r.Context()).Error().Err(err).Msg("denying a device authorization failed")
		}
		h.renderDevice(w, r, deviceDeniedTemplate, http.StatusOK,
			devicePageData{Nonce: httpx.NonceFrom(r.Context())})
		return
	}

	if err := h.svc.ApproveDeviceAuthorization(r.Context(), approval.DeviceCodeID, approval.UserID); err != nil {
		if !errors.Is(err, ErrDeviceUserCode) {
			logging.FromContext(r.Context()).Error().Err(err).Msg("approving a device authorization failed")
		}
		h.renderDeviceExpired(w, r)
		return
	}

	h.renderDevice(w, r, deviceApprovedTemplate, http.StatusOK,
		devicePageData{Nonce: httpx.NonceFrom(r.Context())})
}

// deviceForm reads a posted form, or renders the failure and reports that it did.
func (h *Handler) deviceForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	// 64 KiB: none of these forms has more than four fields, and the body is read into memory.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		h.renderDevice(w, r, deviceEntryTemplate, http.StatusBadRequest, devicePageData{
			Nonce: httpx.NonceFrom(r.Context()),
			Error: deviceBadFormMessage,
		})
		return nil, false
	}
	return r.PostForm, true
}

// renderDeviceSignIn draws the second step for a live authorization.
func (h *Handler) renderDeviceSignIn(w http.ResponseWriter, r *http.Request, status int,
	row db.DeviceCode, userCode, message string,
) {
	token, err := h.svc.issueDeviceEntryToken(row.ID, userCode)
	if err != nil {
		logging.FromContext(r.Context()).Error().Err(err).Msg("issuing a device continuation failed")
		h.renderDeviceExpired(w, r)
		return
	}

	h.renderDevice(w, r, deviceSignInTemplate, status, devicePageData{
		Nonce:      httpx.NonceFrom(r.Context()),
		Token:      token,
		DeviceName: row.DeviceName,
		Error:      message,
	})
}

// renderDeviceApproval draws the last step.
func (h *Handler) renderDeviceApproval(w http.ResponseWriter, r *http.Request,
	deviceCodeID int64, userCode string, userID int64,
) {
	row, err := h.svc.deviceCodeByID(r.Context(), deviceCodeID)
	if err != nil {
		h.renderDeviceExpired(w, r)
		return
	}

	token, err := h.svc.issueDeviceApprovalToken(deviceCodeID, userCode, userID)
	if err != nil {
		logging.FromContext(r.Context()).Error().Err(err).Msg("issuing a device approval failed")
		h.renderDeviceExpired(w, r)
		return
	}

	h.renderDevice(w, r, deviceApproveTemplate, http.StatusOK, devicePageData{
		Nonce:      httpx.NonceFrom(r.Context()),
		Token:      token,
		UserCode:   FormatUserCode(userCode),
		DeviceName: row.DeviceName,
	})
}

func (h *Handler) renderDeviceExpired(w http.ResponseWriter, r *http.Request) {
	h.renderDevice(w, r, deviceEntryTemplate, http.StatusBadRequest, devicePageData{
		Nonce: httpx.NonceFrom(r.Context()),
		Error: deviceExpiredStepMessage,
	})
}

func (h *Handler) renderDevice(w http.ResponseWriter, r *http.Request, tmpl *template.Template,
	status int, data devicePageData,
) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.Execute(w, data); err != nil {
		// The header is already written, so this cannot become an error response — logged and left for the
		// client to notice as a truncated page, exactly as the reset page does.
		logging.FromContext(r.Context()).Error().Err(err).Msg("rendering the device page failed")
	}
}

// prefillUserCode returns a code fit to put back in the form, or nothing.
//
// A value that will not parse is dropped entirely rather than escaped and echoed. It is going into an
// input's value attribute, where html/template would make it safe — but a page that shows somebody's
// arbitrary string back to them is a phishing surface for free, and there is nothing useful to prefill
// with when the answer was never a code.
func prefillUserCode(raw string) string {
	code, err := ParseUserCode(raw)
	if err != nil {
		return ""
	}
	return FormatUserCode(code)
}

// The four states of the verification page, each a whole document so the styling is written once and
// html/template's contextual escaping applies to every interpolated value in the context it lands in.
//
// Nothing here loads a subresource and nothing here runs a script, which is what lets the CSP stay at
// `default-src 'none'` with a nonce for the one inline stylesheet.

var deviceEntryTemplate = template.Must(template.New("device-entry").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in to a device</title>
<style nonce="{{ .Nonce }}">
` + pageStyle + `</style>
</head>
<body>
<h1>Sign in to a device</h1>
{{ if .Error }}<p class="error">{{ .Error }}</p>{{ end }}
<p>Enter the code shown on the device you are signing in.</p>
<form method="post" action="/device">
  <label for="user_code">Code</label>
  <input id="user_code" name="user_code" type="text" value="{{ .UserCode }}" autocomplete="off"
         autocapitalize="characters" spellcheck="false" required autofocus>
  <button type="submit">Continue</button>
</form>
<p class="note">If you did not start a sign-in on another device, close this page. Nobody should ever send
you a code to enter here.</p>
</body>
</html>
`))

var deviceSignInTemplate = template.Must(template.New("device-signin").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in to a device</title>
<style nonce="{{ .Nonce }}">
` + pageStyle + `</style>
</head>
<body>
<h1>Sign in</h1>
{{ if .Error }}<p class="error">{{ .Error }}</p>{{ end }}
<p>To finish signing in on <strong>{{ .DeviceName }}</strong>, sign in to your account here.</p>
<form method="post" action="/device/signin">
  <input type="hidden" name="device_token" value="{{ .Token }}">
  <label for="email">Email</label>
  <input id="email" name="email" type="email" autocomplete="username" required autofocus>
  <label for="password">Password</label>
  <input id="password" name="password" type="password" autocomplete="current-password" required>
  <button type="submit">Sign in</button>
</form>
</body>
</html>
`))

var deviceApproveTemplate = template.Must(template.New("device-approve").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Approve this device?</title>
<style nonce="{{ .Nonce }}">
` + pageStyle + `</style>
</head>
<body>
<h1>Approve this device?</h1>
<p>A device calling itself <strong>{{ .DeviceName }}</strong> is asking to sign in to your account.</p>
<p>It should be showing this code:</p>
<p><code class="code">{{ .UserCode }}</code></p>
<p class="error">If that code is not on a screen in front of you, press Deny. Somebody who sends you a code
to enter is asking you to sign them in as you.</p>
<form method="post" action="/device/approve">
  <input type="hidden" name="device_token" value="{{ .Token }}">
  <button type="submit" name="decision" value="approve">Approve</button>
  <button type="submit" name="decision" value="deny">Deny</button>
</form>
</body>
</html>
`))

var deviceApprovedTemplate = template.Must(template.New("device-approved").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Device approved</title>
<style nonce="{{ .Nonce }}">
` + pageStyle + `</style>
</head>
<body>
<h1>Device approved</h1>
<p>You can close this page. The device will finish signing in within a few seconds.</p>
</body>
</html>
`))

var deviceDeniedTemplate = template.Must(template.New("device-denied").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Device denied</title>
<style nonce="{{ .Nonce }}">
` + pageStyle + `</style>
</head>
<body>
<h1>Nothing was signed in</h1>
<p>That device has been told the request was denied, and it cannot try again with the same code.</p>
<p class="note">If somebody sent you that code, they were trying to sign in to your account. You have not
given them anything, and there is nothing else you need to do.</p>
</body>
</html>
`))
