package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// The device-code flow (Milestone M9), for a machine that has no browser of its own.
//
// # What it is for
//
// M8's loopback flow binds a listener on 127.0.0.1, so the browser that finishes the sign-in has to be on
// the same machine as the client. On a server reached over SSH there is no such browser, and forwarding a
// port is not always possible — which leaves an account that signs in only with Google or GitHub unable to
// log in there at all, since it has no password to fall back to. That is the hole this closes.
//
// # The two codes
//
// A device code, held only by the waiting client and never shown to anyone, and a user code, read off a
// terminal and typed into a browser on another device. They are sized for their jobs rather than alike:
// the first is 256 bits like every other credential here, the second is eight characters because a person
// has to copy it by hand.
//
// # What actually protects this flow
//
// Not the user code's entropy, which is deliberately modest. The device-code grant's real risk is not
// guessing — it is a person being sent a code by somebody else and authorizing a stranger's machine
// (RFC 8628 §5.4). Nothing cryptographic prevents that, so the defenses are: an explicit approval step
// that names the device and echoes the code to compare against, no URL that carries the code and can
// therefore be clicked, a short life, and a rate limit on the one endpoint where a code can be tried.

// DeviceCodeTTL is how long an unfinished device authorization lives.
//
// Twenty minutes rather than the fifteen an OAuth state gets, and the difference is load-bearing: the
// verification page can start a *provider* sign-in, which is its own fifteen-minute round trip out to
// Google or GitHub. A device code that expired first would strand somebody who typed their code at minute
// fourteen and did everything right afterwards.
const DeviceCodeTTL = 20 * time.Minute

// DevicePollInterval is how often a waiting client may ask.
//
// Published to the client in the issuing response and documented in contracts/openapi.yaml, because a
// client cannot import this constant and has to size its own loop from something.
const DevicePollInterval = 5 * time.Second

// maxDeviceNameLength bounds what the approval page will display.
//
// The name is chosen by whoever asked for the code, and the approval page shows it so a person can see
// what they are authorizing. That makes it the one piece of attacker-influenced text on that page, and an
// unbounded one could push the warning beside it off the screen. html/template escapes it; this stops it
// shouting.
const maxDeviceNameLength = 64

// userCodeAlphabet is what a user code is built from.
//
// Twenty characters, chosen by two subtractions from A-Z. Out go A, E, I, O and U, so a draw is very
// unlikely to spell anything a person then has to read out loud to somebody — Y stays, being the only
// sometimes-vowel left and not enough on its own. Out goes L, which is the third member of the 1/I/L
// group that gets misread off a terminal and mistyped into a phone; 0 and O never arrive, digits being
// absent entirely.
//
// Twenty to the eighth is about 2.6e10, or 34.6 bits — far past reach for a code that lives twenty minutes
// behind a rate limit, and short enough to copy by hand. Changing this string or userCodeLength changes
// that number, which is the figure the verification page's rate limit is sized against. They move together
// or not at all.
const userCodeAlphabet = "BCDFGHJKMNPQRSTVWXYZ"

// userCodeLength is how many characters a user code has, before the grouping dash is added for display.
const userCodeLength = 8

// userCodeGroup is where the dash goes. Purely presentational: ParseUserCode strips it, so a person who
// types the code without one is not punished for it.
const userCodeGroup = 4

// Device-flow errors.
//
// The first four are the poll's whole vocabulary and map one-to-one onto what RFC 8628 §3.5 defines. They
// are sentinels rather than messages because a client branches on them; the prose a person reads is
// written by that client, for the reason M8's failure vocabulary gives.
var (
	// ErrDeviceAuthorizationPending is nobody having approved yet. The ordinary answer to almost every
	// poll.
	ErrDeviceAuthorizationPending = errors.New("this device authorization has not been approved yet")

	// ErrDeviceSlowDown is a client asking faster than the interval it was given.
	ErrDeviceSlowDown = errors.New("polling too fast for this authorization's interval")

	// ErrDeviceCodeExpired covers unknown, expired and already-spent device codes, which are one answer
	// for the reason every other credential in this package gets one: the differences are only useful to
	// somebody probing.
	ErrDeviceCodeExpired = errors.New("this device authorization has expired; start again")

	// ErrDeviceAccessDenied is somebody pressing Deny. Distinct from an expiry on purpose — it is the one
	// outcome a waiting client should stop on immediately rather than keep asking about.
	ErrDeviceAccessDenied = errors.New("this device authorization was denied")

	// ErrDeviceUserCode is the verification page's answer to a code it cannot act on: never issued,
	// expired, already approved, already denied, already spent. One answer for all of them.
	ErrDeviceUserCode = errors.New("that code is not valid. Check it and try again, or start again on your device")

	// ErrDeviceContinuation is a verification-page continuation that is missing, expired, tampered with,
	// or of the wrong kind. One answer for all of them, and the wrong-kind case is the one that matters:
	// an entry token presented where an approval token belongs is a browser that knows a code trying to
	// authorize an account it has proved nothing about.
	ErrDeviceContinuation = errors.New("that sign-in step has expired; start again")

	// ErrDeviceFlowUnavailable is an instance with no public_base_url, which cannot tell a client where to
	// send a person. The same shape as ErrPasswordResetUnavailable: a feature that needs configuration
	// says so rather than half-working.
	ErrDeviceFlowUnavailable = errors.New(
		"this instance is not configured for the device sign-in flow")
)

