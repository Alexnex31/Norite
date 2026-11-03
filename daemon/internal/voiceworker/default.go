// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !norite_voiceharness

package voiceworker

// DefaultSignaler is the signaler a daemon joins calls through, or nil and why there is none. Until M33
// writes op 4's, a release has none: joining is refused before anything is started.
func DefaultSignaler() (Signaler, string) {
	return nil, "this version of Norite cannot join voice calls yet"
}
