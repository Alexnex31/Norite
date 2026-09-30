# ADR 0033: Semantic Versioning, `0.x` releases from the first usable client, and a gated `1.0.0`

## Status
Accepted. **Partially supersedes [ADR 0032](0032-agpl-license.md)**: its section "Release posture: phase
betas, and exactly one release", and nothing else. 0032's license, dependency-licensing, deployment-shape,
AGPL §5(d)/§13 and `user_entitlements` decisions stay in force, and its text stays intact as the record of
the policy this replaces. The release posture 0032 carried across from
[ADR 0007](0007-licensing-and-project-posture.md) ends here, so the chain for that one section is
0007 → 0032 → 0033.

## Context
0032 kept 0007's release posture: nothing ships as a release before every milestone is done, a beta build
goes to a small group of testers at each phase boundary, there is exactly one official v1, and the public
flagship accepts no non-developer account before then. It was written at M11 because no milestone was
marked alpha, beta or first-public, which made the end of the sequence (`M0`–`M125` plus suffixed
insertions, realistically multi-year) the only defined point at which anybody outside the project could run
anything.

Three things make that the wrong policy now.

- **A usable product exists much earlier than the end.** `M20a` was inserted precisely so partial
  completion is a working, narrow product rather than a stack of layers: after it, two people can hold a
  text conversation in a real client. A policy under which that product cannot be released treats the
  insertion's whole purpose as internal.
- **The people who can exercise a release are already allowed to run it.** The code is
  `AGPL-3.0-or-later`; anybody may build and self-host any commit today. "Nothing ships" never prevented a
  self-hoster from running Norite. What it prevented was a *named, reproducible version* to run, report
  against and upgrade from, which is the only part of a release that helps the project.
- **A multi-year solo build needs increments.** A tagged version at each phase boundary is a checkpoint a
  bug report can name, a point a self-hoster can pin, and a feedback loop measured in phases rather than
  in the whole plan.

What 0032 got right, and this ADR keeps, is the other half: **the roadmap is a dependency order and the
answer to schedule pressure is not a smaller v1.** Every milestone stays in scope. What changes is that the
versions before `1.0.0` stop being private.

## Decision

### 1. Semantic Versioning 2.0.0, always three components
Release tags are `vMAJOR.MINOR.PATCH`, optionally with a pre-release suffix: `v0.1.0-alpha`,
`v0.3.0-beta.2`, `v1.0.0-rc.1`. **Always three numeric components**: `v0.1-alpha` is not a version, and
it is never used as one, in a tag or in prose. Precedence is SemVer §11's: a pre-release sorts below its
release (`v0.2.0-alpha.1` < `v0.2.0-beta.1` < `v0.2.0`), and build metadata is ignored.

**Milestone tags stay, and are not versions.** `m0` … `m17` and every later `m<N>` remain the internal
marker for a completed milestone, as `CLAUDE.md` describes. Release automation fires only on a `v*` tag
and never on an `m*` one; a commit that completes a phase carries both.

### 2. The progression

| When | Version |
| --- | --- |
| `M20a` is done — the first milestone a person can use | `v0.1.0-alpha` |
| The last milestone of Phase D is done (currently `M24`) | `v0.1.0` |
| The end of each later sequential feature phase | the next MINOR: Phase E → `v0.2.0`, F → `v0.3.0`, … |
| Every milestone is done | the `1.0.0` pre-release stage (section 5) |
| That stage is complete | `v1.0.0` |

The sequential phases after D are E through O, eleven of them, so the end of Phase O is `v0.12.0`. Read the
list from `docs/roadmap.md`'s phase headers rather than from this table if the two ever disagree. A phase
ends when its last milestone is done; since milestones are occasionally built out of order (`M13a` was
built after `M17`), that means every milestone under the phase's header, not the one with the highest
number.

**`v0.1.0-alpha` is the only pre-release this ADR fixes to a milestone.** It is fixed because `M20a` is
the point at which a release first has somebody to reach.

### 3. What does not move the version
- **Phase P**, the flagship Kubernetes track, is a parallel deployment track and not a product phase; the
  roadmap already says it must not be read as coming after `M111`. Its milestones ship inside whatever
  version is current and trigger no MINOR bump.
- **`M124` and `M125`**, which the roadmap appends "logically earlier — same treatment as Phase P", get
  the same treatment here: they ship in whatever version is current when they are done, and do not gate a
  phase boundary.
- **PATCH releases** (`v0.x.1`, `v0.x.2`, …) may be cut at any time between phase boundaries, for fixes.
- **Intermediate pre-releases are optional**, at the author's discretion: an alpha, beta or rc of the next
  version may be cut at any milestone (`v0.2.0-alpha.1` mid-Phase E, `v0.2.0-beta.1` shortly before its
  end). This is an option and not a schedule. No document assigns an alpha or beta label to a future
  milestone, because a label planned in advance is a promise about the state of a build that does not
  exist yet.

### 4. No MAJOR bump before every milestone is done
However large a breaking change is, the version stays `0.x` until the whole sequence is complete. `1.x`
means one thing: **the planned v1 scope exists**. It does not mean "stable enough", "used by enough
people" or "the protocol stopped changing".

