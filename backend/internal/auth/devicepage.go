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
	// One message for a wrong authenticator code and a wrong recovery code alike. Saying which kind was
	// expected would tell somebody holding a stolen password which one to go looking for.
	deviceBadCodeMsg = "That code is not valid. Try the current code from your authenticator, or one of " +
		"your recovery codes."
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
	// Account names who is about to be authorized, on the approval page and nowhere else.
	//
	// The page said "your account" for its whole first draft, which reads as a certainty and is not one: a
	// person can reach this screen signed in as somebody other than they assumed — a second provider
	// account, or, in the direction that matters, an account an attacker authenticated as after obtaining
	// their code. This line is the only place that mismatch can surface before the decision is made.
	Account string
	// Providers are the sign-in buttons to offer, which is whatever this instance has configured. Empty on
	// an instance with no provider set up, where the password form is the whole page.
	Providers []deviceProviderLink
	// Error is one of a fixed set of messages this file owns, never a service error and never user input.
	Error string
}

// deviceProviderLink is one provider button.
//
// The URL carries the entry continuation, which is why these are built per render rather than once: it
// names the authorization being worked on and expires with it.
type deviceProviderLink struct {
	Label string
	URL   string
}

// deviceProviderLabels are the names to show. A map rather than strings.Title on the identifier, because
// "Github" is wrong and the fix for it is a table, not a rule.
var deviceProviderLabels = map[string]string{
	"google": "Google",
	"github": "GitHub",
}

// deviceProviderLabel names a provider for a person, falling back to the identifier.
//
// The fallback is the point. A bare map index yields "" for anything not listed, and the first provider
// added to OAuthProviders without also touching the table above — the exact split-knowledge failure that
// table exists because of — would render an empty anchor: a zero-width link nobody can click, with nothing
// logged and nothing failing. An unlovely "gitlab" is a better page than an invisible one.
func deviceProviderLabel(name string) string {
	if label, ok := deviceProviderLabels[name]; ok {
		return label
	}
	return name
}

// DevicePageRoutes mounts the verification pages, at the root beside /reset.
func (h *Handler) DevicePageRoutes(r chi.Router) {
	r.Get(devicePagePath, h.devicePage)
	r.Post(devicePagePath, h.devicePageSubmit)
	r.Post(devicePagePath+"/signin", h.devicePageSignIn)
	r.Post(devicePagePath+"/2fa", h.devicePageFactor)
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
		// row.UserCode, not entry.UserCode. They agree today only because the token happened to be minted
		// from the same lookup; taking both facts from the row the handler just re-read is what keeps them
		// agreeing, and the approval page's whole job is showing a code that matches the terminal.
		h.renderDeviceSignIn(w, r, http.StatusUnauthorized, row, row.UserCode, deviceBadPasswordMsg)
		return
	}

	h.continueToApproval(w, r, entry.DeviceCodeID, user.ID)
}

// devicePageFactor is the second factor, on the one flow where it can only be asked here.
//
// The waiting CLI redeems its code without proving anything — there is nobody at that terminal — so this
// browser is the only place a factor can be demanded on the device flow. A code entered here is what turns
// a password into an approval.
func (h *Handler) devicePageFactor(w http.ResponseWriter, r *http.Request) {
	form, ok := h.deviceForm(w, r)
	if !ok {
		return
	}

	// A factor token specifically, for the reason devicePageApprove asks for an approval token: an entry
	// token here would be a browser that knows a code proving a factor for an account it never named, and
	// an approval token would mean the step had already been skipped.
	pending, err := h.svc.parseDeviceToken(form.Get("device_token"), deviceFactorTokenType)
	if err != nil {
		h.renderDeviceExpired(w, r)
		return
	}

	proof, err := h.svc.proveFactor(r.Context(), pending.UserID, form.Get("code"))
	if err != nil {
		if !errors.Is(err, ErrInvalidFactorCode) {
			logging.FromContext(r.Context()).Error().Err(err).Msg("device page factor check failed")
			h.renderDeviceExpired(w, r)
			return
		}
		// Re-rendered with the same token, so a mistyped code costs a retry rather than the whole flow.
		// The message is one this file owns and says nothing about which kind of code was wrong.
		h.renderDeviceFactor(w, r, pending.DeviceCodeID, pending.UserID, http.StatusUnauthorized,
			deviceBadCodeMsg)
		return
	}

	h.renderDeviceApproval(w, r, pending.DeviceCodeID, pending.UserID, proof)
}

