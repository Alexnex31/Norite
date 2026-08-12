// Package auth is the account and credential core: registration, password login, device-scoped refresh
// token families, and scoped API tokens.
//
// # The invariant this package exists to protect
//
// A refresh token belongs to a *device*, not to an account. Rotation and reuse-detection both act strictly
// within one `device_id` and never across them, so a user running daemons on a laptop and a desktop under
// one account cannot have one machine's activity log the other out. Every query that revokes is scoped by
// `(user_id, device_id)` for that reason, and the integration tests assert it directly — see ADR 0011 and
// docs/architecture.md §2.
//
// # What is deliberately not here
//
// Password reset (M5), OAuth linking (M6), the device-code flow (M9), invite-gated registration (M10), and
// the general-purpose revoke-all-sessions primitive (M11). Each arrives with its own milestone; this
// package holds only what M4 defines.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// RefreshTokenTTL is how long a refresh session stays valid without being used.
//
// Long enough that a daemon left off over a holiday still reconnects without a re-login, short enough that
// an abandoned session does not stay redeemable forever. Rotation issues a fresh full TTL each time, so an
// actively-used device never expires.
const RefreshTokenTTL = 30 * 24 * time.Hour

// MaxDeviceIDLength bounds the client-supplied device identifier. It is stored verbatim and used as a
// grouping key, so it needs a ceiling like any other free-text input.
const MaxDeviceIDLength = 128

// Service errors. Handlers map these to HTTP statuses; the mapping lives in http.go so this file stays free
// of transport concerns.
var (
	ErrEmailTaken          = errors.New("that email is already registered")
	ErrUsernameTaken       = errors.New("that username is already taken")
	ErrRegistrationClosed  = errors.New("registration requires an invite code")
	ErrInvalidRefreshToken = errors.New("invalid or expired refresh token")
	ErrSessionReuse        = errors.New("refresh token was already used")
	ErrUnknownScope        = errors.New("unknown scope")
	ErrNotFound            = errors.New("not found")
	ErrInvalidUsername     = errors.New("a username may contain only letters, digits, and _ . -")
)

// RegistrationMode mirrors the instance's configured gating.
type RegistrationMode string

const (
	RegistrationOpen   RegistrationMode = "open"
	RegistrationInvite RegistrationMode = "invite"
)

// Service holds the auth domain logic.
type Service struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	ids     *snowflake.Generator
	issuer  *TokenIssuer

	// registrationMode gates POST /auth/register.
	registrationMode RegistrationMode

	now func() time.Time
}

// ServiceOptions configures NewService.
type ServiceOptions struct {
	Pool             *pgxpool.Pool
	IDs              *snowflake.Generator
	Issuer           *TokenIssuer
	RegistrationMode RegistrationMode
}

// NewService builds the auth service.
func NewService(opts ServiceOptions) (*Service, error) {
	switch {
	case opts.Pool == nil:
		return nil, errors.New("auth: a database pool is required")
	case opts.IDs == nil:
		return nil, errors.New("auth: an ID generator is required")
	case opts.Issuer == nil:
		return nil, errors.New("auth: a token issuer is required")
	}

	mode := opts.RegistrationMode
	if mode == "" {
		// Fail closed. An unset mode is a programming error rather than an operator choice, and defaulting
		// to open registration on an instance whose configuration did not say so is the wrong direction to
		// guess in.
		mode = RegistrationInvite
	}

	return &Service{
		pool:             opts.Pool,
		queries:          db.New(opts.Pool),
		ids:              opts.IDs,
		issuer:           opts.Issuer,
		registrationMode: mode,
		now:              time.Now,
	}, nil
}

// RegisterInput is a registration request.
type RegisterInput struct {
	Username    string
	Email       string
	Password    string
	DisplayName string
}

