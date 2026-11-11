// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"html/template"
	"net/http"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
)

// The page an invite link opens in a browser (easier-setup).
//
// An invite link is the instance's address, /join, and the code after '#': the client builds it and
// `norite setup` reads it. Somebody handed one will often open it in a browser first, and a 404 there would
// read as a broken invite. So the instance answers /join with a page saying what to do with the link.
//
// It cannot see the code. A browser never sends a fragment, which is why the code is one: it reaches no
// request log, and nothing on this page depends on it. The page has no form and no script, takes no
// parameter and reads nothing from the request, so it says the same to everybody.

// joinPageData is everything the template renders: the instance's own configured address and the CSP nonce.
// Nothing from the request, so there is no user-controlled text on this page.
type joinPageData struct {
	Nonce string
	// Instance is public_base_url, never the Host header: behind a proxy, Host is whatever the proxy sends,
	// and a page naming it would name wherever a request was aimed (CLAUDE.md, M5). Empty when the operator
	// set none, and the page then names no address.
	Instance string
	// Release is the release tag this instance runs, such as v0.1.2, or "" for a build that is not one.
	Release string
}

// The installer commands are upstream's: the scripts verify each archive against the release this project's
// workflow signed, so they are the same commands whichever instance serves this page. For a release, the
// scripts are the ones at its tag: they restate that release's archive names and signing identity, which a
// later release may change (CLAUDE.md, M20a), so the newest script on main may not install an older one.
var joinPageTemplate = template.Must(template.New("join").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Join a guild on Norite</title>
<style nonce="{{ .Nonce }}">
` + httpx.PageStyle + `</style>
</head>
<body>
<h1>You were invited to a guild on Norite</h1>
<p>Norite is a chat platform used from a terminal{{ if .Instance }}, and this invite is to the instance at
<code class="code">{{ .Instance }}</code>{{ end }}. The link you opened is the whole invite: keep it.</p>
<p><strong>1. Install Norite.</strong> On macOS or Linux:</p>
{{ if .Release }}<p><code class="code">curl -fsSL https://raw.githubusercontent.com/Alexnex31/Norite/{{ .Release }}/scripts/install.sh | sh -s -- --version {{ .Release }}</code></p>
{{ else }}<p><code class="code">curl -fsSL https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.sh | sh</code></p>
{{ end }}<p>On Windows, in PowerShell rather than cmd:</p>
{{ if .Release }}<p><code class="code">&amp; ([scriptblock]::Create((irm https://raw.githubusercontent.com/Alexnex31/Norite/{{ .Release }}/scripts/install.ps1))) -Version {{ .Release }}</code></p>
<p class="note">This installs {{ .Release }}, the version this instance runs: before 1.0, a client of another
minor version, or another pre-release, cannot connect to it.</p>
{{ else }}<p><code class="code">irm https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.ps1 | iex</code></p>
{{ end }}
<p><strong>2. Run <code class="code">norite setup</code> followed by the link</strong>, as it is in your
browser's address bar or in the message it came in, with everything after the <code>#</code>. It creates
your account or signs you in, and joins the guild.</p>
<p class="note">The part of the link after <code>#</code> is the invite code. Your browser kept it: this
instance was not sent it.</p>
</body>
</html>
`))

// JoinPage serves GET and HEAD /join.
type JoinPage struct {
	// Instance is the instance's public_base_url, or "".
	Instance string
	// Release is the release tag this instance runs, or "" for a build that is not a release. The installer
	// commands pin it, and are fetched at it: they install the newest release otherwise, which before 1.0 an
	// instance of another MINOR, or of another pre-release, refuses (gatewayproto.Check).
	Release string
}

func (p JoinPage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	data := joinPageData{Nonce: httpx.NonceFrom(r.Context()), Instance: p.Instance, Release: p.Release}
	if err := joinPageTemplate.Execute(w, data); err != nil {
		// The status is written; a failed write is a client that went away.
		logging.FromContext(r.Context()).Debug().Err(err).Msg("writing the invite page failed")
	}
}
