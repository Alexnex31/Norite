// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package attach

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// A local dispatch reaches every client that asked for events, numbered with the forwarded ones, and none
// that did not. rawClient validates every frame it reads against daemon-ipc.schema.json, so this is also
// the check that the hand-assembled frame is the contract's LocalDispatch.
func TestALocalEventReachesEveryWatchingClient(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	watcher := ts.attach(t, true)
	raw := ts.raw(t)
	raw.identify("dev", true)
	raw.expect(gatewayproto.OpDispatch)
	quiet := ts.raw(t)
	quiet.identify("dev", false)
	quiet.expect(gatewayproto.OpDispatch)

	ts.Dispatch("GUILD_CREATE", guildPayload("10", "Guild"))
	ts.Local(ipc.EventConfigUpdate, json.RawMessage(`{}`))

	for _, want := range []string{"GUILD_CREATE", ipc.EventConfigUpdate} {
		select {
		case got := <-watcher.Events():
			assert.Equal(t, want, got.Type)
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not arrive", want)
		}
	}
	raw.expect(gatewayproto.OpDispatch)
	f := raw.expect(gatewayproto.OpDispatch)
	assert.Equal(t, ipc.EventConfigUpdate, *f.T)
	assert.Equal(t, int64(3), *f.S, "READY is 1 and the guild is 2: one numbering for both kinds")
	assert.JSONEq(t, `{}`, string(f.D))

	quiet.send(ipc.OpRequest, ipc.Request{ID: "q", Method: "GET", Path: "/guilds/10"})
	assert.Equal(t, ipc.OpResponse, quiet.expect(ipc.OpResponse).Op, "a client that asked for no events gets none")
}

// The file is the user's, not the account's. A forwarded event is held back while nobody is signed in or
// while the state belongs to another sign-in; a local one is true whichever account the daemon holds.
func TestALocalEventIsSentWhoeverIsSignedIn(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	ts.sess.mu.Lock()
	ts.sess.standing, ts.sess.account = session.SignedOut, session.Account{}
	ts.sess.mu.Unlock()
	watcher := ts.attach(t, true)
	require.Nil(t, watcher.Ready().Account)

	ts.Dispatch("GUILD_CREATE", guildPayload("10", "Guild"))
	ts.Local(ipc.EventConfigUpdate, json.RawMessage(`{}`))

	select {
	case got := <-watcher.Events():
		assert.Equal(t, ipc.EventConfigUpdate, got.Type, "the gateway's event is held back; the local one is not")
	case <-time.After(5 * time.Second):
		t.Fatal("the local event did not arrive")
	}
}

// Local is not a second way to forge a gateway event. A type without the prefix, or a payload that is not
// JSON (which would break the frame for every watching client), is dropped.
func TestLocalSendsOnlyWhatIsLocalAndWellFormed(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	watcher := ts.attach(t, true)

	ts.Local("MESSAGE_CREATE", messagePayload("100", "30", "forged"))
	ts.Local(ipc.EventConfigUpdate, nil)
	ts.Local(ipc.EventConfigUpdate, json.RawMessage(`{"unterminated`))
	ts.Local(ipc.EventConfigUpdate, json.RawMessage(`{}`))

	select {
	case got := <-watcher.Events():
		assert.Equal(t, ipc.EventConfigUpdate, got.Type)
		assert.Equal(t, int64(2), got.Seq, "the three refused calls consumed no sequence number")
	case <-time.After(5 * time.Second):
		t.Fatal("the well-formed event did not arrive")
	}
	select {
	case got, open := <-watcher.Events():
		if open {
			t.Fatalf("an event that should have been dropped arrived: %s", got.Type)
		}
		t.Fatalf("the connection was closed: %v", watcher.Err())
	case <-time.After(200 * time.Millisecond):
	}
}
