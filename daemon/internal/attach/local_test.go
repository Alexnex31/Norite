// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package attach

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
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

// The instance cannot speak in the daemon's namespace. A gateway event typed as a local one is dropped: a
// client could not tell it from the daemon's own, and would re-read its config for every frame a hostile
// instance sent.
func TestTheInstanceCannotForgeALocalEvent(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	watcher := ts.attach(t, true)

	ts.Dispatch(ipc.EventConfigUpdate, json.RawMessage(`{}`))
	ts.Dispatch(ipc.LocalEventPrefix+"ANYTHING_LATER", json.RawMessage(`{"x":1}`))
	ts.Dispatch("GUILD_CREATE", guildPayload("10", "Guild"))

	select {
	case got := <-watcher.Events():
		assert.Equal(t, "GUILD_CREATE", got.Type, "the forged events were not forwarded, and the real one was")
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway's own event did not arrive")
	}
}

type localFunc func(ctx context.Context, req ipc.Request) ipc.Response

func (f localFunc) Do(ctx context.Context, req ipc.Request) ipc.Response { return f(ctx, req) }

// A request under the daemon's own prefix is answered by the daemon and never handed to the relay, which
// would send it to the instance. It is answered with nobody signed in, since it is about this machine. The
// client-side frame is validated by rawClient, so a local request is the contract's Request unchanged.
func TestALocalRequestIsAnsweredHereAndNeverRelayed(t *testing.T) {
	// Each is answered on the connection's own goroutine, so what was asked is kept under a lock.
	var mu sync.Mutex
	var relayed, asked []string
	relay := relayFunc(func(_ context.Context, req ipc.Request) ipc.Response {
		mu.Lock()
		relayed = append(relayed, req.Path)
		mu.Unlock()
		status := 200
		return ipc.Response{Status: &status, Body: json.RawMessage(`{}`)}
	})
	local := localFunc(func(_ context.Context, req ipc.Request) ipc.Response {
		mu.Lock()
		asked = append(asked, req.Method+" "+req.Path)
		mu.Unlock()
		status := 200
		return ipc.Response{Status: &status, Body: json.RawMessage(`{"split":true}`)}
	})
	ts := newTestServer(t, relay, func(s *Server) { s.local = local })
	ts.sess.mu.Lock()
	ts.sess.standing, ts.sess.account = session.SignedOut, session.Account{}
	ts.sess.mu.Unlock()

	raw := ts.raw(t)
	raw.identify("dev", false)
	raw.expect(gatewayproto.OpDispatch)
	raw.send(ipc.OpRequest, ipc.Request{ID: "a", Method: "POST", Path: ipc.PathConfigSplit})
	f := raw.expect(ipc.OpResponse)
	var resp ipc.Response
	require.NoError(t, json.Unmarshal(f.D, &resp))
	assert.Equal(t, "a", resp.ID)
	require.NotNil(t, resp.Status)
	assert.JSONEq(t, `{"split":true}`, string(resp.Body))

	raw.send(ipc.OpRequest, ipc.Request{ID: "b", Method: "GET", Path: "/guilds/1"})
	raw.expect(ipc.OpResponse)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"POST " + ipc.PathConfigSplit}, asked)
	assert.Equal(t, []string{"/guilds/1"}, relayed, "and an ordinary request still goes to the relay, and only it")
}

// A daemon with nothing to answer local requests refuses them itself. The relay is not asked.
func TestALocalRequestWithNothingToAnswerItIsRefusedNotRelayed(t *testing.T) {
	var relayed atomic.Bool
	relay := relayFunc(func(context.Context, ipc.Request) ipc.Response {
		relayed.Store(true)
		return ipc.Response{}
	})
	ts := newTestServer(t, relay)
	c := ts.attach(t, false)
	_, err := c.Do(context.Background(), "POST", ipc.PathConfigSplit, nil)
	var re *ipc.RelayError
	require.ErrorAs(t, err, &re)
	assert.Equal(t, ipc.RelayBadRequest, re.Code)
	assert.False(t, relayed.Load())
}
