// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package meta serves the instance metadata any client may read without a credential, which today is the
// AGPL section 13 Corresponding Source offer and nothing else.
//
// # Why this is not under /instance
//
// /instance is the administration surface, and its defining property is that it has no unauthenticated
// route in it — the router says so, and mounting one there would put an exemption inside the one group
// whose value is not having any. Section 13's offer is owed to "all users interacting with it remotely
// through a computer network", so a credential-gated answer does not discharge it: the obligation is
// precisely to the person who has not signed in. The two requirements are incompatible, so this is a
// separate, deliberately public route.
//
// # Why the revision is a build-time variable and the URL is configuration
//
// They answer different halves of the offer and can fail in opposite directions. The revision must
// describe the binary that is running, so anything an operator can type is a value that can disagree with
// the code — it is stamped in with -ldflags at release time. The URL must describe where *this* operator's
// source can be had, because section 13 obliges someone who modified Norite to offer their users the
// modified source rather than this project's, so it has to be settable and its default is only correct for
// an unmodified build (see config.Config.SourceURL).
package meta

import (
	"net/http"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
)

// License is the SPDX identifier this instance's own code is distributed under.
//
// A constant rather than configuration: an operator may modify Norite and must then offer their modified
// source, but the terms they received it under are not theirs to restate. A fork that genuinely relicenses
// is editing this line, which is the visible act it should be.
const License = "AGPL-3.0-or-later"

// Revision is the git revision this binary was built from, set at link time with
//
//	-ldflags "-X github.com/Alexnex31/Norite/backend/internal/meta.Revision=<rev>"
//
// The default is deliberately "unknown" rather than a plausible-looking placeholder: a development build
// genuinely does not know, and reporting a revision that does not exist upstream is worse than admitting
// it, because the whole value of the field is that somebody can fetch that exact source.
var Revision = "unknown"

// Response is the body of GET /api/v1/meta. Contract: contracts/openapi.yaml, schema InstanceMeta.
type Response struct {
	License        string `json:"license"`
	SourceURL      string `json:"source_url"`
	SourceRevision string `json:"source_revision"`
}

// Handler serves the source offer. It reads no database and takes no credential, which is what lets it
// answer during a startup in which everything else is still 503.
type Handler struct {
	SourceURL string
}

// ServeHTTP writes the offer.
//
// Rule 4: side-effect-free, as a GET must be. It also touches no per-request state at all — the response is
// constant for the life of the process — so there is nothing here to rate-limit differently or to log.
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, r, http.StatusOK, Response{
		License:        License,
		SourceURL:      h.SourceURL,
		SourceRevision: Revision,
	})
}
