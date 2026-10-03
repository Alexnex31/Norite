// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// Guild invites (M20a, moved from M57): the only way into a guild somebody else created.
//
// They live in this package rather than one of their own, which the architecture's domain list would
// suggest, for the reason channels and roles do: joining is a membership operation, the inverse of
// RemoveMember, and it writes the guild's own audit log and the same membership row. A separate package
// would carry its own copies of Guild, Member and the audit writer, each pinned to these by a test.
//
// # The code, and where it may travel
//
// M10's instance-invite code exactly: sixteen characters of the unambiguous alphabet, minted by
// auth.NewInviteCode and normalized by auth.ParseInviteCode, so the two kinds of invite cannot drift into
// two formats. **It never travels in a request path.** The request logger writes every path, so a code in
// one is a capability in a log (ADR 0029, which moved M10's revoke off a path for exactly this). Preview,
// redemption and revocation take it in the body, and nothing here logs it.
//
// # One answer for every dead code
//
// An unknown code, an expired one, a revoked one and an exhausted one all answer 404, and a malformed one
// too. Distinguishing them tells somebody with no valid code which codes once existed. Every statement
// that answers for a code asks about expiry and uses itself (see invites.sql), so the deletions that tidy
// spent and revoked invites away are not what keeps them quiet.

// The bounds on what an invite may ask for.
const (
	// MaxInviteMaxAge bounds a finite life. A longer one is better asked for as "never", which says what it
	// means; thirty days is long enough for any invitation sent to a person.
	MaxInviteMaxAge = 30 * 24 * time.Hour
	// MaxInviteUses bounds a counted invite. Zero means unlimited.
	MaxInviteUses = 1000
)

// Invite is a guild invite as its guild sees it: the code in full, because the guild needs it back.
type Invite struct {
	ID        snowflake.ID     `json:"id"`
	Code      string           `json:"code"`
	GuildID   snowflake.ID     `json:"guild_id"`
	ChannelID snowflake.ID     `json:"channel_id"`
	InviterID snowflake.ID     `json:"inviter_id"`
	Inviter   *auth.PublicUser `json:"inviter"`
	MaxUses   *int32           `json:"max_uses"`
	Uses      int32            `json:"uses"`
	ExpiresAt *time.Time       `json:"expires_at"`
	CreatedAt time.Time        `json:"created_at"`
}

func inviteFromRow(row db.Invite, inviter *auth.PublicUser) Invite {
	return Invite{
		ID:        snowflake.ID(row.ID),
		Code:      row.Code,
		GuildID:   snowflake.ID(row.GuildID),
		ChannelID: snowflake.ID(row.ChannelID),
		InviterID: snowflake.ID(row.InviterID),
		Inviter:   inviter,
		MaxUses:   row.MaxUses,
		Uses:      row.Uses,
		ExpiresAt: timeOrNil(row.ExpiresAt),
		CreatedAt: row.CreatedAt.Time,
	}
}

// InvitePreview is where a code leads, shown to whoever holds it before they use it.
//
// Deliberately less than a Guild: the name, description and icon a guild shows on its door, and the
// channel the joiner will land in. Not its owner, its settings or its membership — the person reading this
// is not in it yet.
type InvitePreview struct {
	Code      string           `json:"code"`
	ExpiresAt *time.Time       `json:"expires_at"`
	Guild     PreviewGuild     `json:"guild"`
	Channel   PreviewChannel   `json:"channel"`
	Inviter   *auth.PublicUser `json:"inviter"`
}

// PreviewGuild is the guild an invite leads to, as a stranger may see it.
type PreviewGuild struct {
	ID          snowflake.ID `json:"id"`
	Name        string       `json:"name"`
	Description *string      `json:"description"`
	IconHash    *string      `json:"icon_hash"`
}

// PreviewChannel is the channel an invite lands its joiner in.
type PreviewChannel struct {
	ID   snowflake.ID `json:"id"`
	Name *string      `json:"name"`
}

