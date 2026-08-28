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
	"github.com/Alexnex31/Norite/backend/internal/mail"
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
	ErrInvalidRefreshToken = errors.New("invalid or expired refresh token")
	// ErrSessionSignedOut means the credential is still cryptographically valid but the session behind it
	// has been revoked. Only the revoking endpoints raise it — see Service.requireLiveDevice.
	ErrSessionSignedOut = errors.New("this session has been signed out")
	ErrSessionReuse     = errors.New("refresh token was already used")
	ErrUnknownScope     = errors.New("unknown scope")
	ErrNotFound         = errors.New("not found")
	ErrInvalidUsername  = errors.New("a username may contain only letters, digits, and _ . -")
	ErrInvalidTokenName = errors.New("invalid token name")

	// ErrTwoFactorRequired means the credential was good and a second factor is still owed. A branch on
	// the sign-in paths rather than a failure — see twofactor.go.
	ErrTwoFactorRequired = errors.New("this account requires a second factor")
	// ErrInvalidFactorCode is the single answer to every way a code can be wrong: not a live TOTP value,
	// not a shape a recovery code takes, already spent, or belonging to an account with no factor at all.
	// Undifferentiated for the reason every other credential refusal here is.
	ErrInvalidFactorCode = errors.New("that code is not valid")
	// ErrTwoFactorAlreadyEnabled refuses a second enrolment over a live one. Replacing a factor goes
	// through the disable path, which requires proving the current one first.
	ErrTwoFactorAlreadyEnabled = errors.New("this account already has a second factor")
	// ErrNoTwoFactorEnrolment means there is nothing to confirm.
	ErrNoTwoFactorEnrolment = errors.New("no enrolment is in progress")
	// ErrTwoFactorChallenge is the single answer to every way a challenge can fail to parse: expired,
	// signed by another instance, the wrong `typ`, or a shape this package never mints.
	ErrTwoFactorChallenge = errors.New("invalid or expired two-factor challenge")
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

	// mailer sends password-reset email. Nil, or present but disabled, means this instance has no relay
	// and reset reports ErrResetUnavailable rather than accepting a request it cannot fulfill.
	mailer Mailer
	// publicBaseURL is the origin reset links are built from. Configured rather than derived — see
	// config.PublicBaseURL.
	publicBaseURL string

	// oauth is the set of providers this instance offers. Empty on an instance that has configured none,
	// which is the ordinary shape — every OAuth entry point reports ErrUnknownProvider rather than
	// pretending a provider exists.
	oauth OAuthProviders

	// enumerationFloor is how long an always-202 lookup endpoint takes regardless of what it found.
	//
	// A field rather than the constant directly so the tests that are not about timing can zero it; the
	// one that is measures against it. See padToEnumerationFloor.
	enumerationFloor time.Duration

	now func() time.Time
}

// ServiceOptions configures NewService.
type ServiceOptions struct {
	Pool             *pgxpool.Pool
	IDs              *snowflake.Generator
	Issuer           *TokenIssuer
	RegistrationMode RegistrationMode

	// Mailer and PublicBaseURL are optional together: an instance with no relay is a working instance,
	// and password reset is simply unavailable on it (ADR 0020).
	Mailer        Mailer
	PublicBaseURL string

	// OAuth is optional: an instance with no provider credentials simply offers no OAuth sign-in.
	OAuth OAuthProviders
}

// Mailer is the slice of internal/mail this package needs.
//
// Narrow, and an interface rather than the concrete queue, for the reason every seam here is: it lets the
// reset tests drive a relay that is disabled, full, or broken without standing one up.
type Mailer interface {
	Enabled() bool
	Enqueue(msg mail.Message) error
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
		mailer:           opts.Mailer,
		publicBaseURL:    opts.PublicBaseURL,
		oauth:            opts.OAuth,
		ids:              opts.IDs,
		issuer:           opts.Issuer,
		registrationMode: mode,
		enumerationFloor: enumerationFloor,
		now:              time.Now,
	}, nil
}

