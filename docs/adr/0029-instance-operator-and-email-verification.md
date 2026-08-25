# ADR 0029: The instance operator, and verifying an address ourselves

## Status
Accepted. Amends [ADR 0024](0024-oauth-account-linking-and-signup.md) (whose merged refusal message this
milestone both preserves and reworks) and completes the wizard [ADR 0011](0011-token-based-client-auth.md)
left half-built.

## Context
Three things were unfinished after M9, and they turned out to be one problem seen from three sides: **this
instance could not verify an email address itself.**

- `norite instance init` writes a configuration file and leaves an instance with nobody who can log into
  it. M2 built the wizard before `users` existed.
- `registration_mode = invite` refused outright, deliberately, because M4 had no table to redeem against.
- `POST /auth/register` answered 409 on a taken address — the one endpoint here that disclosed account
  existence, and it did so because there was no way to *accept* a registration and sort it out by mail.

That same absence is why ADR 0024 had to merge its two unverified-address refusals into one message, and
why an account whose GitHub address is unverified could not sign in at all. `users.email_verified_at` had
existed since M4 and only the OAuth path ever set it.

## Decision

### The operator is a third authority, and it is minted by the client
An access token says "I am this account"; an API token says "I act for this account". Both are things an
account issues, and a fresh instance has none — which is the chicken-and-egg the wizard left behind.