// continueToApproval takes a browser that has proved whose account this is to the approval step — or to
// the second factor first, if the account has one.
//
// Every route that reaches approval goes through here: the password branch above, and both provider
// branches in oauthhttp.go. That is deliberate and is the same reasoning startSession's proof parameter
// carries — three call sites each remembering to check is three chances to forget, and the one that
// forgot would hand out an approval on an account whose factor was never asked for.
func (h *Handler) continueToApproval(w http.ResponseWriter, r *http.Request, deviceCodeID, userID int64) {
	proof, err := h.svc.factorSatisfied(r.Context(), userID)
	if err != nil {
		if errors.Is(err, ErrTwoFactorRequired) {
			h.renderDeviceFactor(w, r, deviceCodeID, userID, http.StatusOK, "")
			return
		}
		logging.FromContext(r.Context()).Error().Err(err).Msg("resolving the second factor failed")
		h.renderDeviceExpired(w, r)
		return
	}
	h.renderDeviceApproval(w, r, deviceCodeID, userID, proof)
}

// renderDeviceFactor draws the code form.
func (h *Handler) renderDeviceFactor(w http.ResponseWriter, r *http.Request, deviceCodeID, userID int64,
	status int, message string,
) {
	row, err := h.svc.deviceCodeByID(r.Context(), deviceCodeID)
	if err != nil {
		h.renderDeviceExpired(w, r)
		return
	}

	token, err := h.svc.issueDeviceFactorToken(deviceCodeID, row.UserCode, userID)
	if err != nil {
		logging.FromContext(r.Context()).Error().Err(err).Msg("issuing a device factor token failed")
		h.renderDeviceExpired(w, r)
		return
	}

	h.renderDevice(w, r, deviceFactorTemplate, status, devicePageData{
		Nonce:      httpx.NonceFrom(r.Context()),
		Token:      token,
		UserCode:   row.UserCode,
		DeviceName: row.DeviceName,
		Error:      message,
	})
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
		outcome, err := h.svc.DenyDeviceAuthorization(r.Context(), approval.DeviceCodeID, approval.UserID)
		if err != nil {
			logging.FromContext(r.Context()).Error().Err(err).Msg("denying a device authorization failed")
			h.renderDeviceExpired(w, r)
			return
		}

		// One template per outcome, because they are not the same news and this page's whole job is being
		// believed. Telling somebody "nothing was signed in" when a device is about to collect their
		// session would be the worst thing on it.
		h.renderDevice(w, r, denyTemplateFor(outcome), http.StatusOK,
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

	// The same token is handed back to the page, which is what makes the recovery path reachable rather
	// than merely implemented. It is still valid, and presenting it at this handler again denies — which,
	// after an approval, revokes it. See the template.
	h.renderDevice(w, r, deviceApprovedTemplate, http.StatusOK, devicePageData{
		Nonce: httpx.NonceFrom(r.Context()),
		Token: form.Get("device_token"),
	})
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
		Providers:  h.deviceProviderLinks(token),
		Error:      message,
	})
}

// renderDeviceApproval draws the last step.
//
// The user code comes off the row rather than from the caller, which is what lets the OAuth callback reach
// this page: it arrives back from a provider holding a state row and an account, and never saw what
// somebody typed. That is the reason device_codes stores the code and not a hash of it.
// It takes a factorProof so that the approval token — the thing that says this browser has finished
// proving who it is — cannot be minted for an account whose second factor was never asked for. Callers
// obtain one through continueToApproval.
func (h *Handler) renderDeviceApproval(w http.ResponseWriter, r *http.Request,
	deviceCodeID, userID int64, proof factorProof,
) {
	row, err := h.svc.deviceCodeByID(r.Context(), deviceCodeID)
	if err != nil {
		h.renderDeviceExpired(w, r)
		return
	}

	token, err := h.svc.issueDeviceApprovalToken(deviceCodeID, row.UserCode, userID, proof)
	if err != nil {
		logging.FromContext(r.Context()).Error().Err(err).Msg("issuing a device approval failed")
		h.renderDeviceExpired(w, r)
		return
	}

	h.renderDevice(w, r, deviceApproveTemplate, http.StatusOK, devicePageData{
		Nonce:      httpx.NonceFrom(r.Context()),
		Token:      token,
		UserCode:   FormatUserCode(row.UserCode),
		DeviceName: row.DeviceName,
		Account:    h.svc.describeAccount(r.Context(), userID),
	})
}

