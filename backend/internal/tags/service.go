// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tags is the message-tagging surface: a guild's vocabulary, and what it has been applied to.
//
// Authority is decided by [guildauth], never here — the third package to reach that chokepoint after
// `messages` (M15) and `reports` (M16), and the third of the four the M15 extraction was done for.
//
// # Two kinds of tag, and the difference is not a permission
//
// A **shared** tag belongs to the guild. Creating one needs PermManageMessages, because it adds to a
// vocabulary everybody sees and an unbounded shared namespace is a defacement surface. A **private** tag
// belongs to one member, needs nothing beyond membership, and is invisible to everybody else — the filter
// that makes that true lives in the SQL (see ListGuildMessageTags) rather than in a mapper here, so the
// next reader of that table inherits it.
//
// The word "private" is doing real work and it is worth saying what it does not mean: a private tag on a
// message everybody can read is invisible to them, but the *message* is not made private by being tagged.
// Tags annotate; they do not restrict. Restricting who can see a message is M61's whispers.
package tags

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// Service owns the tag operations.
type Service struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	ids     *snowflake.Generator
}

// ServiceOptions configures [NewService].
type ServiceOptions struct {
	Pool *pgxpool.Pool
	IDs  *snowflake.Generator
}

// NewService validates its dependencies at construction rather than at first use.
func NewService(opts ServiceOptions) (*Service, error) {
	switch {
	case opts.Pool == nil:
		return nil, errors.New("tags: a database pool is required")
	case opts.IDs == nil:
		return nil, errors.New("tags: an ID generator is required")
	}
	return &Service{pool: opts.Pool, queries: db.New(opts.Pool), ids: opts.IDs}, nil
}

func (s *Service) inTx(ctx context.Context, fn func(q *db.Queries) error) error {
	return database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(s.queries.WithTx(tx))
	})
}

// CreateInput is a request to add a tag to a guild's vocabulary.
type CreateInput struct {
	GuildID  snowflake.ID
	Name     string
	IsShared bool
}

// Create adds a tag to a guild.
//
// # The permission depends on the kind, which is why `need` is built from the input
//
// A shared tag requires PermManageMessages; a private one requires only membership, which
// PermViewChannel stands for here as it does in guilds.Get. Building `need` from the field rather than
// asking for the larger permission unconditionally is M12's correction to UpdateMember, where a bit only
// ever OR'd into a base made two moderation permissions undeliverable on their own.
//
// # No audit entry, and that is rule 2 as narrowed at M15
//
// A member creating a private tag exercises authority over nobody. A moderator creating a shared one
// changes what the guild's vocabulary *is*, which reads closer to administrative — and it is still not
// audited, deliberately: the entry would name a tag and nothing else, the verb would have to join
// `guilds.AuditActions()` and therefore the audit-log reader's filter vocabulary, and M14's tripwire
// exists to make exactly that addition a decision rather than a reflex. If tags grow a moderation
// dimension — a tag that hides a message, say — that is the point to revisit, and it is in the ledger.
func (s *Service) Create(ctx context.Context, actor auth.Actor, in CreateInput) (Tag, error) {
	if err := checkName(in.Name); err != nil {
		return Tag{}, err
	}

	id, err := s.ids.Next()
	if err != nil {
		return Tag{}, fmt.Errorf("tags: mint tag id: %w", err)
	}

	need := roles.PermViewChannel
	if in.IsShared {
		need = need.Add(roles.PermManageMessages)
	}

	var out Tag
	err = s.inTx(ctx, func(q *db.Queries) error {
		// Authorized on the transaction's querier so the permissions that allow the write are read in the
		// same snapshot the write happens in (rule 1).
		if _, err := guildauth.Authorize(ctx, q, actor, in.GuildID, 0, need); err != nil {
			return err
		}

		// After authorization, or the ceiling becomes something a non-member can measure (M12).
		if err := s.checkCeiling(ctx, q, actor, in); err != nil {
			return err
		}

		row, err := q.CreateMessageTag(ctx, db.CreateMessageTagParams{
			ID:        int64(id),
			GuildID:   int64(in.GuildID),
			Name:      in.Name,
			CreatedBy: int64(actor.UserID),
			IsShared:  in.IsShared,
		})
		if err != nil {
			// The uniqueness guards are the two partial indexes in 000024, so a collision arrives here
			// rather than from a read-then-insert that races. 409 rather than 400: the request is
			// well-formed and the conflict is with state, which is what M10's invite redemption settled.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
				return httpx.Errorf(httpx.ErrConflict, "a tag with that name already exists here")
			}
			return fmt.Errorf("tags: create tag: %w", err)
		}

		out = tagFromRow(row)
		return nil
	})
	return out, err
}