// StartDeviceAuthInput is what a headless client supplies to begin an authorization.
type StartDeviceAuthInput struct {
	// DeviceID identifies this installation, and the session that eventually comes out of the flow is
	// scoped to it. Captured here rather than at poll time because it is also the value the approval page
	// is describing when it names a device.
	DeviceID string
	// DeviceName is what the account's session list — and the approval page — will call this machine.
	DeviceName string
}

// DeviceAuth is an issued authorization, as the client sees it.
type DeviceAuth struct {
	// DeviceCode is the secret half. Returned to the client once and never stored in a recoverable form.
	DeviceCode string
	// UserCode is the half a person reads and types, in its display form (with the grouping dash).
	UserCode string
	// ExpiresAt is when both stop working.
	ExpiresAt time.Time
	// Interval is how often the client may poll.
	Interval time.Duration
}

// StartDeviceAuth issues a device code and the user code that goes with it.
func (s *Service) StartDeviceAuth(ctx context.Context, in StartDeviceAuthInput) (DeviceAuth, error) {
	deviceID, err := normalizeDeviceID(in.DeviceID)
	if err != nil {
		return DeviceAuth{}, err
	}

	rawCode, codeHash, err := GenerateDeviceCode()
	if err != nil {
		return DeviceAuth{}, err
	}

	expiresAt := s.now().Add(DeviceCodeTTL)

	// The user code is short, so two live ones colliding is unlikely rather than impossible. Retried
	// against the unique index rather than pre-checked, because a pre-check races: two requests can both
	// find a code free and both try to use it. Three attempts, because a fourth collision means something
	// other than bad luck.
	var row db.DeviceCode
	var userCode string
	for attempt := range 3 {
		userCode, err = GenerateUserCode()
		if err != nil {
			return DeviceAuth{}, err
		}

		id, idErr := s.ids.Next()
		if idErr != nil {
			return DeviceAuth{}, fmt.Errorf("generating device code ID: %w", idErr)
		}

		row, err = s.queries.CreateDeviceCode(ctx, db.CreateDeviceCodeParams{
			ID:             int64(id),
			DeviceCodeHash: codeHash,
			UserCode:       userCode,
			DeviceID:       deviceID,
			DeviceName:     truncateDeviceName(in.DeviceName),
			ExpiresAt:      timestamptz(expiresAt),
		})
		if err == nil {
			break
		}
		if constraint := uniqueViolation(err); strings.Contains(constraint, "user_code") && attempt < 2 {
			continue
		}
		return DeviceAuth{}, fmt.Errorf("recording device authorization: %w", err)
	}

	return DeviceAuth{
		DeviceCode: rawCode,
		UserCode:   FormatUserCode(userCode),
		ExpiresAt:  row.ExpiresAt.Time,
		Interval:   DevicePollInterval,
	}, nil
}

