// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/gateway"
)

// identified opens a connection, identifies, and returns it with its session id and READY's sequence.
func identified(t *testing.T, url, token string) (*gatewayClient, string) {
	t.Helper()
	c := dialGateway(t, url, nil)
	c.hello()
	c.identify(token, "dev")
	return c, c.ready().SessionID
}

func (g *gatewayClient) resume(token, sessionID string, seq int64) {
	g.t.Helper()
	g.send(gatewayproto.OpResume, map[string]any{"token": token, "session_id": sessionID, "seq": seq})
}

// notResumable reads the op 9 answer a refused RESUME gets.
func (g *gatewayClient) notResumable() {
	g.t.Helper()
	for {
		f := g.read()
		if f.Op == gatewayproto.OpHeartbeatAck {
			continue
		}
		require.Equal(g.t, gatewayproto.OpInvalidSess, f.Op, "payload: %s", f.D)
		assert.JSONEq(g.t, "false", string(f.D))
		return
	}
}

// sendAndSettle posts messages, then a sentinel, and waits for the watcher to receive the sentinel. Fan-out
// handles one event at a time in publish order, so once the sentinel has reached one session, every message
// before it has reached every session, attached or not: what a test resuming afterwards should be replayed
// is then fixed, rather than depending on how far fan-out had got.
func sendAndSettle(t *testing.T, f *guildFixture, watcher *gatewayClient, channelID string, contents ...string) {
	t.Helper()
	for _, content := range contents {
		send(t, f, f.ownerToken, channelID, content)
	}
	send(t, f, f.ownerToken, channelID, "sentinel")
	for {
		if watcher.expect("MESSAGE_CREATE").field(t, "content") == "sentinel" {
			return
		}
	}
}

// The done-when's third clause: RESUME after a disconnect loses no events. Everything sent while the client
// was away arrives on resuming, in order, with sequence numbers continuing where they stopped, then RESUMED,
// then live events as before.
func TestAResumedSessionLosesNothing(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})

	owner := connected(t, url, f.ownerToken)
	member, sessionID := identified(t, url, f.memberToken)

	send(t, f, f.ownerToken, general, "before")
	owner.expect("MESSAGE_CREATE")
	last := member.expect("MESSAGE_CREATE").s
	require.Equal(t, int64(2), last)

	_ = member.ws.CloseNow()
	sendAndSettle(t, f, owner, general, "one", "two", "three")

	back := dialGateway(t, url, nil)
	back.hello()
	back.resume(f.memberToken, sessionID, last)

	want := []string{"one", "two", "three", "sentinel"}
	seq := last
	for _, content := range want {
		got := back.expect("MESSAGE_CREATE")
		assert.Equal(t, content, got.field(t, "content"))
		assert.Equal(t, seq+1, got.s, "sequence numbers continue without a gap")
		seq = got.s
	}
	resumed := back.expect("RESUMED")
	assert.Equal(t, seq+1, resumed.s, "RESUMED follows the replay")

	send(t, f, f.ownerToken, general, "live")
	live := back.expect("MESSAGE_CREATE")
	assert.Equal(t, "live", live.field(t, "content"))
	assert.Equal(t, resumed.s+1, live.s)
}

// A resume is authenticated exactly as IDENTIFY is. The session id is never enough on its own: another
// account's valid token, holding a stolen id, is told there is nothing to resume, and hears none of it.
func TestASessionIDIsNotEnoughToResumeIt(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})

	owner := connected(t, url, f.ownerToken)
	member, sessionID := identified(t, url, f.memberToken)
	_ = member.ws.CloseNow()
	sendAndSettle(t, f, owner, general, "private to the guild")

	thief := dialGateway(t, url, nil)
	thief.hello()
	thief.resume(f.strangerToken, sessionID, 1)
	thief.notResumable()

	// And the refusal leaves the connection usable: the answer to op 9 is to identify.
	thief.identify(f.strangerToken, "dev")
	thief.ready()

	// The member's own session is untouched by the attempt.
	back := dialGateway(t, url, nil)
	back.hello()
	back.resume(f.memberToken, sessionID, 1)
	assert.Equal(t, "private to the guild", back.expect("MESSAGE_CREATE").field(t, "content"))
}

// Signed out while disconnected: the device's token is still unexpired, and RESUME asks what IDENTIFY asks.
func TestASignedOutDeviceCannotResume(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)

	laptop, sessionID := identified(t, url, f.ownerToken)
	_ = laptop.ws.CloseNow()

	phone := f.api.login("owner@example.com", "owner-phone")
	res := f.api.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(phone.AccessToken))
	require.Equal(t, http.StatusOK, res.Code, res)

	back := dialGateway(t, url, nil)
	back.hello()
	back.resume(f.ownerToken, sessionID, 1)
	assert.Contains(t, back.expectClose(gatewayproto.CloseAuthenticationFailed), "signed out")
}

// A client claiming a frame the server never sent is broken or lying, and is refused outright.
func TestResumingPastWhatWasSentIsRefused(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)

	member, sessionID := identified(t, url, f.memberToken)
	_ = member.ws.CloseNow()

	back := dialGateway(t, url, nil)
	back.hello()
	back.resume(f.memberToken, sessionID, 50)
	back.expectClose(gatewayproto.CloseInvalidSeq)
}

// The buffer is bounded, and a client further behind than it reaches is told to identify afresh rather
// than given a replay with a hole in it.
func TestAGapTheBufferCannotCoverIsNotResumable(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url, _ := customGateway(t, f, func(o *gateway.Options) { o.ResumeBuffer = 3 })
	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})

	owner := connected(t, url, f.ownerToken)
	member, sessionID := identified(t, url, f.memberToken)
	_ = member.ws.CloseNow()
	sendAndSettle(t, f, owner, general, "a", "b", "c", "d", "e")

	back := dialGateway(t, url, nil)
	back.hello()
	back.resume(f.memberToken, sessionID, 1)
	back.notResumable()
}

// A session waits for its client for a bounded time, then is gone.
func TestASessionExpiresIfNotResumedInTime(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url, _ := customGateway(t, f, func(o *gateway.Options) { o.ResumeWindow = 100 * time.Millisecond })

	member, sessionID := identified(t, url, f.memberToken)
	_ = member.ws.CloseNow()
	time.Sleep(400 * time.Millisecond)

	back := dialGateway(t, url, nil)
	back.hello()
	back.resume(f.memberToken, sessionID, 1)
	back.notResumable()
}

// A client that resumes before the server has noticed its old socket die takes the stream over; the old
// connection is closed rather than left holding half of it.
func TestResumingElsewhereClosesTheOldConnection(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})

	old, sessionID := identified(t, url, f.memberToken)

	fresh := dialGateway(t, url, nil)
	fresh.hello()
	fresh.resume(f.memberToken, sessionID, 1)
	fresh.expect("RESUMED")
	old.expectClose(gatewayproto.CloseSessionTimedOut)

	send(t, f, f.ownerToken, general, "after the takeover")
	assert.Equal(t, "after the takeover", fresh.expect("MESSAGE_CREATE").field(t, "content"))
}

// A daemon that reconnects by identifying rather than resuming abandons its old stream each time. Those
// sessions must not pile up against the account's cap, or twenty quick reconnects would lock the account
// out of its own gateway until they expired.
func TestIdentifyingAgainSupersedesTheDevicesAbandonedSessions(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)

	for range 20 {
		c, _ := identified(t, url, f.memberToken)
		_ = c.ws.CloseNow()
	}
	identified(t, url, f.memberToken)
}