func timeOrNil(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// CreateInviteInput is a request to create an invite into a channel.
type CreateInviteInput struct {
	ChannelID snowflake.ID
	// MaxUses is how many accounts may join with it; zero is unlimited.
	MaxUses int32
	// MaxAge is how long it lives; zero never expires.
	MaxAge time.Duration
}

// CreateInvite issues an invite into a channel.
//
// PermCreateInvite on the channel, resolved with its overwrites, and the view bit with it: the channel is
// where the joiner lands, so somebody who cannot see it is not the person to send people there. The
// guild comes off the channel row rather than from the request (rule 1, M12's UpdateChannel reasoning).
//
// Not withheld from an Instance Admin, whom guildauth passes at layer 1: the tier reaches as far as a
// guild owner here and no further, which is M17's settlement for tags.
func (s *Service) CreateInvite(ctx context.Context, actor auth.Actor, in CreateInviteInput) (Invite, error) {
	switch {
	case in.MaxUses < 0 || in.MaxUses > MaxInviteUses:
		return Invite{}, httpx.Errorf(httpx.ErrBadRequest, "max_uses must be between 0 and %d", MaxInviteUses)
	case in.MaxAge < 0 || in.MaxAge > MaxInviteMaxAge:
		return Invite{}, httpx.Errorf(httpx.ErrBadRequest,
			"expires_in_seconds must be at most %d", int64(MaxInviteMaxAge/time.Second))
	}

	id, err := s.ids.Next()
	if err != nil {
		return Invite{}, fmt.Errorf("guilds: mint invite id: %w", err)
	}

	var out Invite
	err = s.inTx(ctx, func(ctx context.Context, q *db.Queries) error {
		// Unlocked: the audit entry describes the invite, not the channel row, which is the test
		// guildauth.AuthorizeChannel gives for when the lock is owed.
		channel, guildID, _, err := guildauth.AuthorizeChannelUnlocked(
			ctx, q, actor, in.ChannelID, roles.PermCreateInvite,
		)
		if err != nil {
			return err
		}
		// Text only, for the reason a message may only be sent into one: an invite lands its joiner in
		// the channel, and M20a's client opens text channels and nothing else. Allowing voice later is
		// additive; refusing it later would strand every voice invite already issued.
		if channel.Type != ChannelGuildText {
			return httpx.Errorf(ErrUnsupportedChannelType, "an invite can only lead into a text channel")
		}

		// The ceiling, after authorization so it is not something a non-member can measure. Counted
		// without a lock, as the channel ceiling is: two concurrent creates can overshoot it by one each,
		// which bounds a list rather than guarding anything a race could abuse.
		live, err := q.CountLiveGuildInvites(ctx, int64(guildID))
		if err != nil {
			return fmt.Errorf("guilds: count invites: %w", err)
		}
		if live >= int64(s.maxInvitesPerGuild) {
			return httpx.Errorf(ErrGuildFull, "a guild may hold at most %d live invites", s.maxInvitesPerGuild)
		}

		params := db.CreateGuildInviteParams{
			ID: int64(id), GuildID: int64(guildID), ChannelID: int64(in.ChannelID), InviterID: int64(actor.UserID),
		}
		if in.MaxUses > 0 {
			params.MaxUses = &in.MaxUses
		}
		if in.MaxAge > 0 {
			// Seconds, and the database adds them to its own clock: see CreateGuildInvite.
			seconds := int64(in.MaxAge / time.Second)
			params.ExpiresInSeconds = &seconds
		}

		// Retried on collision rather than checked first, as M10's are, and three collisions at 69 bits is a
		// broken generator, reported rather than looped on. The statement answers a collision with no row
		// instead of an error: this runs in a transaction, and an error would abort it, so the first version
		// of this loop could never actually retry (M20a /code-review).
		var row db.Invite
		for attempt := 0; ; attempt++ {
			if params.Code, err = s.newInviteCode(); err != nil {
				return err
			}
			row, err = q.CreateGuildInvite(ctx, params)
			if err == nil {
				break
			}
			if errors.Is(err, pgx.ErrNoRows) {
				if attempt < 2 {
					continue
				}
				return errors.New("guilds: could not generate an unused invite code")
			}
			// The channel or its guild was deleted after the unlocked authorization read it: the insert's
			// foreign-key check waited for that deletion and then refused. That is the 404 the read would
			// have given a moment later, as messages' channelVanished maps it.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.ForeignKeyViolation {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: create invite: %w", err)
		}

		// Rule 2. The code is not recorded: the log is read under PermViewAuditLog, which is not the bit
		// that lists live codes, and a live code is a way in. The invite's id ties this entry to its
		// revocation and to every join it brought.
		changes := auditDiff{}
		changes.context("channel_id", snowflake.ID(row.ChannelID))
		if row.MaxUses != nil {
			changes.created("max_uses", *row.MaxUses)
		}
		if t := timeOrNil(row.ExpiresAt); t != nil {
			changes.created("expires_at", *t)
		}
		inviteID := snowflake.ID(row.ID)
		if err := s.writeAudit(
			ctx, q, guildID, actor.UserID, ActionInviteCreate, &inviteID, changes.payload(),
		); err != nil {
			return err
		}

		creator, err := q.GetUserByID(ctx, int64(actor.UserID))
		if err != nil {
			return fmt.Errorf("guilds: get inviter: %w", err)
		}
		out = inviteFromRow(row, &auth.PublicUser{
			ID: actor.UserID, Username: creator.Username, DisplayName: creator.DisplayName,
		})
		return nil
	})
	return out, err
}

// ListInvites returns a guild's live invites, codes in full, into the channels the caller can see.
//
// PermManageGuild, the bit that decides who the guild is for. Creating an invite is a smaller grant than
// reading every outstanding one: whoever can list them can hand any of them on.
//
// **Filtered by channel visibility**, as the channel listing is. An invite names its channel, so listing
// one into a channel hidden from the caller tells them the channel exists and hands them a way into it —
// M14's "a channel you cannot see is not yours to manage", which M20a's first version missed
// (/code-review).
func (s *Service) ListInvites(ctx context.Context, actor auth.Actor, guildID snowflake.ID) ([]Invite, error) {
	allowed, err := guildauth.Authorize(ctx, s.queries, actor, guildID, 0, roles.PermManageGuild)
	if err != nil {
		return nil, err
	}
	rows, err := s.queries.ListGuildInvites(ctx, int64(guildID))
	if err != nil {
		return nil, fmt.Errorf("guilds: list invites: %w", err)
	}
	if len(rows) == 0 {
		return []Invite{}, nil
	}

	// One read of the overwrites for every channel the invites lead into, grouped once, as ListChannels
	// does for the same reason.
	seen := make(map[int64]bool, len(rows))
	var channelIDs []int64
	for _, r := range rows {
		if !seen[r.Invite.ChannelID] {
			seen[r.Invite.ChannelID] = true
			channelIDs = append(channelIDs, r.Invite.ChannelID)
		}
	}
	overwrites, err := s.queries.ListGuildPermissionOverwrites(ctx, db.ListGuildPermissionOverwritesParams{
		ChannelIds: channelIDs, GuildID: int64(guildID),
	})
	if err != nil {
		return nil, fmt.Errorf("guilds: list invite channels' overwrites: %w", err)
	}
	byChannel := make(map[snowflake.ID][]db.PermissionOverwrite, len(channelIDs))
	for _, ow := range overwrites {
		byChannel[snowflake.ID(ow.ChannelID)] = append(byChannel[snowflake.ID(ow.ChannelID)], ow)
	}

	out := make([]Invite, 0, len(rows))
	for _, r := range rows {
		channel := snowflake.ID(r.Invite.ChannelID)
		if !allowed.AllowsInChannel(channel, byChannel[channel], roles.PermViewChannel) {
			continue
		}
		inviter := auth.PublicUserOf(r.Invite.InviterID, r.InviterUsername, r.InviterDisplayName)
		out = append(out, inviteFromRow(r.Invite, inviter))
	}
	return out, nil
}

// PreviewInvite says where a live code leads, to any signed-in account holding it.
//
// No authorization beyond the credential, deliberately: an invite is the guild's own decision to show
// itself to whoever holds the code, and the preview is how the person given one decides whether to use it.
// A signed-in account only (M20a's G1), so the endpoint is not an anonymous oracle for codes; a browser
// preview for somebody without an account is Phase O's to open, and opening it later is additive.
func (s *Service) PreviewInvite(ctx context.Context, rawCode string) (InvitePreview, error) {
	code, err := auth.ParseInviteCode(rawCode)
	if err != nil {
		return InvitePreview{}, httpx.ErrNotFound
	}
	row, err := s.queries.GetGuildInvitePreview(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return InvitePreview{}, httpx.ErrNotFound
		}
		return InvitePreview{}, fmt.Errorf("guilds: preview invite: %w", err)
	}
	return InvitePreview{
		Code:      row.Code,
		ExpiresAt: timeOrNil(row.ExpiresAt),
		Guild: PreviewGuild{
			ID: snowflake.ID(row.GuildID), Name: row.GuildName,
			Description: row.GuildDescription, IconHash: row.GuildIconHash,
		},
		Channel: PreviewChannel{ID: snowflake.ID(row.ChannelID), Name: row.ChannelName},
		Inviter: auth.PublicUserOf(row.InviterID, row.InviterUsername, row.InviterDisplayName),
	}, nil
}