// RedeemDeviceCode is one poll from a waiting client.
//
// Every return is either a token pair or one member of the fixed vocabulary above. A caller that adds a
// fifth outcome here has to add it to contracts/openapi.yaml and to every client in the same change,
// which is the point of keeping the set this small.
func (s *Service) RedeemDeviceCode(ctx context.Context, rawCode string, ip netip.Addr) (TokenPair, error) {
	hash, err := ParseDeviceCode(rawCode)
	if err != nil {
		// Shape-checked before the database is touched, so a flood of junk costs a string comparison each
		// rather than an indexed lookup. Reported as an expiry like everything else unknown.
		return TokenPair{}, ErrDeviceCodeExpired
	}

	// One statement: records this poll and reports the state as it was before it did.
	polled, err := s.queries.PollDeviceCode(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenPair{}, ErrDeviceCodeExpired
		}
		return TokenPair{}, fmt.Errorf("polling device authorization: %w", err)
	}

	// Checked before anything else, and before the outcome is even looked at. A client polling every
	// second must keep being told to slow down rather than being rewarded once its authorization lands,
	// which is what RFC 8628 §3.5 asks for and the only thing that makes the interval mean anything.
	if polled.PreviousPolledAt.Valid &&
		s.now().Sub(polled.PreviousPolledAt.Time) < DevicePollInterval {
		return TokenPair{}, ErrDeviceSlowDown
	}

	if polled.DeniedAt.Valid {
		return TokenPair{}, ErrDeviceAccessDenied
	}
	if polled.UserID == nil {
		return TokenPair{}, ErrDeviceAuthorizationPending
	}

	// Spent in SQL, so two processes holding the same device code produce one session between them rather
	// than one each. The row read above is a snapshot; this is the decision.
	row, err := s.queries.ConsumeDeviceCode(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenPair{}, ErrDeviceCodeExpired
		}
		return TokenPair{}, fmt.Errorf("consuming device authorization: %w", err)
	}

	// The same session machinery a password login and an OAuth exchange use, scoped to the device identity
	// captured when the code was issued — never to anything the approving browser said.
	return s.startSession(ctx, snowflake.ID(*row.UserID), row.DeviceID, row.DeviceName, ip)
}

// LookUpDeviceCode finds a live authorization by the code somebody typed.
//
// Every reason a code cannot be acted on — never issued, expired, already approved, denied, spent — is one
// answer, for the reason every other credential lookup in this package gives. A person who mistyped theirs
// is told the same actionable thing either way.
// Returns the normalized code alongside the row, because every later step carries it — the page shows it
// back for comparison against the terminal, and the row holds only its hash.
func (s *Service) LookUpDeviceCode(ctx context.Context, rawUserCode string) (string, db.DeviceCode, error) {
	code, err := ParseUserCode(rawUserCode)
	if err != nil {
		// Shape-checked before the database is touched, so a run of guesses at the verification page costs
		// a string scan each rather than an indexed lookup.
		return "", db.DeviceCode{}, ErrDeviceUserCode
	}

	row, err := s.queries.GetDeviceCodeByUserCode(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", db.DeviceCode{}, ErrDeviceUserCode
		}
		return "", db.DeviceCode{}, fmt.Errorf("looking up device authorization: %w", err)
	}
	return code, row, nil
}

// deviceCodeByID re-reads a live authorization between the verification page's steps.
func (s *Service) deviceCodeByID(ctx context.Context, id int64) (db.DeviceCode, error) {
	row, err := s.queries.GetDeviceCodeByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.DeviceCode{}, ErrDeviceUserCode
		}
		return db.DeviceCode{}, fmt.Errorf("looking up device authorization: %w", err)
	}
	return row, nil
}

// ApproveDeviceAuthorization records that somebody authorized this device, which is the moment a waiting
// client's next poll starts returning a token pair.
//
// Single-use lives in the query's WHERE clause, so a replayed approval — a reloaded page, a stale tab,
// somebody pressing the button twice — matches nothing rather than re-pointing an authorization at another
// account. That is what makes the approval token safe to be an ordinary signed value with no store of its
// own.
func (s *Service) ApproveDeviceAuthorization(ctx context.Context, deviceCodeID, userID int64) error {
	_, err := s.queries.ApproveDeviceCode(ctx, db.ApproveDeviceCodeParams{
		ID:     deviceCodeID,
		UserID: &userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDeviceUserCode
		}
		return fmt.Errorf("approving device authorization: %w", err)
	}
	return nil
}

// DeviceDenyOutcome is what pressing Deny actually managed to do.
//
// Three answers rather than one, because the page has to tell the truth and the three are not the same
// news. The distinction exists at all because of the order people press things: approving and then
// realizing is the likeliest way anybody escapes the phishing this flow is vulnerable to, and Deny is
// where they go.
type DeviceDenyOutcome int

