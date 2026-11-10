// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"strings"

	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
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

// ErrInstanceUnknown refuses a link while the daemon names no instance yet. Sending the code anyway would
// send it to whichever instance the daemon then reaches, and a code is a way into a guild on the instance
// that issued it, which another instance's operator has no business holding.
var ErrInstanceUnknown = errors.New("the daemon has not said which instance it is signed in to yet, so that " +
	"link cannot be checked against it; try again in a moment")

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
	// paste. A percent-encoded path is refused because the instance is rebuilt from the decoded one, where a
	// %3F or %23 would turn into a query or a fragment that ParseInstanceURL drops without a word.
	beforeCode, _, _ := strings.Cut(s, "#")
	if strings.ContainsRune(s, '\'') || strings.Contains(beforeCode, "%") {
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

// For refuses an invite whose link names another instance than signedIn, the one this machine is signed in
// to. Redeeming it there would look the code up on the wrong instance and answer that it is unknown. A bare
// code names no instance and is taken to be for signedIn.
func (i Invite) For(signedIn string) error {
	if i.Instance == "" || ipc.SameInstance(i.Instance, signedIn) {
		return nil
	}
	return fmt.Errorf("that invite is for %s, and this machine is signed in to %s",
		termsafe.Text(i.Instance), termsafe.Text(signedIn))
}

// CodeFor reads raw, a link or a code, and returns the code to send to signedIn, the instance this machine is
// signed in to, or "" when that is not known yet. A link for another instance, or one that cannot be checked
// yet, is refused before anything is sent.
func CodeFor(raw, signedIn string) (string, error) {
	inv, err := ParseInvite(raw)
	if err != nil {
		return "", err
	}
	if inv.Instance != "" && signedIn == "" {
		return "", ErrInstanceUnknown
	}
	if err := inv.For(signedIn); err != nil {
		return "", err
	}
	return inv.Code, nil
}

// SignedInTo is the instance the daemon behind c named when it was attached to, or "" when it named no
// account, or c is not an attachment that says.
func SignedInTo(c daemonclient.Caller) string {
	ready, ok := ReadyOf(c)
	if !ok || ready.Account == nil {
		return ""
	}
	return ready.Account.InstanceURL
}

// ReadyOf is the READY the daemon behind c answered its attach with, when c is an attachment that keeps it.
func ReadyOf(c daemonclient.Caller) (ipc.Ready, bool) {
	r, ok := c.(interface{ Ready() ipc.Ready })
	if !ok {
		return ipc.Ready{}, false
	}
	return r.Ready(), true
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
