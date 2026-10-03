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

// PublicUser is the part of an account other people see: messages' author shape (M20a), here naming who
// issued an invite and who joined.
type PublicUser struct {
	ID          snowflake.ID `json:"id"`
	Username    string       `json:"username"`
	DisplayName string       `json:"display_name"`
}

// publicUser builds one from a LEFT JOIN on users that drops a deleted account, as messages' author
// does: an id that resolves to no name is a deleted account, and is null rather than the placeholder.
func publicUser(id int64, username, displayName *string) *PublicUser {
	if username == nil || displayName == nil {
		return nil
	}
	return &PublicUser{ID: snowflake.ID(id), Username: *username, DisplayName: *displayName}
}

// Invite is a guild invite as its guild sees it: the code in full, because the guild needs it back.
type Invite struct {
	ID        snowflake.ID `json:"id"`
	Code      string       `json:"code"`
	GuildID   snowflake.ID `json:"guild_id"`
	ChannelID snowflake.ID `json:"channel_id"`
	InviterID snowflake.ID `json:"inviter_id"`
	Inviter   *PublicUser  `json:"inviter"`
	MaxUses   *int32       `json:"max_uses"`
	Uses      int32        `json:"uses"`
	ExpiresAt *time.Time   `json:"expires_at"`
	CreatedAt time.Time    `json:"created_at"`
}

func inviteFromRow(row db.Invite, inviter *PublicUser) Invite {
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
	Code      string         `json:"code"`
	ExpiresAt *time.Time     `json:"expires_at"`
	Guild     PreviewGuild   `json:"guild"`
	Channel   PreviewChannel `json:"channel"`
	Inviter   *PublicUser    `json:"inviter"`
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

// MemberAdded is GUILD_MEMBER_ADD's payload: the new member, and the name nobody in the guild could look up
// otherwise (there is no GET /users/{id}).
type MemberAdded struct {
	Member
	User PublicUser `json:"user"`
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
			params.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(in.MaxAge), Valid: true}
		}

		// Retried on collision rather than checked first, as M10's are: the unique constraint cannot race,
		// and three collisions at 69 bits is a broken generator, reported rather than looped on.
		var row db.Invite
		for attempt := 0; ; attempt++ {
			if params.Code, err = auth.NewInviteCode(); err != nil {
				return err
			}
			row, err = q.CreateGuildInvite(ctx, params)
			if err == nil {
				break
			}
			var pgErr *pgconn.PgError
			if attempt < 2 && errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
				continue
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
		out = inviteFromRow(row, &PublicUser{
			ID: actor.UserID, Username: creator.Username, DisplayName: creator.DisplayName,
		})
		return nil
	})
	return out, err
}

// ListInvites returns a guild's live invites, codes in full.
//
// PermManageGuild, the bit that decides who the guild is for. Creating an invite is a smaller grant than
// reading every outstanding one: whoever can list them can hand any of them on.
func (s *Service) ListInvites(ctx context.Context, actor auth.Actor, guildID snowflake.ID) ([]Invite, error) {
	if _, err := guildauth.Authorize(ctx, s.queries, actor, guildID, 0, roles.PermManageGuild); err != nil {
		return nil, err
	}
	rows, err := s.queries.ListGuildInvites(ctx, int64(guildID))
	if err != nil {
		return nil, fmt.Errorf("guilds: list invites: %w", err)
	}
	out := make([]Invite, 0, len(rows))
	for _, r := range rows {
		inviter := publicUser(r.Invite.InviterID, r.InviterUsername, r.InviterDisplayName)
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
		Inviter: publicUser(row.InviterID, row.InviterUsername, row.InviterDisplayName),
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

		if _, err := q.GetGuildMember(ctx, db.GetGuildMemberParams{
			GuildID: invite.GuildID, UserID: int64(actor.UserID),
		}); err == nil {
			guild, err := q.GetGuild(ctx, invite.GuildID)
			if err != nil {
				return fmt.Errorf("guilds: get guild: %w", err)
			}
			out = guildFromRow(guild)
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

		member, err := q.AddGuildMember(ctx, db.AddGuildMemberParams{
			GuildID: spent.GuildID, UserID: int64(actor.UserID),
		})
		if err != nil {
			// The guild was deleted after the invite was read; the cascade took the invite too, so the
			// answer is the one an unknown code gets. A unique violation cannot reach here — the account
			// lock serializes this account's redemptions and the membership read above saw none.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.ForeignKeyViolation {
				return httpx.ErrNotFound
			}
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

		guild, err := q.GetGuild(ctx, spent.GuildID)
		if err != nil {
			return fmt.Errorf("guilds: get guild: %w", err)
		}
		out = guildFromRow(guild)
		account, err := q.GetUserByID(ctx, int64(actor.UserID))
		if err != nil {
			return fmt.Errorf("guilds: get joiner: %w", err)
		}

		// The joiner first, by name, as Create does for an owner: the event is what adds the guild to their
		// live connections, so the guild's later events — the next one included — find them.
		if err := s.events.Queue(ctx, dispatch.Event{
			Type: "GUILD_CREATE", Audience: dispatch.Users, GuildID: guildID, Users: []snowflake.ID{actor.UserID},
		}, out); err != nil {
			return err
		}
		return s.events.Queue(ctx, dispatch.Event{
			Type: "GUILD_MEMBER_ADD", Audience: dispatch.Guild, GuildID: guildID,
		}, MemberAdded{
			Member: memberFromRow(member, []snowflake.ID{}),
			User:   PublicUser{ID: actor.UserID, Username: account.Username, DisplayName: account.DisplayName},
		})
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

		need := roles.PermManageGuild
		if snowflake.ID(invite.InviterID) == actor.UserID {
			need = 0
		}
		if _, err := guildauth.Authorize(ctx, q, actor, guildID, 0, need); err != nil {
			return err
		}

		deleted, err := q.DeleteGuildInvite(ctx, invite.ID)
		if err != nil {
			return fmt.Errorf("guilds: delete invite: %w", err)
		}
		if deleted == 0 {
			// Revoked or spent by somebody else since the read.
			return httpx.ErrNotFound
		}

		changes := auditDiff{}
		changes.context("channel_id", snowflake.ID(invite.ChannelID))
		changes.context("inviter_id", snowflake.ID(invite.InviterID))
		changes.context("uses", invite.Uses)
		inviteID := snowflake.ID(invite.ID)
		return s.writeAudit(ctx, q, guildID, actor.UserID, ActionInviteRevoke, &inviteID, changes.payload())
	})
}
