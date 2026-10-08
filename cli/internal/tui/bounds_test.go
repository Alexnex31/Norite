// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// What the client holds is bounded against the instance rather than by it (M20a /security-sweep). These
// tests play an instance that breaks its own bounds, which daemontest's fake cannot: it holds every answer
// to the contract, and the contract is what a hostile instance ignores. hostile answers directly, with no
// socket, so a thousand requests cost nothing.

type hostile struct {
	guilds   int // how many guilds the listing names
	channels int // how many text channels each guild's listing names
	listings atomic.Int64
}

func (h *hostile) Do(_ context.Context, _, path string, _ any) (ipc.Result, error) {
	var body any
	switch {
	case path == "/users/@me/guilds":
		gs := make([]apicontract.Guild, h.guilds)
		for i := range gs {
			gs[i] = guild(fmt.Sprint(10_000+i), "G")
		}
		body = gs
	case strings.HasSuffix(path, "/channels"):
		h.listings.Add(1)
		chs := make([]apicontract.Channel, h.channels)
		for i := range chs {
			chs[i] = channel(fmt.Sprint(1_000_000+i), "1", "c")
		}
		body = chs
	default:
		return ipc.Result{Status: 404}, nil
	}
	raw, err := json.Marshal(body)
	return ipc.Result{Status: 200, Body: raw}, err
}

func (h *hostile) Ready() ipc.Ready         { return signedIn("alice") }
func (h *hostile) Events() <-chan ipc.Event { return nil }
func (h *hostile) Done() <-chan struct{}    { return nil }
func (h *hostile) Err() error               { return nil }
func (h *hostile) Close() error             { return nil }

// TestHomeAsksForNoMoreGuildsThanAnAccountCanBeIn: a listing naming more guilds than any server lets an
// account join is read up to that bound, and no further guild's channels are asked for.
func TestHomeAsksForNoMoreGuildsThanAnAccountCanBeIn(t *testing.T) {
	s := &hostile{guilds: maxHomeGuilds + 500, channels: 1}
	msg := loadHome(1, s)().(homeMsg)
	require.NoError(t, msg.err)
	assert.Len(t, msg.entries, maxHomeGuilds)
	assert.EqualValues(t, maxHomeGuilds, s.listings.Load(), "one channel listing per guild kept, and no more")
	assert.True(t, msg.over)

	h := newHome()
	h.load(msg.entries, msg.over)
	assert.Contains(t, h.view(nil, 100, 40), "not all are shown")

	// And what home holds is bounded however it arrives, not only through loadHome.
	var direct homeModel
	direct.load(make([]guildEntry, maxHomeGuilds+1), false)
	assert.Len(t, direct.guilds, maxHomeGuilds)
	assert.True(t, direct.cut)
}

// TestHomeKeepsABoundedNumberOfChannels: guilds whose listings together exceed maxHomeRows are kept up to
// it, and home says so.
func TestHomeKeepsABoundedNumberOfChannels(t *testing.T) {
	s := &hostile{guilds: 100, channels: 600}
	msg := loadHome(1, s)().(homeMsg)
	require.NoError(t, msg.err)
	rows := 0
	for _, e := range msg.entries {
		rows += len(e.channels)
	}
	assert.Equal(t, maxHomeRows, rows)
	assert.True(t, msg.over)

	// Of a channel, home keeps what it draws: the overwrites grow with the guild, and are not drawn.
	assert.Nil(t, msg.entries[0].channels[0].PermissionOverwrites)
}

// TestAnnouncedGuildsPastTheBoundAreNotFetched: an instance announcing a new guild in every frame, once
// home is full, provokes no request and grows nothing.
func TestAnnouncedGuildsPastTheBoundAreNotFetched(t *testing.T) {
	s := &hostile{channels: 1}
	m := New(Options{})
	m.sess = s
	entries := make([]guildEntry, maxHomeGuilds)
	for i := range entries {
		entries[i] = guildEntry{guild: guild(fmt.Sprint(10_000+i), "G")}
	}
	m.home.load(entries, false)

	raw, err := json.Marshal(guild("99999999", "one more"))
	require.NoError(t, err)
	assert.Nil(t, m.onEvent(ipc.Event{Type: "GUILD_CREATE", Data: raw}), "nothing is fetched for it")
	assert.Len(t, m.home.guilds, maxHomeGuilds)
	assert.True(t, m.home.cut)

	// A guild home already holds is still refetched, as GUILD_CREATE for a known guild asks.
	raw, err = json.Marshal(guild("10000", "G"))
	require.NoError(t, err)
	assert.NotNil(t, m.onEvent(ipc.Event{Type: "GUILD_CREATE", Data: raw}))
}