// RedeemInvite joins the caller to the guild a live code leads to, and returns that guild.
//
// # In order, and why
//
//  1. The account's ownership lock (slot 4), first. The joined ceiling is a count over the account's
//     memberships, and guild creation, which adds one, takes the same lock — so two redemptions and a
//     creation cannot each see room for one more. M13a's lesson: a lock on the row being changed does not
//     cover a count over rows it does not touch.
//  2. The invite read, to learn its guild. A current member stops here with that guild and nothing spent:
//     a second click is not an error, and it is not a second use either.
//  3. The ceiling.
//  4. One statement spends the use, with every guard in its WHERE, so concurrent redemptions of a counted
//     invite are decided by its row lock and exactly max_uses of them get in (M10's shape). The read in
//     step 2 decides nothing a racer could change: if the invite expired or ran out in between, the
//     statement matches nothing and the answer is the same 404.
//  5. The membership, the audit entry and the events, in the same transaction.
//
// An exhausted invite is deleted in step 4's transaction, which keeps the guild's list and count tidy; every
// read asks about uses as well, so a deletion that did not happen would disclose nothing.
//
// No ban list exists to consult: guild bans are built by no milestone yet, and M74 owns that question
// (docs/security-ledger.md). A kicked member can come back through any live invite, which is also what a
// kick means on Discord.
func (s *Service) RedeemInvite(ctx context.Context, actor auth.Actor, rawCode string) (Guild, error) {
	code, err := auth.ParseInviteCode(rawCode)
	if err != nil {
		return Guild{}, httpx.ErrNotFound
	}

	var out Guild
	err = s.inTx(ctx, func(ctx context.Context, q *db.Queries) error {
		if err := q.LockAccountOwnership(ctx, int64(actor.UserID)); err != nil {
			return fmt.Errorf("guilds: lock account: %w", err)
		}

		invite, err := q.GetLiveGuildInvite(ctx, code)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get invite: %w", err)
		}
		guildID := snowflake.ID(invite.GuildID)

		// The guild before the invite, which is the order deleting a guild takes them in, and FOR SHARE so
		// the row answered with is not one a transfer or rename in flight is changing. LockGuildForShare
		// says why each half matters; both were /code-review findings on M20a. A guild deleted before this
		// point is the 404 an unknown code gets.
		guild, err := q.LockGuildForShare(ctx, invite.GuildID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: lock guild: %w", err)
		}
		out = guildFromRow(guild)

		if _, err := q.GetGuildMember(ctx, db.GetGuildMemberParams{
			GuildID: invite.GuildID, UserID: int64(actor.UserID),
		}); err == nil {
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("guilds: get membership: %w", err)
		}

		joined, err := q.CountGuildsJoinedBy(ctx, int64(actor.UserID))
		if err != nil {
			return fmt.Errorf("guilds: count joined guilds: %w", err)
		}
		if joined >= int64(s.maxJoinedGuildsPerAccount) {
			return httpx.Errorf(ErrGuildFull,
				"an account may be in at most %d guilds", s.maxJoinedGuildsPerAccount)
		}

		spent, err := q.RedeemGuildInvite(ctx, code)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: redeem invite: %w", err)
		}
		if spent.MaxUses != nil && spent.Uses >= *spent.MaxUses {
			if _, err := q.DeleteGuildInvite(ctx, spent.ID); err != nil {
				return fmt.Errorf("guilds: delete spent invite: %w", err)
			}
		}

		// Neither a unique violation nor a foreign-key one can reach here: the account lock serializes this
		// account's redemptions, the membership read above saw none, and the guild is held FOR KEY SHARE.
		member, err := q.AddGuildMember(ctx, db.AddGuildMemberParams{
			GuildID: spent.GuildID, UserID: int64(actor.UserID),
		})
		if err != nil {
			return fmt.Errorf("guilds: add member: %w", err)
		}

		changes := auditDiff{}
		changes.context("invite_id", snowflake.ID(spent.ID))
		changes.context("inviter_id", snowflake.ID(spent.InviterID))
		if err := s.writeAudit(
			ctx, q, guildID, actor.UserID, ActionMemberJoin, &actor.UserID, changes.payload(),
		); err != nil {
			return err
		}

		// The joiner first, by name, as Create does for an owner: the event is what adds the guild to their
		// live connections, so the guild's later events — the next one included — find them.
		if err := s.events.Queue(ctx, dispatch.Event{
			Type: "GUILD_CREATE", Audience: dispatch.Users, GuildID: guildID, Users: []snowflake.ID{actor.UserID},
		}, out); err != nil {
			return err
		}
		// A Member, as GUILD_MEMBER_UPDATE carries and the member listing returns: not the joiner's name.
		// No REST read returns another member's name, and the gateway never discloses more than REST
		// (M18). The first version carried it and /code-review found the gap; the name reaches the
		// guild when the joiner first posts, on the message, which REST serves too.
		return s.events.Queue(ctx, dispatch.Event{
			Type: "GUILD_MEMBER_ADD", Audience: dispatch.Guild, GuildID: guildID,
		}, memberFromRow(member, []snowflake.ID{}))
	})
	if err != nil {
		return Guild{}, err
	}
	return out, nil
}