// Register creates an account.
//
// It does not log the new account in. Registration and login are separate operations with separate inputs —
// login needs a device_id, which registration has no business requiring — and fusing them would mean a
// client that only wanted to create an account also had to invent a device identity.
func (s *Service) Register(ctx context.Context, in RegisterInput) (db.User, error) {
	// The gate comes first, before any validation work or database round trip: a closed instance should
	// cost an attacker exactly one cheap rejection, and should not reveal through timing or error detail
	// whether the email they tried is already registered.
	if s.registrationMode != RegistrationOpen {
		return db.User{}, ErrRegistrationClosed
	}

	// Normalized and validated here rather than only by the handler's struct tags, so every caller gets
	// the same rule — the tags bound the input's size, this decides what a username may actually be.
	username := NormalizeUsername(in.Username)
	if !ValidUsername(username) {
		return db.User{}, ErrInvalidUsername
	}
	email := strings.TrimSpace(strings.ToLower(in.Email))
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = username
	}

	hash, err := HashPassword(ctx, in.Password)
	if err != nil {
		return db.User{}, err
	}

	// Uniqueness is checked here for a good error message and enforced by the UNIQUE constraints for
	// correctness. The check alone would be a race — two simultaneous registrations both see the address as
	// free — which is why the insert's constraint violation is also mapped below rather than trusted not to
	// happen.
	taken, err := s.queries.UserExistsByEmail(ctx, email)
	if err != nil {
		return db.User{}, fmt.Errorf("checking email availability: %w", err)
	}
	if taken {
		return db.User{}, ErrEmailTaken
	}
	taken, err = s.queries.UserExistsByUsername(ctx, username)
	if err != nil {
		return db.User{}, fmt.Errorf("checking username availability: %w", err)
	}
	if taken {
		return db.User{}, ErrUsernameTaken
	}

	id, err := s.ids.Next()
	if err != nil {
		return db.User{}, fmt.Errorf("generating user ID: %w", err)
	}

	user, err := s.queries.CreateUser(ctx, db.CreateUserParams{
		ID:           int64(id),
		Username:     username,
		Email:        email,
		PasswordHash: &hash,
		DisplayName:  displayName,
	})
	if err != nil {
		if constraint := uniqueViolation(err); constraint != "" {
			// Lost the race described above. Report it as the same conflict the pre-check would have.
			if strings.Contains(constraint, "username") {
				return db.User{}, ErrUsernameTaken
			}
			return db.User{}, ErrEmailTaken
		}
		return db.User{}, fmt.Errorf("creating user: %w", err)
	}
	return user, nil
}

// LoginInput is a password login request.
type LoginInput struct {
	Email      string
	Password   string
	DeviceID   string
	DeviceName string
	IP         netip.Addr
}

// Login verifies a password and starts a refresh session for the given device.
//
// A successful login supersedes that device's previous family: any live session for the same
// (user, device_id) is revoked before the new one is created. Without that, every login would leave another
// redeemable refresh token behind, so an old token stolen months ago would stay valid indefinitely. Other
// devices are untouched.
func (s *Service) Login(ctx context.Context, in LoginInput) (TokenPair, error) {
	deviceID, err := normalizeDeviceID(in.DeviceID)
	if err != nil {
		return TokenPair{}, err
	}
	email := strings.TrimSpace(strings.ToLower(in.Email))

	user, err := s.queries.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Burn the same argon2id work a real verification costs, so response time does not reveal
			// whether the address is registered. Returning early here is exactly the timing oracle that
			// lets an attacker enumerate an entire user base without guessing a single password.
			return TokenPair{}, VerifyPasswordForMissingUser(ctx, in.Password)
		}
		return TokenPair{}, fmt.Errorf("looking up account: %w", err)
	}

	stored := ""
	if user.PasswordHash != nil {
		stored = *user.PasswordHash
	}
	if err := VerifyPassword(ctx, stored, in.Password); err != nil {
		if errors.Is(err, ErrInvalidCredentials) || errors.Is(err, ErrPasswordNotSet) {
			// Both are reported to the client identically. "This account exists but signs in with Google"
			// is precisely the kind of detail that turns a login form into an account-discovery tool.
			return TokenPair{}, ErrInvalidCredentials
		}
		return TokenPair{}, err
	}

	return s.startSession(ctx, snowflake.ID(user.ID), deviceID, in.DeviceName, in.IP)
}

// startSession revokes the device's previous family and issues a fresh pair.
func (s *Service) startSession(ctx context.Context, userID snowflake.ID, deviceID, deviceName string, ip netip.Addr) (TokenPair, error) {
	raw, hash, err := GenerateRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}

	sessionID, err := s.ids.Next()
	if err != nil {
		return TokenPair{}, fmt.Errorf("generating session ID: %w", err)
	}

	err = database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.queries.WithTx(tx)

		if _, err := q.RevokeSessionsForDevice(ctx, db.RevokeSessionsForDeviceParams{
			UserID:   int64(userID),
			DeviceID: deviceID,
		}); err != nil {
			return fmt.Errorf("revoking the previous session for this device: %w", err)
		}

		_, err := q.CreateSession(ctx, db.CreateSessionParams{
			ID:               int64(sessionID),
			UserID:           int64(userID),
			DeviceID:         deviceID,
			RefreshTokenHash: hash,
			DeviceName:       optionalString(deviceName),
			IpAddress:        optionalAddr(ip),
			ExpiresAt:        timestamptz(s.now().Add(RefreshTokenTTL)),
		})
		if err != nil {
			return fmt.Errorf("creating session: %w", err)
		}
		return nil
	})
	if err != nil {
		return TokenPair{}, err
	}

	return s.issuePair(userID, sessionID, raw)
}

