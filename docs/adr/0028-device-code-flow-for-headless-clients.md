# ADR 0028: The device-code flow, and what actually protects it

## Status
Accepted. Amends [ADR 0011](0011-token-based-client-auth.md) (which named the flow before either browser
flow existed) and continues [ADR 0027](0027-loopback-redirect-for-the-oauth-callback.md).

## Context
M8 gave `norite login --provider` a loopback listener. That listener binds `127.0.0.1`, so the browser
that finishes the sign-in has to be on the same machine as the CLI — which on a server reached over SSH it
is not. A link pasted into a phone's browser cannot reach the server's loopback port, and `--no-browser`
only helps where a port is forwarded.

For an account that has a password this is an inconvenience: `norite login` still works. For an account
that signs in only with Google or GitHub it is a wall, and there was no way over it. That is the hole M9
closes, and it is why the verification page has to offer providers rather than only a password — a
password-only page would add nothing that M7 did not already do.

`device_code` has been in `architecture.md` §2's DDL since before any of it was built, and three of its
columns were wrong against rules this codebase does not bend. Those corrections are recorded there, in the
migration, and summarized below.

## Decision

### The poll is `POST /auth/device/token`, not `GET /auth/device/code/{code}`
The successful call spends the code and starts a session. Rule 4 forbids a GET from mutating, and the rule
is standing in for something concrete: a prefetch, a link checker or an automatic retry would fire it on
somebody's behalf. Separately, request paths are logged and this one is a credential (rule 8), so it
belongs in a body either way.

### The device code is hashed; the user code is not
The device code redeems for a token pair, which makes it a credential like every other one here, stored
only as its SHA-256.

The user code is the deliberate exception, and it is the one a reviewer will stop on. Two reasons, and both
are needed. It is **not a bearer credential**: holding it authorizes nothing, because whoever enters it
must still authenticate as themselves and press Approve, and what that authorizes is somebody else's
machine acting as *their* account — so a stolen user code buys an attacker the ability to give their own
account away. And it **has to be readable back**, because the approval page shows it for comparison against
the screen that produced it, and the OAuth callback that reaches that page arrives holding a state row and
an account, never what somebody typed. Hashing it would have cost the flow its only real defense to gain
consistency with a rule whose reason does not apply.

### Approval is a separate, explicit step
The device-code grant's live risk is not somebody guessing a short code. It is somebody being *sent* one
and authorizing a stranger's machine without understanding that is what they did — a pattern with real
campaigns behind it against other implementations. Nothing cryptographic prevents it.

So signing in authorizes nothing. It produces a page that names the device asking, shows the code back,
warns in plain words, and requires one more deliberate action, where a decision that is neither approve nor
deny denies. This costs a click on every legitimate sign-in and is worth it, because the alternative
failure is silent and total. See §14.21 for the residual risk, which is real and stated.

### No `verification_uri_complete`
RFC 8628 §3.3.1 offers a URL with the user code embedded, usually rendered as a QR code. It completes the
entry step from the URL, which is exactly what turns the attack above into a single click on a link an
attacker sends. The `?code=` parameter `/device` does accept only prefills a field that still has to be
looked at and submitted; that is the whole distance between the two.

**That claim is narrower than it first reads, and the narrowing was found by review rather than by
design.** The verification page's provider buttons are links to
`/api/v1/auth/oauth/{provider}/authorize?device_token=…`, and the continuation in them is not bound to the
browser it was issued to. An attacker who starts their own authorization, enters their own code and copies
that link out of the page holds a ten-minute URL that carries a victim past the entry step — no code typed,
and the entry page's "nobody should ever send you a code" warning never seen. That is the same shape as the
parameter this section refuses, arrived at by a different route.

What it does *not* skip is the approval page, which is where this flow's defense actually is: the device is
still named, the code is still shown for comparison against a screen the victim does not have, the warning
is repeated, and Deny is still the default button. So the property that survives is "no link reaches an
authorized device without a human decision on a page that describes it", which is the one worth stating —
not "no link skips a step". The wording above is kept and qualified rather than deleted, because the
distinction between those two claims is the whole finding.

Closing it properly means the entry continuation being unusable by a browser other than the one it was
issued to, and there is nothing to bind it with: this surface has no sessions and no cookies until Phase O.
It is left open, stated here and in §14.21, rather than papered over.

### A device flow carries no flow challenge, and this is not the binding becoming optional
`/authorize` requires exactly one of `flow_challenge` and `device_token`. A challenge exists so that the
*code* a flow produces is redeemable only by the client that began the flow (see `GenerateOAuthFlowVerifier`),
and a device flow produces no code at all: the waiting client has held its credential since before a
browser was involved.