// checkCeiling bounds what a guild and a member may accumulate. See the constants for why the private
// ceiling is the one that matters.
func (s *Service) checkCeiling(
	ctx context.Context, q *db.Queries, actor auth.Actor, in CreateInput,
) error {
	if in.IsShared {
		n, err := q.CountGuildSharedTags(ctx, int64(in.GuildID))
		if err != nil {
			return fmt.Errorf("tags: count shared tags: %w", err)
		}
		if n >= MaxSharedTagsPerGuild {
			return httpx.Errorf(httpx.ErrConflict,
				"a guild may hold at most %d shared tags", MaxSharedTagsPerGuild)
		}
		return nil
	}

	n, err := q.CountMemberPrivateTags(ctx, db.CountMemberPrivateTagsParams{
		GuildID: int64(in.GuildID), CreatedBy: int64(actor.UserID),
	})
	if err != nil {
		return fmt.Errorf("tags: count private tags: %w", err)
	}
	if n >= MaxPrivateTagsPerMember {
		return httpx.Errorf(httpx.ErrConflict,
			"you may hold at most %d private tags in one guild", MaxPrivateTagsPerMember)
	}
	return nil
}

// checkName bounds a tag name in the service rather than only at the handler.
//
// Runes, not bytes — the handler's `max=50` is go-playground/validator's rune count, and M15 shipped a
// byte-based check against a rune-based tag that would have refused any near-limit message in a non-Latin
// script. A 50-character ceiling is reached far sooner in Japanese than in English, so the same mistake
// would bite harder here.
//
// A name is trimmed of nothing and validated for emptiness only. Tag names are user vocabulary, and the
// instance deciding which characters a guild may file things under is not its business — rule 19 is what
// makes them safe to print, at the client, where the renderer is.
func checkName(name string) error {
	switch {
	case name == "":
		return httpx.Errorf(httpx.ErrBadRequest, "name is required")
	case utf8.RuneCountInString(name) > MaxTagNameLength:
		return httpx.Errorf(httpx.ErrBadRequest, "name must be at most %d characters", MaxTagNameLength)
	}
	return nil
}

// List returns the tags a caller can see in a guild: every shared one, plus their own private ones.
//
// Authorized on PermViewChannel, which every member holds by default — so in practice this asks "are you
// in this guild", and a non-member is answered 404 exactly as guilds.Get answers it.
func (s *Service) List(ctx context.Context, actor auth.Actor, guildID snowflake.ID) ([]Tag, error) {
	if _, err := guildauth.Authorize(
		ctx, s.queries, actor, guildID, 0, roles.PermViewChannel,
	); err != nil {
		return nil, err
	}

	rows, err := s.queries.ListGuildMessageTags(ctx, db.ListGuildMessageTagsParams{
		GuildID: int64(guildID), CreatedBy: int64(actor.UserID),
	})
	if err != nil {
		return nil, fmt.Errorf("tags: list guild tags: %w", err)
	}

	out := make([]Tag, 0, len(rows))
	for _, row := range rows {
		out = append(out, tagFromRow(row))
	}
	return out, nil
}