// Refresh rotates a refresh token within its own device's family.
//
// The rotation is single-use: the presented token is revoked in the same transaction that creates its
// successor, so presenting it a second time is unambiguous evidence of replay — either the client retried
// after a lost response, or the token was stolen. Both are treated as compromise, because the two are
// indistinguishable from here and the safe reading is the pessimistic one.
//
// **Reuse revokes only the affected device's family.** That scoping is the entire point of `device_id`, and
// getting it wrong would mean a stolen token on one machine logging the user out everywhere — the failure
// this schema was designed to prevent.
func (s *Service) Refresh(ctx context.Context, rawToken string) (TokenPair, error) {
	hash, err := ParseRefreshToken(rawToken)
	if err != nil {
		// Wrong shape entirely — reject without a database round trip.
		return TokenPair{}, ErrInvalidRefreshToken
	}

	session, err := s.queries.GetSessionByRefreshTokenHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenPair{}, ErrInvalidRefreshToken
		}
		return TokenPair{}, fmt.Errorf("looking up session: %w", err)
	}

	if session.RevokedAt.Valid {
		// Revoked, but never rotated away from: this session was ended deliberately — a logout, or a fresh
		// login superseding this device's previous family. Presenting its token is not replay, and treating
		// it as such would be actively harmful: logging out, logging back in, and then retrying the stale
		// token once would revoke the *new* session along with the old one.
		//
		// replaced_by_id is what distinguishes the two, and it is set only by rotation.
		if session.ReplacedByID == nil {
			return TokenPair{}, ErrInvalidRefreshToken
		}

		// Rotated away from, and presented again — replay.
		if _, err := s.queries.RevokeSessionsForDevice(ctx, db.RevokeSessionsForDeviceParams{
			UserID:   session.UserID,
			DeviceID: session.DeviceID,
		}); err != nil {
			return TokenPair{}, fmt.Errorf("revoking the compromised device family: %w", err)
		}
		logging.FromContext(ctx).Warn().
			Str("user_id", snowflake.ID(session.UserID).String()).
			Str("device_id", session.DeviceID).
			Msg("refresh token reuse detected — revoked this device's session family")
		return TokenPair{}, ErrSessionReuse
	}

	if !session.ExpiresAt.Valid || !session.ExpiresAt.Time.After(s.now()) {
		return TokenPair{}, ErrInvalidRefreshToken
	}

	newRaw, newHash, err := GenerateRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}
	newSessionID, err := s.ids.Next()
	if err != nil {
		return TokenPair{}, fmt.Errorf("generating session ID: %w", err)
	}

	err = database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.queries.WithTx(tx)

		// The successor is inserted first so the old row can point at it. Same device_id, so the family
		// stays intact and scoped.
		if _, err := q.CreateSession(ctx, db.CreateSessionParams{
			ID:               int64(newSessionID),
			UserID:           session.UserID,
			DeviceID:         session.DeviceID,
			RefreshTokenHash: newHash,
			DeviceName:       session.DeviceName,
			IpAddress:        session.IpAddress,
			ExpiresAt:        timestamptz(s.now().Add(RefreshTokenTTL)),
		}); err != nil {
			return fmt.Errorf("creating the rotated session: %w", err)
		}

		// Revoking and linking in one statement is what makes the old token single-use. The WHERE clause
		// requires it to still be live, so two concurrent refreshes with the same token cannot both
		// succeed — the loser updates zero rows and is failed here rather than silently issuing a second
		// valid pair.
		replaced := int64(newSessionID)
		if _, err := q.RotateSession(ctx, db.RotateSessionParams{
			ID:           session.ID,
			ReplacedByID: &replaced,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrSessionReuse
			}
			return fmt.Errorf("rotating session: %w", err)
		}
		return nil
	})
	if err != nil {
		return TokenPair{}, err
	}

	return s.issuePair(snowflake.ID(session.UserID), newSessionID, newRaw)
}

// Logout revokes the session a refresh token belongs to.
//
// Takes the refresh token rather than the access token: the access token is what the caller is
// authenticating with, but the refresh session is the thing that actually needs killing — an access token
// expires on its own within AccessTokenTTL regardless.
//
// An unknown or already-revoked token is not an error. Logging out is idempotent by nature, and a client
// retrying after a dropped response should not be told its logout failed.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	hash, err := ParseRefreshToken(rawToken)
	if err != nil {
		return nil
	}

	session, err := s.queries.GetSessionByRefreshTokenHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("looking up session: %w", err)
	}
	if session.RevokedAt.Valid {
		return nil
	}

	if _, err := s.queries.RevokeSession(ctx, session.ID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("revoking session: %w", err)
	}
	return nil
}

// issuePair signs an access token and packages it with the refresh token.
func (s *Service) issuePair(userID, sessionID snowflake.ID, refreshToken string) (TokenPair, error) {
	access, expiresAt, err := s.issuer.Issue(userID, sessionID)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken:  access,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		TokenType:    "Bearer",
	}, nil
}

// normalizeDeviceID validates the client-supplied device identifier.
func normalizeDeviceID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	switch {
	case id == "":
		return "", fmt.Errorf("%w: device_id is required", ErrInvalidCredentials)
	case len(id) > MaxDeviceIDLength:
		return "", fmt.Errorf("%w: device_id must be at most %d bytes", ErrInvalidCredentials, MaxDeviceIDLength)
	}
	return id, nil
}

func optionalString(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func optionalAddr(a netip.Addr) *netip.Addr {
	if !a.IsValid() {
		return nil
	}
	return &a
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
