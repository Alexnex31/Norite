package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"runtime"
	"unicode/utf8"

	"github.com/alexedwards/argon2id"
)

// Password policy.
//
// A length floor and no composition rules, which is the current NIST guidance and the opposite of the
// classic "one uppercase, one symbol" advice: composition rules push people toward predictable
// substitutions while ruling out strong passphrases. The ceiling exists purely to bound work — argon2id
// hashes its input, so a megabyte-long password would otherwise be a free way to make the server do
// unbounded work per login attempt.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 256
)

// ErrPasswordTooShort and ErrPasswordTooLong report policy violations.
var (
	ErrPasswordTooShort   = fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	ErrPasswordTooLong    = fmt.Errorf("password must be at most %d characters", MaxPasswordLength)
	ErrPasswordNotSet     = errors.New("this account has no password set")
	ErrInvalidCredentials = errors.New("invalid credentials")
)

// hashParams are the argon2id cost parameters.
//
// argon2id (not argon2i or argon2d) because it is the variant RFC 9106 recommends for password hashing:
// hybrid resistance to both side-channel and GPU-cracking attacks. The figures are the library's own
// recommended defaults, which track the RFC's second recommended option — 64 MiB of memory is the parameter
// that actually costs an attacker, since memory is what does not parallelise cheaply on a GPU.
//
// These are deliberately *not* configurable. An operator who tunes them down to make logins faster has
// silently weakened every stored password, and the stored hash records its own parameters anyway, so a
// future increase can re-hash on next login without a migration.
var hashParams = &argon2id.Params{
	Memory:      64 * 1024, // 64 MiB
	Iterations:  1,
	Parallelism: 4,
	SaltLength:  16,
	KeyLength:   32,
}

// dummyHash is compared against when an account does not exist or has no password.
//
// Login must take the same time whether or not the email is registered. Without this, an attacker measures
// the difference between "no such user, returned immediately" and "user found, argon2id ran for ~50ms" and
// enumerates the entire user base without ever guessing a password. It is computed once at startup rather
// than per request, because doing the work is the point but doing it twice is not.
var dummyHash = mustHash("norite-timing-equalizer-not-a-real-password")

func mustHash(password string) string {
	h, err := argon2id.CreateHash(password, hashParams)
	if err != nil {
		panic("auth: could not compute the timing-equalizer hash: " + err.Error())
	}
	return h
}

// maxConcurrentHashes bounds how many argon2id operations may run at once.
//
// Each one allocates hashParams.Memory — 64 MiB — for its whole duration, which is the entire point of a
// memory-hard KDF and also a denial-of-service surface: 16 concurrent logins measured at 1 GiB of heap, and
// the per-IP rate limiter does nothing about a distributed flood, where a few hundred sources each sending
// one login a second would exhaust any machine.
//
// Gating makes the worst case arithmetic instead of unbounded: slots x 64 MiB. Sized from GOMAXPROCS
// (cgroup-aware since Go 1.25, so a small pod on a big node stays small) and floored at 2 so a
// single-core instance can still serve logins. Excess requests wait for a slot rather than being refused —
// a brief queue is a far better failure than an OOM kill, and each waiter is already bounded by its own
// request context.
var maxConcurrentHashes = max(2, runtime.GOMAXPROCS(0)/2)

// hashSlots is the gate itself. A buffered channel rather than x/sync/semaphore: the acquire needs to be
// selectable against a context, which a plain channel does natively and without another dependency.
var hashSlots = make(chan struct{}, maxConcurrentHashes)

// withHashSlot runs fn while holding one of the gate's slots.
//
// A caller whose context is canceled while waiting — a client that hung up, or a request past its deadline
// — gives up without ever starting the work, which is what keeps a queue from turning into the same memory
// problem one step later.
func withHashSlot(ctx context.Context, fn func() error) error {
	select {
	case hashSlots <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("%w: server is busy verifying other credentials", ctx.Err())
	}
	defer func() { <-hashSlots }()

	return fn()
}

// HashPassword applies the password policy and returns an argon2id encoded hash.
//
// The returned string carries its own algorithm, parameters, and salt, so verification needs nothing else
// stored alongside it and a future parameter change stays backwards-compatible.
func HashPassword(ctx context.Context, password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}

	var hash string
	err := withHashSlot(ctx, func() error {
		var err error
		hash, err = argon2id.CreateHash(password, hashParams)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return hash, nil
}

// ValidatePassword checks the policy without hashing.
//
// Length is counted in runes, not bytes: a 12-character passphrase in a non-Latin script is a 12-character
// passphrase, and rejecting it for being "too short" while accepting 12 ASCII letters would be both wrong
// and quietly discriminatory.
func ValidatePassword(password string) error {
	switch n := utf8.RuneCountInString(password); {
	case n < MinPasswordLength:
		return ErrPasswordTooShort
	case n > MaxPasswordLength:
		return ErrPasswordTooLong
	default:
		return nil
	}
}

// VerifyPassword reports whether password matches the stored hash.
//
// storedHash may be empty, meaning the account has no password (OAuth-only, from M6). That case still runs
// a full comparison against the dummy hash before returning, for the same timing reason as an unknown
// account — otherwise "this email exists but has no password" becomes measurably distinct.
func VerifyPassword(ctx context.Context, storedHash, password string) error {
	if storedHash == "" {
		_ = withHashSlot(ctx, func() error {
			_, _ = argon2id.ComparePasswordAndHash(password, dummyHash)
			return nil
		})
		return ErrPasswordNotSet
	}

	var match bool
	err := withHashSlot(ctx, func() error {
		var err error
		match, err = argon2id.ComparePasswordAndHash(password, storedHash)
		return err
	})
	if err != nil {
		// A malformed stored hash is a data-integrity problem, not a wrong password, and the difference
		// matters to an operator reading logs. The caller still tells the client only "invalid credentials".
		return fmt.Errorf("comparing password against stored hash: %w", err)
	}
	if !match {
		return ErrInvalidCredentials
	}
	return nil
}

// VerifyPasswordForMissingUser burns the same work a real verification would.
//
// Called on the login path when no account matched, so that the handler's total time does not reveal
// whether the email exists. Returns ErrInvalidCredentials always — the caller must not be able to
// accidentally treat it as success.
func VerifyPasswordForMissingUser(ctx context.Context, password string) error {
	_ = withHashSlot(ctx, func() error {
		_, _ = argon2id.ComparePasswordAndHash(password, dummyHash)
		return nil
	})
	return ErrInvalidCredentials
}

// constantTimeEquals compares two secrets without leaking their contents through timing.
//
// Used for the fixed-length hashes this package compares itself; argon2id's own comparison is already
// constant-time internally.
func constantTimeEquals(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
