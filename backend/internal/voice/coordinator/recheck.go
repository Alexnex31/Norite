// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package coordinator

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// onRecheck takes a re-check from the bus. It returns at once: the bus delivers the next message only once
// this one returns, and a re-check reads the database.
//
// What a busy guild costs is bounded here (M33's D4). A process holding no call in the guild does nothing
// but read a map. Re-checks of one guild coalesce, one running and one waiting, so a burst of fifty role
// edits costs two. And the SFU is called only for a call whose right was lost.
func (c *Coordinator) onRecheck(payload []byte) {
	var r dispatch.VoiceRecheck
	if err := json.Unmarshal(payload, &r); err != nil {
		c.log.Error().Err(err).Msg("a voice re-check could not be decoded")
		return
	}
	c.requestRecheck(r.GuildID)
}

// requestRecheck re-checks a guild's calls on a goroutine of its own, coalesced with any running: the bus's
// re-checks, and a join a re-check saw while it was under way.
func (c *Coordinator) requestRecheck(guildID snowflake.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.inGuild[guildID] == 0 {
		return
	}
	if st := c.rechecks[guildID]; st != nil {
		st.again = true
		return
	}
	c.rechecks[guildID] = &recheckState{}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			c.recheck(c.ctx, guildID)
			c.mu.Lock()
			st := c.rechecks[guildID]
			if !st.again || c.ctx.Err() != nil {
				delete(c.rechecks, guildID)
				c.mu.Unlock()
				return
			}
			st.again = false
			c.mu.Unlock()
		}
	}()
}

// local is the calls this process holds in a guild, joins under way included.
func (c *Coordinator) local(guildID snowflake.ID) []*call {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*call
	for _, set := range []map[string]*call{c.calls, c.joining} {
		for _, cl := range set {
			if cl.guild == guildID {
				out = append(out, cl)
			}
		}
	}
	return out
}

// recheck re-resolves the calls this process holds in a guild, channel by channel, against fresh rows.
//
//   - A channel that is gone, or no longer the guild's voice channel, has its room closed.
//   - A caller who lost the right to connect is evicted, with a cutoff that refuses a provision issued before
//     it: a join authorized before the change and provisioned after it.
//   - A caller whose server mute, deafen or Speak changed has its row updated, and the channel is told.
func (c *Coordinator) recheck(ctx context.Context, guildID snowflake.ID) {
	cutoff, err := c.opts.IDs.Next()
	if err != nil {
		c.log.Error().Err(err).Msg("a voice re-check could not mint its cutoff")
		return
	}
	byChannel := map[snowflake.ID][]*call{}
	for _, cl := range c.local(guildID) {
		byChannel[cl.channel] = append(byChannel[cl.channel], cl)
	}
	// The calls kept, and whether each may speak in its own channel, for one update of the guild's rows
	// rather than one per channel, each of which read the whole guild's (M33's /code-review).
	speaks := map[*call]bool{}
	for channelID, calls := range byChannel {
		if err := c.recheckChannel(ctx, guildID, channelID, calls, cutoff, speaks); err != nil {
			c.log.Error().Err(err).Msg("a voice re-check failed")
		}
	}
	if err := c.updateRights(ctx, guildID, speaks); err != nil {
		c.log.Error().Err(err).Msg("a voice re-check could not update its callers' rights")
	}
}