Somebody who stole a device flow's state and completed it in their own browser would be shown an approval
page for *their own* provider account, authorizing a device they already control. That is not an attack.
The risk on this path is a person being talked into approving, which the step above is for. What is stored
in `flow_challenge` there is the continuation's hash, so a state row can be traced to the page visit that
started it; nothing verifies it, and the column's comment says so rather than leaving a future reader to
assume otherwise.

### The destination survives both hops inside a signature
Which device is being authorized rides in `oauth_states.device_code_id` across the provider round trip —
the same problem `client_redirect_uri` solved at M8 and the same answer — and in the `dvc` claim of the
signed continuation across the username form. Neither is ever read from the callback's URL or the form's
body, because both are presented by whoever is looking at the page. Both properties have a test that was
confirmed by making the handler read the untrusted copy.

### Two continuations on the page, not one
An entry token says a browser has entered a live code. An approval token says the same browser has also
proved whose account this is. One token with an optional user field would authorize before authentication
had happened, with only the handler's memory in between — so `parseDeviceToken` takes the type it wants
rather than reporting the type it found.

### The fallback is detected; `--device-code` asks for it
Somebody at the far end of an SSH session should not have to know which of two sign-in flows their
situation calls for. Detection only ever redirects a sign-in that was *already* going to need a browser, so
a password login is untouched, and it is never silent — the same rule ADR 0025 applies to the keyring
fallback, for the same reason.

`--no-browser` keeps M8's meaning: print the link, I will open it myself. It is a working flow with a
forwarded port, and overloading it would give one flag two incompatible behaviours.

### A second HTML password form, accepted with reservations
The verification page takes a password as well as offering providers, which adds a credential-stuffing
target and a phishing template to an instance that has otherwise kept those to one (`/reset`). Accepted
because the alternative is `--device-code` being a dead end for a password-only account, and because "do
not type your password into a machine you are only visiting" is a real reason to choose this flow. It sits
behind the same stricter rate-limit bucket as `/reset` and calls the same `verifyCredentials` the JSON
endpoint does, so the anti-enumeration properties cannot be reintroduced differently by a second
implementation.

### The user-code alphabet is a security parameter
Eight characters from twenty — A-Z less the vowels, so a draw does not spell anything, and less `L`, which
is misread as `1` or `I`; no digits at all, so `0` and `O` never meet. That is 34.6 bits, which is the
figure the verification page's rate limit is sized against. Anyone making the code friendlier is changing
that number: the alphabet, the length and the limit move together or not at all.

## Consequences
- The instance is now the only party that ever sees both halves of a device authorization, and the browser
  leg is entirely on its own origin — no provider registration changes, exactly as at M8.
- `OAuthOutcome.SignedIn()` no longer means "has an exchange code". A device flow signs somebody in and
  mints none.
- The device flow needs `public_base_url`, which was already required with SMTP or OAuth configured. With
  none of the three, `POST /auth/device/code` answers 503 `device_flow_unavailable` rather than issuing a
  code with nowhere to send anybody — the shape M5 established for a missing mail relay.
- `GET /auth/oauth/providers` still does not exist. The verification page reads `OAuthProviders.Names()`
  in-process, which is its first caller; the TUI's login screen (`5a`) will want the endpoint, and that is
  a contract addition when it lands.

## Alternatives considered
- **A password-only verification page**: rejected. `norite login` already does headless password sign-in,
  so it would add almost nothing, and the account that actually needs this flow still could not use it.
- **Hashing the user code**: rejected, above. It would have removed the comparison the approval page rests
  on for a consistency whose reason does not apply to this value.
- **Approving implicitly on a successful sign-in**: rejected. It is one fewer page and it makes the
  phishing attack a single click for anybody who can already sign in.
- **A browser session (cookie) for the page's three steps**: rejected — ADR 0011 retired cookies for this
  surface and Phase O brings them back as a BFF layer. A signed continuation is what M6 already uses for a
  multi-step browser flow, and it needs no new state to revoke or sweep.
- **Making `oauth_states.flow_challenge` nullable** for the device path: rejected.
  `emit_pointers_for_null_types` turns it into `*[]byte` and every existing read site grows a nil check,
  for a distinction that is better
  stated in a comment than in the type — the same trade 000006 made for `client_redirect_uri`.
- **A `GET` poll matching `architecture.md`'s original sketch**: rejected on rules 4 and 8, and the document
  is corrected rather than the code bent to it.
