// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// The invite link (easier-setup): the instance's address, JoinPath, and a guild invite code after '#', as in
// https://chat.example.com/join#PGTQRBXGTCDFFWBH. It is what a newcomer is handed instead of an address and
// a code apart, and `norite setup` takes it whole.
//
// The code is the fragment because a browser never sends one: opened in a browser, the link reaches the
// instance's page at JoinPath and the code reaches no server, proxy or log on the way (ADR 0029's reason for
// carrying codes in bodies). The page has no script, so it cannot read the code either.

// JoinPath is where an invite link points on its instance, and where the instance serves the page that says
// what to do with one.
const JoinPath = "/join"

// maxTypedCode bounds what is taken for a code: a code is sixteen characters, and what a person or a chat
// client adds to one is dashes and spaces. The instance decides whether it is one it issued; this only
// refuses what cannot be.
const maxTypedCode = 64

// MaxInvite bounds what is read as an invite, a link or a code, in bytes: an instance's address and a code
// are far below it, and the terminal client's box holds no more.
const MaxInvite = 512

// errNotAnInvite is the one refusal for something that is neither a link nor a code.
var errNotAnInvite = errors.New("that is not an invite: paste the code, or the whole link it came in")

// Invite is what somebody was handed: a guild invite code, and the instance its link names.
type Invite struct {
	// Instance is the instance the link names, as credentials.ParseInstanceURL returns it, or "" for a bare
	// code, which names none.
	Instance string
	// Code is the code as it was given. The instance normalises it.
	Code string
}

// InviteLink is the link for code on the instance at instanceURL.
func InviteLink(instanceURL, code string) string {
	return strings.TrimRight(instanceURL, "/") + JoinPath + "#" + code
}

// ParseInvite reads a link or a bare code.
//
// A link is a stranger's text that decides where a password is about to go, so its instance passes the same
// parser every other instance URL does, and anything a link does not need is refused rather than ignored: a
// query, a path that does not end at JoinPath, a link with no code.
func ParseInvite(raw string) (Invite, error) {
	s := strings.TrimSpace(raw)
	if len(s) > MaxInvite {
		return Invite{}, errNotAnInvite
	}
	if !strings.ContainsAny(s, "/#") {
		if !typedCode(s) {
			return Invite{}, errNotAnInvite
		}
		return Invite{Code: s}, nil
	}
	if !strings.Contains(s, "://") {
		// As ParseInstanceURL reads a bare host: https, so a slip cannot downgrade the connection.
		s = "https://" + s
	}
	// A quote is refused so that a link can always be printed back inside single quotes, as a command to
	// paste. Before the code, only what an instance's address is written with is taken (linkChar), because
	// that address is printed back unquoted, in `norite login --instance …` for one, and url.Parse lets a
	// host or a path hold `$(…)`, backquotes, `;` and spaces: a stranger's link would otherwise put a
	// command in a line the person is told to paste. It refuses a percent-encoded path too, which matters
	// because the instance is rebuilt from the decoded one, where a %3F or %23 would turn into a query or a
	// fragment that ParseInstanceURL drops without a word.
	beforeCode, _, _ := strings.Cut(s, "#")
	if strings.ContainsRune(s, '\'') || strings.ContainsFunc(beforeCode, func(r rune) bool { return !linkChar(r) }) {
		return Invite{}, errNotAnInvite
	}
	u, err := url.Parse(s)
	if err != nil || u.RawQuery != "" || u.ForceQuery {
		return Invite{}, errNotAnInvite
	}
	if u.User != nil {
		// Checked here because the instance is rebuilt from the host alone below, where ParseInstanceURL
		// would never see it.
		return Invite{}, errors.New("an invite link must not carry a username or password")
	}
	base, ok := strings.CutSuffix(strings.TrimRight(u.Path, "/"), JoinPath)
	// A base with a dot segment names one instance to this check and another to the server that resolves
	// it, so it is refused rather than cleaned.
	if !ok || (base != "" && path.Clean(base) != base) || !typedCode(u.Fragment) {
		return Invite{}, errNotAnInvite
	}
	instance, err := credentials.ParseInstanceURL(u.Scheme + "://" + u.Host + base)
	if err != nil {
		return Invite{}, fmt.Errorf("that link does not name a usable instance: %w", err)
	}
	return Invite{Instance: instance, Code: u.Fragment}, nil
}

// linkChar reports whether r may appear in a link before its code: what a scheme, a host, a port, an IPv6
// literal and a path are written with, '@' so that a username is refused with its own message below, and the
// letters, digits and marks of a name in another script. None of them means anything to a shell.
func linkChar(r rune) bool {
	switch {
	case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	case r < utf8.RuneSelf:
		return strings.ContainsRune("-._~:/[]@", r)
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// typedCode reports whether s could be a code somebody typed or pasted: letters, digits, dashes, spaces and
// tabs, within maxTypedCode. The separators are the ones the instance strips (auth.ParseInviteCode).
func typedCode(s string) bool {
	if s == "" || len(s) > maxTypedCode {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == ' ', r == '\t':
		default:
			return false
		}
	}
	return true
}

// Context is ctx sending what is made with it to the instance the invite's link names, or nowhere: the
// daemon checks that instance against the sign-in it would send the request as, at the moment it would send
// it, and answers ipc.RelayOtherInstance, having sent nothing, when they differ. A bare code names no
// instance and is sent to whichever the daemon is signed in to.
//
// The daemon checks rather than the client, against the READY it attached with, because that READY names no
// instance while the daemon is starting, which refused a good link with advice that retrying would not
// help, and because a sign-in can change after it (easier-setup's second /code-review).
func (i Invite) Context(ctx context.Context) context.Context {
	if i.Instance == "" {
		return ctx
	}
	return ipc.ForInstance(ctx, i.Instance)
}

// LocalOnly reports whether instanceURL names this machine or its link alone: localhost, a loopback, a
// link-local or an unspecified address. A link built from one works on this machine and nowhere else, which
// is what the inviter is told.
func LocalOnly(instanceURL string) bool {
	u, err := url.Parse(instanceURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// A name other than localhost: whatever it resolves to here, it may resolve elsewhere too.
		return false
	}
	return addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified()
}
