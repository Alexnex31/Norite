// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package cliapp

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/cli/internal/output"
	"github.com/Alexnex31/Norite/cli/internal/verbs"
)

// Revision is the git commit this binary was built from, set at link time:
//
//	-ldflags "-X github.com/Alexnex31/Norite/cli/internal/cliapp.Revision=<commit>"
//
// goreleaser stamps the full commit, which always resolves in the repository, where a tag goreleaser has
// stripped of its "v" would not. Empty in a build nobody stamped, which then reads the toolchain's own
// record instead (revisionOf).
var Revision = ""

// SourceURL is where this build's source can be had. Upstream by default, and a fork's own build sets it
// the way Revision is set: it is the build that knows what it was built from, not the instance it talks to.
var SourceURL = "https://github.com/Alexnex31/Norite"

// The notice's fixed parts: what AGPL §0 calls Appropriate Legal Notices. A copyright notice, the license
// and the freedom it grants, the absence of warranty, and where the terms are.
const (
	license   = "AGPL-3.0-or-later"
	copyright = "Copyright (C) 2026 Alexandre Duffez"
	rights    = "Norite is free software: you can redistribute it and/or modify it under the terms of the " +
		"GNU Affero General Public License, version 3 or (at your option) any later version."
	warranty = "It comes with ABSOLUTELY NO WARRANTY, to the extent permitted by law."
	terms    = "The full terms are in the LICENSE file this program came with, and at " +
		"https://www.gnu.org/licenses/agpl-3.0.html."
)

// aboutTimeout bounds asking the instance through the daemon. The notice is the command's point and it
// prints without the instance, so a daemon slow to answer costs a few seconds, not the relay's two minutes.
const aboutTimeout = 5 * time.Second

// aboutCommand prints AGPL §5(d)'s Appropriate Legal Notices for this program, and the instance's own
// §13 source offer beside them (M20a).
//
// Two sources, kept apart and labeled, because they are two obligations about two programs. This build's
// notice describes the binary on this machine, which may talk to any instance and be built by somebody who
// runs none, so its revision and source are stamped into it. The instance's offer describes the server, and
// it is the instance's to make; it is printed as the instance's, and its text is a stranger's (rule 19).
// `6d`, the screen this becomes at M44, once took the client's source from the instance, which conflated
// the two.
//
// It never fails for want of the instance. A daemon that is stopped or signed out leaves the instance's
// block saying why, and the command exits 0: a notice that refused to print would be a worse notice.
func aboutCommand(connect verbs.Connector) *cli.Command {
	return &cli.Command{
		Name:  "about",
		Usage: "Print what this build is, the license it comes with, and your instance's source offer",
		Description: "This build's version, revision and source, and the notice the AGPL asks a program to\n" +
			"show. Beside it, if a daemon is signed in, the source offer of the instance it is signed in to.\n" +
			"`norite licenses` prints the licenses of the third-party code in this binary.",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			out := aboutView{
				Build:     buildOf(Version, Revision, SourceURL),
				License:   license,
				Copyright: copyright,
				Rights:    rights,
				Warranty:  warranty,
				Terms:     terms,
			}
			meta, err := instanceMeta(ctx, connect)
			if err != nil {
				reason := err.Error()
				out.InstanceUnavailable = &reason
			} else {
				out.Instance = meta
			}
			return output.Render(cmd.Root().Writer, cmd.Root().Bool(JSONFlagName), out)
		},
	}
}

func instanceMeta(ctx context.Context, connect verbs.Connector) (*instanceView, error) {
	ctx, cancel := context.WithTimeout(ctx, aboutTimeout)
	defer cancel()
	c, detach, err := connect(ctx)
	if err != nil {
		return nil, err
	}
	defer detach()
	m, err := ops.InstanceMeta(ctx, c)
	if err != nil {
		return nil, err
	}
	return &instanceView{License: m.License, SourceURL: m.SourceUrl, SourceRevision: m.SourceRevision}, nil
}

type buildView struct {
	Version  string `json:"version"`
	Revision string `json:"revision"`
	// RevisionFrom says where the revision came from: "stamped" at link time, "vcs" from the toolchain's
	// record of the checkout it was built in, or "unknown".
	RevisionFrom string `json:"revision_from"`
	// Modified is true when the toolchain recorded uncommitted changes in that checkout: the revision is
	// then where the source started, not all of it.
	Modified  bool   `json:"modified"`
	SourceURL string `json:"source_url"`
}

// buildOf describes this binary from its stamped values, reading the toolchain's record when nothing was
// stamped.
func buildOf(version, stamped, source string) buildView {
	info, _ := debug.ReadBuildInfo()
	b := revisionOf(stamped, info)
	b.Version, b.SourceURL = version, source
	return b
}

// revisionOf decides the revision, in order of how much it can be trusted: a value stamped at link time;
// the commit `go build` recorded when it built inside a checkout (vcs.revision, with vcs.modified); and
// otherwise "unknown", the backend's word for the same thing — never a plausible-looking placeholder, since
// the field's whole use is fetching that exact source.
func revisionOf(stamped string, info *debug.BuildInfo) buildView {
	if stamped != "" {
		return buildView{Revision: stamped, RevisionFrom: "stamped"}
	}
	b := buildView{Revision: "unknown", RevisionFrom: "unknown"}
	if info == nil {
		return b
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Revision, b.RevisionFrom = s.Value, "vcs"
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	if b.RevisionFrom == "unknown" {
		b.Modified = false
	}
	return b
}

type instanceView struct {
	License        string `json:"license"`
	SourceURL      string `json:"source_url"`
	SourceRevision string `json:"source_revision"`
}

type aboutView struct {
	Build buildView `json:"build"`
	// Instance is the instance's own offer, or null when it could not be asked, InstanceUnavailable then
	// saying why.
	Instance            *instanceView `json:"instance"`
	InstanceUnavailable *string       `json:"instance_unavailable"`
	License             string        `json:"license"`
	Copyright           string        `json:"copyright"`
	Rights              string        `json:"rights"`
	Warranty            string        `json:"warranty"`
	Terms               string        `json:"terms"`
}

// Text is the notice as a person reads it. This build's values are this binary's own; the instance's are a
// stranger's text and pass termsafe (rule 19), as does the reason it could not be asked.
func (a aboutView) Text(t *output.Text) {
	b := a.Build
	t.Line("norite %s, %s", b.Version, license)
	t.Line("")
	t.Line("This build")
	t.Line("  version   %s", b.Version)
	revision := b.Revision
	switch {
	case b.RevisionFrom == "vcs" && b.Modified:
		revision += " (built from a checkout with uncommitted changes)"
	case b.RevisionFrom == "vcs":
		revision += " (read from the build, not stamped)"
	}
	t.Line("  revision  %s", revision)
	t.Line("  source    %s", b.SourceURL)
	t.Line("")
	t.Line("Your instance")
	if a.Instance != nil {
		t.Line("  source    %s", output.Clean(a.Instance.SourceURL))
		t.Line("  revision  %s", output.Clean(a.Instance.SourceRevision))
		t.Line("  license   %s", output.Clean(a.Instance.License))
		t.Line("  The instance's own offer of its source (AGPL section 13), which may differ from this build's.")
	} else {
		t.Line("  could not be asked: %s", output.Clean(*a.InstanceUnavailable))
	}
	t.Line("")
	t.Line("Your rights")
	for _, line := range []string{a.Copyright, a.Rights, a.Warranty, a.Terms,
		"`norite licenses` prints the licenses of the third-party code in this binary."} {
		t.Line("  %s", line)
	}
}