// Delete removes a tag and, by cascade, every application of it.
//
// # Who may
//
// A private tag: its creator, and nobody else — not a moderator, not the owner. It is invisible to them,
// and a permission to delete what you cannot see is a permission to delete at random.
//
// A shared tag: its creator, or a PermManageMessages holder. The creator is included because taking back
// something you added to a shared vocabulary is not moderation; the bit is included because a shared tag
// is the guild's and somebody has to be able to remove a bad one after its author leaves.
//
// An Instance Admin passes by layer 1 as everywhere else.
func (s *Service) Delete(ctx context.Context, actor auth.Actor, guildID, tagID snowflake.ID) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		decision, err := guildauth.Authorize(ctx, q, actor, guildID, 0, roles.PermViewChannel)
		if err != nil {
			return err
		}

		tag, err := s.loadInGuild(ctx, q, actor, decision, guildID, tagID)
		if err != nil {
			return err
		}

		// Checked after the row loads, which makes it an authorization question rather than an input one
		// — M12's "refuse before explaining". Somebody who is not in the guild never reached this line.
		if !mayDeleteTag(actor, decision, tag) {
			return httpx.Errorf(httpx.ErrForbidden, "you may not delete that tag")
		}

		affected, err := q.DeleteMessageTag(ctx, db.DeleteMessageTagParams{
			ID: int64(tagID), GuildID: int64(guildID),
		})
		if err != nil {
			return fmt.Errorf("tags: delete tag: %w", err)
		}
		if affected == 0 {
			return httpx.ErrNotFound
		}
		return nil
	})
}

// mayDeleteTag is a named function rather than an inline condition for the reason
// guilds.mayFlipMessageAudit is: it mixes ownership with a permission, and an unexplained compound
// boolean in a handler reads as a caller that forgot one of them.
func mayDeleteTag(actor auth.Actor, decision guildauth.Decision, tag db.MessageTag) bool {
	if snowflake.ID(tag.CreatedBy) == actor.UserID {
		return true
	}
	// A private tag somebody else owns is invisible, so no authority reaches it — PermManageMessages
	// included. Unreachable in practice, because loadInGuild has already refused it as not-found; kept
	// because this function is the one that answers "may they", and a caller that reached it another way
	// must get the same answer.
	if !tag.IsShared {
		return decision.InstanceAdmin()
	}
	return decision.Allows(roles.PermManageMessages)
}

// loadInGuild reads a tag, refusing one that belongs to a different guild or that this caller cannot see.
//
// The guild in the path is what was authorized, so a tag id from elsewhere must not be reachable through
// it — M15's loadInChannel, one object over, and 404 rather than 403 for the same reason: whether an id
// names a tag in a guild you are not in is exactly what the refusal withholds.
//
// # The visibility refusal is 404 and it has to be
//
// A private tag somebody else owns does not exist as far as this caller is concerned, so it is refused
// here rather than at the authority check below. The difference is an oracle: if an invisible tag loaded
// and then failed [mayDeleteTag], the answer would be 403, and 403-versus-404 would tell anybody holding
// a tag id whether it named a private tag in this guild — and tag ids are snowflakes, so a list of
// plausible ids becomes a map of which private tags exist and roughly when they were made. That is the
// split M12 settled for guilds, M13 for channels and M16a for hidden channels, arriving a fourth time.
//
// The listing's SQL carries the same filter for the many-row case. Two copies of one rule is what M15
// warns about, and the alternative here is worse: a single-row read cannot express "and silently return
// nothing" without becoming a listing, and pushing the check into the query would put the caller's
// identity into a lookup by primary key. The pin is a test that drives both paths with the same actor.
//
// An Instance Admin sees everything, by layer 1, as they do everywhere else.
func (s *Service) loadInGuild(
	ctx context.Context, q *db.Queries, actor auth.Actor, decision guildauth.Decision,
	guildID, tagID snowflake.ID,
) (db.MessageTag, error) {
	tag, err := q.GetMessageTag(ctx, int64(tagID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.MessageTag{}, httpx.ErrNotFound
		}
		return db.MessageTag{}, fmt.Errorf("tags: get tag: %w", err)
	}
	if snowflake.ID(tag.GuildID) != guildID {
		return db.MessageTag{}, httpx.ErrNotFound
	}
	if !tagVisibleTo(actor, decision, tag) {
		return db.MessageTag{}, httpx.ErrNotFound
	}
	return tag, nil
}

// tagVisibleTo reports whether this caller may know the tag exists at all.
//
// Shared tags are the guild's, so every member sees them. A private tag is its creator's alone. This is
// the single-row half of the `is_shared OR created_by = $n` predicate the listing carries.
func tagVisibleTo(actor auth.Actor, decision guildauth.Decision, tag db.MessageTag) bool {
	return tag.IsShared ||
		snowflake.ID(tag.CreatedBy) == actor.UserID ||
		decision.InstanceAdmin()
}
