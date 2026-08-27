# ADR 0014: BYOK end-to-end encryption, DM-only, gated behind external audit

## Status
Accepted. Amended by [ADR 0031](0031-two-factor-authentication.md), which names an input this ADR's threat
model was assuming without stating: device linking is authorized by the primary device, the primary device
is reached by signing in, and until M11a a sign-in was protected by one factor. Nothing in the linking flow
below changes; what changes is that the authentication under it is now scheduled to be two-factor, and
M11a is a hard dependency of M100.

## Context
Real E2E encryption is valuable but carries **compounding, not merely additive**, cryptographic risk: it
depends on two independent custom protocol surfaces (a fully-custom device-linking flow, and correct
integration of a third-party ratchet library into this project's own key-management model), either of which
can silently break forward secrecy or device-trust with no visible symptom.

## Decision
Opt-in, restricted to the `DM` channel type only — never `GROUP_DM`, never any guild channel, never
whispers, never voice. A `DM` is a fixed, non-growable 1:1 relationship in the schema, so there's no "member
added" transition to design. The pairwise-vs-group scaling limitation (Signal's double ratchet doesn't scale
to multi-member groups without N² sessions or a separate group-key scheme) is why `GROUP_DM`/guild channels
are excluded outright, rather than adding a third independent crypto protocol.

Cryptographic base: `go.mau.fi/libsignal`, a mature pure-Go Signal-protocol port, chosen over a from-scratch
build. **Its license compatibility with the project's restrictive custom license (ADR 0007) is a blocking
prerequisite**, checked before any integration code is written. Device linking (Signal-style "link new
device") is fully custom, no off-the-shelf equivalent — a second, real piece of custom crypto protocol. No
history-transfer mechanism exists for a newly linked device: it sees only messages sent after linking,
matching the permanent-loss framing below, deliberately adding zero new custom-crypto surface.

**The daemon, not the CLI/GUI, holds the E2E keystore and ratchet state and performs all decryption** —
consistent with the daemon being the sole holder of ordinary auth credentials (ADR 0011). CLI/GUI receive
plaintext over the already-trusted local IPC socket, the same as every other event. Key material lives in a
durable, locally-encrypted keystore (`modernc.org/sqlite`, pure Go), master key in the OS keychain, surviving
daemon restarts. All keystore writes route through one dedicated writer goroutine to avoid SQLite
lock-contention blocking the WS event loop. A mandatory local FTS5 search index over the decrypted message
store is built on this same keystore, since server-side search is unavailable for encrypted content.

**A real external cryptographic audit of the library integration and the device-linking protocol is a hard
release gate**: a build/instance-level flag keeps E2E technically unavailable to any account beyond the
developer's own test accounts until the audit passes — enforced by the flag, not a documentation policy.
Property-based/fuzz testing is a cheap complementary layer, never a substitute for the audit.

No key-loss backup, permanent, by design: losing a device means permanent loss of that device's encrypted
history, matching Signal's real model. For account export, the daemon performs its own local decrypt-and-
export step, producing a standalone `local_e2e_export.zip` presented alongside — never merged into — the
server-side export, since the server never had access to the content and merging the two archives client-side
would be a real memory/CPU/disk bottleneck.

## Consequences
- Encrypted conversations lose server-side search, moderation visibility, audit-diffing, edit-history, and
  link-preview generation — any server-side feature reading message content must explicitly exclude
  E2E-encrypted DMs, never assume plaintext is available (see `CLAUDE.md` rule 13).
- Whispers are explicitly excluded from E2E scope specifically so the Instance Admin break-glass moderation
  path (ADR 0013) always has plaintext to fall back on for whisper-vector abuse reports — a deliberate,
  documented user-facing inconsistency ("DMs can be E2E, whispers can't").
- Device revocation (the general-purpose revoke-all-sessions primitive) also revokes E2E device-link trust in
  the same action — a revoked device is cut off from both its API session and ongoing encrypted conversations.

## Alternatives considered
- **A from-scratch X3DH/ratchet implementation**: rejected — reinventing well-trodden, easy-to-subtly-break
  cryptography is strictly worse than integrating a mature, production-used library, even accounting for the
  integration risk that remains.
- **E2E for `GROUP_DM`/guild channels via Sender Keys or similar**: rejected for v1 — a third independent
  custom crypto protocol on top of two already-flagged risks is more risk than the feature is worth right now.
- **CLI/GUI independently hold keys**: rejected — see ADR 0011's reasoning, reproduced here for E2E
  specifically: duplicated ratchet-session handling per client type and raw ciphertext over an already-trusted
  local socket, for no benefit.