// TestAPaneHoldsAMessageAtMostItsBound: a message as large as a frame is held as what a correct instance
// could have sent — its content, two names — so the pane's count bound is a memory bound too.
func TestAPaneHoldsAMessageAtMostItsBound(t *testing.T) {
	huge := strings.Repeat("界", 200_000)
	m := message("1000", "20", "2", huge, huge)
	tags := make([]apicontract.AppliedMessageTag, 10_000)
	m.Tags, m.ReplyToId = &tags, ptr(huge)

	p := newPane("10", "20")
	p.put(m, 80)
	other := message("1001", "21", "2", "Bob", "elsewhere")
	p.merge([]apicontract.Message{m, other})

	require.Len(t, p.msgs, 1, "a page's message from another channel is not kept")
	held := p.msgs[0]
	assert.LessOrEqual(t, utf8.RuneCountInString(held.Content), ops.MaxContent+1)
	assert.LessOrEqual(t, utf8.RuneCountInString(held.Author.DisplayName), maxName+1)
	assert.LessOrEqual(t, utf8.RuneCountInString(held.Author.Username), maxName+1)
	assert.Nil(t, held.Tags)
	assert.Nil(t, held.ReplyToId)

	// A message within its bounds is kept exactly as sent.
	fine := message("1002", "20", "2", "Bob", strings.Repeat("界", ops.MaxContent))
	p.put(fine, 80)
	assert.Equal(t, fine.Content, p.msgs[1].Content)
	assert.Equal(t, "Bob", p.msgs[1].Author.DisplayName)
}

// TestTheTailDrawsWhatTheWholeLayoutWould: a frame lays out only the newest messages that fill it, and must
// draw exactly the rows a layout of every held message would, at every scroll offset.
func TestTheTailDrawsWhatTheWholeLayoutWould(t *testing.T) {
	p := newPane("10", "20")
	p.resize(60)
	p.loaded = true
	for i := range 80 {
		// Lengths vary so that messages wrap to different heights.
		p.put(message(fmt.Sprint(1000+i), "20", "2", "Bob", strings.Repeat("word ", 1+i%23)), 60)
	}
	const width, height = 60, 20
	area := height - 3
	all := p.tail(width, 1<<30)
	for scroll := 0; scroll <= len(all)-area; scroll++ {
		p.scroll = scroll
		got := strings.Split(p.view(width, height), "\n")[1 : 1+area]
		end := len(all) - scroll
		assert.Equal(t, all[end-area:end], got, "scroll %d", scroll)
	}
}

// TestPagingUpReachesTheOldestAndStops: PgUp walks back to the oldest held message and no further.
func TestPagingUpReachesTheOldestAndStops(t *testing.T) {
	f := newFixture(t)
	for i := range 60 {
		f.held = append(f.held, message(fmt.Sprint(200+i), "20", "2", "Bob", fmt.Sprintf("line %02d", i)))
	}
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil), Channel: "20"}, 80, 24)
	c.shows("line 59")
	for range 30 {
		c.press("pgup")
	}
	top := c.screen()
	assert.Contains(t, top, "line 00", "the oldest message is reachable")
	c.press("pgup")
	assert.Equal(t, top, c.screen(), "and PgUp stops there")
}

// TestAnEditWhileScrolledMovesNothing: a message edited to the same height, while the pane is scrolled up,
// leaves the view where it was; an arrival raises the offset by exactly its own rows.
func TestAnEditWhileScrolledMovesNothing(t *testing.T) {
	p := newPane("10", "20")
	p.loaded = true
	for i := range 40 {
		p.put(message(fmt.Sprint(1000+i), "20", "2", "Bob", "short"), 80)
	}
	p.scroll = 5
	edited := message("1039", "20", "2", "Bob", "edit")
	p.put(edited, 80)
	assert.Equal(t, 5, p.scroll, "an edit of the same height moves nothing")

	p.put(message("2000", "20", "2", "Bob", "one\ntwo"), 80)
	assert.Equal(t, 5+3, p.scroll, "an arrival raises the offset by its header and two lines")
}
