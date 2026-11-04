// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build norite_voiceharness

package voiceworker

import "os"

// harnessVariable names the harness a daemon built with norite_voiceharness signals through. The tag is
// never set by anything that builds a release; a test in cmd/daemond holds a build without it to contain
// none of this file, as it does norite_testrelease.
const harnessVariable = "NORITE_VOICE_HARNESS"

// DefaultSignaler is M26's harness at the URL NORITE_VOICE_HARNESS names, for the manual pass and nothing
// else, until M33 writes op 4's.
func DefaultSignaler() (Signaler, string) {
	url := os.Getenv(harnessVariable)
	if url == "" {
		return nil, "this daemon was built for the voice harness, and " + harnessVariable + " names none"
	}
	return Harness{URL: url}, ""
}
