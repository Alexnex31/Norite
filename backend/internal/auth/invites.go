package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// Instance invites: the codes that make registration_mode = "invite" mean something.
//
// M4 wired the mode and, with no table to redeem against, made it a hard refusal — deliberately, so an
// operator who gated registration got gating rather than silence. This is the other half.

// inviteCodeLength is how many characters an invite code carries.
//
// Sixteen of the twenty-letter alphabet auth's device codes draw from is about 69 bits, against a device
// code's 34.6. The difference is lifetime: a device code lives twenty minutes behind a rate limit, while
// an invite can be created with no expiry at all and guessed at for as long as the instance runs. It is
// also not typed under time pressure — it arrives in a message and is pasted — so the cost of the extra
// characters is close to nothing.
const inviteCodeLength = 16

// Invite errors.
var (
	// ErrInviteRequired means this instance is gated and no code was supplied.
	//
	// Distinct from ErrInviteInvalid because the two are genuinely different problems for the caller:
	// one is "you need one of these", the other is "the one you have does not work". Neither discloses
	// anything about whether any particular code exists.
	ErrInviteRequired = errors.New("registration on this instance requires an invite code")

	// ErrInviteInvalid covers every way a code can fail: unknown, exhausted, or expired.
	//
	// One error for three cases, matching how this package treats every other credential. Distinguishing
	// them would let somebody with no valid code learn which codes exist by watching the message change.
	ErrInviteInvalid = errors.New("that invite code is not valid")

	// ErrInviteExpiry rejects an expiry in the past, which would create an invite nobody can ever use.
	ErrInviteExpiry = errors.New("an invite's expiry must be in the future")

	// ErrInviteMaxUses rejects a use count below one, which would do the same.
	ErrInviteMaxUses = errors.New("an invite must allow at least one use")
)

// CreateInviteInput describes an invite to mint.
type CreateInviteInput struct {
	// CreatedBy is the administrator issuing it, or zero when the instance operator did — who is not an
	// account and has no id to record. See migration 000009.
	CreatedBy snowflake.ID
	// MaxUses is how many accounts may be created with it. Zero means unlimited.
	MaxUses int32
	// ExpiresIn is how long it lives. Zero means it never expires.
	ExpiresIn time.Duration
}

// CreateInstanceInvite mints an invite code.
func (s *Service) CreateInstanceInvite(ctx context.Context, in CreateInviteInput) (db.InstanceInvite, error) {
	if in.MaxUses < 0 {
		return db.InstanceInvite{}, ErrInviteMaxUses
	}
	if in.ExpiresIn < 0 {
		return db.InstanceInvite{}, ErrInviteExpiry
	}

	params := db.CreateInstanceInviteParams{}
	if in.CreatedBy != 0 {
		id := int64(in.CreatedBy)
		params.CreatedBy = &id
	}
	if in.MaxUses > 0 {
		max := in.MaxUses
		params.MaxUses = &max
	}
	if in.ExpiresIn > 0 {
		params.ExpiresAt = timestamptz(s.now().Add(in.ExpiresIn))
	}

	// Retried on collision rather than checked first, the way the device flow handles its user code. A
	// check-then-insert races; the unique constraint does not, and at 69 bits a collision means the
	// generator is broken rather than that the instance got unlucky — so the retry exists to make that
	// failure loud rather than to be reached.
	for range 3 {
		code, err := randomCode(inviteCodeLength)
		if err != nil {
			return db.InstanceInvite{}, err
		}
		params.Code = code

		invite, err := s.queries.CreateInstanceInvite(ctx, params)
		if err == nil {
			return invite, nil
		}
		if uniqueViolation(err) == "" {
			return db.InstanceInvite{}, fmt.Errorf("creating invite: %w", err)
		}
	}
	// Three collisions at 69 bits is not bad luck. Reported rather than retried forever, because the
	// cause is a broken generator and looping would hide it as a slow endpoint.
	return db.InstanceInvite{}, errors.New("could not generate an unused invite code")
}

// ListInstanceInvites returns every outstanding invite, newest first.
func (s *Service) ListInstanceInvites(ctx context.Context) ([]db.InstanceInvite, error) {
	invites, err := s.queries.ListInstanceInvites(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing invites: %w", err)
	}
	return invites, nil
}

// DeleteInstanceInvite revokes an invite. Reports ErrNotFound when there was nothing to revoke, so a
// mistyped code is distinguishable from a successful revocation.
func (s *Service) DeleteInstanceInvite(ctx context.Context, rawCode string) error {
	code, err := ParseInviteCode(rawCode)
	if err != nil {
		// A code that cannot be one this instance issued deletes nothing, and saying so is the same
		// answer as a well-formed code that does not exist.
		return ErrNotFound
	}

	rows, err := s.queries.DeleteInstanceInvite(ctx, code)
	if err != nil {
		return fmt.Errorf("deleting invite: %w", err)
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// ParseInviteCode normalizes what somebody typed into the form the database stores.
//
// The same treatment ParseUserCode gives a device code, and for the same reason: case, spaces and dashes
// are things a person gets wrong or a chat client adds on its own, and none of them carry meaning. What is
// left must be in the alphabet, because anything else cannot be a code this instance issued and there is
// nothing to look up.
func ParseInviteCode(raw string) (string, error) {
	code, ok := normalizeTypedCode(raw, inviteCodeLength)
	if !ok {
		return "", ErrInviteInvalid
	}
	return code, nil
}

// redeemInvite spends one use of a code, inside the caller's transaction.
//
// Takes the queries handle rather than opening its own, so redemption and the account insert commit or
// roll back together. Split apart, a registration that failed after redeeming would burn a use of somebody
// else's invite.
func redeemInvite(ctx context.Context, q *db.Queries, rawCode string) error {
	code, err := ParseInviteCode(rawCode)
	if err != nil {
		return err
	}

	if _, err := q.RedeemInstanceInvite(ctx, code); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown, exhausted, or expired — one answer, decided by the query's WHERE clause rather
			// than by three checks here that could disagree with it.
			return ErrInviteInvalid
		}
		return fmt.Errorf("redeeming invite: %w", err)
	}
	return nil
}
