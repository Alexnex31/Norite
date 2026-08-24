package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
)

// Instance administration: the operations an operator or an Instance Admin performs on the instance
// itself, rather than on an account or a guild.

// ErrAlreadyBootstrapped means this instance already has an administrator.
//
// Reported rather than swallowed. A second bootstrap is not a harmless retry — it is either an operator
// who does not know the instance is already set up, or an operator token being replayed by somebody who
// found one. Both want to hear about it, and neither wants a silently-created second admin account.
var ErrAlreadyBootstrapped = errors.New("this instance already has an administrator")

// BootstrapInput is the first administrator's account.
//
// The same fields registration takes, and deliberately no more: the bootstrap admin is an ordinary account
// that happens to hold the Instance Admin tier, not a separate kind of user. Making it one would mean
// every later query about accounts had a second table to remember.
type BootstrapInput struct {
	Username    string
	Email       string
	Password    string
	DisplayName string
}

// Bootstrap creates the instance's first administrator.
//
// # Why this is not just Register with a flag
//
// Registration is a public endpoint governed by the instance's own policy — it can be closed, it can
// require an invite, and from M10 on it requires the address to be verified before the account works.
// Every one of those is wrong for the account that has to exist before any of them can be configured, and
// routing bootstrap through Register would mean each of those gates growing an exception for it. An
// exception in a gate is how the gate stops working.
//
// So this is its own path, with its own single guard: there must be no administrator yet.
//
// # Why the guard is "no admins" and not "no users"
//
// An instance can legitimately have accounts and no administrator — a botched first setup, or M71's
// last-admin rail being circumvented by a database restore. Keying on users would make bootstrap
// unavailable exactly when it is needed. Keying on admins also means the endpoint disables itself the
// moment it succeeds, with no flag anywhere claiming it already ran.
func (s *Service) Bootstrap(ctx context.Context, in BootstrapInput) (db.User, error) {
	username := NormalizeUsername(in.Username)
	if !ValidUsername(username) {
		return db.User{}, ErrInvalidUsername
	}
	email := strings.TrimSpace(strings.ToLower(in.Email))
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = username
	}

	// Hashed before the transaction opens, never inside it. argon2id holds 64 MiB for tens of
	// milliseconds, and doing that with a connection checked out would occupy one of a deliberately small
	// pool (§15.3) for the whole of it — plus, here, while holding an instance-wide advisory lock.
	hash, err := HashPassword(ctx, in.Password)
	if err != nil {
		return db.User{}, err
	}

	id, err := s.ids.Next()
	if err != nil {
		return db.User{}, fmt.Errorf("generating user ID: %w", err)
	}

	var user db.User
	err = database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.queries.WithTx(tx)

		// Taken first, so the count below is a decision rather than an observation. See the query's own
		// comment: without it, two concurrent bootstraps both read zero and both insert.
		if err := q.LockInstanceBootstrap(ctx); err != nil {
			return fmt.Errorf("locking instance bootstrap: %w", err)
		}

		admins, err := q.CountInstanceAdmins(ctx)
		if err != nil {
			return fmt.Errorf("counting instance admins: %w", err)
		}
		if admins > 0 {
			return ErrAlreadyBootstrapped
		}

		user, err = q.CreateUser(ctx, db.CreateUserParams{
			ID:           int64(id),
			Username:     username,
			Email:        email,
			PasswordHash: &hash,
			DisplayName:  displayName,
			// Verified on creation. The operator read this instance's signing key off its own disk, which
			// is strictly stronger evidence than following a link sent to an address — and requiring mail
			// here would make an instance with no SMTP relay impossible to set up at all.
			EmailVerifiedAt: timestamptz(s.now()),
		})
		if err != nil {
			// The same conflict mapping registration uses, so a taken username or address reads the same
			// whichever endpoint hit it. Worth having even though this path runs once: the operator who
			// typed a duplicate address wants to know that, not a 500.
			if conflict := registerConflict(err); conflict != nil {
				return conflict
			}
			return fmt.Errorf("creating the bootstrap account: %w", err)
		}

		// granted_by is NULL: nobody in this table granted this tier. See migration 000008.
		if _, err := q.CreateInstanceAdmin(ctx, db.CreateInstanceAdminParams{UserID: user.ID}); err != nil {
			return fmt.Errorf("granting the instance admin tier: %w", err)
		}
		return nil
	})
	if err != nil {
		return db.User{}, err
	}
	return user, nil
}
