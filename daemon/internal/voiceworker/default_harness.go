// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build norite_voiceharness

package voiceworker

import "os"

// harnessVariable names the harness a daemon built with norite_voiceharness signals through. The tag is
// never set by anything that builds a release; a test in cmd/daemond holds a build without it to contain
// none of this file, as it does norite_testrelease.
const harnessVariable = "NORITE_VOICE_HARNESS"

// DefaultSignaler is M26's harness at the URL NORITE_VOICE_HARNESS names, for a manual pass with no
// instance, and the gateway's when it names none.
func DefaultSignaler(gateway *Gateway) (Signaler, string) {
	if url := os.Getenv(harnessVariable); url != "" {
		return Harness{URL: url}, ""
	}
	return gateway, ""
}
