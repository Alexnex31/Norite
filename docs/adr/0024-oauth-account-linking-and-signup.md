# ADR 0024: OAuth links only on a provider-verified email, and creates no account until a username is chosen

## Status
Accepted. Built at Milestone M6. Refines [ADR 0011](0011-token-based-client-auth.md), which established
token-based client auth and named OAuth as a login path but left the linking rule and the signup shape
open. Uses the `typ`-claim mechanism from [ADR 0022](0022-access-token-signing-and-scope-model.md).

## Context
"Sign in with Google" has to answer two questions that look like product decisions and are actually
security decisions.

**What happens when a provider hands us an email that already belongs to an account?** The friendly answer
is to link them — the person clearly owns both. The problem is that "clearly" is doing all the work: at
several providers an account's email address is whatever the account holder typed, never confirmed. If
matching on the address alone were enough, anyone could put someone else's address on a fresh provider
account and sign straight into their Norite account. This is not theoretical; it is the standard OAuth
account-takeover pattern.

**What username does an account created through a provider get?** `users.username` is `NOT NULL` and
`UNIQUE`, and since M4 it is also restricted to an allow-list. A provider gives us a display name (often
"Firstname Lastname", which is not a valid username) or a handle (GitHub) or nothing useful (Google). None
of them is a username the person chose.

## Decision

**An identity links to an existing account only when the provider reports the address verified.** When it
does not, the sign-in is refused with a distinct error telling the person to sign in with their password
and link the provider from settings. `EmailVerified` is carried through the provider layer as its own
field and is never inferred: a provider that does not say an address is verified is treated as not having
said so.

For GitHub this means a second API call. `/user` omits the address entirely whenever it is private — the
default for many accounts — and carries no verification flag at all; `/user/emails` is the only place the
flag exists. A verified address is preferred over the primary one, so an account whose primary address
happens to be unverified is not refused while a verified one sits beside it.

**Once linked, sign-in consults the provider's immutable user ID and nothing else.** The email is not
re-checked, so changing it at the provider neither detaches nor redirects an established link.

**No account is created until a username is chosen, and nothing is written to `users` before that point.**
The callback returns a short-lived signed continuation token carrying the verified identity; the account,
its `oauth_identities` row, and its first session are created together in one transaction when the chosen
username arrives. The token is distinguished from every other token by a `typ` claim, so an access token
cannot be spent as a signup and a signup token cannot authenticate a request.

**A username suggestion is offered, never applied.** It is prefilled into a form the person edits.

**The callback never returns tokens.** It renders a page carrying a single-use exchange code, which a
client trades at `POST /auth/oauth/exchange` along with its `device_id`.

**A sign-in is bound to the client that started it.** `/authorize` requires a `flow_challenge` — the
base64url SHA-256 of a secret the client keeps — and the exchange requires that secret as `flow_verifier`.
It is PKCE's construction applied to the client-to-Norite hop, which is the one hop the flow otherwise had
no binding on: `state` proves only that this server issued the request, not that this client made it. The
challenge is carried from the `oauth_states` row onto the `oauth_exchange_codes` row, and through the
signup token for the username step, so every code is bound however it was produced. The binding is
mandatory.

## Consequences

- **The takeover path is closed at the point it would be exploited.** The refusal is a real cost: someone
  whose provider genuinely has not verified their address cannot use the button, and has to be told why
  rather than left pressing it. That is why the refusal has its own error and its own sentence.
- **There is no "pending account" state.** This is the consequence that compounds. Every later milestone —
  member lists, mentions, moderation, exports — would otherwise have to know that some `users` rows are not
  real yet, and the first query that forgot would produce a ghost account nobody can sign in to.
- **Replay of a signup token is harmless without any extra machinery.** `oauth_identities`' unique
  constraint on `(provider, provider_user_id)` refuses the second attempt, so single-use falls out of the
  schema rather than needing a table and a cleanup job. This is why the continuation token is signed rather
  than stored, unlike every other short-lived value in `internal/auth`.
- **A verifier has to live server-side**, which is what `oauth_states` exists for. PKCE's guarantee is that
  the verifier is known only to the client that started the flow; putting it in the `state` parameter — the
  obvious stateless design — would send it through the browser and the provider and reduce PKCE to
  decoration.
- **Credentials never travel in a URL.** The exchange code is the only thing that crosses the browser, and
  it is worthless without a second request.
