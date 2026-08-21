# ADR 0027: The OAuth callback returns to a loopback listener the client names

## Status
Accepted. Built at Milestone M8. Amends [ADR 0011](0011-token-based-client-auth.md), which described the
loopback port as registered *with the provider* — a shape this ADR shows cannot be built — and extends
[ADR 0024](0024-oauth-account-linking-and-signup.md), whose exchange code this hands to a client rather
than displaying.

## Context

M6 built the backend's OAuth flow: the provider redirects to the instance, the instance exchanges the
authorization code with its own client secret, and the callback renders a page carrying a single-use
exchange code. A client trades that code, plus the flow verifier it kept, for a token pair.

M8 was meant to be the command-line half — a browser opens, the person consents, `norite login` ends up
holding a credential. It could not be built, and the reason took a while to see.

**Nothing delivered the code to the client.** The callback rendered it as text for a human to copy. There
was no client-supplied redirect anywhere in the backend, the contracts or the migrations.

Meanwhile three documents described a different design. `docs/architecture.md` §2 and §14.15, and ADR 0011
itself, said the loopback port was "a fixed registered local port with a documented fallback-port list,
since **GitHub OAuth Apps require an exact pre-registered callback URL**". That clause only makes sense if
`http://127.0.0.1:51763/callback` is what is registered *at GitHub* — which means the provider redirects
straight to the user's machine, which means the CLI completes the provider token exchange, which means the
client secret ships inside a binary anyone can read. That is precisely what ADR 0024's design avoids by
keeping the secret and the exchange on the instance.

Those paragraphs predate ADR 0024 and were never reconciled with it. One line, written at M6, records what
was actually intended — `migrations/000004_oauth.up.sql`, on `oauth_exchange_codes`:

> *"The same shape serves the CLI's loopback flow at M8: the code is the only thing that crosses the
> browser, and it is worthless without a second request."*

So the listener was always meant to receive **the exchange code from the instance**, not the provider's
callback. Only the hop that carries it there — instance callback → `302` → `127.0.0.1` — was never built.

## Decision

**The callback returns to a loopback listener the client names when the flow starts.** `/authorize` takes
an optional `client_redirect_uri`; it is stored on the `oauth_states` row and, when the flow completes,
the callback issues a `302` carrying `?code=…` instead of rendering. Absent, everything behaves exactly as
it did at M6 — which is what a browser gets, and what M9's device-code flow will get.

Four calls inside that are worth recording, because each will be questioned.

**An exchange code may travel in a URL, where a token pair may not.** ADR 0024 refused to return tokens
from the callback because "a redirect carrying a token pair would put credentials in a URL, in browser
history, and in every proxy log on the way". Every clause of that stays true and none of it applies here.
A token pair is long-lived and bearer; this is a single-use code with a two-minute life that is useless
without the flow verifier, which never leaves the client process. And the hop is to an address on the
machine the browser is already running on, so there is no proxy and no network. The distinction is not
"a code is less sensitive than a token" — it is that this credential is bound to a second secret the
recipient does not have.

**The redirect is validated by host, not against a list of allowed ports.** It must be `http` on a
loopback IP literal with an explicit port, no userinfo, no query, no fragment, at most 256 bytes. Any port.
A port allowlist was considered and rejected: the provider never sees this URI, so nothing needs
pre-registering; `cli` and `backend` are separately versioned modules that cannot import each other, so
two lists would have to be kept in step by hand, and a CLI whose list grew one entry would fail against
last year's instance only when the earlier ports happened to be busy. It also stops no attack the host
check and the verifier binding do not already stop — a squatter on any port receives something it cannot
redeem, which has its own test.

**`localhost` is refused where `127.0.0.1` is accepted.** This surprises people, so it is stated here
rather than only in a comment. A name is resolved by the browser through `/etc/hosts` and DNS, either of
which can be made to point off the machine; an IP literal is resolved by nobody. RFC 8252 §8.3 says the
same. The cost is that a client must write the literal, which is one line.

**The destination survives the sign-up form inside the signature, never in the form.** A brand-new identity
reaches a "choose your username" page, and the code is minted only when that page comes back. The redirect
therefore rides in the signed continuation token beside the flow challenge, for the same reason that one
does: the form is submitted by whoever is looking at it, so a hidden redirect field would let the submitter
choose where somebody else's exchange code is delivered.

**The port list stays a client-side convention.** The CLI tries a fixed primary port and walks a documented
fallback list, which is what makes the port predictable enough to write down and to allow through a local
firewall. It is not a protocol requirement and the instance knows nothing about it.

## Consequences

- **`norite login --provider google` works end to end**, including for an account that does not exist yet.
- **Three documents were wrong and are corrected**: `docs/architecture.md` §2 and §14.15, and ADR 0011's
  own sentence. The provider registration is, and always was, the instance's own callback
  (`{public_base_url}/api/v1/auth/oauth/{provider}/callback`). Nothing about M8 changes what is registered
  with Google or GitHub.
- **A failure can be reported to the listener too**, as a code from a fixed seven-word vocabulary with no
  `error_description`. That is a deliberate rule-19 answer: a loopback listener is a socket any local
  process can write to, so keeping free-form text off it entirely beats sanitizing it on the far side.
  The client writes its own prose from the code.
- **A declined consent now consumes its `oauth_states` row**, where before it was left for the sweeper.
  The state has to be spent before the redirect is knowable, and there is nothing left to retry with.
- **A failure before the state is consumed still renders a page** — an unknown, expired or replayed state
  genuinely does not know a listener, and the only other place a destination could come from is the
  request being refused.
- **ADR 0011's `--no-browser` sentence is refined rather than reversed.** It said `--no-browser` falls back
  to the device-code flow; that flow is M9. At M8 it prints the URL and keeps listening, which is right for
  SSH with a forwarded port and survives M9 unchanged — at M9 a headless context additionally falls back to
  a device code.

## Alternatives considered

- **Register the loopback URL with the providers directly**, as the older paragraphs described: rejected,
  and not really available. It requires the client secret in the CLI binary, where it is not a secret;
  GitHub OAuth Apps have no public-client flow to fall back on; and every self-hosted instance would need
  its own registration to match. It reverses ADR 0024 without saying so.
- **Ship M8 as browser-plus-paste**, with the person copying the code from the page into a waiting prompt:
  rejected. It needs no backend change and it is what the callback page's own wording already invites, but
  it fails the milestone's stated criterion and leaves M8 barely distinguishable from M9.
- **An ephemeral port (`:0`)**: rejected, though it is RFC 8252's own recommendation and would delete the
  port-collision failure mode entirely. The fixed list is what makes the port knowable in advance, which
  is worth something to anyone writing a firewall rule or a support document. Reversible at no cost — the
  instance validates the host and does not care.
- **A server-side port allowlist**: rejected, above.
- **Carrying the redirect through the sign-up form as a hidden field**: rejected, above. It is the obvious
  implementation and it hands the choice to exactly the party the binding exists to constrain.
