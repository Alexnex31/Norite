// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

func bufferedTypes(t *testing.T, s *session) []string {
	t.Helper()
	var out []string
	for _, b := range s.buf {
		var f gatewayproto.Frame
		require.NoError(t, json.Unmarshal(b.frame, &f))
		out = append(out, *f.T)
	}
	return out
}

// A session not yet READY is a candidate for every event, because it does not know its guilds yet, and a
// FormerMembers event has no rows left to check it against. So a guild deleted while an unrelated account
// was identifying reached that account's pending queue, and was sent after READY: the id of a guild it was
// never in, and the fact that it was just deleted. Only a guild READY listed may be deleted from a stream.
func TestAGuildDeletionReachesOnlyStreamsThatListedTheGuild(t *testing.T) {
	srv := &Server{opts: Options{Logger: zerolog.Nop(), ResumeBuffer: DefaultResumeBuffer}}
	const listed, stranger snowflake.ID = 10, 20
	deleted := func(id snowflake.ID) dispatch.Event {
		return dispatch.Event{Type: "GUILD_DELETE", Audience: dispatch.FormerMembers, GuildID: id, Data: json.RawMessage(`{}`)}
	}

	s := &session{srv: srv, userID: 1}
	s.deliver(deleted(stranger))
	s.deliver(deleted(listed))
	s.becomeReady(ready{}, map[snowflake.ID]struct{}{listed: {}})

	assert.Equal(t, []string{"READY", "GUILD_DELETE"}, bufferedTypes(t, s))
	assert.NotContains(t, s.guilds, listed)
}

// A session dropped while its connection was still attached — an IDENTIFY refused after registering it, or a
// revocation — has no resume window for that connection's close to open.
func TestADroppedSessionOpensNoResumeWindow(t *testing.T) {
	srv := &Server{opts: Options{Logger: zerolog.Nop(), ResumeWindow: DefaultResumeWindow}}
	c := &conn{}
	s := &session{srv: srv, userID: 1}
	s.attach(c)
	s.stop()
	s.detach(c)
	assert.Nil(t, s.expiry)
}

// The frame is appended rather than encoded, so it is held equal to what encoding gatewayproto.Frame produces,
// byte for byte, including for content and a type that need escaping. The payload is built as every payload
// is, by json.Marshal in dispatch.Publisher.Queue: encoding a RawMessage again only re-applies the escaping
// that produced it, which is what makes copying it the same bytes.
func TestAnAppendedFrameIsTheEncodedFrame(t *testing.T) {
	payload, err := json.Marshal(map[string]string{"id": "1", "content": "<b>\"quoted\" & \u2028 é</b>"})
	require.NoError(t, err)
	for _, event := range []string{"MESSAGE_CREATE", `ODD"TYPE<>`} {
		got, err := appendDispatchFrame(42, event, payload)
		require.NoError(t, err)
		seq, typ := int64(42), event
		want, err := json.Marshal(gatewayproto.Frame{Op: gatewayproto.OpDispatch, D: payload, S: &seq, T: &typ})
		require.NoError(t, err)
		assert.Equal(t, string(want), string(got))
	}
}

func candidateIDs(srv *Server, ev dispatch.Event) []string {
	var out []string
	for _, s := range srv.candidates(ev) {
		out = append(out, s.id)
	}
	return out
}

// The index is a pre-filter, so what matters is that it never loses a session that should be a candidate
// and never keeps one that was dropped.
func TestTheSessionIndexFollowsEachSessionsGuilds(t *testing.T) {
	srv := &Server{opts: Options{Logger: zerolog.Nop(), ResumeBuffer: DefaultResumeBuffer}}
	guildEvent := func(g snowflake.ID) dispatch.Event {
		return dispatch.Event{Type: "MESSAGE_CREATE", Audience: dispatch.Guild, GuildID: g, Data: json.RawMessage(`{}`)}
	}
	a := &session{srv: srv, id: "a", userID: 1}
	b := &session{srv: srv, id: "b", userID: 2}
	srv.idx.register(a)
	srv.idx.register(b)

	assert.ElementsMatch(t, []string{"a", "b"}, candidateIDs(srv, guildEvent(10)), "not READY: a candidate for every guild")

	a.becomeReady(ready{}, map[snowflake.ID]struct{}{10: {}})
	b.becomeReady(ready{}, map[snowflake.ID]struct{}{20: {}})
	assert.Equal(t, []string{"a"}, candidateIDs(srv, guildEvent(10)))
	assert.Equal(t, []string{"b"}, candidateIDs(srv, guildEvent(20)))

	b.deliver(dispatch.Event{Type: "GUILD_CREATE", Audience: dispatch.Users, Users: []snowflake.ID{2}, GuildID: 10, Data: json.RawMessage(`{}`)})
	assert.ElementsMatch(t, []string{"a", "b"}, candidateIDs(srv, guildEvent(10)), "GUILD_CREATE joins the guild")
	a.deliver(dispatch.Event{Type: "GUILD_DELETE", Audience: dispatch.Users, Users: []snowflake.ID{1}, GuildID: 10, Data: json.RawMessage(`{}`)})
	assert.Equal(t, []string{"b"}, candidateIDs(srv, guildEvent(10)), "GUILD_DELETE leaves it")

	users := dispatch.Event{Type: "GUILD_CREATE", Audience: dispatch.Users, Users: []snowflake.ID{2, 2, 3}, GuildID: 30}
	assert.Equal(t, []string{"b"}, candidateIDs(srv, users), "named accounts, each once")

	b.stop()
	assert.Empty(t, candidateIDs(srv, guildEvent(10)), "a dropped session is gone")
	assert.Empty(t, candidateIDs(srv, users))
	b.deliver(dispatch.Event{Type: "GUILD_CREATE", Audience: dispatch.Users, GuildID: 40, Data: json.RawMessage(`{}`)})
	assert.Empty(t, candidateIDs(srv, guildEvent(40)), "and an event already on its way does not put it back")

	c := &session{srv: srv, id: "c", userID: 3}
	srv.idx.register(c)
	c.stop()
	c.becomeReady(ready{}, map[snowflake.ID]struct{}{50: {}})
	assert.Empty(t, candidateIDs(srv, guildEvent(50)), "nor does a READY that lost the race with a revocation")
}
