// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The harness is on this machine when its URL's host is, read as a host and never as text: a name that
// merely begins like one would admit the loopback candidates of whatever that host answers.
func TestTheHarnessIsLocalByItsHost(t *testing.T) {
	for url, want := range map[string]bool{
		"http://127.0.0.1:8189":             true,
		"http://127.0.0.10:8189":            true,
		"http://[::1]:8189":                 true,
		"http://localhost:8189":             true,
		"http://LOCALHOST:8189/":            true,
		"http://localhost.example.com:8189": false,
		"http://127.0.0.1.example.com:8189": false,
		"http://example.com/://127.0.0.1":   false,
		"http://10.0.2.2:8189":              false,
		"http://[::ffff:127.0.0.1]:8189":    true,
		"not a url\x7f":                     false,
	} {
		require.Equal(t, want, Harness{URL: url}.loopback(), url)
	}
}
