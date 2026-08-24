package login

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/Alexnex31/Norite/cli/internal/termsafe"
)

// `norite login --device-code`, and what `--provider` falls back to on a machine with no browser of its
// own (Milestone M9).
//
// Ask the instance for a pair of codes, print the short one and where to type it, and poll until somebody
// has approved it on another device. Nothing local is needed at all — no listener, no port, no browser —
// which is the whole point: it is the one sign-in flow that works over SSH on a server, for an account
// that has no password.
//
// # What is safe to print here, and what is not
//
// The user code, and only the user code. The device code is a credential — it is what redeems for a token
// pair — so it never reaches the terminal or a log (rule 8).
//
// # A new rule-19 boundary, and a different one from M8's
//
// M8's listener took its input from a local socket any process could write to. This takes it from the
// instance, over TLS — and is still not trusted, because `--instance` is a URL somebody handed the person
// running this. Two of the three values are checked rather than sanitized, because both direct an action:
// the user code is what somebody will type, and the verification URI is what they will visit and enter
// credentials into. A value that also bounds something is rejected, not cleaned — the rule
// ParseInstanceURL already follows.

// pollBackoff is added to the interval each time the instance says slow_down.
//
// Additive rather than doubling, which is what RFC 8628 §3.5 specifies: the instance is asking for more
// space, not reporting an outage, and a client that doubled its way to minutes would leave somebody
// staring at an approved page while nothing happened.
const pollBackoff = 5 * time.Second

// maxPollInterval bounds where that can end up, in case an instance answers slow_down to everything.
const maxPollInterval = 60 * time.Second

// deviceAuth is what the instance issued.
type deviceAuth struct {
	// DeviceCode is a credential. Never printed, never logged.
	DeviceCode string `json:"device_code"`
	// UserCode is the half a person reads and types.
	UserCode string `json:"user_code"`
	// VerificationURI is where they go to type it.
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// signInWithDeviceCode runs the device flow and returns the pair it produced.
func (r *Runner) signInWithDeviceCode(ctx context.Context, s session) (tokenPair, error) {
	auth, err := s.api.startDeviceAuth(ctx, deviceCodeRequest{
		DeviceID:   s.deviceID,
		DeviceName: s.deviceName,
	})
	if err != nil {
		return tokenPair{}, err
	}

	verification, err := checkVerificationURI(s.instanceURL, auth.VerificationURI)
	if err != nil {
		return tokenPair{}, err
	}
	userCode, err := checkUserCode(auth.UserCode)
	if err != nil {
		return tokenPair{}, err
	}

	r.printf("\nTo sign in, open this on any device with a browser:\n\n  %s\n\n", verification)
	r.printf("and enter this code:\n\n  %s\n\n", userCode)
	r.printf("Waiting for approval. The code expires in %s. Press Ctrl-C to stop.\n",
		humanDuration(lifetimeOf(auth)))

	return r.pollForApproval(ctx, s, auth)
}

// pollForApproval asks until there is an answer, the code dies, or the caller gives up.
func (r *Runner) pollForApproval(ctx context.Context, s session, auth deviceAuth) (tokenPair, error) {
	interval := intervalOf(auth)
	deadline := r.clock().Add(lifetimeOf(auth))

	for {
		// Waited before the first ask as well as between them, deliberately. Nobody can have approved a
		// code that was issued a millisecond ago, so an immediate poll spends a request on a certain
		// "pending" — and, worse, sets the instance's clock for what counts as too fast.
		if err := r.sleep(ctx, interval); err != nil {
			return tokenPair{}, err
		}

		pair, err := s.api.pollDeviceAuth(ctx, auth.DeviceCode)
		switch {
		case err == nil:
			return pair, nil

		case errors.Is(err, errDeviceAuthorizationPending):
			// The ordinary answer, and the one that says nothing at all. Keep going.

		case errors.Is(err, errDeviceSlowDown):
			// The instance asking for room. Widening this rather than ignoring it is what stops a
			// mis-tuned client being throttled off the instance for the rest of the flow.
			interval = min(interval+pollBackoff, maxPollInterval)

		default:
			return tokenPair{}, err
		}

		if r.clock().After(deadline) {
			// Belt to the instance's braces: it will answer expired_token on its own, and this stops a
			// client whose code was issued by an instance that then went away from polling forever.
			return tokenPair{}, errors.New(
				"that code expired before it was approved; run `norite login` again")
		}
	}
}

// checkUserCode refuses anything the instance's own contract says it cannot have issued.
//
// Checked rather than sanitized. This value is about to be printed as an instruction — "type this" — so a
// version of it that has been cleaned up is worse than none: somebody would type the cleaned version and
// be told it is wrong, with nothing on screen explaining why. The shape is in contracts/openapi.yaml.
func checkUserCode(raw string) (string, error) {
	for _, r := range raw {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return "", errors.New("the instance issued a sign-in code this client cannot display")
		}
	}
	if raw == "" || len(raw) > 32 {
		return "", errors.New("the instance issued a sign-in code this client cannot display")
	}
	return raw, nil
}

