// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"net/url"
	"strings"
)

// SameInstance reports whether two instance URLs name one instance: scheme, host, port and path prefix.
//
// Here rather than in daemon/credentials, beside ParseInstanceURL, because the automation port asks it and
// may import no credential source (M22), and the CLI asks it of an invite link (easier-setup). Two copies of
// one comparison drift, and the copy that drifts is the one deciding where a request goes.
//
// The host is compared without case and without the dot that ends a fully qualified name, and the port as
// the connection uses it: none, an empty one and the scheme's own (443, 80) are one port. Written either
// way, the address reaches the same server, and one written `:443` by the inviter is otherwise refused for
// somebody signed in without it (easier-setup's second /code-review).
func SameInstance(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil || ua.Host == "" || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && hostOf(ua) == hostOf(ub) && portOf(ua) == portOf(ub) &&
		strings.TrimRight(ua.Path, "/") == strings.TrimRight(ub.Path, "/")
}

// hostOf is u's host, lowered and without a final dot. An IPv6 literal loses its brackets, which the port
// compared beside it makes harmless.
func hostOf(u *url.URL) string {
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// portOf is the port a connection to u uses, "" for the scheme's own.
func portOf(u *url.URL) string {
	port := u.Port()
	switch {
	case port == "":
		return ""
	case strings.EqualFold(u.Scheme, "https") && port == "443", strings.EqualFold(u.Scheme, "http") && port == "80":
		return ""
	}
	return port
}
