// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"net/url"
	"strings"
)

// SameInstance reports whether two instance URLs name one instance: scheme, host and path prefix.
//
// Here rather than in daemon/credentials, beside ParseInstanceURL, because the automation port asks it and
// may import no credential source (M22), and the CLI asks it of an invite link (easier-setup). Two copies of
// one comparison drift, and the copy that drifts is the one deciding where a request goes.
func SameInstance(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil || ua.Host == "" || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) &&
		strings.TrimRight(ua.Path, "/") == strings.TrimRight(ub.Path, "/")
}