// checkVerificationURI refuses a destination that is not this instance.
//
// The strictest check in this file, because this is a URL somebody is about to open and type a password
// into. It must be the instance they asked for: an instance that answers with somewhere else is either
// misconfigured or hostile, and printing it either way would make this command the delivery mechanism.
//
// Compared against the instance URL already resolved and already validated, not against anything in the
// response.
func checkVerificationURI(instanceURL, raw string) (string, error) {
	const refused = "the instance pointed this sign-in somewhere other than itself, so it was not opened"

	want, err := url.Parse(instanceURL)
	if err != nil {
		return "", errors.New(refused)
	}
	got, err := url.Parse(raw)
	if err != nil {
		return "", errors.New(refused)
	}

	// Scheme and host both, and host includes the port. An instance reached over https that answers with
	// http on the same host has downgraded the one page where a password gets typed.
	if got.Scheme != want.Scheme || got.Host != want.Host || got.User != nil {
		return "", errors.New(refused)
	}

	// The parser's own re-serialization, so what is printed is what was understood — the same reason
	// ParseOAuthClientRedirect returns one on the other side of this flow.
	//
	// Checked against the sanitizer rather than passed through it, because this string is an instruction:
	// somebody is about to open it. Cleaning it silently would print an address that is not the one the
	// instance sent, and refusing costs a legitimate instance nothing, since a verification URI has no
	// reason to contain anything the sanitizer removes. url.Parse rejects ASCII controls and stops there —
	// the bidi overrides are multi-byte and travel through it and through String() untouched, which is
	// enough to make a path render as another host (rule 19).
	out := got.String()
	if termsafe.Text(out) != out {
		return "", errors.New(refused)
	}
	return out, nil
}

// lifetimeOf is how long the instance says the code lives, bounded.
//
// A value from the instance that decides how long this command sits there, so it is clamped rather than
// believed: zero or negative would make the loop exit before its first poll, and an implausible one would
// hang a terminal for a week.
func lifetimeOf(auth deviceAuth) time.Duration {
	life := time.Duration(auth.ExpiresIn) * time.Second
	return clampDuration(life, time.Minute, time.Hour)
}

// intervalOf is how often the instance says to ask, bounded the same way and for the same reason — a zero
// here is a busy loop against somebody else's server.
func intervalOf(auth deviceAuth) time.Duration {
	return clampDuration(time.Duration(auth.Interval)*time.Second, time.Second, maxPollInterval)
}

func clampDuration(d, lo, hi time.Duration) time.Duration {
	return min(max(d, lo), hi)
}

// humanDuration renders a wait the way somebody would say it.
func humanDuration(d time.Duration) string {
	minutes := int(d.Round(time.Minute).Minutes())
	if minutes <= 1 {
		return "a minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}

// sleep waits, or reports that the caller gave up.
func (r *Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.after != nil {
		select {
		case <-r.after(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// clock is the current time, indirected so a test can run a twenty-minute flow instantly.
func (r *Runner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// deviceCodeFallbackNotice explains, once, why this flow was chosen rather than the browser one.
//
// Never silent, for the reason ADR 0025 gives about the keyring: a degradation nobody is told about is one
// discovered later, at a worse time. Somebody who expected a browser to open needs to know it will not,
// and somebody who did not know this flow existed needs to know what they are looking at.
func deviceCodeFallbackNotice(provider string) string {
	if provider == "" {
		return "No browser is reachable from this machine, so signing in with a code instead."
	}
	return fmt.Sprintf(
		"No browser is reachable from this machine, so signing in with a code instead of opening %s here.",
		providerLabel(provider))
}
