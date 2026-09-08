# ADR 0032: AGPL-3.0-or-later, and a dependency policy that splits by module

## Status
Accepted. **Supersedes [ADR 0007](0007-licensing-and-project-posture.md) in full.** All three of 0007's
substantive sections — license posture, release posture, dependency licensing — are absorbed and restated
below, so nothing in 0007 remains authoritative and no file should cite it as live authority again. Through
0007 this also supersedes [ADR 0005](0005-agpl-license.md), the project's original AGPL decision, whose
rationale is adopted wholesale rather than merely revived and whose "no per-file license headers"
consequence is reversed. The chain stays linear: 0005 → 0007 → 0032.

## Context
ADR 0007 published no public license at all. The repository was visible under default copyright, all rights
reserved, and the only way anyone outside the copyright holder acquired the right to run Norite was an
individually-issued, Ed25519-signed license file — sold, one-time, per self-hosted instance. That posture
was chosen deliberately over both AGPL (0005, superseded) and a novel BSL/SSPL-style public license text
(0007's own earlier draft, reversed within the same ADR).

0007 parked the license question rather than closing it, with an explicit instruction: revisiting means a
new ADR superseding it, made once with the commercial model in front of it, and not re-litigated on the way
past. **This ADR is that revisit**, and it is the condition 0007 itself set rather than a drift.

What changed is the reading of what the commercial model actually needs. 0007 assumed legal exclusivity was
load-bearing: that a self-hosted licensee reselling or forking into a competing hosted offering was a
failure the license had to block. That is the strategy of a company selling software. Norite's strategy is
to operate the flagship — and the flagship's differentiation is that it is well-run, not that nobody else
is permitted to run one. Meanwhile the exclusivity cost real things: nobody could evaluate the code, no
distribution could package it, and every self-hoster needed a transaction before they could start.

The technical trigger is separate and sharper. [ADR 0014](0014-e2e-encryption.md)'s device-linking design
is built on `go.mau.fi/libsignal`, which is GPL-3.0. Under 0007 that was a **blocking prerequisite** — a
license-compatibility check scheduled at M97, years out, with the E2E design resting on an answer nobody
had. Under AGPL-3.0-or-later the question is answered by the licenses themselves, and M97 becomes the
integration milestone it should always have been.

## Decision

### 1. `AGPL-3.0-or-later`, for the whole work
The verbatim FSF text lives at `LICENSE`, fetched from `https://www.gnu.org/licenses/agpl-3.0.txt` and not
paraphrased — 0005 already required this and it remains correct. One license covers the entire repository
including `docs/`, `contracts/`, `docker/` and the justfile, not only the code.

**The "or (at your option) any later version" clause in `README.md`'s notice is what legally makes this
`-or-later`.** The SPDX identifiers on individual files declare it machine-readably; they do not create it.
If the prose said "version 3" and stopped, the project would be `AGPL-3.0-only` whatever the headers say.
The two must agree, and the prose is the one that governs.

### 2. What is reversed from ADR 0007
- The all-rights-reserved posture, and `LICENSE` as a notice declining to grant rights.
- "Not AGPL, not open source, not a drafted custom public license either."
- Rights granted individually, per customer, by a signed file rather than publicly by a license text.
- The sold self-hosted license as the commercial mechanism.

### 3. What is reversed from ADR 0005
Its consequence that no per-file license headers are needed. Every hand-written, non-generated `.go` file
now carries two lines — `SPDX-FileCopyrightText` then `SPDX-License-Identifier: AGPL-3.0-or-later`.

**Nothing else in the repository carries a header, and that is a decision rather than an omission.** Not
SQL (`backend/migrations/*.sql` are mechanical historical artefacts; `backend/internal/db/queries/*.sql`
open on the `-- name: … :one` annotations sqlc parses), not Markdown, not YAML, not the justfile. `LICENSE`
covers the whole work, so a header in those files buys nothing and costs a second comment syntax in every
future mechanical pass. Generated `.go` files are excluded for a mechanical reason: their generator
rewrites them wholesale, so a hand-added header would vanish on the next `just sqlc-generate` and the
staleness check would then fail. Detect them by the `Code generated … DO NOT EDIT.` marker, never by a path
list, which goes stale the moment output moves.

**REUSE conformance is not a goal.** REUSE exists for projects with many contributors, heterogeneous
licenses and downstream conformance scanners. Norite has one license, one author and no audit to pass. The
two-line header happens to be what REUSE would want, so adopting it later needs no second pass over the
code — but that is a side effect, not the reason.

Use `AGPL-3.0-or-later`, never the bare `AGPL-3.0`: SPDX deprecated the bare identifier precisely because
it was ambiguous about the "or later" clause. And **freeze the copyright year** — one year per file, never
a range, never a bump script. Protection runs from creation, not from what a comment says, so a maintained
range is a maintenance sink with no legal effect.

### 4. ADR 0007's parked question is answered, and one of its premises was wrong
0007's parked section contemplated Apache-2.0 or MIT and stated that *"the self-hosted license-issuance
mechanism is what actually grants rights either way"* — the argument being that the project's own license
choice changed nothing, because the signed file did the real work.

**Under AGPL that premise is false, and saying so is the point of this paragraph.** Rights are granted
publicly, by the license text, to everyone. The signed license file grants nothing; there is nothing left
for it to grant. A supersession that leaves a contradicting premise standing is exactly the drift this
project's ADR hygiene exists to prevent, so the correction is recorded here rather than left implicit in
0007 being marked superseded.

### 5. What remains valid from ADR 0007
Its reasoning against drafting a novel BSL/SSPL-style public license — real drafting and review cost, an
ongoing maintenance burden, and a text that must anticipate every case a public license needs to cover.
Still correct, and now doubly so: AGPL-3.0 is a standard, widely-interpreted text needing no bespoke legal
review, which is precisely what 0007 wanted and could not get from a custom license.

### 6. The relationship to ADR 0005
0005's rationale is adopted wholesale. The failure mode it named — a permissive license letting anyone run
the code as a closed-source hosted service and never contribute back — is still the right thing to guard
against, and AGPL's network-use clause is still the instrument that guards it. 0005 stays superseded
because 0032 supersedes both; its reasoning is not superseded, only its status.

### 7. Why a GPL-3.0 dependency is acceptable
> GPL-3.0 §13 permits combining a GPL-3.0 work with an AGPL-3.0 work into a single conveyable work. The
> GPL terms continue to apply to the GPL part, and AGPL §13's network clause applies to the combination.

This is what makes `go.mau.fi/libsignal` usable in the daemon, and it is a case libsignal's own license
names and blesses rather than a gap being exploited. **It depends on the dependency being GPL version 3.**
A `GPL-2.0-only` library does not qualify — GPL-2.0 and AGPL-3.0 are not compatible in either direction —
while `GPL-2.0-or-later` does, because it can be taken up to v3. The dependency-licensing section below
explains why that distinction needs a check the license-type classifier cannot express.

**A GPL-specific obligation the permissive licenses do not carry:** distributing a binary containing
GPL-3.0 code obliges you to offer its **Corresponding Source**, including libsignal's source and the
scripts used to build and install it. Publishing the whole repository with pinned `go.mod`/`go.sum`
satisfies this in practice, but it is a distribution obligation rather than an attribution one, and it is
recorded here so it is not discovered at M97.

### 8. The irreversibility, and the much smaller question that replaces 0007's parked one
Relicensing is a one-way funnel. Inbound works — GPL-3.0 combines into AGPL-3.0 by §13. **Outbound never
works**: nothing moves from AGPL-3.0 into a permissive or proprietary license without every copyright
holder's agreement.

> Once libsignal is linked into the daemon at M97, **that binary is permanently AGPL/GPL-locked, including
> for the copyright holder**, who does not hold copyright in libsignal. No CLA, no future ADR, and no
> amount of sole ownership can undo it. The only way back is removing libsignal entirely.

This also removes the ability to grant AGPL §7 *additional permissions* covering the whole daemon, since
part of it will no longer be the owner's to grant over. **If a formal plugin linking exception is ever
wanted, it must be drafted before M97** or scoped explicitly to the owner's own code. The Consequences
section's sentence about WASM plugins as separate works is the cheap version and is likely sufficient; the
formal version has a deadline.

**The backend is different, deliberately.** E2E is client-side by design — the daemon owns the keystore,
the ratchet runs in the daemon, the backend relays ciphertext it cannot read — so the backend needs no
copyleft dependency and has none. `contracts/dependency-licenses.txt` confirms it: across all four modules
the only identifiers present are Apache-2.0, BSD-2-Clause, BSD-3-Clause, ISC and MIT. **As long as that
holds, the backend remains relicensable at the copyright holder's sole discretion.**

Preserving that needs two things, and CI can only enforce the first:

- **No copyleft dependency in `backend/`** — enforced by the per-module split below.
- **No un-assigned contribution to `backend/`** — a discipline, not a check, because nothing but a human
  refusing to merge can enforce it. A single merged patch held by somebody else ends the option
  permanently, which is why `CLAUDE.md` carries it as rule 23. The project takes backend contributions
  under a signed copyright assignment and client contributions under a DCO sign-off, so the discipline is
  a gate on one module rather than a closed door on the repository — see the Consequences below.

So the question this ADR parks is not 0007's. The *daemon's* license is settled forever the moment M97
lands. What remains open is only whether the backend's permissive-only discipline is worth keeping — a much
smaller question, and one that costs nothing to keep open. Dropping the split later is one ADR away;
re-establishing it after a copyleft dependency has landed is not.

### 9. The name is reserved under AGPL §7(e), not claimed as a trademark
`README.md`'s notice declines to grant permission to use the Norite name and branding for forks or
derivative services in a way likely to cause confusion. **This wording is deliberate and must not be
"simplified" into a trademark claim.** There is no registered mark. In France trademark rights are
attributive — an unregistered sign confers no positive right, only a possible unfair-competition action
requiring proof of confusion and harm. AGPL §7(e) expressly permits adding a term *declining to grant*
rights in names and marks, which is valid with or without registration. Declining to grant is accurate;
asserting a mark that does not exist is not.

### 10. For the ADRs that still cite 0007
Six ADRs reference 0007 as live authority — [0013](0013-public-matchmaking-and-community-features.md),
[0014](0014-e2e-encryption.md), [0019](0019-platform-scope-and-commercial-model.md),
[0020](0020-operations.md), [0021](0021-flagship-kubernetes-deployment.md) and
[0031](0031-two-factor-authentication.md). Per this project's convention an ADR body is a historical record
and is not rewritten, so each keeps its sentence and gains a bracketed editorial note pointing here — the
treatment [ADR 0023](0023-voice-connection-authentication.md) gave [ADR 0012](0012-voice-in-v1.md).
**Their references now resolve to this ADR.** Two of them made claims that are now wrong rather than merely
stale: 0014's libsignal license gate (answered in section 7) and 0020's "final reviewed license text",
which under AGPL does not exist because there is no bespoke text to review.

[ADR 0005](0005-agpl-license.md)'s two references to 0007 are about supersession rather than authority and
stay as they are.

## Release posture: phase betas, and exactly one release
Carried across from ADR 0007 essentially intact, because it is still the decision:

- **Nothing ships as a release before the milestone sequence is complete.** The roadmap is a dependency
  order and stays one; the answer to schedule pressure is not a smaller v1.
- **A beta build goes to a small group of testers at each phase boundary.** Enough of the product to
  exercise what that phase added — not a public launch, not a support commitment, and not a version anybody
  is invited to depend on. This is what `M20a` exists to make possible.
- **One official v1**, after every milestone is done and the whole thing has been reviewed and tested.

Two edits to what 0007 said about it.

**The commercial consequence changes.** 0007 read: *the self-hosted license-issuance mechanism has no
customers until v1.* Under AGPL it has no customers **ever** — self-hosting is free and unrestricted, and
no license file can make it otherwise. What survives is the other half, still true and still load-bearing:
**the flagship accepts no non-developer account before v1.** That closes `M67a`'s registration-abuse
exposure by policy rather than by code, which is why `M67a` is scheduled rather than urgent, and why
`README.md` must not promise open registration before there is one. ADR 0031 §Context depends on this
sentence; it is unchanged in substance.

**A phase beta is distribution, and distribution triggers attribution.** MIT and BSD require their notices
to accompany the distributed binary. That obligation is unmet by `contracts/dependency-licenses.txt`, which
answers a different question (see the dependency-licensing section), and it is met by the embedded
`THIRD-PARTY-NOTICES.txt`
shipped with every binary. **The first phase beta is the real deadline for it.** Recording that here is
what stops it being discovered then.

## Dependency licensing: enforced, and split by module
The mechanism carries across from ADR 0007 unchanged, and it is good machinery: an allow-list enforced by
`go-licenses` in `just license-check` and CI's `security` job, the acceptable set stated in this ADR rather
than invented at a call site, and a committed inventory at `contracts/dependency-licenses.txt` so a change
to the set appears in a diff and is reviewed like any other change.

**The rationale is rewritten, because 0007's no longer holds.** It said a copyleft transitive dependency
*"is a legal problem in a binary shipped to a paying customer under a signed license file"*. There is no
paying customer and no signed license file. The new reason is section 8's, and it is narrower and sharper:

> Copyleft in the **clients** is now fine, and is exactly what admits libsignal. Copyleft in the
> **backend** would silently end its relicensability.

So the allow-list splits by module rather than applying one set to all four:

- **`backend/`** — permissive only: MIT, BSD-2-Clause, BSD-3-Clause, ISC, Apache-2.0, Unlicense. **MPL-2.0
  is denied here**, not because it is dangerous — it is file-level copyleft a Go binary does not trigger,
  which is what 0007 correctly argued — but because the backend's whole value in this split is being
  trivially relicensable, and MPL is a term someone would have to reason about.
- **`daemon/`, `cli/`, `gui/`** — AGPL-compatible: everything above, plus MPL-2.0 and GPL-3.0.

Disqualifying everywhere: SSPL, BUSL, Elastic-2.0, CC-BY-SA, and anything unidentifiable.

This also resolves a latent contradiction in the state 0007 left. Its prose allowed MPL-2.0 with sound
reasoning, while the flag set it configured (`--disallowed_types=forbidden,restricted,reciprocal`) denied
MPL as `reciprocal`. No dependency was MPL, so nothing broke and nobody noticed. The split resolves it by
module rather than globally.

**One requirement the type classifier cannot express, and it is the subtle one.** `GPL-2.0-only` must be
denied while `GPL-2.0-or-later` is allowed (section 7). `go-licenses` classifies by *type*, and both land
in `restricted` — so allowing `restricted` for the clients admits both variants indiscriminately. The gap
is closed by a second, cheap layer over the committed inventory: match the SPDX identifier itself, anchored,
so that `GPL-2.0-or-later` is not caught by a `GPL-2.0` prefix. Two layers rather than one because the type
buckets are the right tool for the broad policy and the wrong tool for this distinction.

**Two things CI cannot check at all**, needing a reviewer rather than a tool:

- **cgo-linked C libraries.** `go-licenses` knows only Go modules. Phase E brings Opus, RNNoise and the
  WebRTC APM. FFmpeg, if ever linked, is LGPL *or* GPL depending on build flags — acceptable in a client,
  fatal in the backend, and the flags used must be known for the notices file to be accurate.
- **Phase N video codecs.** x264 is GPL: acceptable in a client, denied in the backend. H.264 additionally
  carries patent obligations no copyright license resolves. VP8/VP9/AV1 remain the cleaner path.

## Consequences
- **Anyone may fork, self-host, run and modify Norite**, and must offer source to the users of any modified
  network service they run. The flagship's differentiation becomes operation, community and the name.
- **AGPL §13 obliges the flagship to offer the Corresponding Source to its network users**, corresponding
  to the *running* revision. This is served as a **public, unauthenticated** endpoint carrying the source
  URL, the revision the binary was built from, and the SPDX identifier. Unauthenticated is not a
  convenience: §13's offer is owed to network users, so a credential-gated answer does not discharge it —
  which is why it cannot live under `/instance`, whose defining property is that it has no exemption in it
  and whose router says so. The revision must be injected at build time, never hardcoded. Two operational
  consequences follow: the flagship must deploy only revisions that exist in the public repository, since
  private patches create a real obligation to publish them — configuration and secrets are not source and
  are not covered — and publishing the revision tells anyone which known vulnerabilities apply to the
  running instance. The second is accepted; the mitigation is prompt upgrading, required regardless, plus
  the
  coordinated-disclosure workflow in `SECURITY.md` that keeps a fix commit from becoming a public exploit
  before the release it belongs to.
- **Some organisations refuse AGPL software by policy.** ADR 0005 named this and it is still true and still
  an accepted trade: "stays open" over "maximum adoption".
- **A third-party `.wasm` plugin communicating only across the declared capability ABI is a separate work**,
  not a derivative of the daemon. Stating this costs nothing and is the difference between an ecosystem and
  an unanswered question — a plugin author who cannot find an answer writes no plugin. The formal version
  of this has a deadline; see section 8.
- **`entitlements`, `user_entitlements` and `internal/license/` survive as seams.** `user_entitlements`
  stops being inert at M72a, which resolves per-account guild limits from it; the other two remain unbuilt.
  AGPL does not
  restrict charging for a service you host, and one cannot infringe one's own copyright, so flagship
  subscription perks are unaffected and ADR 0013's paid custom-emoji perk still rests on a real seam. The
  self-hosted signed-license machinery has lost the customer 0007 designed it for and is kept unbuilt
  rather than deleted, the treatment ADR 0006 gave the voice seams and for the same reason: an unbuilt seam
  costs nothing, a removed one costs a redesign. ADR 0020's distinction between the license file's
  Ed25519-JWT scheme and release signing survives intact.
- **What remains commercially available needs no license grant at all:** flagship subscription perks, paid
  support, hosting and managed instances, and — if the question ever returns, though it is not planned — a
  backend-only commercial exception, preserved by section 8's discipline.
- **The project is open to contribution, on terms that differ by module** — stated in `CONTRIBUTING.md`
  and pointed at from the PR template. The client modules take a DCO `Signed-off-by`, under which the
  contributor keeps their copyright; there is nothing left to protect in `daemon/`, which is permanently
  copyleft-locked once libsignal lands. `backend/` additionally requires a signed copyright assignment,
  for section 8's reason and no other: it is the one module whose licensing is still an open question, and
  one contribution held elsewhere closes it for good.

  Two consequences worth stating rather than discovering. **An assignment is a real deterrent**, so some
  fixes that would have arrived as a backend patch will arrive as an issue instead, or not at all — that
  is the price of keeping the option, and it is being paid deliberately. And **the instrument itself is
  not drafted**: `CONTRIBUTING.md` states the requirement and says the text will be provided when the
  first backend contributor appears, rather than publishing an unreviewed legal document. Under French
  law an assignment must satisfy CPI art. L.131-3 — each right, its scope, purpose, place and duration
  specified — and art. L.131-1 voids a global assignment of future works, so this is the item in this ADR
  most worth a lawyer's time before anybody signs anything.

## Alternatives considered
- **Keep ADR 0007's all-rights-reserved posture.** Rejected. It rests on legal exclusivity being part of
  the strategy, and it is not — the flagship competes on being well-run. It also left libsignal, and
  therefore the entire E2E design, resting on an unanswered license question scheduled for M97.
- **A permissive license (Apache-2.0 or MIT)**, the two 0007's parked section contemplated. Rejected for
  ADR 0005's original reason, which nothing has weakened: neither prevents a closed-source hosted fork,
  which is the specific failure mode worth guarding against for a self-hostable chat platform. Apache-2.0's
  explicit patent grant is a genuine advantage and is not enough to outweigh it.
- **GPL-3.0 without the network clause.** Rejected: it permits precisely the closed hosted fork above,
  since running a service is not distribution. The network clause is the whole reason AGPL is the choice.
- **Dual-licensing — AGPL plus a sold commercial exception.** Rejected for now, not on principle but
  because it is not exercisable: it requires holding copyright in the whole work, and section 8 puts the
  daemon permanently out of reach at M97. The backend half is preserved by the module split at no cost, so
  nothing is lost by declining to build the machinery today.
- **One global dependency allow-list rather than a per-module split.** Rejected in both directions.
  Permissive-only everywhere denies libsignal and kills ADR 0014's design. AGPL-compatible everywhere ends
  the backend's relicensability the first time something copyleft appears transitively, silently and
  without a decision being made.
