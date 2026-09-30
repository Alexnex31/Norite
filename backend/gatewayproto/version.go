// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gatewayproto

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DevVersion is what a build with no stamped version reports: a `go build` of a checkout, `just dev`, a
// self-hoster building their own server.
const DevVersion = "dev"

// MinorWindow is how many MINOR versions behind the server a client may be, once MAJOR is at least 1.
//
// docs/architecture.md has promised "a defined MINOR-version-back window" since before there was a number;
// this is the number. It governs nothing until 1.0.0, because while MAJOR is 0 the MINOR must match exactly
// (ADR 0033). A client *ahead* of the server is refused at any distance: SemVer lets a newer MINOR add what
// an older server has never heard of.
const MinorWindow = 2

// semverPattern is semver.org's own expression, with an optional leading "v" and the build metadata left
// to be stripped by the caller. Strict on purpose: a two-component "0.1" is not a version (ADR 0033).
var semverPattern = regexp.MustCompile(`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
	`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
	`(?:\+[0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*)?$`)

// Version is a parsed release version. Build metadata is discarded, as SemVer §10 requires.
type Version struct {
	Major, Minor, Patch uint64
	// Pre is the pre-release suffix without its hyphen, empty for a release.
	Pre string
}

// ParseVersion parses a strict three-component SemVer version.
func ParseVersion(s string) (Version, error) {
	m := semverPattern.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("gatewayproto: %q is not a vMAJOR.MINOR.PATCH version", s)
	}
	var v Version
	var err error
	if v.Major, err = strconv.ParseUint(m[1], 10, 64); err != nil {
		return Version{}, fmt.Errorf("gatewayproto: %q: %w", s, err)
	}
	if v.Minor, err = strconv.ParseUint(m[2], 10, 64); err != nil {
		return Version{}, fmt.Errorf("gatewayproto: %q: %w", s, err)
	}
	if v.Patch, err = strconv.ParseUint(m[3], 10, 64); err != nil {
		return Version{}, fmt.Errorf("gatewayproto: %q: %w", s, err)
	}
	v.Pre = m[4]
	return v, nil
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Compatibility is the handshake's verdict on a pair of versions.
type Compatibility struct {
	// Compatible is whether the two may talk.
	Compatible bool
	// Checked is false when either side is a dev build, which is compatible with anything and logged as
	// such by both sides, so a mismatch between two source builds is never mistaken for a verified pair.
	Checked bool
	// Reason says why a pair is incompatible, in words a person upgrading one side can act on.
	Reason string
}

// Check applies ADR 0033's strict rule to a server and a client version.
//
//   - Either side "dev": compatible, unchecked. Only a source build reports it, and refusing would lock a
//     self-hoster's own server out of every released client. An empty version is not "dev": a client
//     that sent none would otherwise skip the check against every release (M18 review).
//   - A pre-release on either side: compatible only with the identical version, pre-release included.
//     SemVer §9 lets a pre-release break what its core version promises, and two alphas of one MINOR may
//     differ in exactly the way this check exists to catch.
//   - MAJOR 0: MAJOR and MINOR must match; any PATCH difference is fine, since a PATCH carries fixes only.
//   - MAJOR 1 and above: MAJOR must match, and the client may be up to MinorWindow MINORs behind.
//
// A version that does not parse is incompatible rather than treated as dev: "dev" is a specific promise
// about where a binary came from, and a garbled version string makes no promise at all.
func Check(server, client string) Compatibility {
	if isDev(server) || isDev(client) {
		return Compatibility{Compatible: true, Checked: false}
	}
	sv, err := ParseVersion(server)
	if err != nil {
		return Compatibility{Checked: true, Reason: "the server reports a version this client cannot read"}
	}
	cv, err := ParseVersion(client)
	if err != nil {
		return Compatibility{Checked: true, Reason: fmt.Sprintf("%q is not a release version", client)}
	}

	if sv.Pre != "" || cv.Pre != "" {
		if sv == cv {
			return Compatibility{Compatible: true, Checked: true}
		}
		return Compatibility{Checked: true, Reason: fmt.Sprintf(
			"a pre-release only connects to the identical version: server %s, client %s", sv, cv)}
	}

	if sv.Major != cv.Major {
		return Compatibility{Checked: true, Reason: fmt.Sprintf(
			"major versions differ: server %s, client %s; upgrade the %s", sv, cv, older(sv, cv))}
	}
	if sv.Major == 0 {
		if sv.Minor != cv.Minor {
			return Compatibility{Checked: true, Reason: fmt.Sprintf(
				"before 1.0.0 every minor version may break the last: server %s, client %s; upgrade the %s",
				sv, cv, older(sv, cv))}
		}
		return Compatibility{Compatible: true, Checked: true}
	}
	switch {
	case cv.Minor > sv.Minor:
		return Compatibility{Checked: true, Reason: fmt.Sprintf(
			"the client is newer than the server: server %s, client %s; upgrade the server", sv, cv)}
	case sv.Minor-cv.Minor > MinorWindow:
		return Compatibility{Checked: true, Reason: fmt.Sprintf(
			"the client is more than %d minor versions behind: server %s, client %s; upgrade the client",
			MinorWindow, sv, cv)}
	}
	return Compatibility{Compatible: true, Checked: true}
}

func isDev(s string) bool {
	return strings.EqualFold(s, DevVersion)
}

// older names the side to upgrade when two releases differ.
func older(server, client Version) string {
	if server.Major < client.Major || (server.Major == client.Major && server.Minor < client.Minor) {
		return "server"
	}
	return "client"
}