// errEmailTakenSilently is internal: the address already has an account, discovered either by the check
// inside the transaction or by losing the unique constraint to a simultaneous registration.
//
// Never returned to a caller. Register turns it into the same silent success a new address produces,
// because a distinguishable answer — from either route — is the enumeration oracle this milestone closed.
// It rolls the transaction back, which is what leaves a redeemed invite unspent.
var errEmailTakenSilently = errors.New("email taken")

// RegisterInput is a registration request.
type RegisterInput struct {
	Username    string
	Email       string
	Password    string
	DisplayName string
	// InviteCode is required while the instance is gated and ignored while it is open. Ignored rather
	// than rejected: a client that always sends one is not doing anything wrong, and an open instance
	// refusing a stray code would make the two modes differ in a way no caller can predict.
	InviteCode string
}

// Register creates an account.
//
// It does not log the new account in. Registration and login are separate operations with separate inputs —
// login needs a device_id, which registration has no business requiring — and fusing them would mean a
// client that only wanted to create an account also had to invent a device identity.
func (s *Service) Register(ctx context.Context, in RegisterInput) (db.User, error) {
	// The gate comes first, before any validation work or database round trip: a gated instance should
	// cost an attacker exactly one cheap rejection, and should not reveal through timing or error detail
	// whether the email they tried is already registered.
	//
	// Only the *presence* of a code is checked here. Whether it is redeemable is decided inside the
	// transaction below, together with the account insert — checking it here as well would spend a use
	// on a registration that then failed its username check, burning somebody else's invite.
	gated := s.registrationMode != RegistrationOpen
	if gated {
		if strings.TrimSpace(in.InviteCode) == "" {
			return db.User{}, ErrInviteRequired
		}
		// And the code has to be *shaped* like one before anything expensive happens.
		//
		// "One cheap rejection" above was true only of a request with no code at all. Any well-formed
		// garbage reached HashPassword thirty lines down and spent 64 MiB and tens of milliseconds in one
		// of maxConcurrentHashes slots — so the gate that reads as the protection against exactly this
		// was not it. Bounded rather than unbounded, since the hash gate and the /auth rate limit still
		// apply, but the comment claimed a property the code did not have.
		//
		// A shape check only. Whether the code is *redeemable* stays in the transaction below, where it
		// shares the account insert: checking that here as well would spend a use on a registration that
		// then failed its username check and burn somebody else's invite. A well-formed but unknown code
		// therefore still reaches the hash, and closing that needs a pre-check that races — which is the
		// thing this ordering exists to avoid, so it is the right place to stop.
		if _, err := ParseInviteCode(in.InviteCode); err != nil {
			return db.User{}, err
		}
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

	// The username is public — it is an @handle, and any client that can look one up already discloses
	// whether it is taken — so refusing here reveals nothing. Checked *before* the address, so a taken
	// username is reported plainly rather than swallowed by the silence the address branch requires.
	//
	// "Unavailable" rather than "taken" is load-bearing: it consults reservations as well as accounts,
	// and without that half this check was the last leg of the oracle the rest of this function closes.
	// See migration 000011 and the reservation below.
	unavailable, err := s.queries.UsernameUnavailable(ctx, username)
	if err != nil {
		return db.User{}, fmt.Errorf("checking username availability: %w", err)
	}
	if unavailable {
		return db.User{}, ErrUsernameTaken
	}

	id, err := s.ids.Next()
	if err != nil {
		return db.User{}, fmt.Errorf("generating user ID: %w", err)
	}

	// An instance with no relay creates the account already verified — see the insert below.
	verifiedOnCreation := pgtype.Timestamptz{}
	if !s.VerificationRequired() {
		verifiedOnCreation = timestamptz(s.now())
	}

	// Redemption, the insert and the verification token commit together, so a registration that fails
	// part-way neither burns a use of somebody else's invite nor leaves a token behind for an account that
	// does not exist.
	var (
		user db.User
		msg  mail.Message
	)
	err = database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.queries.WithTx(tx)

		// Redeemed *before* the address is looked at, which is the ordering a review found wrong the
		// first time. With the address checked first, a taken one returned the silent 202 while a free
		// one fell through to `invite_invalid` — so on a gated instance anybody could test any address
		// with a made-up code, for free, without even spending an invite. Measured at 202 against 403.
		//
		// This way an unusable code is refused identically whatever the address, and the address branch
		// below is reachable only by somebody who already holds a real invite.
		if gated {
			if err := redeemInvite(ctx, q, in.InviteCode); err != nil {
				return err
			}
		}

		// The address, and this is where the account-existence oracle used to be.
		//
		// Until M10 a taken address answered 409, so anyone could probe any address. It answered that way
		// because there was no way to accept the registration and sort it out by mail — which is what
		// email verification now provides. Both branches return the same nil, the handler answers 202
		// either way, and what differs is which message goes to the address itself.
		//
		// Rolling back rather than returning early is what keeps the invite unspent: whoever holds it did
		// nothing wrong, and burning a use because somebody typed an address that already exists would
		// cost them the thing they were given.
		taken, err := q.UserExistsByEmail(ctx, email)
		if err != nil {
			return fmt.Errorf("checking email availability: %w", err)
		}
		if taken {
			return errEmailTakenSilently
		}

		user, err = q.CreateUser(ctx, db.CreateUserParams{
			ID:           int64(id),
			Username:     username,
			Email:        email,
			PasswordHash: &hash,
			DisplayName:  displayName,
			// Unverified when this instance can send mail, verified on creation when it cannot.
			//
			// The second is the accepted limitation M10 ships with. An instance with no relay cannot
			// verify an address by any route, so requiring it would mean nobody could register at all —
			// and the enumeration hole above stays open there regardless, since there is no mail to carry
			// the difference. See VerificationRequired.
			EmailVerifiedAt: verifiedOnCreation,
		})
		if err != nil {
			// Lost the race the pre-checks cannot close: two registrations for the same address or name
			// both pass their check and one loses at the constraint.
			//
			// The email case must come back as silence, not as a conflict. Reporting it would leave the
			// oracle intact through a window — narrow, but reachable on purpose by firing two requests at
			// once, which is not a hard attack to write. Reported as the taken branch is, minus the
			// notice: the loser cannot tell whether the winner was itself or somebody else, and sending a
			// second "somebody tried to register" mail for one attempt would be noise.
			switch conflict := registerConflict(err); {
			case errors.Is(conflict, ErrEmailTaken):
				return errEmailTakenSilently
			case conflict != nil:
				return conflict
			}
			return fmt.Errorf("creating user: %w", err)
		}

		if s.VerificationRequired() {
			msg, err = s.buildVerification(ctx, q, user)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errEmailTakenSilently) {
			// Nothing was created and any invite was rolled back — so this branch has to claim the username
			// some other way, or the two branches leave different state and two requests read the difference.
			// That was the oracle a security review found still open after the response itself had been made
			// uniform; migration 000011 carries the full account of it.
			//
			// Deliberately outside the transaction that just rolled back, and deliberately not fatal: the
			// caller has already been answered, and a failed reservation costs one probe of exposure rather
			// than a reason to behave differently — behaving differently is the whole thing being avoided.
			if err := s.queries.ReserveUsername(ctx, username); err != nil {
				logging.FromContext(ctx).Error().Err(err).
					Msg("reserving a username for a registration that created no account failed")
			}

			// The notice is queued rather than sent inline, so this branch does the same amount of
			// *blocking* work as the other one.
			if s.mailer != nil && s.mailer.Enabled() {
				if err := s.mailer.Enqueue(registrationNoticeMessage(s.publicBaseURL, email)); err != nil {
					logging.FromContext(ctx).Warn().Err(err).
						Msg("queueing a registration notice failed")
				}
			}
			return db.User{}, nil
		}
		return db.User{}, err
	}

	// After the commit, never inside it — the ordering rule gateway dispatch follows (CLAUDE.md rule 5).
	// A transaction that rolled back after the mail was queued would send a link to a token that does not
	// exist, and to an account that does not either.
	if msg.To != "" {
		if err := s.mailer.Enqueue(msg); err != nil {
			logging.FromContext(ctx).Warn().Err(err).
				Str("user_id", snowflake.ID(user.ID).String()).
				Msg("queueing a verification email failed")
		}
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
	user, err := s.verifyCredentials(ctx, in.Email, in.Password)
	if err != nil {
		return TokenPair{}, err
	}

	// The factor is asked about only after the password is right, which is what keeps every failure above
	// byte-identical to an instance with no second factor anywhere. A challenge returned before this point
	// — or a different answer for an account that has one — would be a fresh account-existence oracle on
	// top of the one M10 spent a milestone closing.
	proof, err := s.factorSatisfied(ctx, user.ID)
	if err != nil {
		if errors.Is(err, ErrTwoFactorRequired) {
			in.DeviceID = deviceID
			return TokenPair{}, s.twoFactorRequired(user.ID, in)
		}
		return TokenPair{}, err
	}

	return s.startSession(ctx, snowflake.ID(user.ID), deviceID, in.DeviceName, in.IP, proof)
}

// CompleteTwoFactorLogin finishes a sign-in that owed a factor.
//
// The device the session lands on comes out of the challenge, never from this call: a client that could
// name a different device on the second half of a sign-in could move somebody's session onto an identity
// of its choosing.
func (s *Service) CompleteTwoFactorLogin(ctx context.Context, rawChallenge, code string,
) (TokenPair, error) {
	challenge, err := s.parseTwoFactorChallenge(rawChallenge)
	if err != nil {
		return TokenPair{}, err
	}

	proof, err := s.proveFactor(ctx, challenge.UserID, code)
	if err != nil {
		return TokenPair{}, err
	}

	return s.startSession(ctx, snowflake.ID(challenge.UserID), challenge.Login.DeviceID,
		challenge.Login.DeviceName, challenge.Login.IP, proof)
}

// TwoFactorRequiredError is ErrTwoFactorRequired with the continuation the client needs to carry on.
//
// A typed error rather than a bare sentinel plus a second return value, so a handler that forgets to look
// for it produces a refusal rather than a sign-in — the same reason OAuthCallbackError carries its
// redirect. The challenge is minted here, in the service that holds the signing key, rather than handed to
// a handler to assemble.
type TwoFactorRequiredError struct {
	Challenge string
	ExpiresAt time.Time
}

func (e *TwoFactorRequiredError) Error() string { return ErrTwoFactorRequired.Error() }

// Unwrap makes errors.Is(err, ErrTwoFactorRequired) true, so a caller that only needs to know *that* a
// factor is owed does not have to know this type exists.
func (e *TwoFactorRequiredError) Unwrap() error { return ErrTwoFactorRequired }

// twoFactorRequired builds the error a sign-in path returns when the account owes a factor.
func (s *Service) twoFactorRequired(userID int64, in LoginInput) error {
	token, err := s.issueTwoFactorChallenge(userID, in)
	if err != nil {
		return err
	}
	return &TwoFactorRequiredError{Challenge: token, ExpiresAt: s.now().Add(twoFactorChallengeTTL)}
}

// verifyCredentials resolves an email address and password to an account, or refuses.
//
// Split out of Login because the device-flow verification page has to do the same thing and must not do it
// slightly differently (M9). Everything below is a property that survives only if there is one
// implementation of it: the dummy hash on a missing account, the single error for wrong-password and
// no-password-at-all, and the absence of any early return between them.
func (s *Service) verifyCredentials(ctx context.Context, rawEmail, password string) (db.User, error) {
	email := strings.TrimSpace(strings.ToLower(rawEmail))

	user, err := s.queries.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Burn the same argon2id work a real verification costs, so response time does not reveal
			// whether the address is registered. Returning early here is exactly the timing oracle that
			// lets an attacker enumerate an entire user base without guessing a single password.
			return db.User{}, VerifyPasswordForMissingUser(ctx, password)
		}
		return db.User{}, fmt.Errorf("looking up account: %w", err)
	}

	stored := ""
	if user.PasswordHash != nil {
		stored = *user.PasswordHash
	}
	if err := VerifyPassword(ctx, stored, password); err != nil {
		if errors.Is(err, ErrInvalidCredentials) || errors.Is(err, ErrPasswordNotSet) {
			// Both are reported to the client identically. "This account exists but signs in with Google"
			// is precisely the kind of detail that turns a login form into an account-discovery tool.
			return db.User{}, ErrInvalidCredentials
		}
		return db.User{}, err
	}

	// An unverified address is refused with the *same* answer a wrong password gets, and the check lives
	// here rather than in Login for a reason found by review: the device verification page authenticates
	// through this function too, and when the gate sat in Login that page handed out a full token pair on
	// an account ordinary login refused. Anything that turns a password into a session comes through here,
	// so this is the only place the gate cannot be walked around.
	//
	// Reporting it distinctly reopens the oracle registration closes, in two requests: register an address
	// with a password of your choosing, then log in with it — if the address was free an account now
	// exists with that password and the login says "unverified"; if it was taken nothing was created and
	// it says "wrong password". Measured at 403 against 401 before this was written.
	//
	// So the difference goes where every other difference in this milestone goes: the mailbox. The person
	// who owns the address gets a fresh link and an explanation; the caller gets the same refusal either
	// way. The reminder follows the password check, so guessing at addresses queues nothing.
	//
	// A verified-by-creation account never reaches this: an instance with no relay marks accounts verified
	// on creation, and every account predating M10 was backfilled by migration 000010.
	if !user.EmailVerifiedAt.Valid {
		s.remindToVerify(ctx, user)
		return db.User{}, ErrInvalidCredentials
	}
	return user, nil
}

