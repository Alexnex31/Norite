# ADR 0025: Credential storage falls back to a 0600 file where there is no OS keyring

## Status
Accepted. Built at Milestone M7. Refines [ADR 0011](0011-token-based-client-auth.md), which chose
token-based client auth and named `zalando/go-keyring` as the storage without saying what happens on a
machine that has no keyring.

## Context

ADR 0011 says "one keychain entry (`zalando/go-keyring`), one process". On a desktop that is the whole
story: macOS Keychain, Windows Credential Manager, and a running GNOME or KDE keyring all work, and the
library talks to each of them natively.

On a headless Linux server there is no Secret Service. No session bus, nothing implementing the D-Bus
interface the library needs, and every call fails — often after a timeout rather than promptly.

That is not a corner case for this project. [ADR 0009](0009-cli-and-gui-client-architecture.md) makes the
CLI a primary client and names "SSH-friendly headless auth" as part of why it exists; the roadmap has the
CLI managing an instance over `docker exec` and in CI. A keyring-only design would mean `norite login`
cannot work on a substantial share of the machines the CLI was built for, and would fail there *after* the
password had already been typed and accepted.

The gap survived this long because the headless story that **is** written down covers a different problem
with a similar name: the OAuth browser leg, where a machine with no browser falls back to a device code
(M9). Nothing addressed a machine with no keyring.

## Decision

**The OS keyring where the machine has one; a `0600` file in the daemon's state directory where it does
not.** The state directory is already `0700` and per-user by construction, and the fallback is chosen per
machine, not per user preference — there is no flag to force either.

**The backend is chosen by writing and deleting a probe entry, not by reading one.** A read of a missing
entry is reported the same way by a working keyring and a broken one, and not uniformly across platforms,
so a read-based probe would select the keyring for a machine that cannot store anything — and the failure
would land at the one moment there is a real token to lose. The probe result is cached for the process,
because on a headless box the failing call costs a D-Bus timeout.

**The fallback file is written in plaintext.** Encrypting it would need a key; the key would have to live
beside it; and a decryption key stored next to its ciphertext is obfuscation dressed in a security
vocabulary. It is worse than plaintext, because it invites treating the file as safe.

**The degradation is never silent.** `norite login` reports which of the two it used, in the same sentence
that confirms the login.

**Only the refresh token is stored.** An access token lives fifteen minutes, which is shorter than the gap
between the restarts persistence would let it survive, so writing one down adds a credential at rest and
buys nothing. Beside the secret sits a small non-secret record — instance and account — in a plain file, so
that showing which account is logged in never has to open the keyring, and this installation's device ID in
a third. The device ID is separate because a logout removes the record and must not remove the identity:
logging out locally revokes nothing on the instance, so a newly minted ID would add a session-list entry
while the refresh family the old one named stayed live for its full TTL (ADR 0011).

## Consequences

- **`norite login` works on a headless server**, which is what the CLI is for. This is the point.
- **A refresh token on such a machine is protected by file permissions rather than by a keyring.** That is
  a real reduction, and it is the same boundary this codebase already relies on for plugin capability
  grants and pinned `.wasm` hashes ([ADR 0015](0015-plugin-sandboxing.md), CLAUDE.md rule 12) — things at
  least as dangerous as one account's session. It is stated plainly rather than papered over, and anyone
  who finds it unacceptable for their threat model can run a keyring daemon, which is what the login output
  points at by naming the file.
- **A desktop user whose keyring is broken finds out at login**, from a line saying where the credential
  actually went, rather than months later.
- **Two storage paths mean two things to test.** Both are, including the file path's exposure of an
  operator-supplied instance URL to a filename — sanitized, because a filename built from untrusted text is
  how a write lands somewhere it was never meant to.
- **The daemon module owns the format, and the CLI imports it.** That is the first cross-module dependency
  in the repository (`cli` → `daemon`, by relative `replace`). It follows from ADR 0011 making the daemon
  the sole holder of its account's tokens: the daemon defines what a stored credential is, and the CLI
  writes one on its behalf until M20 moves even that behind the IPC socket. The alternative — two
  implementations of one on-disk shape — drifts the first time either changes, and the failure mode is a
  login that appears to work and a daemon that cannot find its credential.

## Alternatives considered

- **Keyring only; fail on machines without one**: rejected. It is the honest version of ADR 0011 as
  written, and it makes the CLI unusable on servers, which contradicts ADR 0009's reason for having a CLI
  at all.
- **Encrypt the fallback file with a machine-derived key** (hostname, machine-id, a hash of both):
  rejected. It resists a copied file and nothing else, since anything that can read the file can derive
  the key on the machine it came from. The gain is against an attacker who exfiltrates one file and
  nothing else; the cost is that the file looks protected.
- **Prompt for a passphrase and encrypt with that**: rejected for M7. It genuinely protects the file at
  rest, and it means the daemon cannot start unattended — which is the entire point of a daemon that
  reconnects after a reboot. Worth revisiting only alongside the E2E keystore's own key handling (M98,
  which puts that store's master key in the same OS keychain this ADR is about), where the same question is
  asked about material that cannot be re-obtained by logging in again.
- **A file always, ignoring the keyring**: rejected. It would be simpler and one path to test, and it
  throws away real protection on the majority of machines to avoid a branch.
- **Refuse to run without a keyring unless a flag is passed**: rejected as a worse version of the same
  outcome. Someone who hits it will pass the flag without reading why, so the flag buys a support burden
  rather than a decision — and the honest answer is that on that machine there is no other option anyway.