// RevokeInvite deletes an invite: its creator's to do, or a PermManageGuild holder's.
//
// Authorized against the invite's own guild, read off its row (rule 1): somebody holding a code for a
// guild they are not in gets the 404 an unknown code gets, from the same chokepoint every guild route uses.
// The creator still needs to be a member, so an invite outlives its creator's departure until a moderator
// revokes it or it expires.
//
// And against its channel: an invite into a channel the caller cannot see answers 404, as the listing
// leaves it out, rather than letting a manager act on a channel they are not shown (M14's rule).
func (s *Service) RevokeInvite(ctx context.Context, actor auth.Actor, rawCode string) error {
	code, err := auth.ParseInviteCode(rawCode)
	if err != nil {
		return httpx.ErrNotFound
	}
	return s.inTx(ctx, func(ctx context.Context, q *db.Queries) error {
		invite, err := q.GetLiveGuildInvite(ctx, code)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get invite: %w", err)
		}
		guildID := snowflake.ID(invite.GuildID)

		// Membership, then the channel, then the permission, so the refusals come in that order: a caller
		// who cannot see the invite's channel is answered as though the code named nothing, 404, before
		// anything about their authority is said. Asking for PermManageGuild first answered such a caller
		// 403 (M20a's second /code-review), which says the channel is there.
		allowed, err := guildauth.Authorize(ctx, q, actor, guildID, 0, 0)
		if err != nil {
			return err
		}
		overwrites, err := q.ListChannelPermissionOverwrites(ctx, db.ListChannelPermissionOverwritesParams{
			ChannelID: invite.ChannelID, GuildID: invite.GuildID,
		})
		if err != nil {
			return fmt.Errorf("guilds: list channel overwrites: %w", err)
		}
		if !allowed.AllowsInChannel(snowflake.ID(invite.ChannelID), overwrites, roles.PermViewChannel) {
			return httpx.ErrNotFound
		}
		if snowflake.ID(invite.InviterID) != actor.UserID && !allowed.Allows(roles.PermManageGuild) {
			return httpx.ErrForbidden
		}

		// The row as deleted, so the entry records the uses it had when it went: the read above was
		// unlocked, and a redemption committing in between would otherwise go unrecorded.
		deleted, err := q.DeleteGuildInvite(ctx, invite.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Revoked or spent by somebody else since the read.
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: delete invite: %w", err)
		}

		changes := auditDiff{}
		changes.context("channel_id", snowflake.ID(deleted.ChannelID))
		changes.context("inviter_id", snowflake.ID(deleted.InviterID))
		changes.context("uses", deleted.Uses)
		inviteID := snowflake.ID(deleted.ID)
		return s.writeAudit(ctx, q, guildID, actor.UserID, ActionInviteRevoke, &inviteID, changes.payload())
	})
}