// deviceProviderLinks builds one button per configured provider.
//
// A link into the ordinary /authorize endpoint, carrying the entry continuation instead of a flow
// challenge. That is the whole join between the two flows: from there the provider round trip is the one
// M6 built and M8 extended, and the callback finds its way back here because the continuation put a
// device on the state row.
//
// The continuation in a URL is not a leak. It names an authorization and asserts nothing about who is
// signing in, no-referrer is already set on these pages, and the next thing it reaches is this instance.
func (h *Handler) deviceProviderLinks(entryToken string) []deviceProviderLink {
	var out []deviceProviderLink
	for _, name := range h.svc.oauth.Names() {
		out = append(out, deviceProviderLink{
			Label: deviceProviderLabel(name),
			URL:   oauthAuthorizePath(name) + "?device_token=" + url.QueryEscape(entryToken),
		})
	}
	return out
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

// denyTemplateFor picks the page that matches what Deny actually managed to do.
func denyTemplateFor(outcome DeviceDenyOutcome) *template.Template {
	switch outcome {
	case DeviceDenyRevoked:
		return deviceRevokedTemplate
	case DeviceDenyTooLate:
		return deviceTooLateTemplate
	default:
		return deviceDeniedTemplate
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
{{ if .Providers }}
<p class="note">Sign in with a provider:</p>
<p>{{ range .Providers }}<a class="provider" href="{{ .URL }}">{{ .Label }}</a> {{ end }}</p>
<p class="note">or with your email and password:</p>
{{ end }}
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

// The second-factor step. Deliberately says nothing about *which* account it is for: the approval page one
// step later is where the account is named, because that is the screen whose whole job is letting somebody
// notice they are signing in as somebody they did not expect.
var deviceFactorTemplate = template.Must(template.New("device-factor").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Two-factor code</title>
<style nonce="{{ .Nonce }}">
` + pageStyle + `</style>
</head>
<body>
<h1>Two-factor code</h1>
{{ if .Error }}<p class="error">{{ .Error }}</p>{{ end }}
<p>Enter the current code from your authenticator to finish signing in on
<strong>{{ .DeviceName }}</strong>.</p>
<form method="post" action="/device/2fa">
  <input type="hidden" name="device_token" value="{{ .Token }}">
  <label for="code">Code</label>
  <input id="code" name="code" type="text" inputmode="text" autocomplete="one-time-code" required autofocus>
  <button type="submit">Continue</button>
</form>
<p class="note">Lost your authenticator? Enter one of your recovery codes instead.</p>
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
<p>A device calling itself <strong>{{ .DeviceName }}</strong> is asking to sign in
as <strong>{{ .Account }}</strong>.</p>
<p class="note">If that is not the account you meant to use, press Deny and start again.</p>
<p>It should be showing this code:</p>
<p><code class="code">{{ .UserCode }}</code></p>
<p class="error">If that code is not on a screen in front of you, press Deny. Somebody who sends you a code
to enter is asking you to sign them in as you.</p>
<form method="post" action="/device/approve">
  <input type="hidden" name="device_token" value="{{ .Token }}">
  <!-- Deny first, and that is not a layout preference. A form submitted without a button being chosen -
       Enter from a text field, a keyboard user pressing Return, an assistive-technology default - sends
       the first submit button in DOM order. The handler treats anything that is not exactly "approve" as
       a denial precisely so this page fails closed; putting Approve first would undo that in markup. -->
  <button type="submit" name="decision" value="deny" class="deny">Deny</button>
  <button type="submit" name="decision" value="approve">Approve</button>
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
<hr>
<p class="note">Not what you meant to do? For the next few minutes this can still be stopped, and the
device will never finish signing in.</p>
<form method="post" action="/device/approve">
  <input type="hidden" name="device_token" value="{{ .Token }}">
  <button type="submit" name="decision" value="deny" class="deny">This wasn&#39;t me — stop it</button>
</form>
</body>
</html>
`))

// The two outcomes a Deny can have other than the ordinary one. Separate documents rather than one with a
// conditional, so that what each says can be read whole — these are the pages somebody reads at the worst
// moment this flow has.
var deviceRevokedTemplate = template.Must(template.New("device-revoked").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Device stopped</title>
<style nonce="{{ .Nonce }}">
` + pageStyle + `</style>
</head>
<body>
<h1>That device has been stopped</h1>
<p>It had already been approved, and it has now been prevented from finishing. It never received a way in
to your account, and it cannot try again with the same code.</p>
<p class="note">If somebody sent you that code, they were trying to sign in as you. Nothing of yours was
given away, and there is nothing else you need to do.</p>
</body>
</html>
`))

var deviceTooLateTemplate = template.Must(template.New("device-too-late").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>That device already signed in</title>
<style nonce="{{ .Nonce }}">
` + pageStyle + `</style>
</head>
<body>
<h1>That device has already signed in</h1>
<p class="error">This could not be undone from here. The device finished signing in before you pressed
Deny, so it is holding a session on your account right now.</p>
<p>Sign that device out from your account&#39;s device list, and change your password if you did not
recognize it.</p>
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
