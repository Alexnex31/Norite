# ADR 0005: AGPL-3.0 license

## Status
**Superseded** by [ADR 0007: Licensing and project posture](0007-licensing-and-project-posture.md). The
project moved off AGPL-3.0 entirely once the architecture plan established a real commercial posture — a
free flagship instance plus sold self-hosted licenses — that AGPL's copyleft terms don't cleanly support
(AGPL doesn't stop a self-hosted licensee from reselling/forking, which the commercial model requires
blocked). Rather than replace it with another public license, the project now publishes **no public license
at all**: default copyright applies (all rights reserved), and self-hosted customers receive an individually-
granted, cryptographically-signed license file instead of relying on public license text. The `LICENSE` file
at the repo root reflects this current policy directly — see ADR 0007. Historical rationale below is kept
intact for why AGPL was the *original* choice.

## Context
This is a self-hosted, open-source chat platform. A permissive license (MIT/Apache-2.0) maximizes
contributor/adoption friendliness but allows anyone to take the code, run it as a closed-source hosted SaaS,
and never contribute improvements back — a well-known failure mode for self-hosted OSS projects that become
popular (this is exactly the gap the AGPL's network-use clause closes, versus the plain GPL).

## Decision
License the project AGPL-3.0. The verbatim license text lives at the repo root `LICENSE` (sourced from
`https://www.gnu.org/licenses/agpl-3.0.txt`, not paraphrased).

## Consequences
- Anyone who modifies this project and runs it as a network service (not just anyone who redistributes the
  binary) must make their modified source available to users of that service. This is the AGPL's
  distinguishing clause over the plain GPL-3.0.
- Some companies have blanket policies against using/contributing to AGPL code, which can reduce corporate
  contribution — an accepted tradeoff for a project prioritizing "stays open" over "maximum adoption."
- Every new source file should be considered implicitly covered by this license; no need for per-file
  license headers unless the project later decides otherwise, but don't mix in dependencies with
  incompatible licenses (check before adding a new dependency with a copyleft-incompatible or
  non-OSI-approved license).
- This is independent of the project's branding — see the trademark/naming note in
  `docs/architecture.md` Context section and `CLAUDE.md`: a permissive-looking license does not neutralize
  the separate trademark risk of shipping under Discord's name/logo.

## Alternatives considered
- **MIT / Apache-2.0**: simpler, maximally contributor-friendly, explicit patent grant (Apache-2.0), but
  neither prevents a closed-source SaaS fork — rejected given the project's stated goal of staying an open
  self-hosted alternative, similar to the reasoning behind other AGPL-licensed self-hosted chat platforms
  (e.g. Revolt).
