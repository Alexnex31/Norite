// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// TestHomeSaysWhatTheDaemonKnowsAboutANewerRelease: asked when the client attaches, asked again when the
// daemon says it changed, and one sentence on home's first line (7a's health block, until M42 draws it).
func TestHomeSaysWhatTheDaemonKnowsAboutANewerRelease(t *testing.T) {
	f := newFixture(t)
	f.d.OnLocal(ipc.PathUpdate, ipc.UpdateStatus{Version: "0.1.0", Installs: true, Available: "0.1.1"})
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil)}, 100, 24)
	c.shows("↑ 0.1.1 ready")

	// The daemon installed it and is running it: this client is the older program.
	f.d.OnLocal(ipc.PathUpdate, ipc.UpdateStatus{Version: "0.1.1", Installs: true, Trial: "0.1.1"})
	f.d.Dispatch(ipc.EventUpdate, []byte(`{}`))
	c.shows("↑ 0.1.1 installed · restart norite")
	assert.NotContains(t, c.screen(), "ready")

	// It did not stay up.
	f.d.OnLocal(ipc.PathUpdate, ipc.UpdateStatus{Version: "0.1.0", Installs: true, RolledBack: "0.1.1", Available: "0.1.2"})
	f.d.Dispatch(ipc.EventUpdate, []byte(`{}`))
	c.shows("↓ 0.1.1 undone")

	// Nothing to say is nothing drawn.
	f.d.OnLocal(ipc.PathUpdate, ipc.UpdateStatus{Version: "0.1.0", Installs: true})
	f.d.Dispatch(ipc.EventUpdate, []byte(`{}`))
	c.until("the notice is gone", func(screen string) bool { return !strings.ContainsAny(screen, "↑↓") })
}

// A daemon that installs nothing still says a release is out, and does not call it ready.
func TestAReleaseThisDaemonWillNotInstallIsOutAndNotReady(t *testing.T) {
	f := newFixture(t)
	f.d.OnLocal(ipc.PathUpdate, ipc.UpdateStatus{Version: "0.1.0", Available: "0.1.1", Reason: "not made by the installer"})
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil)}, 100, 24)
	c.shows("↑ 0.1.1 is out")
	assert.NotContains(t, c.screen(), "ready")
}

// A daemon from before the request refuses it, and the client draws home as it always did.
func TestADaemonThatDoesNotKnowTheRequestIsNoError(t *testing.T) {
	f := newFixture(t)
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil)}, 100, 24)
	c.shows("signed in as @alice")
	assert.NotContains(t, c.screen(), "↑")
	assert.NotContains(t, c.screen(), "trying again")
}

// The version is the daemon's text: cleaned, and cut, before it is drawn on every frame.
func TestTheNoticeIsBoundedAndInert(t *testing.T) {
	n := noticeOf(ipc.UpdateStatus{Installs: true, Available: "0.1.1\x1b[2J\u202e" + strings.Repeat("9", 500)})
	if n == nil {
		t.Fatal("no notice")
	}
	if strings.ContainsRune(n.text, 0x1b) || strings.ContainsRune(n.text, 0x202e) {
		t.Errorf("the notice carries what acts on a terminal: %q", n.text)
	}
	if got := len([]rune(n.text)); got > maxNoticeVersion+16 {
		t.Errorf("the notice is %d runes", got)
	}
	// Each state has its own tone, and undone is said before anything else.
	for want, st := range map[int]ipc.UpdateStatus{
		toneUndone:    {RolledBack: "0.1.1", Trial: "0.1.2", Available: "0.1.3", Installs: true},
		toneInstalled: {Trial: "0.1.2", Available: "0.1.3", Installs: true},
		toneReady:     {Available: "0.1.3", Installs: true},
	} {
		if got := noticeOf(st); got == nil || got.tone != want {
			t.Errorf("status %+v: notice %+v", st, got)
		}
	}
}

// An answer that names an attachment which has gone is dropped, as every answer is: it describes a daemon
// this client is no longer talking to.
func TestAnAnswerFromAnAttachmentThatHasGoneIsDropped(t *testing.T) {
	f := newFixture(t)
	// The live attachment's own answer is waited for first, or it would arrive after the stale one and
	// replace it, and this would pass whatever the client did with a stale answer.
	f.d.OnLocal(ipc.PathUpdate, ipc.UpdateStatus{Version: "0.1.0", Installs: true, Available: "0.1.1"})
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil)}, 100, 24)
	c.shows("↑ 0.1.1 ready")
	c.send(updateMsg{gen: -7, notice: &updateNotice{text: "↑ 9.9.9 ready"}})
	c.settle()
	assert.NotContains(t, c.screen(), "9.9.9")
	assert.Contains(t, c.screen(), "↑ 0.1.1 ready")
}
