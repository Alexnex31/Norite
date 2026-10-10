// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

// The requests about the daemon updating itself (M24, ADR 0020).
//
// Trust tier (rule 16): the attach socket's, and no other. Changing which program runs as this account is
// the account's own programs' to ask, and the port for scripts refuses every path under LocalPathPrefix.
// Like every request the daemon answers itself they take no body, and what a path carries is one of a
// fixed set of words, never anything of the user's: a daemon older than these requests relays the path to
// its instance.
const (
	// PathUpdate is GET, and answers an UpdateStatus.
	PathUpdate = LocalPathPrefix + "update"
	// PathUpdateCheck is POST: read the release listing now, and answer the UpdateStatus that leaves.
	PathUpdateCheck = PathUpdate + "/check"
	// PathUpdateApply is POST: download, verify and install what a check chose. The answer is sent before
	// the daemon restarts into the new version.
	PathUpdateApply = PathUpdate + "/apply"
	// PathUpdateMode and PathUpdateChannel are POST, with one of the words below after them.
	PathUpdateMode    = PathUpdate + "/mode/"
	PathUpdateChannel = PathUpdate + "/channel/"
)

// What an installation does about a new release.
const (
	// UpdateAuto installs a verified release by itself. The default for an install the installer made.
	UpdateAuto = "auto"
	// UpdateNotify checks, says so, and installs when asked.
	UpdateNotify = "notify"
	// UpdateOff asks nobody anything.
	UpdateOff = "off"
)

// Which releases an installation is offered.
const (
	ChannelStable     = "stable"
	ChannelPrerelease = "prerelease"
)

// EventUpdate tells a watching client that what PathUpdate answers has changed. It carries an empty
// object; the client asks.
const EventUpdate = LocalEventPrefix + "UPDATE"

// UpdateStatus is the answer to every request about updating.
type UpdateStatus struct {
	// Version is the version this daemon is.
	Version string `json:"version"`
	// Mode is UpdateAuto, UpdateNotify or UpdateOff, and Channel ChannelStable or ChannelPrerelease.
	Mode    string `json:"mode"`
	Channel string `json:"channel"`
	// Installs is whether this daemon can install a release itself. When it cannot, Reason says why: a
	// development build, an install the installer did not make, a platform where it is not done.
	Installs bool   `json:"installs"`
	Reason   string `json:"reason"`
	// Available is a newer version the last check chose, or "".
	Available string `json:"available"`
	// Trial is a version installed and not yet confirmed: the one running, until it has stayed up.
	Trial string `json:"trial"`
	// RolledBack is a version that was installed, did not stay up, and was undone, or "".
	RolledBack string `json:"rolled_back"`
	// LastCheck is when the listing was last read, RFC 3339, or "" when it never was.
	LastCheck string `json:"last_check"`
	// LastError is why the last check or installation failed, or "".
	LastError string `json:"last_error"`
}
