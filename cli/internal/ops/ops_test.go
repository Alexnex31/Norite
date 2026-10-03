// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ops_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

var at = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func message(id, content string) apicontract.Message {
	return apicontract.Message{Id: id, ChannelId: "20", AuthorId: ptr("1"), Content: content, CreatedAt: at,
		Author: &apicontract.PublicUser{Id: "1", Username: "alice", DisplayName: "Alice"},
		Tags:   &[]apicontract.AppliedMessageTag{}}
}

func ptr[T any](v T) *T { return &v }

// TestAnIDFromTheInstanceNeverBecomesAPath: the client builds paths from ids the instance sent, and an
// instance is a stranger's server. An id that is not digits is refused before any request exists, so
// "../auth/logout" cannot become one; the relay would refuse it too, and this is the first line.
func TestAnIDFromTheInstanceNeverBecomesAPath(t *testing.T) {
	d := daemontest.New(t)
	ctx := context.Background()
	for _, id := range []string{"../auth/logout", "20/../../instance", "20?limit=1", "", strings.Repeat("9", 21)} {
		// Refused by ops itself, not by the fake finding no such route: the fake is a test's stand-in, and the
		// property is that no request is built at all.
		const refused = "is not an id the instance could have given"
		_, err := ops.ListMessages(ctx, d, id, ops.Page{Limit: 10})
		assert.ErrorContains(t, err, refused, "%q as a channel", id)
		_, err = ops.ListChannels(ctx, d, id)
		assert.ErrorContains(t, err, refused, "%q as a guild", id)
		_, err = ops.SendMessage(ctx, d, id, "hi", nil)
		assert.ErrorContains(t, err, refused, "%q as a channel to send to", id)
		_, err = ops.ListMessages(ctx, d, "20", ops.Page{Limit: 10, Before: &id})
		assert.ErrorContains(t, err, refused, "%q as a cursor", id)
	}
	assert.Empty(t, d.Requests(), "no request was made for any of them")
}

// TestAMessageIsBoundedAsTheInstanceBoundsIt: empty and over-long are refused without a round trip, in
// runes, as the instance's validator counts (M15).
func TestAMessageIsBoundedAsTheInstanceBoundsIt(t *testing.T) {
	d := daemontest.New(t).On("sendMessage", daemontest.Created(message("30", "ok")))
	ctx := context.Background()
	for _, text := range []string{"", "  \n ", strings.Repeat("日", ops.MaxContent+1)} {
		_, err := ops.SendMessage(ctx, d, "20", text, nil)
		var usage *clierr.UsageError
		assert.ErrorAs(t, err, &usage, "%d runes", len([]rune(text)))
	}
	assert.Empty(t, d.Requests())

	_, err := ops.SendMessage(ctx, d, "20", strings.Repeat("日", ops.MaxContent), ptr("29"))
	require.NoError(t, err, "4,000 runes fit, though they are 12,000 bytes")
	require.Len(t, d.Requests(), 1)
	assert.JSONEq(t, `{"content":"`+strings.Repeat("日", ops.MaxContent)+`","reply_to_id":"29"}`,
		string(d.Requests()[0].Body))
}

// TestAPageAsksForWhatItWasGiven: limit always, and the cursors only when set.
func TestAPageAsksForWhatItWasGiven(t *testing.T) {
	d := daemontest.New(t).On("listChannelMessages", daemontest.OK([]apicontract.Message{message("30", "hi")}))
	ctx := context.Background()
	got, err := ops.ListMessages(ctx, d, "20", ops.Page{Limit: 50, Before: ptr("40")})
	require.NoError(t, err)
	require.Len(t, got, 1)
	q := d.Requests()[0].Query
	assert.Equal(t, "50", q.Get("limit"))
	assert.Equal(t, "40", q.Get("before"))
	assert.False(t, q.Has("after"))

	_, err = ops.ListMessages(ctx, d, "20", ops.Page{Limit: ops.MaxPage + 1})
	var usage *clierr.UsageError
	assert.ErrorAs(t, err, &usage)
}

// TestASentMessageComesBackAsAnEvent is the fake's half of the client's done-when: a message the fake
// accepts reaches every client watching, the sender's included, numbered as the daemon numbers dispatches
// (READY is 1); a client that asked for no events gets none; dropping a client ends its stream with the
// reason, as a daemon stopping does.
func TestASentMessageComesBackAsAnEvent(t *testing.T) {
	d := daemontest.New(t).On("sendMessage", daemontest.Created(message("30", "hello")))
	signedIn := ipc.Ready{Standing: "signed_in", Guilds: []ipc.GuildSummary{}}
	sender, watcher, quiet := d.Attach(signedIn, true), d.Attach(signedIn, true), d.Attach(signedIn, false)

	_, err := ops.SendMessage(context.Background(), sender, "20", "hello", nil)
	require.NoError(t, err)
	for name, c := range map[string]*daemontest.Client{"sender": sender, "watcher": watcher} {
		select {
		case ev := <-c.Events():
			assert.Equal(t, "MESSAGE_CREATE", ev.Type, name)
			assert.Equal(t, int64(2), ev.Seq, name)
			assert.Contains(t, string(ev.Data), `"hello"`, name)
		case <-time.After(time.Second):
			t.Fatalf("%s received nothing", name)
		}
	}
	assert.Nil(t, quiet.Events())

	d.Drop(&ipc.CloseError{Code: ipc.CloseResync, Reason: "resync"})
	_, open := <-watcher.Events()
	assert.False(t, open, "a dropped client's stream ends")
	var ce *ipc.CloseError
	require.ErrorAs(t, watcher.Err(), &ce)
	assert.Equal(t, ipc.CloseResync, ce.Code)
	_, err = ops.ListGuilds(context.Background(), watcher)
	assert.Error(t, err, "a dropped client performs nothing")
}