The operator is possession of the instance's own HS256 signing key: an unsubjected JWT with `typ:
"operator"`, a two-minute life, minted by `norite instance bootstrap` from `instance.toml` and presented as
a Bearer credential to `/api/v1/instance/*`.

It concedes nothing new. Anyone who can read that file can already forge an access token for any account on
the instance. What it buys is that the authority is *stated*: a bootstrap request proves filesystem access
rather than being trusted for arriving early, so there is no window in which whoever reaches a
freshly-migrated instance first becomes its administrator. The alternative designs both lose that — a
one-time token printed to a log is a secret in a place rule 8 says secrets do not go, and "the first
account wins" is a race an attacker on the network can enter.

Two properties are worth stating because they are what keep the tier from leaking:

- **`typ` is load-bearing, and not against the token you would guess.** An access token is refused by the
  empty-subject check, not by `typ` — disabling `typ` leaves that test passing. What `typ` actually defends
  against is a **device entry token**: same issuer, same key, live expiry, and no subject, because at that
  point in M9's flow nobody has authenticated yet. Without the check, entering a valid device code would
  hand that browser instance-operator authority.
- **`/instance` mounts outside the ordinary Bearer chain**, so "can an operator token authenticate an
  ordinary request" is answered by the router rather than by each verifier remembering a check.

### `backend/operatortoken` is a public package, and the CLI imports it
The format has two implementers in two modules. Go's `internal/` rule makes `backend/internal/auth`
unreachable from the CLI, which left a package outside `internal/` or a second copy of the claim shape.

A second copy drifts, and the failure mode is bad: a bootstrap reporting an invalid signature against a
token that is perfectly well formed, sending an operator hunting for a key problem that does not exist. So
the format — claims, `typ`, TTL, algorithm pin — lives in `backend/operatortoken`, and the authorization
decision stays in `internal/auth` where the routes are. `daemon/credentials` is the same decision made at
M7 for the same reason.

The cost is a second cross-module edge, `cli` → `backend`. Two tests span it: one asserts the `iss`
constant still matches on both sides, and one mints a token exactly as the CLI does and runs it through the
check the middleware runs.

### Bootstrap is a sibling command, not a step inside the wizard
`docs/roadmap.md` describes first-administrator creation as a step added *to* `norite instance init`. It is
`norite instance bootstrap` instead.

When the wizard finishes, the backend has not been started and the schema has not been migrated, so there
is nothing to create an account on. Asking for an administrator's password there and holding it until a
server appears would mean keeping it in memory across an unbounded wait, or writing it down. The wizard
prints the ordered next steps instead — start the backend, then bootstrap — because that order is not
guessable and getting it wrong is the ordinary first experience of self-hosting.

Bootstrap refuses unless there are zero administrators, which makes it self-disabling rather than needing a
flag that records whether it ran. The count is taken under an advisory lock: checking and then acting is a
read-modify-write, and under READ COMMITTED two concurrent bootstraps both read zero and both insert. There
is no row to lock, because the guard is the *absence* of rows.

### Registration answers identically, and the difference goes to the mailbox
`POST /auth/register` returns **202 with a fixed body** whether or not the address already has an account,
and never returns the account. A verification link goes to a new address; a "somebody tried to register
with your address" notice goes to one that already has an account.

The notice is what makes the silence honest rather than merely quiet. Somebody has to learn the two cases
differ, and the only party entitled to know is whoever controls the address. It carries no link and asks
for nothing — a "was this you?" button would be a phishing template written by us, and there is nothing to
confirm.

A taken **username** is still reported. A username is an `@handle`, public by construction and discoverable
by any client that can look one up; an address is not. That asymmetry is deliberate.

**Login had to become uniform too, and this was nearly missed.** Refusing an unverified account with its own
message reopens the same oracle in two requests: register an address with a password of your choosing, then
log in with it — if the address was free an account now exists with that password and the login says
"unverified"; if it was taken nothing was created and it says "wrong password". Measured at 403 against 401
before the fix. So an unverified account is refused exactly as a wrong password is, and a *correct* password
on one queues a fresh link and an explanation to the address instead. Guessing at addresses queues nothing,
because the mail follows the password check.

Timing is part of the guarantee and is not automatic. `HashPassword` runs *before* the address check, so
both branches pay argon2id's 64 MiB and tens of milliseconds — which swamps the single insert they differ
by. Moving it below makes the taken branch ~1 ms against ~31 ms; a test fails on the ratio. The race the
pre-check cannot close — two simultaneous registrations, one losing at the unique constraint — comes back
as the same silence, because reporting it would leave the oracle reachable on purpose by firing two
requests at once.

### An unverified provider address takes a detour, and both cases look the same
M6 refused an address its provider would not vouch for, and had to: it could not check one itself, so
creating an account would have recorded an unverified claim as verified. GitHub permits an account to hold
entirely unverified addresses, so those users could not sign in at all.

Now the evidence comes from the mailbox instead. But the obvious implementation reopens the very oracle ADR
0024 merged its messages to avoid: if an unknown address proceeded to a username form while a registered
one was refused, anybody could learn whether an address has an account here by presenting it unverified at
a provider.

So **both cases render one "check your email" page** and the difference travels by mail — an unknown
address gets a link that resumes the sign-up, a registered one gets a warning and the route back (sign in
by password, link from settings). Opening the mailed link is the proof of control the provider declined to
give, which is why the username form moved behind it.

Linking to an **existing** account stays refused. That is the takeover direction and no mail we send
changes who controls the provider account.

A loopback client is told to stop with M8's existing `email_unverified` code, identical in both cases. The
flow is not going to finish on that machine, and a client that was not told would sit out its whole
timeout.

### Invite management is admin-gated REST, logged but not yet audited
`instance_admins` lands 61 milestones before M71, which owns it, because bootstrap and invite management
both need something to gate on and "whoever registered first" records nothing. M71 keeps what it is
actually about: grant, revoke, multiple admins, and the last-admin rail.

Invite creation and revocation are logged structurally rather than written to `instance_audit_log`, which
is M72's table. Rule 14 enumerates bans, report resolution, entitlement changes and tier grants; minting an
invite is not among them. M72's roadmap entry gains a line requiring it to cover invite management, so this
is a deferral with a name on it rather than a gap.

### Two deviations from the DDL in `architecture.md`
- `instance_invites.created_by` is **nullable**. The document has it `NOT NULL`, which cannot hold: an
  invite may be issued by the operator, who is not an account.
- The invite code is stored **in plaintext**. An invite exists to be handed to somebody, so it must be
  readable back — a list that cannot show its own contents is not a list. What it authorizes is bounded to
  creating an account subject to every other rule registration enforces; it reaches no existing account and
  no data. Hashing would make it a show-once credential, which is right for a value its owner holds and
  wrong for one they distribute. This is M9's `user_code` exception applied to a different value.

## Consequences

**Accepted, and stated in three places rather than discovered:** an instance with no SMTP relay creates
accounts already verified, and the enumeration oracle stays open there. It cannot verify an address by any
route, so requiring verification would mean nobody could register at all, and there is no mail to carry the
difference between the branches. The wizard warns when SMTP is declined, the server logs it once at
startup, and a test pins it. The failure mode to guard against is this becoming quiet.

**An invite-only instance is password-registration-only.** There is nowhere to carry a code through a
provider redirect, so OAuth sign-up still refuses outright while gating is on. It fails closed, so it is
safe; it is a real limitation and it is in the contract.

**A device-code flow that hits an unverified provider address is left pending.** The waiting client polls
until its code expires rather than being told, because the device grant has no vocabulary for "we have
emailed you". Bounded by the twenty-minute TTL and worth revisiting when that vocabulary is next touched.

**Migration `000010` backfills every existing account as verified.** Leaving them NULL would lock every
user out of an instance that worked a minute earlier — an outage delivered as a hardening — and nobody
could recover, since the resend path needs an account and reset needs a relay the instance may not have.
An instance with real users should read it as: accounts predating verification are grandfathered.

**`registration_closed` is gone**, replaced by `invite_required` and `invite_invalid`. The old name was
accurate only while there was nothing to redeem.