// recheckChannel ends what lost its right in one channel, and adds the calls it keeps to speaks.
func (c *Coordinator) recheckChannel(
	ctx context.Context, guildID, channelID snowflake.ID, calls []*call, cutoff snowflake.ID,
	speaks map[*call]bool,
) error {
	row, err := c.q.GetChannel(ctx, int64(channelID))
	gone := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !gone {
		return err
	}
	if gone || row.GuildID == nil || snowflake.ID(*row.GuildID) != guildID || row.Type != ChannelGuildVoice {
		// The rows went with the channel, so there is nobody in the channel to tell: its CHANNEL_DELETE says as
		// much. Each caller is told why before the room closes, which would close them saying nothing.
		ending := map[*call]string{}
		for _, cl := range calls {
			ending[cl] = endedChannelGone
		}
		c.endAll(ctx, ending)
		if err := c.opts.SFU.CloseRoom(ctx, channelID, cutoff); err != nil {
			c.log.Warn().Err(err).Msg("the SFU could not close a deleted channel's room")
		}
		return nil
	}

	users := make([]snowflake.ID, 0, len(calls))
	seen := map[snowflake.ID]bool{}
	for _, cl := range calls {
		if !seen[cl.user] {
			seen[cl.user] = true
			users = append(users, cl.user)
		}
	}
	rights, err := guildauth.Voice(ctx, c.q, guildID, channelID, users)
	if err != nil {
		return err
	}
	ending := map[*call]string{}
	for _, cl := range calls {
		r, member := rights[cl.user]
		switch {
		case member && r.Connect:
			speaks[cl] = r.Speak
		case member:
			ending[cl] = endedNoConnect
		default:
			ending[cl] = endedNotMember
		}
	}
	// Each told why first. The eviction after it closes whatever else of the user's is in the room, which
	// this process holds no call for, and refuses any provision issued before the cutoff.
	c.endAll(ctx, ending)
	evicted := map[snowflake.ID]bool{}
	for cl := range ending {
		if !evicted[cl.user] {
			evicted[cl.user] = true
			if err := c.opts.SFU.Evict(ctx, channelID, cl.user, cutoff); err != nil {
				c.log.Warn().Err(err).Msg("the SFU could not evict a caller who lost the right")
			}
		}
	}
	return nil
}

// endAll drops and ends each call, saying its reason, all at once: telling a client why waits a moment for
// the reason to leave, and a guild-wide change ending many calls would otherwise wait once per call.
func (c *Coordinator) endAll(ctx context.Context, ending map[*call]string) {
	for cl := range ending {
		c.drop(cl)
	}
	c.endEach(ctx, ending)
}

// drop takes a call off this process's books for a re-check: an established one is forgotten, and a join
// under way is canceled, which it answers as forbidden once its provision returns.
func (c *Coordinator) drop(cl *call) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.joining[cl.session] == cl {
		if cl.canceled == "" {
			cl.canceled = canceledByRecheck
		}
		return
	}
	if c.calls[cl.session] == cl {
		delete(c.calls, cl.session)
		c.forgetLocked(cl)
	}
}

// updateRights writes the server mute, deafen and suppress of the calls kept in a guild, where they
// changed, and tells the channel. A join under way has no row to write yet: it is marked, and asks for
// another re-check once established, since what was read here may be newer than what it recorded.
func (c *Coordinator) updateRights(ctx context.Context, guildID snowflake.ID, speaks map[*call]bool) error {
	if len(speaks) == 0 {
		return nil
	}
	c.mu.Lock()
	for cl := range speaks {
		if c.joining[cl.session] == cl {
			cl.rechecked = true
		}
	}
	c.mu.Unlock()
	users := make([]snowflake.ID, 0, len(speaks))
	held := make(map[snowflake.ID]bool, len(speaks))
	for cl, speak := range speaks {
		users = append(users, cl.user)
		held[cl.conn] = speak
	}
	flags, err := memberFlags(ctx, c.q, guildID, users)
	if err != nil {
		return err
	}
	rows, err := c.q.ListVoiceStatesInGuild(ctx, int64(guildID))
	if err != nil {
		return err
	}
	for _, r := range rows {
		speak, ok := held[snowflake.ID(r.ConnectionID)]
		if !ok {
			continue
		}
		user := snowflake.ID(r.UserID)
		f, suppress := flags[user], !speak
		if r.Mute == f.mute && r.Deaf == f.deaf && r.Suppress == suppress {
			continue
		}
		var err error
		c.rowLock(snowflake.ID(r.ConnectionID), func() {
			err = database.RunInTx(ctx, c.opts.Pool, func(ctx context.Context, tx pgx.Tx) error {
				updated, err := db.New(tx).UpdateVoiceStateRights(ctx, db.UpdateVoiceStateRightsParams{
					ConnectionID: r.ConnectionID, Mute: f.mute, Deaf: f.deaf, Suppress: suppress,
				})
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				if err != nil {
					return err
				}
				return c.queueState(ctx, updated)
			})
		})
		if err != nil {
			return err
		}
	}
	return nil
}
