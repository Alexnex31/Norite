// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"strings"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
)

// Policy is what decides which release, if any, this installation moves to.
type Policy struct {
	// Current is the version running.
	Current gatewayproto.Version
	// Instance is the version the signed-in instance reported in HELLO, or "" when none has: signed out,
	// never connected, or an instance built without a version.
	Instance string
	// Prereleases is the opt-in to alphas, betas and release candidates.
	Prereleases bool
	// Bad lists versions this installation installed and rolled back from.
	Bad []string
}

// Choose returns the release to move to: the highest that passes every rule, or false when none does.
//
//   - Channel. A pre-release is offered only to an installation that asked for them.
//   - Order. Only a version of higher SemVer precedence than the one running. Nothing here ever goes
//     back: rolling back restores a binary this installation kept and does not come through here.
//   - The instance. Only a version the instance's handshake accepts (gatewayproto.Check): a daemon is
//     never moved to a version that cannot talk to the instance it is signed in to. With no instance to
//     ask, the running version stands in, so the move stays inside what the running version could talk
//     to.
//   - Not known bad.
func Choose(p Policy, releases []Release) (Release, bool) {
	var (
		best  Release
		found bool
	)
	for _, r := range releases {
		if r.Version.Pre != "" && !p.Prereleases {
			continue
		}
		if r.Version.Compare(p.Current) <= 0 {
			continue
		}
		if !p.talksToInstance(r.Version) || p.knownBad(r.Version) {
			continue
		}
		if !found || r.Version.Compare(best.Version) > 0 {
			best, found = r, true
		}
	}
	return best, found
}

func (p Policy) talksToInstance(candidate gatewayproto.Version) bool {
	if instance, err := gatewayproto.ParseVersion(p.Instance); err == nil {
		return gatewayproto.Check(instance.String(), candidate.String()).Compatible
	}
	// Nobody to ask. A release is compared with the release the running version is or leads to, so that a
	// release candidate with no instance still becomes its release: Check refuses any two different
	// versions when either is a pre-release, which is right for a connection and would strand this.
	current, release := p.Current, candidate
	current.Pre, release.Pre = "", ""
	return gatewayproto.Check(current.String(), release.String()).Compatible
}

func (p Policy) knownBad(v gatewayproto.Version) bool {
	for _, bad := range p.Bad {
		if strings.EqualFold(bad, v.String()) {
			return true
		}
	}
	return false
}