- **A sign-in is bound to the client that started it, and a browser alone cannot finish one.** The `state`
  proves only that *this server* issued the authorization request; it says nothing about who started it, so
  on its own it leaves login CSRF — an attacker consents with their own provider account, hands the callback
  to someone else, and the victim's client redeems a code that signs them into the attacker's account. The
  binding closes it: `/authorize` requires a `flow_challenge`, the exchange requires the matching
  `flow_verifier`, and the challenge rides the state row and then the exchange-code row (and the signup
  token, for the username step). The cost is that opening `/authorize` in a browser produces a code nothing
  can spend — accepted, because that path is exactly what the attack is built from.
- **The two unverified-address refusals differ, and that difference reports whether an address is
  registered.** `ErrOAuthLinkRequired` says an account exists; `ErrOAuthEmailUnverified` says none does.
  They are deliberately distinct because the advice differs and a person given the wrong one is stuck, and
  the disclosure is accepted because it is strictly less than registration already gives: `POST
  /auth/register` answers 409 "that email is already registered" on any address, with no provider flow
  required. The line this codebase draws is between *registration*, which necessarily discloses, and
  *login and reset*, which must not — and these refusals sit on the registration side of it. Collapsing
  them to close a gap that is not closed elsewhere would trade a real usability failure for nothing.
- **Two GET endpoints mutate, against the letter of CLAUDE.md rule 4.** `/authorize` inserts an
  `oauth_states` row and `/callback` consumes it. Both are browser navigations that OAuth requires to be
  GETs, so the shape is not negotiable. The rule's purpose is not violated: it exists for REST hygiene on
  the token-authenticated surface (there is no CSRF exposure without ambient credentials, and these two
  endpoints are unauthenticated), and neither writes anything reachable by an authenticated caller. The
  cost is that a prefetching browser or a retried GET leaves abandoned `oauth_states` rows, which is what
  `DeleteExpiredOAuthStates` and M11's cleanup job exist for.
- **An invite-only instance refuses new accounts through a provider but still permits linking**, because
  the gate is on account creation, not on providers.
- **GitHub costs two requests per sign-in.** Unavoidable given where the verification flag lives.

## Alternatives considered

- **Auto-link on any email match**: rejected. It is the friendliest flow and it hands over an account to
  anyone who can type an address at a provider that does not check.
- **Never auto-link; require linking from an authenticated session**: defensible and strictly safer, and
  rejected as disproportionate. It means a first-time "Sign in with Google" fails for anyone who already
  has an account, with no way to tell them apart from an attacker — the refusal we accept is narrower,
  applying only when the provider itself declines to vouch.
- **Trust the provider's `email` and skip `email_verified`**: rejected. Google effectively always verifies,
  which is exactly what would make the missing check invisible until the first provider that does not.
- **A pending-account row with a `completed_at` column**: rejected. See consequences — it moves a one-time
  cost into a permanent obligation on every future query.
- **Deriving the username from the email's local part**: rejected as a default. It is a fine *suggestion*
  and a poor decision to make on someone's behalf, because it leaks part of an address into a permanent
  public identifier they never chose.
- **Returning the token pair from the callback**: rejected. It works, and it puts credentials in a URL, in
  browser history, in the `Referer` header, and in every proxy log along the way.
- **A per-flow cookie set at `/authorize`, instead of a client-held verifier**: the textbook answer for a
  browser-driven flow, and rejected because it protects the wrong leg. It would bind the *browser*, which
  is not who redeems the code — and it would be the first cookie in a codebase that retired them
  ([ADR 0011](0011-token-based-client-auth.md)) to protect a flow whose real clients are a CLI and a
  daemon. The verifier binds the party that actually presents the credential, and works unchanged for M8's
  loopback listener.
- **Letting each client compare the `state` it sent with the one it got back**: the standard native-app
  mitigation, sufficient for M8's own flow, and rejected because it protects only the clients that remember
  to do it and leaves `/exchange` open to every client that does not. This package's preference is
  consistently for the guarantee to live where it cannot be forgotten — single-use in
  `ConsumePasswordResetToken`'s `WHERE`, the PKCE verifier server-side, and now this.
- **Making the binding optional so a bare browser flow still works**: rejected, and the reason is the whole
  point. The attack is constructed by whoever starts the flow, so an attacker wanting to skip the check
  would simply start one without a challenge. An optional binding is not a binding.
- **Skipping PKCE because this is a confidential client**: defensible — the client secret already prevents
  code redemption — and rejected because the redirect leg travels through a browser this server does not
  control, and from M8 through a loopback listener other local processes can see.