// startSession revokes the device's previous family and issues a fresh pair.
//
// It takes a factorProof, and that parameter is the enforcement rather than a formality: proof is a value
// only twofactor.go can construct, so a caller cannot mint a session without having first asked whether
// the account owes a second factor. A future third way to sign in will not compile until its author has.
// See twofactor.go's header for why the rule is a type rather than a check.
func (s *Service) startSession(ctx context.Context, userID snowflake.ID, deviceID, deviceName string,
	ip netip.Addr, proof factorProof,
) (TokenPair, error) {
	if !proof.authorizes(int64(userID)) {
		// Not reachable from any caller in this package, which is the point: it is here so that if one
		// ever does become reachable it fails closed rather than issuing a pair.
		return TokenPair{}, ErrTwoFactorRequired
	}

	raw, hash, err := GenerateRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}

	sessionID, err := s.ids.Next()
	if err != nil {
		return TokenPair{}, fmt.Errorf("generating session ID: %w", err)
	}

	err = database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		return s.writeSession(ctx, s.queries.WithTx(tx), sessionID, userID, deviceID, deviceName, ip, hash)
	})
	if err != nil {
		return TokenPair{}, err
	}

	return s.issuePair(userID, sessionID, raw)
}

// writeSession is startSession's database half, taking the queries handle rather than opening its own
// transaction.
//
// Split out so a caller with something else to commit atomically alongside the session can do so. The
// device flow is the one that needs it: spending the code and creating the session have to be one thing,
// or a transient failure between them burns an authorization the person can only replace by walking back
// to another device (M9).
func (s *Service) writeSession(ctx context.Context, q *db.Queries, sessionID, userID snowflake.ID,
	deviceID, deviceName string, ip netip.Addr, hash TokenHash,
) error {
	if _, err := q.RevokeSessionsForDevice(ctx, db.RevokeSessionsForDeviceParams{
		UserID:   int64(userID),
		DeviceID: deviceID,
	}); err != nil {
		return fmt.Errorf("revoking the previous session for this device: %w", err)
	}

	// FirstSeen is deliberately left unset here, which the query reads as now(): this is a sign-in, and a
	// sign-in is when a device is first seen. Rotation is the path that carries the existing value forward
	// (000013). Signing in again on a device that was already signed in resets it, which is right — the
	// previous family was just revoked two statements above.
	if _, err := q.CreateSession(ctx, db.CreateSessionParams{
		ID:               int64(sessionID),
		UserID:           int64(userID),
		DeviceID:         deviceID,
		RefreshTokenHash: hash,
		DeviceName:       optionalString(deviceName),
		IpAddress:        optionalAddr(ip),
		ExpiresAt:        timestamptz(s.now().Add(RefreshTokenTTL)),
	}); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
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
			// Carried forward, not recomputed: this is what the device's "signed in since" means, and a
			// rotation is not a new sign-in. Recomputing would reset it every fifteen minutes (000013).
			FirstSeen: session.FirstSeen,
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
