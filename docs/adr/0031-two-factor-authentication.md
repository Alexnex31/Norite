# ADR 0031: Two-factor authentication, and why it lands in Phase B

## Status
Accepted. Extends [ADR 0022](0022-access-token-signing-and-scope-model.md) and
[ADR 0011](0011-token-based-client-auth.md) without reversing either; the factor model it adds is the input
[ADR 0014](0014-e2e-encryption.md)'s device-linking flow depends on and never named. Scheduled as `M11a`.

## Context
Eight milestones (M4–M11) built this project's auth core, and it is careful work. An account-existence
oracle was closed across three endpoints; `registration_reservations` exists because two registration
branches otherwise left different username-namespace state; `HashPassword` runs before the address check so
both branches pay argon2id, with a test asserting the ratio; the device-code flow's phishing surface is
reasoned through at length in `architecture.md` §14.21.

Against all of that, the factor model is "a password, or a provider's word". A repo-wide search for `2FA`,
`TOTP`, `MFA`, `two-factor` and `WebAuthn` returns nothing — no column, no endpoint, no ADR, and no entry
in §17 marking it as a deliberate exclusion. It was an omission, not a decision, and this ADR is the
decision.

The reason it is not merely a missing feature is [ADR 0014](0014-e2e-encryption.md). Linking a new device
to an account's E2E identity is authorized by the primary device, and the primary device is reached by
signing in. So whatever protects a sign-in also protects the E2E device-trust chain, and E2E is the one
feature in this plan whose entire promise is that the operator cannot read the messages. `architecture.md`
§17 already accepts that E2E carries *compounding* rather than additive risk across two custom protocol
surfaces; the factor model underneath is a third input to that compounding, and it was not named.

`instance_admins` is the other case. An Instance Admin can ban platform-wide, resolve reports, and — per
screen `6c` — read whisper content attached to a report. That authority is reachable today with one factor.

## Decision

### It is built in Phase B, before M12, and the reason is not urgency
There are no users. Nothing is exposed: ADR 0007's release posture means the flagship accepts no
non-developer account before v1. So this is not a response to risk in the present tense.

It is scheduled here because of what a second factor has to be threaded through: `POST /auth/login`, the
refresh path, the device-code approval page, the OAuth exchange, and password reset. Those five took M4
through M11 to get right, and **each carries an anti-enumeration property that a factor prompt can undo
without any test noticing**:

- a factor challenge returned only for accounts that *have* a factor is a new oracle on top of the one M10
  spent a milestone closing, and it is reachable in one request;
- the unverified-account gate lives in `verifyCredentials` rather than in `Login` specifically so the
  device-verification page could not bypass it (M10) — a factor step that calls a different helper
  reopens that;
- reset's and verification's uniform timing is now a deliberate floor (`padToEnumerationFloor`), and any
  new branch inside that budget has to stay inside it.

Building against code that is still the newest in the repository is the cheap version of this milestone.
Building it at M100, against five paths nobody has touched in eighty milestones, is the expensive one. That
is the whole scheduling argument, and it is worth stating plainly because "no users yet" is a perfectly good
argument for the opposite conclusion and is wrong here for a reason that has nothing to do with risk.

### TOTP plus recovery codes, not WebAuthn first
TOTP is the smallest thing that changes the picture, and it is the thing that works where this client is
meant to run. A passkey flow needs a browser and a platform authenticator; the CLI is built for headless
boxes and SSH sessions, which is the same constraint that produced M9's device-code fallback — a milestone
that exists because M8's loopback listener binds `127.0.0.1` and the phone completing the flow cannot reach
it. A second factor that cannot be used over SSH would be a second factor the primary client cannot use.

WebAuthn is an addition on top later, not a replacement, and nothing here forecloses it: the verification
step is one endpoint with a factor-kind discriminator, not a TOTP-shaped hole.

### Recovery codes are credentials, and are stored like every other credential here
SHA-256 hashes, single-use enforced in the statement's `WHERE` rather than by a Go-side check — the same
discipline `ConsumePasswordResetToken` and `RedeemInstanceInvite` already follow, for the same reason. The
raw values exist exactly once, in the response that generated them.

They are the path that must work when the authenticator is lost, which makes them the highest-value target
on the account and the reason the set is regenerable and the count is small.

