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
