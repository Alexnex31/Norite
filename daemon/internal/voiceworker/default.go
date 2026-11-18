// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !norite_voiceharness

package voiceworker

// DefaultSignaler is the signaler a daemon joins calls through: op 4 over its own gateway connection
// (M33), in every release.
func DefaultSignaler(gateway *Gateway) (Signaler, string) {
	return gateway, ""
}