### The TOTP secret is encrypted at rest, unlike every other secret in this schema
Every other credential here is *hashed*, because nothing ever needs the original back. A TOTP secret is
different in kind: verifying a code requires the secret itself, so it cannot be hashed, and storing it as a
bare base32 string would make a database read equivalent to a permanent bypass of the factor. It is
encrypted under the instance key — the same key ADR 0029 already treats as the highest authority on the
instance, on the grounds that whoever can read `instance.toml` can already forge an access token for any
account.

This is a genuine weakening relative to the rest of the schema and it is stated rather than smoothed over:
a database compromise that also yields the config file yields every TOTP secret. The alternative is an HSM
or a KMS, which is infrastructure this project does not have and would not have on a self-hosted instance.

### Enabling and disabling are session-state changes
Enrolment, disabling, and regenerating recovery codes all sit behind `RequireLiveSession` alongside the
endpoints M11 put there, and disabling the factor revokes every other session through `revokeEverything`
rather than through a cleanup path of its own (rule 17). A signed-out credential must not be able to remove
the factor protecting the account, which is the same rule that stopped it minting an API token — and
`AuthenticateInstanceAdmin` learned the same lesson one level up when `/instance` turned out to sit outside
the original wording.

### The response shape and the timing must not distinguish a 2FA account from one without
This is the M10 lesson applied to a new surface, and it is stated as a done-when rather than left to care:
a login against an account with a factor and one without must be indistinguishable in status, body and
duration to a caller who does not have the password. The measurement is the one M10 and M11 already
established — medians over N requests, asserted as a ratio.

## Consequences
- **M71 (Instance Admin tier) and M100 (E2E device linking) gain a hard dependency on M11a**, recorded in
  the roadmap's dependency-notes section. Both are authorities that a single factor should not reach.
- **A fifth thing can lock a person out of their account**, after the password, the mailbox, the provider
  and the device. Recovery codes are the answer and they are only an answer if people keep them, which is a
  product problem this ADR does not solve — it is the reason the codes are regenerable and the reason the
  enrollment flow shows them once, prominently, rather than burying them in settings.
- **The daemon is unaffected.** It holds a refresh token, not a password, and a factor is proved when a
  session is *established* — so a daemon that was signed in before the factor existed keeps working, and
  `norite login` is where the prompt appears. This is the same seam that makes the device-code flow work.
- **Nothing in the E2E design changes**, but ADR 0014's threat model gains an input it was implicitly
  assuming. The linking flow was already correct; what was missing was any statement of how strong the
  authentication under it is.

## What building it changed about this ADR

Three things the implementation settled that the decision above had left implicit, recorded here rather
than only in commit messages.

**The enforcement is a type, and that was not a foregone conclusion.** The plan said "a proof that
`startSession` and `ApproveDeviceAuthorization` require", and the second half turned out to be impossible:
approving is a separate request from authenticating, so a proof obtained in the earlier one cannot cross
into the later one. What crosses is the approval token, whose meaning is "this browser has finished proving
who it is" — so the gate moved to where that token is *minted*. Same property, one request earlier.

**The device page needed a third continuation, not a reuse of the second.** ADR 0028 argued for two rather
than one because a token with an optional user field authorizes before authentication has happened. A
browser that has typed a correct password on an account with a factor is at neither existing point, so
`device_factor` is its own type. Forgetting that `parseDeviceToken` extracts a subject only for approval
tokens was a real bug — the factor token was minted with a user and parsed without one, and no code could
ever have passed.

**Rotating the instance signing key invalidates every enrolled authenticator.** The sealing key is derived
from it, so the two are coupled. There is no key rotation today; when there is, re-enrollment is the answer
and the alternative — a separately configured secret — buys independence at the cost of a setting every
operator must not lose. Named here so the first rotation is a decision rather than a discovery.

## Alternatives considered
- **Exclude it, recorded in §17 alongside federation and call recording.** Defensible, and it was on the
  table. Rejected because the argument for it — "OAuth providers carry the second factor for accounts that
  use them" — is true and does not cover password accounts, which are the majority path and the one the
  device-code flow exists to serve.
- **Schedule it late, near M100 where E2E device linking lands.** Rejected on the retrofit cost above: the
  five paths it touches are the ones a late change is most likely to break quietly, and the property it
  would break is anti-enumeration, which fails silently by construction.
- **WebAuthn/passkeys first.** Rejected as the *first* factor for this client, not on merit: it needs a
  browser, and the primary client is a terminal on a machine that may not have one. The same reasoning
  produced M9.
- **Hash the TOTP secret like everything else.** Not possible — verification needs the secret — and worth
  recording so the asymmetry reads as forced rather than careless.
