// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// unevenAudience takes a long time over the first event it is asked about and none over the rest, which is
// the shape that would let a later event overtake an earlier one if they could run side by side.
type unevenAudience struct{ slow string }

func (a unevenAudience) Allowed(_ context.Context, ev dispatch.Event, c []snowflake.ID) ([]snowflake.ID, error) {
	if ev.Type == a.slow {
		time.Sleep(100 * time.Millisecond)
	}
	return c, nil
}

// Lanes run events side by side, and a guild's events must still arrive in the order they were published:
// all of one guild's events take one lane, so a slow audience check holds the events behind it rather than
// letting them pass.
func TestLanesKeepAGuildsEventsInOrder(t *testing.T) {
	srv := &Server{opts: Options{
		Logger: zerolog.Nop(), ResumeBuffer: DefaultResumeBuffer, FanoutLanes: 4,
		Audience: unevenAudience{slow: "MESSAGE_CREATE"},
	}}
	s := &session{srv: srv, id: "a", userID: 1}
	srv.idx.register(s)
	s.becomeReady(ready{}, map[snowflake.ID]struct{}{7: {}})
	srv.startLanes()

	for _, typ := range []string{"MESSAGE_CREATE", "MESSAGE_UPDATE", "MESSAGE_DELETE"} {
		payload, err := json.Marshal(dispatch.Event{Type: typ, Audience: dispatch.Guild, GuildID: 7, Data: json.RawMessage(`{}`)})
		require.NoError(t, err)
		srv.onEvent(payload)
	}
	srv.closeLanes()

	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, []string{"READY", "MESSAGE_CREATE", "MESSAGE_UPDATE", "MESSAGE_DELETE"}, bufferedTypes(t, s))
}