const (
	// DeviceDenyStopped is the ordinary case: nothing had been authorized, and now nothing can be.
	DeviceDenyStopped DeviceDenyOutcome = iota
	// DeviceDenyRevoked is an approval taken back before the waiting client collected it. The code is
	// spent, so the next poll gets expired_token and no session is ever created.
	DeviceDenyRevoked
	// DeviceDenyTooLate is an approval the client already redeemed. There is a live session now, and
	// nothing on this page can reach it — the person has to be told that plainly.
	DeviceDenyTooLate
)

// DenyDeviceAuthorization ends an authorization now, and reports how much of it there was left to end.
//
// Worth having rather than letting the code expire: a person who realizes they were sent a code by
// somebody else can stop it at once, and the waiting client is told to give up on its next poll instead of
// sitting there for the rest of the twenty minutes.
//
// The second and third outcomes are what this function used to get wrong. It reported success for every
// row it did not match, so pressing Deny a second after pressing Approve rendered "nothing was signed in"
// while the device was authorized and about to collect — the page telling somebody they were safe at
// exactly the moment they were not, on the one recovery path ADR 0028 promises.
func (s *Service) DenyDeviceAuthorization(ctx context.Context, deviceCodeID, userID int64,
) (DeviceDenyOutcome, error) {
	if _, err := s.queries.DenyDeviceCode(ctx, deviceCodeID); err == nil {
		return DeviceDenyStopped, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return DeviceDenyStopped, fmt.Errorf("denying device authorization: %w", err)
	}

	// Nothing to deny, which most often means it has already been approved — by the person now pressing
	// Deny. Spending the code is the strongest thing still available, and it is scoped to the account the
	// approval named so one account's regret cannot revoke another's sign-in.
	if _, err := s.queries.RevokeApprovedDeviceCode(ctx, db.RevokeApprovedDeviceCodeParams{
		ID:     deviceCodeID,
		UserID: &userID,
	}); err == nil {
		return DeviceDenyRevoked, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return DeviceDenyStopped, fmt.Errorf("revoking device authorization: %w", err)
	}

	// Already redeemed, already expired, or never this account's. All three are past reach from here.
	return DeviceDenyTooLate, nil
}

// GenerateUserCode mints a code a person can read off one screen and type into another.
//
// Rejection sampling rather than a modulo: 256 is not a multiple of 20, so folding a random byte would
// make the first sixteen letters of the alphabet more likely than the last four. That costs about 6% of
// draws to reject and buys the entropy figure above being true rather than approximately true.
func GenerateUserCode() (string, error) {
	const limit = 256 - (256 % len(userCodeAlphabet)) // 240: the largest whole number of buckets

	out := make([]byte, 0, userCodeLength)
	buf := make([]byte, userCodeLength)
	for len(out) < userCodeLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generating user code: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, userCodeAlphabet[int(b)%len(userCodeAlphabet)])
			if len(out) == userCodeLength {
				break
			}
		}
	}
	return string(out), nil
}

// ParseUserCode normalizes what somebody typed into the form the database stores.
//
// Generous about presentation and strict about content. Case, spaces and dashes are all things a person
// gets wrong or a phone keyboard adds on its own, and none of them carry meaning — so they are fixed
// rather than refused. Anything left that is not in the alphabet is refused, because it cannot be a code
// this instance issued and there is nothing to look up.
func ParseUserCode(raw string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		switch {
		case r == '-' || r == ' ' || r == '\t':
			continue
		case strings.ContainsRune(userCodeAlphabet, r):
			b.WriteRune(r)
		default:
			return "", ErrDeviceUserCode
		}
		if b.Len() > userCodeLength {
			return "", ErrDeviceUserCode
		}
	}
	if b.Len() != userCodeLength {
		return "", ErrDeviceUserCode
	}
	return b.String(), nil
}

// FormatUserCode renders a stored code for display, grouped so it can be read aloud and typed in halves.
func FormatUserCode(code string) string {
	if len(code) != userCodeLength {
		return code
	}
	return code[:userCodeGroup] + "-" + code[userCodeGroup:]
}

// truncateDeviceName bounds a device name to what the approval page can show.
//
// By runes rather than bytes, so cutting a name does not leave half a character behind — the same reason
// the CLI's own truncation works that way.
func truncateDeviceName(name string) string {
	name = strings.TrimSpace(name)
	runes := []rune(name)
	if len(runes) <= maxDeviceNameLength {
		return name
	}
	return string(runes[:maxDeviceNameLength])
}
