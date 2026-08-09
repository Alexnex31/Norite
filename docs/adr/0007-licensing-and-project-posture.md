# ADR 0007: No public license (default copyright, all rights reserved), individually-granted self-hosted licenses

## Status
Accepted. Supersedes [ADR 0005](0005-agpl-license.md). Revises this ADR's own earlier "custom source-available
license" draft posture: rather than drafting and legally reviewing a novel BSL/SSPL-style public license
text, the project publishes no public license at all.

## Context
The project's audience starts as personal use (the developer, optionally a small invited circle) but the
architecture must support a commercial future without a rewrite: a free flagship instance the developer
operates, plus self-hosted instances sold to other operators via a one-time license. An earlier version of
this decision proposed drafting a custom, restrictive, source-available public license (BSL/SSPL-style) to
express that posture — but that requires real legal review of novel license text before it can be trusted,
is a genuine ongoing drafting/maintenance burden, and still has to anticipate every use case a public license
text needs to cover. Since the actual commercial mechanism is already "each self-hosted customer gets an
individually-issued, cryptographically-signed license file" (a direct grant, not a public license anyone can
rely on), a public license text turns out to add legal surface without adding any real capability.

## Decision
**Publish no public license.** The repository is visible (for self-hosting trust and transparency, and
because a public GitHub repo is the simplest way to share it with early collaborators) but grants **no
rights** to any viewer by default — under default copyright law, "all rights reserved" is what applies to
unlicensed code, full stop. `LICENSE` at the repo root states this explicitly (a short notice, not a full
license text) rather than leaving the repo without a `LICENSE` file at all, since an explicit "no rights
granted" statement removes any ambiguity a missing file might invite ("public repo" is not "open source,"
and this project says so directly rather than relying on default-copyright reasoning nobody reads).

Rights to actually run a self-hosted instance are granted **individually**, not publicly: a self-hosted
customer receives a direct, separate grant in the form of the already-planned offline, cryptographically-
signed license file (Ed25519-signed JWT-like structure, `entitlements` blob, no phone-home — unchanged from
the original design). That signed file *is* the license grant for that specific customer; there is no
separate license *text* to draft, review, or keep in sync with it. The flagship instance needs no such grant
at all — the developer/company runs their own copyrighted code directly.

The commercial model itself is unchanged: two independent deployments of the same codebase, no shared
multi-tenant architecture, no "Platform Operator" tier — a free flagship instance (optional per-user
subscription perks via the inert `user_entitlements` seam) and self-hosted instances sold via one-time
license purchase.

## Consequences
- **The "pending legal review" gap this ADR previously carried is closed.** A short, standard
  all-rights-reserved notice needs no bespoke legal drafting the way a novel restrictive public license text
  would; `LICENSE`'s contents and this ADR's stated policy are the same thing now, not a known-stale
  placeholder waiting on review.
- Nobody may fork, self-host, redistribute, or build on this code without a direct grant from the copyright
  holder — a stricter default than even a restrictive public license would have been, which is the intended
  posture for the personal-use/early-commercial phase.
- The self-hosted license-issuance mechanism (a small storefront/payment page generating signed license
  files) is doing real legal work now, not just technical work — it *is* the only way anyone outside the
  developer gets any rights at all. It remains effectively separate infrastructure from the chat platform's
  own runtime.
- If the project later wants public contributions, a wider audience, or a more conventional open/source-
  available posture, that's a **new** ADR superseding this one and a real decision to publish a public
  license — not a default this project drifts into.
- Every new source file is covered by the same no-public-rights posture automatically; there's no per-file
  header or license-text maintenance burden.
- This does not change anything about dependency licensing hygiene: still watch for copyleft-incompatible
  dependency licenses before adding one, since a dependency's own license terms apply regardless of this
  project's posture (this already gated the `go.mau.fi/libsignal` integration — see
  [ADR 0014](0014-e2e-encryption.md)).

## Alternatives considered
- **Draft and legally review a custom BSL/SSPL-style public license** (this ADR's own earlier decision):
  rejected on reconsideration — real drafting/review cost and ongoing maintenance for a public grant nobody
  actually needs, since the real commercial mechanism (individually-signed license files) grants rights
  directly and doesn't depend on a public license text existing at all.
- **Keep AGPL-3.0** ([ADR 0005](0005-agpl-license.md)): rejected — doesn't prevent a self-hosted licensee
  reselling or forking into a competing hosted offering, which the commercial model requires blocked; a
  copyleft public license is also a strictly *weaker* default than "no rights granted at all."
- **No `LICENSE` file at all** (rely on default copyright silently): rejected — an explicit notice removes
  ambiguity for anyone who finds the public repo and might otherwise assume "visible on GitHub" means
  "usable."