**While the MAJOR is 0, any MINOR version may be incompatible with the one before it** — in the wire
protocol, the local IPC handshake, the config file, the database schema or the CLI's `--json` output. That
is SemVer §4's meaning for `0.y.z`, and it is stated plainly here rather than left to a reader who knows
the specification. Two consequences are specified elsewhere: `M20`'s version handshake treats MINOR as the
breaking component while MAJOR is 0, and `M24`'s updater never installs a version its instance would
refuse.

### 5. A mandatory stage between "every milestone is done" and `v1.0.0`
"Every milestone" means all of them — `M0` through `M125`, every suffixed insertion that has not been
retired, and Phase P — not `M125` by name. `M125` sits on an appended track and is not necessarily the last
to finish.

When the last is done, the project enters a **`1.0.0` pre-release stage**: `v1.0.0-rc.N`, optionally
preceded by `v1.0.0-beta.N`, for a full review and test of the whole product. **`v1.0.0` is cut only after
that stage**, and is the official release.

The external cryptographic review of the E2E work ([ADR 0014](0014-e2e-encryption.md), `M103`) is a hard
gate on *trusting* end-to-end encryption, and belongs in this stage if it has not been passed earlier.
`v1.0.0` does not ship without it.

### 6. Access and distribution
- **Self-hosting is open from the first release.** Anyone may self-host any tagged version as soon as it
  exists. **A `0.x` release carries no support or stability commitment**: MINOR versions may break
  compatibility, there is no guaranteed migration path from one `0.x` to the next, and no third party has
  reviewed the code. `README.md` says this where it describes releases.
- **Developer testers are not gated.** A build may be handed to a tester at any time. The old rule of a
  beta build per phase boundary to a small group no longer restricts anything; a phase boundary is now
  simply where a MINOR version is cut.
- **The public flagship stays closed** to non-developer accounts until the feature set is well advanced,
  at the author's judgement, **and above all until the Phase P deployment track is ready**. The opening is
  deliberately not tied to a version number: the flagship is the one deployment needing horizontal scale
  and HA ([ADR 0021](0021-flagship-kubernetes-deployment.md)), and a version number says nothing about
  whether that deployment exists.
- **E2E is not presented as trustworthy before its gate.** A `0.x` release containing Phase M work must not
  describe end-to-end encryption as reviewed or trustworthy before `M103`'s review passes. The
  build/instance-level flag ADR 0014 specifies is what enforces it, and release notes must not contradict
  the flag.

## Consequences
- **The README's status badge stays `pre-alpha` until `v0.1.0-alpha` actually ships.** It describes a
  state of the project, and changing it now would describe a release that does not exist.
- **Distribution starts at `v0.1.0-alpha`, and two obligations 0032 dated to "the first phase beta" move
  to that tag.** Attribution: every release archive already carries its binary's `THIRD-PARTY-NOTICES.txt`
  and `LICENSE`. Disclosure: `SECURITY.md`'s coordinated-fix workflow first has somebody to coordinate with.
- **Code signing arrives after the first release, unless it is moved.** `M24` wires Sigstore/cosign
  signing and verification, and it is Phase D's last milestone, so `v0.1.0-alpha` at `M20a` ships with the
  checksum file GoReleaser produces and no signature. [ADR 0020](0020-operations.md) already describes
  signing as absent in the early-use phase; the release notes of every unsigned release must say so.
- **`M67a`'s scheduling argument now covers the flagship alone.** 0032 made registration anti-automation
  "not urgent" because nothing was publicly open. The flagship stays closed, so that still holds for it;
  a self-hoster who opens registration on a `0.x` instance before `M67a` does so without that protection.
- **The migration squash `architecture.md` §2 contemplates "at v1" now strands `0.x` instances** unless it
  ships with an upgrade path from the last `0.x` schema. Section 6 permits that — there is no guaranteed
  migration path before `1.0.0` — but whoever squashes has to decide it, not discover it.
- **Release automation**: GoReleaser publishes on a `v*` tag only, and marks any version with a
  pre-release suffix as a pre-release on GitHub (`release.prerelease: auto`). The workflow refuses a tag
  that is not a three-component SemVer version before GoReleaser runs, because GoReleaser's version
  parser is lenient about missing components and is not relied on to refuse `v0.1-alpha`.

## Alternatives considered
- **Keep 0032's single-release posture.** Rejected for the three reasons in Context. It protected nothing
  the AGPL does not already give away, and cost the project every feedback loop shorter than the whole
  plan.
- **Calendar versioning, or milestone numbers as versions.** Rejected. Neither says anything about
  compatibility, which is the one question a self-hoster upgrading needs answered, and milestone numbers
  are not monotonic in build order (`M13a` after `M17`, Phase P in parallel).
- **`1.0.0` at the first usable client, with MAJOR bumps for breaking changes after it.** Rejected. `1.x`
  would stop meaning "the planned scope exists", and under a plan this long the MAJOR number would measure
  churn rather than anything a user cares about.
- **A MINOR bump for Phase P milestones.** Rejected: Phase P changes how the flagship is deployed, not
  what Norite does, and bumping on it would make a self-hoster's version number move for work that never
  reaches them.
- **Fixing alpha and beta labels to milestones in advance.** Rejected: it is a schedule for the state of
  builds that do not exist yet, and the 0007/0032 posture already showed that a release rule written far
  ahead of its builds gets revisited rather than followed.
