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
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.inGuild[r.GuildID] == 0 {
		return
	}
	if st := c.rechecks[r.GuildID]; st != nil {
		st.again = true
		return
	}
	c.rechecks[r.GuildID] = &recheckState{}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			c.recheck(c.ctx, r.GuildID)
			c.mu.Lock()
			st := c.rechecks[r.GuildID]
			if !st.again || c.ctx.Err() != nil {
				delete(c.rechecks, r.GuildID)
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
	for channelID, calls := range byChannel {
		if err := c.recheckChannel(ctx, guildID, channelID, calls, cutoff); err != nil {
			c.log.Error().Err(err).Msg("a voice re-check failed")
		}
	}
}

func (c *Coordinator) recheckChannel(
	ctx context.Context, guildID, channelID snowflake.ID, calls []*call, cutoff snowflake.ID,
) error {
	row, err := c.q.GetChannel(ctx, int64(channelID))
	gone := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !gone {
		return err
	}
	if gone || row.GuildID == nil || snowflake.ID(*row.GuildID) != guildID || row.Type != ChannelGuildVoice {
		// The rows went with the channel, so there is nobody to tell: its CHANNEL_DELETE says as much.
		if err := c.opts.SFU.CloseRoom(ctx, channelID, cutoff); err != nil {
			c.log.Warn().Err(err).Msg("the SFU could not close a deleted channel's room")
		}
		for _, cl := range calls {
			c.drop(cl)
			c.end(ctx, cl)
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
	var kept []*call
	evicted := map[snowflake.ID]bool{}
	for _, cl := range calls {
		if r, ok := rights[cl.user]; ok && r.Connect {
			kept = append(kept, cl)
			continue
		}
		if !evicted[cl.user] {
			evicted[cl.user] = true
			// Every connection of the user's in the room, and any provision issued before the cutoff.
			if err := c.opts.SFU.Evict(ctx, channelID, cl.user, cutoff); err != nil {
				c.log.Warn().Err(err).Msg("the SFU could not evict a caller who lost the right")
			}
		}
		c.drop(cl)
		c.end(ctx, cl)
	}
	return c.updateRights(ctx, guildID, kept, rights)
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

// updateRights writes the server mute, deafen and suppress of the established calls kept, where they
// changed, and tells the channel. Joins under way record their rights as they are written.
func (c *Coordinator) updateRights(
	ctx context.Context, guildID snowflake.ID, kept []*call, rights map[snowflake.ID]guildauth.VoiceRights,
) error {
	if len(kept) == 0 {
		return nil
	}
	users := make([]snowflake.ID, 0, len(kept))
	for _, cl := range kept {
		users = append(users, cl.user)
	}
	flags, err := memberFlags(ctx, c.q, guildID, users)
	if err != nil {
		return err
	}
	rows, err := c.q.ListVoiceStatesInGuild(ctx, int64(guildID))
	if err != nil {
		return err
	}
	held := make(map[snowflake.ID]bool, len(kept))
	for _, cl := range kept {
		held[cl.conn] = true
	}
	for _, r := range rows {
		if !held[snowflake.ID(r.ConnectionID)] {
			continue
		}
		user := snowflake.ID(r.UserID)
		f, suppress := flags[user], !rights[user].Speak
		if r.Mute == f.mute && r.Deaf == f.deaf && r.Suppress == suppress {
			continue
		}
		err := database.RunInTx(ctx, c.opts.Pool, func(ctx context.Context, tx pgx.Tx) error {
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
		if err != nil {
			return err
		}
	}
	return nil
}
