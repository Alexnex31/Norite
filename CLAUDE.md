# CLAUDE.md

This file gives Claude Code (and any other AI agent) the context needed to work on this repo without
re-deriving decisions already made. The full architecture/design rationale lives in `docs/architecture.md`
(committed in this repo) — **read that file for anything not covered here**; this file is the fast-loading
summary + the rules that must hold on every change. `docs/adr/` has short, focused records of the most
contested individual decisions — read the relevant one before proposing to change any of them.

## What this is

Norite is a voice-and-text chat platform, licensed `AGPL-3.0-or-later`. It has two deployment shapes and
**both are first-class**: the free, global, publicly-hosted flagship instance the author operates, and
self-hosting, which is real, fully-built and free — aimed at enterprises and other private groups who want
their own instance. Neither is the product and neither is the sideline; they get the same code, the same
quality bar and the same support commitment. The roadmap looks lopsided (Phase P spends twelve milestones
on the flagship's Kubernetes track against M96 plus documentation for self-hosting) and that is deployment
complexity, not priority — the flagship is the one deployment needing real horizontal scale and HA
(ADR 0021). **Four clients**: a scriptable CLI (the command tree — one action, exit, pipeable), a
full-screen **TUI** (the in-terminal application: panes, chords, 27 specified screens), a native GUI
mirroring the TUI's information architecture, and a lower-priority web SPA built later. The CLI, TUI and
GUI share one local background daemon per OS user account; the CLI and TUI share one command tree, so
`M-x` in the TUI runs every verb (ADR 0026). "CLI" here means the command tree only; where it once meant
both, that conflation is what left the roadmap with six milestones of TUI capabilities and none that drew a
screen. Servers ("guilds"), channels, roles/permissions, real-time text and voice messaging, DMs, presence,
invites, public matchmaking, BYOK end-to-end encryption, client-side plugins, and more all ship as real v1
scope — see `docs/architecture.md` Section 7 for the full list. **Licensed
[AGPL-3.0-or-later](LICENSE)** — free software, publicly, to everyone: anyone may fork, self-host, run and
modify it, and must offer source to the users of any modified network service they run. The name is
reserved under §7(e) by declining to grant it, not by a trademark claim. See
`docs/adr/0032-agpl-license.md`.

## Architecture at a glance

- **Modular monolith backend**, not microservices. Single Go binary, well-separated `internal/` packages,
  each domain (`auth`, `users`, `guilds`, `channels`, `roles`, `messages`, `invites`, `gateway`, `presence`,
  `voice`) owns its own `service.go`/`http.go`/`model.go`/`events.go`.
- **Client daemon.** The CLI and native GUI are thin "attach" UIs over one shared local daemon, which holds
  the actual persistent WebSocket gateway connection, presence/Deep Work state, in-memory scrollback, the
  WASM plugin host, and local bot automation. Two IPC channels: a Unix domain socket / named pipe (CLI+GUI,
  OS-permission-protected, reuses the gateway's own op-code/DISPATCH protocol) and a separate localhost TCP
  port with its own per-session secret (external bot/automation scripts). Voice is deliberately *not* in the
  daemon — a voice-worker subprocess, spawned on demand, owns the whole audio pipeline so a media bug can
  never take down messaging/presence/plugins.
- **Monorepo**: `backend/` (Go) + `cli/` + `gui/` + `daemon/` (Go) + `internal/voice` (Pion SFU/TURN) +
  `contracts/` (OpenAPI + gateway event schema + CLI `--json` schemas, the source of truth every side
  codegens from) + `docker/` (local dev compose stack) + `frontend/` (the later, tertiary web SPA).
- **Real-time**: a WebSocket "gateway" modeled directly on Discord's real protocol (op-codes, HELLO/
  IDENTIFY/READY handshake, heartbeats, RESUME, DISPATCH events), carrying a semver version field
  (MAJOR must match exactly, a defined MINOR-version-back window is tolerated; while MAJOR is 0, MINOR
  must match too — M20's entry). REST is used for all CRUD;
  the gateway is used for live push plus a handful of gateway-only client ops (typing, presence, voice
  state). The same protocol is reused, unmodified, over the local daemon↔CLI/TUI/GUI socket.
- **IDs**: Discord-style Snowflakes (`bigint`, time-sortable), not UUIDs, not serial — see
  `internal/platform/snowflake`. Always JSON-marshaled as a quoted string (avoid JS float64 precision loss).
- **Auth transport**: token-based (Bearer access + refresh tokens, `device_id`-scoped families, OS-keychain
  storage via `zalando/go-keyring`, scoped `api_tokens`) is primary and exclusive for the CLI/TUI/GUI — the
  daemon is the sole holder of its account's tokens, never an attach client. Cookie/CSRF is retired entirely
  for the CLI/TUI/GUI/daemon REST surface; it returns, decided but not yet built, as a BFF-style
  httpOnly-cookie exchange layer in front of the same token API once the web SPA is built (Phase O).
- **E2E encryption**: the daemon — never the CLI/TUI/GUI independently — holds the E2E keystore/ratchet state
  and performs decryption, relaying plaintext to attach clients over the already-trusted local IPC socket.
  Opt-in, `DM` channel type only.

## Tech stack

**Backend**: Go, `chi` (router), `sqlc` + `pgx/v5` (DB access — no ORM), `coder/websocket`,
`alexedwards/argon2id`, `golang-jwt/jwt/v5`, `golang.org/x/oauth2`, `pquerna/otp` (TOTP),
`go-playground/validator/v10`,
`golang-migrate`, `oapi-codegen`, `ulule/limiter`, `testify` + `testcontainers-go`, Postgres (`tsvector` +
GIN + `pg_trgm` for search), Valkey (the BSD fork of Redis 7.2.4 — activated only for the flagship's
horizontal-scale event bus/rate limiting; self-hosted single-process instances never touch it. The
`EVENTS_BACKEND=redis` seam and `ulule/limiter`'s "Redis-backed store" keep that name, which describes the
protocol rather than the product — see M114).

**CLI** (command tree): Go, `urfave/cli` v3 (command tree, flag parsing, `--json`/`--help`, completions —
chosen over `spf13/cobra`), `pelletier/go-toml` v2.

**TUI** (in-terminal client): Bubble Tea + Lip Gloss + Bubbles (Charm stack) with a custom pane/split
engine, `teatest` for testing, `BourgeoisBear/rasterm` for inline images. Screens, keymap and tokens are
specified in `docs/design/tui/` and are normative.

**Native GUI**: Go, Gio (`gioui.org`) — immediate-mode, hand-built widgets, golden-image testing for the
highest-value surfaces.

**Daemon**: Go, `wazero` (WASM plugin sandbox, pure Go/no cgo), `zalando/go-keyring`, `fsnotify`,
`gofrs/flock`, `modernc.org/sqlite` (E2E keystore, pure Go/no cgo).

**Voice-worker**: Go, `pion/webrtc` + `pion/interceptor`, libopus compiled to WebAssembly under `wazero`
(ADR 0036, no cgo), pure-Go capture per OS, RNNoise and WebRTC's APM (AEC3 and AGC2, replacing `libspeexdsp`
per ADR 0023) in WebAssembly too since M30. The *only* binary in the stack where cgo is permitted, and since
M29 it uses none.

**Web SPA** (later, tertiary client): React, TypeScript, Vite, TanStack Query, Zustand, Zod, Tailwind +
shadcn/ui, `react-hook-form`, Vitest + Playwright.

## Non-negotiable rules

These apply to every milestone, not just a final pass — treat a PR that violates these as incomplete, not
"good enough for now":

1. **Every mutating handler checks permissions before writing.** Resolve effective permissions via
   `roles.Resolve(...)` (or an explicit hierarchy check for role/member management) using data freshly
   loaded for the *specific* guild/channel in the request path. Never trust a client-supplied ID without
   verifying it belongs to the actor's claimed context.
2. **Every guild-scoped *administrative* mutation writes an audit log entry**, in the same DB transaction
   as the mutation. A mutation is administrative when it changes what the guild *is*, or exercises
   authority over somebody else — whoever performs it. `guilds.AuditActions()` is the current evidence of
   that definition rather than the definition itself: a new administrative mutation nobody remembered to
   audit is missing from that list, so treating the list as the test would rule it out exactly when the
   rule is needed. **Message content is deliberately outside it**, settled at M15: a member posting where
   they are permitted exercises authority over nobody, while auditing every send would put the product's
   highest-volume write on the audit path and bury the moderation signal the log exists to carry. A
   moderator acting on somebody *else's* message is administrative and is audited. The word was added
   here at M15 — before it, the rule read "every guild-scoped mutation" and M15 would have been the first
   milestone to knowingly violate its letter, which is how a rule stops constraining the next case.
   Guilds that want every message recorded opt in at **M16b**, which writes to its own table and never to
   `audit_log_entries`; **turning that option off is itself administrative and is audited here**, or it
   would be the one setting you could change to hide what you did next.
3. **All SQL goes through sqlc-generated, parameterized queries.** No `fmt.Sprintf`-built SQL, ever.
4. **No mutating logic in GET handlers.** GET must stay side-effect-free — the CSRF double-submit scheme
   depends on this, but that scheme itself only exists for the future web SPA's BFF layer; the
   token-authenticated CLI/TUI/GUI/daemon REST surface has no CSRF exposure at all (no ambient browser
   credentials), so this rule's purpose there is REST hygiene, not CSRF defense.
5. **Gateway events dispatch only after the originating DB transaction commits**, never before, never inside it.
6. **New gateway dispatch types update `contracts/gateway-events.schema.json` in the same commit.** New REST
   endpoints update `contracts/openapi.yaml` in the same commit. These are source-of-truth contracts every
   client codegens from — don't let them drift.
7. **New hot-path queries ship with the index they rely on**, in the same migration. Verify with
   `EXPLAIN ANALYZE` before merging anything on the message/member/channel-list hot paths.
8. **Never log secrets, tokens, password hashes, or raw refresh/reset tokens.** Refresh tokens and password
   reset tokens are stored only as SHA-256 hashes, never in plaintext, never logged.
9. **User-generated content is never rendered as raw HTML.** Every client's renderer goes through an
   allow-listed markdown-subset renderer; never `dangerouslySetInnerHTML` (web) or unescaped terminal output
   (CLI) on user content.
10. **Voice (audio) is real v1 functionality, not deferred.** Voice/video schema, permission bits, and
    channel types already exist and are largely *active* now (`GUILD_VOICE`, `PermConnectVoice`, etc.) —
    don't remove or restructure them. What's still deferred-but-seamed is video/screen-share specifically:
    `PermVideoVoice`, `GUILD_STAGE_VOICE`, and the `supports_video` capability plumbing must never be
    removed even though video itself isn't built yet.
11. **Voice/audio media (capture, encode, the SFU connection) must run in the isolated voice-worker
    subprocess**, spawned on-demand by the daemon — never in the daemon process itself. A crash there must
    never take down messaging/presence/plugins.
12. **Every WASM host-function exposed to plugins must be capability-gated** against that plugin's approved
    manifest grants (hash-pinned at approval time); every plugin instance gets enforced CPU/memory quotas and
    a wall-clock timeout per invocation — no plugin code path is exempt "just this once."
13. **Any server-side feature that reads message content** (search, moderation visibility, audit-diffing,
    edit-history, link-preview generation, account export) **must explicitly exclude E2E-encrypted DMs** —
    never assume plaintext is available; E2E is never offered for `GROUP_DM`, guild channels, whispers, or
    voice — `DM` only, full stop.
14. **Every Instance Admin action** (bans, report resolution, license/entitlement changes, admin-tier grants)
    **writes to `instance_audit_log`** in the same transaction, with the same rigor required for guild-scoped
    audit logging.
15. **New gateway dispatch types and REST endpoints that affect CLI-observable state must also update the
    CLI's `--json` output schema** in `contracts/`, in the same commit — a versioned source-of-truth
    contract, not best-effort output.
16. **Any new local IPC surface** (daemon↔CLI/TUI/GUI socket, the local bot-automation port) **must state its
    trust tier explicitly** in its design — OS-permission-protected (first-party clients) vs.
    secret-protected (external scripts) — never assume the two are interchangeable.
17. **A ban or self-service account deletion must invoke the general-purpose revoke-all-sessions primitive**
    (force-close live connections, revoke refresh/scoped tokens, revoke linked-device E2E trust) — no
    separately-implemented, potentially-incomplete cleanup path for either.
18. **Any server-side feature that fetches a user-supplied URL** (link previews today, anything similar
    later) **must route through the SSRF-protected dialer** (rejects private/loopback/link-local resolved
    IPs, checked at actual connect time), with a strict request timeout and a response-size cap — never a
    plain, unbounded `http.Get` on a user-controlled URL.
19. **Any untrusted text rendered by the CLI** (usernames, message content, link-preview titles, plugin
    manifest descriptions, webhook display names, anything else user- or third-party-controlled) **must pass
    through the terminal-safe sanitization function first** — never written to the terminal raw.
20. **Any server-side feature that gates delivery or visibility to a user** (DM send, whisper send, friend
    request, presence, notification dispatch, and guild-channel gateway fan-out specifically) **must check
    the `blocks` table.** This includes the real-time gateway dispatch path, not only REST mutation handlers.
    **One stated exception: instance announcements (M93a)**. They are authored by the instance rather than
    by any account, so there is no author for a block to name; the sending admin appears only in
    `instance_audit_log`. Anything that shows its sender to readers is not an announcement, and the rule
    applies to it in full.
21. **Any new REST endpoint or gateway event affecting CLI-observable or browser-observable state must be
    sanity-checked against real browser constraints** (CORS, request chattiness, BFF-auth-compatibility) at
    the time it's added — never deferred silently to Phase O just because the web client isn't built yet.
22. **Copyleft is allowed in the clients, never in the backend.** `daemon/`, `cli/` and `gui/` may take
    AGPL-compatible dependencies including GPL-3.0 and MPL-2.0 — that is what makes `go.mau.fi/libsignal`
    usable at M97. `backend/` takes permissive dependencies only: MIT, BSD-2-Clause, BSD-3-Clause, ISC,
    Apache-2.0, Unlicense — **and not MPL**. ADR 0032 gives the reason: the daemon is permanently
    copyleft-locked the moment libsignal lands, while the backend stays relicensable for exactly as long as
    it is copyleft-free, and that option costs nothing to keep. `just license-check` enforces it, but
    **CI cannot see C libraries linked via cgo** — Phase E's Opus/RNNoise/APM and Phase N's video codecs
    get checked by hand.
23. **Never merge `backend/` code from anyone but the copyright holder without a signed copyright
    assignment.** CI protects the dependency half of rule 22; nothing can protect this half but not
    merging. A single un-assigned contribution ends the backend's relicensability permanently, and a
    substantial patch pasted into an issue and copied in is the same contamination as a merged PR —
    reimplement independently rather than copy. The client modules take contributions under a DCO
    `Signed-off-by` instead, where the contributor keeps their copyright: the daemon is permanently
    copyleft-locked at M97 anyway, so there is no option there left to protect. `CONTRIBUTING.md` states
    both; the assignment instrument itself is not written yet and must be reviewed before anyone signs it.
24. **Every hand-written `.go` file carries its two-line SPDX header** — `SPDX-FileCopyrightText` then
    `SPDX-License-Identifier: AGPL-3.0-or-later`, the first two lines, blank line after. Never the bare
    `AGPL-3.0`, and never a maintained year range. Generated files are exempt, detected by their
    `Code generated … DO NOT EDIT.` marker rather than by a path list. Nothing else in the repository
    carries a header — not SQL, not Markdown, not YAML; `LICENSE` covers the whole work.
    **`just spdx-check` enforces all of it and CI runs the same checks**, because the header pass was
    one-off and what decays a rule like this is the next file, not the ones it converted.

## Directory layout (see `docs/architecture.md` §1 for full detail)

```
backend/       Go modular monolith — cmd/server, internal/{config,platform,auth,users,guilds,guildauth,
               channels,roles,messages,dispatch,gateway,presence,voice,db}, migrations/, and gatewayproto/
               (the gateway's wire format, outside internal/ because the daemon imports it)
cli/           The `norite` binary — the scriptable command tree (internal/cliapp) *and* the TUI
               (shell, panes, chords, screens); one binary, two front ends onto one command tree
gui/           The native GUI — Gio app, mirrors the TUI's screens; shares the daemon/config model
daemon/        Shared background daemon — gateway client, dual IPC, plugin host, config/state files
internal/voice/  Pion-based SFU, embedded TURN server (lives under backend/, server-side infra)
contracts/     openapi.yaml (REST), gateway-events.schema.json (WS), CLI --json schemas — source of truth
               (also dependency-licenses.txt, the committed license inventory — ADR 0032)
docker/        docker-compose.yml (postgres, valkey, backend hot-reload) — local dev + self-hosted prod
frontend/      React SPA — the later, tertiary web client (Phase O)
```

## Common commands

- `just dev` — run the full local stack (docker-compose: postgres, valkey, backend w/ air; frontend w/ vite
  joins at Phase O)
- `just test` — every Go module's tests, **with `-race`**, because that is what CI runs and a gate that
  differs from CI is not a gate. Since M30, the voice-worker's timing and differential tests run after the rest
  without it, in CI too, four of them since M31:
  - the two differential tests, which the detector slows eighteenfold through wazero's interpreter;
  - the latency budget, which times the program the detector slows;
  - M31's shaped-link test, which times a queue and a delay in two real calls. **Needs a running
  container runtime**: the backend's integration tests
  start a real Postgres via `testcontainers-go`, and since M18 a Valkey for the Redis paths. Costs about 1.6x, not the 10x the race detector is
  reputed to, since most of that time is Postgres round trips. Frontend tests join at Phase O.
- `just test-short` — unit tests only, skipping everything container-backed. Fast inner loop, not a
  substitute for `just test` before pushing.
- `just lint` — `go vet` + `golangci-lint` per module (frontend ESLint/`tsc --noEmit` at Phase O)
- `just db-migrate` — apply pending migrations (runs the server's own `-migrate-only` mode, so it takes the
  exact same advisory-lock-guarded code path a real startup does)
- `just sqlc-generate` / `just sqlc-check` — regenerate the committed sqlc layer / fail if it's stale
- `just security-scan` — `govulncheck ./...` plus `just license-check` (+ `pnpm audit` and `Trivy` once
  frontend/ and Dockerfiles exist)
- `just spdx-check` — fail if any hand-written `.go` file is missing its rule-24 header, if a generated one
  grew a header, or if a non-Go file did. Sees untracked files too, so it answers the same as CI on a file
  you have written but not yet staged.
- `just release-dry-run` — the release short of signing and publishing: `goreleaser check`, every archive
  for every OS and arch into `dist/`, then `norite about` from this machine's archive must report a stamped
  revision `git cat-file` finds. Signing needs the release workflow's identity, so a `v*` tag is its first
  real run (M20a). CI's `build` job runs the same snapshot release on every push.
- `just trusted-root` — regenerate the trusted root the daemon's updater verifies releases against, from
  Sigstore through TUF, and commit the diff after reading it. CI fails when the committed copy is behind.
- `just build-local` — plain `go build` of all four binaries into `./bin/`, at paths that do not vary by
  platform the way `just build`'s goreleaser output does. `just notices` and CI's assertions both read them.
- `just install` — build this checkout's client the way a release does and install it as `install.sh`
  does from a checkout: `~/.local/bin`, the daemon registered and restarted, and no install receipt, so the
  updater never writes a release over a development build. What is left is `norite setup`. Arguments go to
  the script (`--bin-dir DIR`, `--no-daemon`).
- `just install-server --url URL [--docker-postgres]` — the other side: `install-server.sh` building this
  checkout's server, an instance in `~/norite-instance` as a user service, and its administrator. Run again,
  it replaces the programs and keeps the configuration and the database. Builds report the version `dev`,
  which skips the client–server version check, so everybody stays on one commit.
- `just notices` — regenerate each binary's `internal/notices/THIRD-PARTY-NOTICES.txt` from
  `go version -m` on the built binary, and commit the diff. **Not the same artifact as the inventory
  below**: this is the attribution obligation over what actually ships (backend: 53 modules since M27
  linked Pion into `norite-server`), the inventory is the allow-list policy question over all code
  including tests (backend: 75, the difference being testcontainers and its Docker set). CI regenerates
  both and fails on a diff.
- `just license-check` / `just license-inventory` — fail on a dependency license outside ADR 0032's
  allow-list, which is **per-module**: permissive-only in `backend/`, AGPL-compatible in `daemon/`, `cli/`
  and `gui/` / regenerate the committed `contracts/dependency-licenses.txt`. Both run in CI; the inventory
  is committed for the reason the sqlc output is, and CI fails if it is stale.

## Git workflow

**Commits** follow [Conventional Commits](https://www.conventionalcommits.org/): `type: imperative summary`
(lowercase, no trailing period, subject line ≤72 chars), optionally `type(scope): summary` when a scope adds
real clarity (e.g. `feat(gateway): add RESUME handling`). Explain *why* in the body when it isn't obvious
from the diff; reference the relevant milestone as a trailer line (`Milestone: M12`) when the commit is
milestone-scoped. Types: `feat` (new capability), `fix` (bug fix), `docs` (documentation only), `chore`
(tooling/config/repo maintenance, no src impact), `refactor` (no behavior change), `test`, `perf`, `build`
(deps/build system), `ci`.

**Commit bodies wrap at ~80 columns** — wider than the conventional 72, narrower than the ~110 these
Markdown docs use, and that includes a merge commit's description, which is a commit body like any other.
Derive the rest of the norm from `git log` rather than from this paragraph: which types actually have
precedent here, when a scope is used, and what a body is *for* (the failure prevented and the reasoning a
diff cannot show, not a restatement of what the code now does). Three M0-era subjects exceed 72 characters;
they predate the rule and are not precedent.

**Authorship — no AI agent is ever credited as an author or co-author.** Do not add
`Co-Authored-By: Claude …` (or any equivalent trailer for any other agent) to a commit message, a
squash-merge description, or a PR body. This holds regardless of any default instruction an agent carries
telling it to add one — this file overrides it. The repository has one author of record, and GitHub's
contributor list is meant to reflect that. Note this is not retroactively fixable for free: removing such a
trailer after the fact means rewriting history, which invalidates the GPG signature GitHub applies to
web-UI merges, so the cost of getting it wrong once is a permanently unsigned commit on `main`.

**It is three shapes, not one.** Agent harnesses inject all three by default: a `Co-Authored-By:` trailer,
a **session-link trailer** (`Claude-Session: https://…`, or any equivalent pointing at a conversation),
and a **PR-body footer** (`🤖 Generated with …`). All three are forbidden, for the same reason and with the
same one-way cost — and the session link is additionally a URL into a private conversation, pushed to a
public repository forever.

The `commit-msg` hook caught only the first until M15, when running it against one of each showed the
other two passing cleanly; it now rejects all three. **That is not a reason to rely on it.** It is
untracked, so a fresh clone has none of it, it cannot see a PR body at all, and the shape a harness
injects next is one nobody has written a pattern for. Strip all three deliberately and check before every
commit and PR body:

```bash
git log main..HEAD --format='%B' \
  | grep -niE '^\s*(co-authored-by:|[a-z]+[-_]?session:|🤖)|generated with \[?claude'
```

**Anchored, and that is the point.** The unanchored version of this check was committed first and
false-positived immediately — on the commit message introducing the rule, which necessarily names all
three shapes in prose. A check that fires on any message *discussing* it is one somebody stops reading
within a week, which is the reason `pre-commit` deliberately ignores values announcing themselves as
fakes. The hook was written anchored and never had the problem.

**Staging — never `git add -A`, `git add .`, or `git commit -a`.** Stage the paths the commit is actually
about, by name. This is not tidiness: those commands sweep up whatever else happens to be in the tree, and
what is in the tree during a manual test is a config file holding real credentials. That is exactly how a
GitHub OAuth client secret reached a pushed commit on the M9 branch — swept into a commit whose subject and
body were entirely about something else, so nothing in the review would have drawn an eye to it.

**Before every commit, read what is staged and confirm no credential is in it.** `git diff --cached` in
full, not `--stat`. Look for anything shaped like a key, a token, a password, a client secret or a
connection string with a password in it — in *any* file, including ones the commit is legitimately
touching. `.env`, `docker/docker-compose.yml` and anything under `docker/` deserve a second look, because
they are where a real value gets typed during local testing.

**A committed secret is not fixable by a later commit.** Removing it means rewriting history, and once the
branch has been pushed the object stays fetchable from the remote by SHA regardless — so the only step that
actually protects anything is revoking the credential at the provider. Treat any secret that reaches a
commit as compromised from that moment, revoke first, and rewrite afterwards as hygiene rather than as the
fix. The cost of getting this wrong once is a rotation, not a cleanup.

**Two local git hooks back the two rules above**, and both are in `.git/hooks/`, which git does not track —
so a fresh clone has neither, and neither is where the rule lives. They are a backstop under the habit, not
a replacement for it, for the same reason the project skills are described here rather than relied on:
this file is the authority, and anything enforced only by an untracked file is enforced only on one
machine.

- `commit-msg` rejects a message crediting an AI agent — all three shapes above, since M15: the
  `Co-Authored-By:` trailer it always caught, plus the session-link trailer and the "Generated with …"
  footer, which were found to pass it by testing one of each rather than by assuming the first pattern
  covered them.
- `pre-commit` rejects staged content that looks like a live credential: a provider token format
  (`ghp_`, `AKIA`, `AIza`, a PEM private-key header), a secret-named setting given a long opaque value, or
  a `.env` file forced past `.gitignore`. It deliberately ignores values that announce themselves as fakes
  — `dev-only`, `insecure`, `example`, `changeme`, `test…key` and similar — because this repository is full
  of those on purpose, and a hook that cried wolf on the compose stack's own signing key would be
  `--no-verify`'d within a week and protect nothing afterwards. It never blocks a commit that *removes* a
  secret, since standing between somebody and that cleanup is the one way it could do harm.

Rewriting either to be stricter is fine; making either the reason a rule above is not followed is not.
`--no-verify` exists and is sometimes right — a deliberate fake the filters do not recognise is the usual
case, and labelling the value is the better fix.

**Branching**: `main` always reflects the state right after the most recently *completed* milestone — never
a half-finished one. Each milestone (see `docs/roadmap.md`) gets its own branch, named `m<N>-<kebab-slug>`
matching the milestone's title (e.g. `m1-backend-skeleton`). Child branches off a
milestone branch are fine for exploring an approach within that milestone; merge them back into the
milestone branch, not `main`, before the milestone branch itself merges into `main` once its "done when"
criteria are met.

**Every milestone branch reaches `main` through a Pull Request — always, even solo. Never a direct/local
merge into `main`, and never a fast-forward merge**, starting from Milestone M1 onward (M0 predates this rule
and was fast-forward-merged directly — a one-time exception, not a precedent). **A regular merge commit is
the default** (settled 2026-08-12, from M4 on): the milestone's sub-commits stay on `main`, so the detailed
history survives even if the PR does not — which already happened once, see the M0–M3 note below. Squash is
still acceptable for a milestone whose intermediate commits genuinely aren't worth keeping, but it is now
the exception; never fast-forward either way. **`main` is tagged at every milestone completion**
(`git tag m12`) — no longer optional.

The merge commit's **subject must be the PR title and its body the milestone summary** — GitHub's defaults
("Merge pull request #N from …") are not acceptable, because they are what turns the first-parent view into
noise.

**A milestone PR's title starts `M<N> — `, and therefore so does the merge commit's subject.** Derived from
`git log main --oneline --first-parent`, where every milestone entry has carried it since M7 and the only
subjects without one are the PRs that were not milestones (the AGPL relicensing, the M72a/M72b planning
PR, the TUI split). The prefix is what makes the first-parent view answer "which milestone was that?"
without opening anything, and it is the half of this convention that an agent drafting a PR keeps
dropping — the rest of the title reads fine on its own, so nothing about the draft looks wrong. The
≤72-character limit counts the prefix: the longest so far is M16's at 66.

Written here because it was enforced by hand three times before it was written anywhere. The title after
the prefix is a description, not a restatement of the milestone name — `M14 — the guild audit log, and
what it records`, not `M14 — Guild audit log`.

Together the merge commit, the PR and the tag are how history stays navigable: **`git log main
--oneline --first-parent`** is the milestone-level view (plain `--oneline` now shows every sub-commit), the
merged PR holds the review discussion, and a diff between two milestone tags (`git diff m4..m12`) jumps
straight to "what changed between these two milestones." That first-parent view reads as one entry per
milestone **from M4 on only** — M0–M3 predate the convention and left loose commits directly on `main`, so
for that range the tags are still the only clean navigation.

**Exception, M0–M3: those PRs no longer exist.** The repository was re-created on 2026-08-11 and the commit
history pushed to a fresh remote, which does not carry pull requests. Commits, tags and content are
byte-for-byte identical (`main` is still `4a621e0`, tags `m0`–`m3` unchanged), but the review discussion and
per-commit breakdown for the first four milestones are gone; the tags are the only navigation for that
range. From M4 on, the PR-plus-tag pairing above applies normally again.

## Milestone status

**Phases B, C and D are complete** (B through M11a, C through M17 with M13a built last, D through M24).
**Phase E is under way, M30 done.** The dependency-ordered roadmap, `M0` through `M125` plus suffixed
insertions, with Phase P (the flagship's Kubernetes track) running in parallel, is `docs/roadmap.md`, which is
also the authority for what each milestone owes. What follows is a summary: the reasoning of a milestone is in
its roadmap entry, its ADRs, `docs/security-ledger.md` and its commits, and this file's history keeps the long
versions of these entries.

**A completed milestone's line says `done` and nothing about its tag.** The tag is always the milestone number
lowercased, and `git tag` states it. Never write `(tag pending)`.

**`M<N>a` means "inserted after `M<N>`"**, so a milestone can go at its dependency position without
renumbering every reference. Letters run `a`, `b`, `c` in insertion order. Fourteen exist: `M11a`, `M13a`,
`M16a`, `M16b`, `M20a`, `M37a`, `M56a`, `M62a`, `M67a`, `M72a`, `M72b`, `M76a`, `M78a`, `M93a`. A list that
names its own members drifts on the next insertion, so check it:
`grep -cE '^- \*\*M[0-9]+[a-z] — ' docs/roadmap.md`. **A suffixed milestone can be retired and its
number is never reused**: `M17a` was folded into M20.

**Releases follow Semantic Versioning, and a milestone tag is not a version** (ADR 0033). `v0.1.0-alpha` at
M20a, `v0.1.0` at the end of Phase D, the next MINOR at the end of each later feature phase through Phase O
(`v0.12.0`); Phase P and M124–M125 move no version. PATCH releases may be cut at any time; other pre-releases
are the author's option. The version stays `0.x` until every milestone is done, and while MAJOR is 0 any MINOR
may break compatibility. `1.0.0` comes last, after a `v1.0.0-rc.N` stage. Self-hosting is open from the first
release, without a support commitment before `1.0.0`; the flagship stays closed to non-developer accounts
until Phase P is ready.

**Tags**: release tags have three components (`v0.1.0-alpha`); release automation fires on `v*` only; `m<N>`
is the milestone marker, and a commit completing a phase carries both. **Tagging is the author's step: an
agent never creates a `v*` tag or bumps a version.**

- **M0 — monorepo scaffolding**: done.
- **M1 — backend skeleton**: done. Config, logging, httpx, database, rate limiting, sqlc, migrations,
  `cmd/server`.
- **M2 — CLI skeleton and `norite instance init`**: done. The `norite` command tree and the wizard.
- **M3 — daemon lifecycle stub**: done. The single-instance lock, the log, `norite daemon` over systemd,
  launchd and the Windows logon task.
- **M4 — backend auth core**: done. Snowflakes, `dbtest`, argon2id, HS256 access tokens, device-scoped refresh
  families, scoped API tokens (ADRs 0022, 0023). The first milestone merged with a merge commit.
- **M5 — transactional email and password reset**: done. `internal/mail`'s non-blocking queue, the always-202
  reset, the first HTML page.
- **M6 — OAuth backend flow**: done. Google and GitHub with PKCE, the client-bound exchange code, the sweeper
  (ADR 0024).
- **M7 — `norite login`, password plus keychain**: done. `daemon/credentials`, the keyring-or-file store,
  `termsafe` (ADR 0025).
- **M8 — CLI OAuth loopback flow**: done. The loopback listener the instance redirects to (ADR 0027).
- **M9 — headless device-code fallback**: done. The verification page, with providers and an explicit approval
  step (ADR 0028).
- **M10 — `norite instance init` finish, and registration hardening**: done. The operator token, bootstrap,
  instance invites, verification mail, anti-enumeration registration (ADR 0029).
- **M11 — session revocation primitive**: done. `revokeEverything`, `logout/all`, the sessions listing (ADR
  0030).
- **M11a — two-factor authentication**: done. TOTP and recovery codes behind the `factorProof` type, on every
  sign-in path and the device page (ADR 0031).
- **M12 — guilds, channels and roles**: done. Seven tables, `roles.Resolve`, the `authorize` chokepoint,
  fifteen routes, `apicontract`; it took the audit table from M14 and `Resolve` from M13.
- **M13 — overwrites, hierarchy, role assignment**: done. Standing in `Resolution`, assignment
  escalation-checked, category overwrites copied at creation.
- **M13a — guild ownership transfer**: done (built after M17). Every owner check decides from the locked guild
  row; the owned-guild ceiling is counted under advisory slot 4.
- **M14 — guild audit log**: done. The read surface, `PermViewAuditLog` (bit 19), `changes` diffs built in
  every writer.
- **M15 — core messaging**: done. `messages`, `guildauth` extracted, bit 20, rule 2 narrowed to administrative
  mutations.
- **M16 — guild reports**: done. `reports`, the reporter never named to a moderator.
- **M16a — message edit history**: done. The history route, with the current version, in one snapshot.
- **M16b — opt-in message audit**: done. `PermViewMessageAudit` (bit 21), the per-guild recording switch,
  `message_audit_entries`.
- **M17 — message tagging**: done. `tags`, private tags invisible as 404, tags carried on every `Message`.
- **M18 — gateway protocol core**: done. HELLO/IDENTIFY/READY, heartbeats, RESUME, dispatch after commit,
  audiences resolved at fan-out, Redis and in-process buses, revocation closes.
- **M19 — daemon as gateway client**: done. The session source, refresh on the server's clock, the gateway
  client, bounded state, the `daemon` → `backend` edge.
- **M20 — daemon↔client local IPC**: done. The attach socket, the relay, forty verbs, `--json` schemas, exit
  codes.
- **M20a — first usable client**: done. The terminal client's home and channel pane, guild invites,
  `norite register`, the first release pipeline.
- **M21 — config file**: done. `daemon/config` with edits by byte range, the split toggle, `norite config`,
  `DAEMON_CONFIG_UPDATE`.
- **M22 — local bot-automation port**: done. The secret-protected port, API tokens forwarded to one instance,
  `messages.type` written by the instance, `AUTO` in both front ends.
- **M23 — daemon lifecycle polish**: done. `norite logs tail`, stop through the socket, the crash file,
  `daemon status --json`, `--linger`, the Windows task as XML.
- **M24 — client auto-update**: done. The signed-release updater (sigstore against an embedded trusted root),
  staging, swap, trial and rollback, `norite update` (ADR 0020 as amended).
- **M25 — mic-permission and global-hotkey spike**: done. A finding, no code: the worker opens the device
  itself, push-to-talk is the daemon's in pure Go (ADR 0034).
- **M26 — Pion SFU core**: done. `backend/internal/voice`: rooms, admission by DTLS fingerprint,
  allocation-free forwarding over slots, the data channel, the active speaker, a bound for everything a peer
  sends (ADR 0023 as amended).
- **M27 — embedded TURN plus STUN**: done. `voice/turn` relaying to the SFU alone, an ICE-lite SFU with a
  remote filter, `voice.StartStack` under `[voice]`.
- **M28 — voice-worker subprocess and IPC**: done. `norite-voice` and its WebRTC connection (no audio until
  M29), `daemon/voiceipc` on two dedicated pipes, the supervisor in `daemon/internal/voiceworker` with its
  `Signaler` seam, `norite voice join|leave|status` (taken from M34), and the worker in every package and the
  updater. Its `/security-sweep` found that the pipe's bounds do not confine an exploited worker, which runs
  as the account, so M29 owes the decision on OS confinement. Its second `/code-review` found that a call must
  end with the session's sign-in and not the gateway's, which a logout made offline never reached.
- **M29 — Opus audio pipeline**: done. libopus in WebAssembly under wazero with no cgo (ADR 0036), capture
  in pure Go on each OS, a jitter buffer per slot, the mix, the RFC 6464 level. Confinement is recorded in
  the ledger, not built. Its manual pass found two things:
  - every talk-spurt started on a stale comfort-noise packet;
  - wazero's arm64 compiler mistranslated the encoder. This is fixed upstream but not released. A `-Os`
    build of that one file works around it, a differential test holds the compiler to the interpreter,
    and CI's `test-arm64` runs both.
- **M30 — Audio DSP**: done. RNNoise, AEC3 and AGC2 in one WebAssembly module that imports nothing (ADR 0036
  as amended), the order in Go, the reference stamped per frame, the settings in `config.toml` and the join.
  Its reviews found a sender too slow for the DSP mute at the far end, and AEC3 resetting under a sender
  behind, which brought the stamp back. Its manual pass, in two VMs, found AGC2 raising the echo
  canceller's residue from its default 15 dB, and help naming keys `norite config` refuses. A real room
  with real devices has not been walked.

## What keeps coming back

The milestones' lessons, kept as rules once their stories were cut: each name says where it was learned, and
that milestone's roadmap entry, commits and ledger entries hold the detail.

**Before building**
- **Planning finds that the entry is not the milestone.** Read the roadmap entry against `architecture.md`,
  the ADRs, the screens and the code before building. Every milestone from M12 to M29 found something wrong or
  missing that way, while every test was green.
- **Read the done-when clause by clause before calling a milestone done**, and ask of each clause what would
  have to break for it to go red (M18, M23). A clause that cannot fail is not a clause.
- **Ask what failure a done-when's test reproduces before building to it** (M27): a relay forced on a client
  that never needed one proved nothing.
- **Build a named library with the release's flags before designing around it** (M25): read its source, not
  its README. A library's default is a decision made for somebody else's deployment (M27).
- **A deferral repeated from a comment is not evidence** (M7, M19): two of M7's three were closed by reading
  the code they described.

**Proving it**
- **Prove a guard by removing it**: watch a named test fail, put it back, and check that the mutation landed.
  A mutation that does not compile, matches nothing, breaks the statement around it, or passes for another
  reason proves nothing (M14, M15, M17). Run mutations with `-count=1`, in a process group a timeout kills
  whole (M28). A mutation that survives is a finding about the tests (M23).
- **A guard held twice needs three runs to prove**: each alone, then both (M16b, M17).
- **A test can pass for a reason other than its name**: a deadline that closes the connection anyway, actors
  that overlap, a host that serves no archive (M17, M24, M27), a benchmark whose every call failed (M29).
- **A test's silence is not a device's** (M29). Digital silence hid the comfort noise that real quiet
  makes DTX send, and every talk-spurt played 440 ms late. Feed a test the signal the device delivers.
- **A test of what is sent is not a test of what is heard** (M30). A sender too slow for real time encoded
  half its frames, and its test counted encodes, while the far end played silence for ten seconds. Assert
  at the far end.
- **A simplification no test can see is not proof it is safe** (M30). The reference's stamp was removed
  because removing it failed no test, and no test held a sender behind for a second, which is when AEC3
  resets without it. Ask what regime would need the thing before deleting it.
- **Code compiled for the host is only as right as the compiler for each architecture** (M29). Hold it to
  a reference byte for byte, on every architecture a release ships.
- **A stand-in must fail the way the real thing does**: a proxy that forwards the FIN, sequence numbers that
  jump, a seed whose ids cluster or whose distributions share a modulus (M16, M19, M26), a room whose
  delay never moves (M30). A schema-checked fake cannot see wording or a screen, and a test that feeds a
  parser raw output has not tested what the runner hands it (M19, M20a, M24).
- **Test the configuration the defaults produce** and the one the documentation tells an operator to write, on
  a machine with more than one interface (M27, M28). Run `-race` and vet every new test file for every OS it
  claims to run on (M27, M28).
- **When the question is "which program", the test needs two programs** (M25). A setting's storage is not the
  setting: change it the way a person does (M25).
- **A race is staged, never raced**: hold one side with a hook, a gated bus or a held transaction, and wait
  for the other to block. A race the test cannot provoke is pinned by asserting the property instead (M13a,
  M15, M18).
- **Assert whole messages, not `Contains`** (M13a): a sentinel's text leaked into every refusal while every
  test passed. `TestRefusalsCarryOnlyTheirOwnMessage` holds it.
- **A test that waits on a channel the guard was meant to close hangs instead of failing**: bound every wait a
  removed guard could leave open (M28).
- **Gates in a chain need `&&`, not `;`** (M18). Hold a cost by counting, not by a wall-clock limit (M21).
- **A job inserted among another job's steps takes the rest of them** (M29). YAML says nothing, and the
  steps it took stopped running where they belonged. Read a workflow's job list back after editing it.

**Designing it**
- **When a value that never changed starts changing, enumerate every reader of it**; when an event starts
  mattering, every writer (M13a, M18). When one method needs a cutoff or a lock, ask which of its siblings
  race the same way (M26).
- **Refuse on unlocked data, then lock**, and take the weakest lock that orders what must be ordered
  (`FOR NO KEY UPDATE`, M13a). A fix that adds a lock is a new place for that bug.
  `TestARefusedCallerTakesNoLockOnTheGuild` walks every refusal with the row held.
- **Rule 1 and read consistency are different properties**: two reads that must agree share one
  `REPEATABLE READ` read-only snapshot (M16a).
- **Lifting a requirement and lifting a refusal are two changes** that look like one (M16a). A default with no
  documented exit gets routed around, so give it one in the chokepoint, where a grep finds every use:
  `AuthorizeChannelIgnoringVisibility` (M16a).
- **When a rule says what a check must not key on, ask separately what it must** (M18).
- **Two surfaces for one object must agree, and the contract says which is right** (M18).
- **Whatever a stranger's server, a peer or a worker sends is bounded where it is kept**, not only where it is
  read, and against a hostile sender (M19, M24). Measure a bound against the live shape (M24) and as a cost
  (M21). A ceiling that follows another is bounded with it (M20a). A bound on what a peer declares is not a
  bound on what it can make the server hold (M26).
- **A guard measured against a wrapping number stops guarding at the wrap** (M26). A reason cut to a bound
  must cut on a character boundary, or its encoding exceeds the bound (M28).
- **An identifier minted after the thing that needs it cannot scope it** (M27). The fix that removes the
  sending beats a filter on where it goes (M27).
- **Assign a `time.AfterFunc` result under the lock its callback takes** (M27).
- **`os/exec` gives a child the parent's whole environment by default** (M28).
- **A fix for one platform's cosmetics is a change to how that platform stops the program** (M23).
- **When a fix narrows a rule, ask what the narrower rule cannot say**, and make it say so in the output
  (M21).
- **Measure the off path before designing the on path** (M16b), what grows with something other than the
  audience (M18), and plans under the generic plan pgx prepares, on skewed data (M13a,
  `TestMemberRoleReadsCannotScanTheWholeGuild`).
- **A message describes the outcome**: write it after the last step that can fail, from what happened, and
  read every sentence a person will see against the real program (M19, M22, M28).
- **Anything written for a platform nobody ran says so**, in the entry, the guide, the commit and the code
  (M23).

**Reviewing and testing by hand**
- **Run the review again after fixing what it found**: a round's fixes are code nobody has reviewed (M22,
  M24). `/code-review`, `/security-sweep` and `/optimization-review` ask different questions, and each found
  what the others walked past (M15, M19).
- **The manual pass finds what reads wrong**: wording and screens, which no review and no schema-checked fake
  sees (M19, M20a, M22).
- **A manual pass that needs an init system, a reboot, a login session or another OS runs in an unprivileged
  VM**, or on a machine the author names, and only after asking. Never a privileged container: one ended the
  author's desktop session at M23. Check the hash of what a pass runs (M26), and where a virtual device is
  wired rather than what it was asked to be wired to (M25).

## Conventions, by area

What exists on the backend today, and the conventions the next milestone should follow rather than
re-derive:

- **Startup order** (`cmd/server/main.go`): config → pool (verified with a real round trip) → HTTP listener
  → blocking advisory-lock-guarded migration → *then* readiness. `/api/v1/healthz` answers 503
  `{"status":"starting"}` until migrations finish, then 200. `-migrate-only` runs migrations and exits —
  that's what `just db-migrate` and, later, the flagship's Helm pre-upgrade Job use.
- **Middleware chain** (`cmd/server/router.go`), fixed by `docs/architecture.md` §2:
  SanitizeInboundRequestID → RequestID → EchoRequestID → RealIP *(mounted only when
  `NORITE_TRUST_PROXY_HEADERS=true`)* → Recoverer → SecureHeaders → logging.RequestLogger → RateLimit →
  refuseWhileStarting.
  `AuthenticateBearer` slots in below RateLimit at M4. SanitizeInboundRequestID and RealIP are one decision
  made twice — whether a client-supplied forwarded header may be believed — and both take it from that one
  setting, at the top, where nothing below re-opens it. A request ID is not cosmetic: it is echoed to the
  client, written to every log line, and returned in every error body. Domain routers mount in the
  rate-limited group inside `/api/v1`; `/healthz` sits outside it on purpose.
- **Errors**: return `httpx.ErrNotFound` / `ErrForbidden` / … (or `httpx.Errorf(sentinel, …)`) from
  services and let `httpx.WriteError` map them. 5xx detail is logged, never returned — a 5xx message is
  replaced by a generic string unless its `StatusError` sets `MessageIsPublic`, which is how the two
  deliberate `503`s (no mail relay, no public base URL) say *which* feature is off instead of claiming the
  instance is broken. Set it only on a literal you wrote; the detail belongs in `Err`. Every response
  carries `X-Request-Id`; every error body carries `request_id`.
- **A new error `code` goes in `contracts/openapi.yaml`'s `Error` enum in the same commit** (rule 6), and a
  new field on a response goes in that response's schema. `cmd/server/contract_test.go` checks the *route
  set* only, so payload drift is invisible to it — `contract_payload_test.go` is the half that reads real
  responses, and it exists because eight emitted codes and one unsatisfiable schema had accumulated behind
  that gap.
- **Validation messages name the wire field**, because `NewHandler` registers a `json`-tag name func on the
  validator. Without it the message quotes the Go field (`DeviceID`), which appears in no contract.
- **Transactions**: `database.RunInTx`. Mutation + audit-log write go in one `fn` (rule 2). A gateway event
  is queued *inside* `fn` with `dispatch.Publisher.Queue`, which registers it with `database.AfterCommit`, so
  it is published once the commit succeeds and never on a rollback (rule 5). Since M18, not after `RunInTx`
  returns: that was the convention, and it could not reach the five revocations deep inside helpers that
  never see the commit. `AfterCommit` outside a transaction panics, on purpose.
- **Rate limiting**: always build limiters through `internal/platform/ratelimit` — it is what enforces the
  global "IPv6 groups by /64" rule (rule-adjacent, `docs/architecture.md` §11). Give a new stricter limit
  its own `Bucket`.
- **sqlc**: add `.sql` files under `backend/internal/db/queries/`, run `just sqlc-generate`, commit the
  output. CI's `codegen` job fails if the committed output is stale.
- **Logging**: `logging.FromContext(ctx)` inside handlers. The request logger deliberately omits query
  strings, headers, and bodies (rule 8) — don't add them back.
- **Toolchain pinning: seven versions move together, and two of them have no pin to read.**
  - `golangci-lint` (config in `.golangci.yml`) and `sqlc`: exact versions in `ci.yml`'s `env:` block and in
    the justfile. `just lint` warns when the local golangci-lint differs.
  - `goreleaser`: in the justfile, `ci.yml` and `release.yml`, with `release.yml`'s `COSIGN_VERSION` and the
    cosign-installer tag beside it. Never `latest`, or an upstream release is first tested by the run that
    publishes.
  - `go-licenses`: `go_licenses_version` / `GO_LICENSES_VERSION`, at its `/v2` module path. It decides from
    the code in front of it (v1 reports one licence per `LICENSE` file, v2 every one), so its version changes
    the result.
  - `docker/docker-compose.yml`'s `golang:` image must keep up with the workspace's highest `go` directive
    (**1.26.0**), or `just dev` cannot build the backend. CI never runs compose, so nothing else notices.
  - **The Go a source-processing tool was built with** must be at least the toolchain's. `golangci-lint` and
    `govulncheck` built by an older Go cannot parse a newer standard library, and fail every module.
    `go version -m $(which golangci-lint)` is the only way to read it.
  - **`govulncheck` is deliberately unpinned**: it answers from a database fetched at run time, so a new
    advisory failing an unrelated PR is the product, and pinning the binary would not prevent it. After a
    toolchain upgrade, reinstall it with `go install golang.org/x/vuln/cmd/govulncheck@latest`.
- **Every generator is invoked as `go run …@{{version}}`** (`sqlc`, `oapi-codegen`, `go-licenses`,
  `goreleaser`), never from a binary on `PATH`, which a distro package can shadow with another version: that
  once made `just license-inventory` produce a different inventory from CI's. `golangci-lint` stays a real
  binary because it is too slow under `go run`, which is why it is the one that warns on a mismatch.
- **CI runs six jobs**: `lint`, `test`, `test-arm64`, `codegen`, `security` and `build`. `security` is
  `govulncheck` per module, the command `just security-scan` runs, and it is blocking. `test-arm64` runs the
  voice-worker's tests on an arm64 runner, because its codec is WebAssembly compiled to the host's
  instructions and is only as correct as wazero's compiler for that architecture (M29).

And on the CLI side, from M2:

- **Command tree** lives in `cli/internal/cliapp`, never in `cmd/app` — that keeps it constructible in a
  test without spawning a process, which is how help output and flag plumbing are covered. `cmd/app/main.go`
  owns process lifetime and nothing else. Mount a new command group by adding it to `New`'s `Commands`.
- **A mistyped command must exit non-zero.** urfave/cli's default prints help and returns nil, so the root
  `Action` explicitly errors on an unconsumed argument (with a "did you mean" via `cli.SuggestCommand`).
  Don't remove it — `norite instnace init && echo ok` printing "ok" is exactly the silent success a scriptable
  CLI must never produce.
- **Terminal output goes through `prompter.printf`/`println`**, not bare `fmt.Fprintf` — the ignored write
  error is justified once, in one place, rather than at every call site (and errcheck enforces this).
- **Anything interactive must degrade**: check `term.IsTerminal` and fail with an actionable message rather
  than blocking on input that will never arrive, or reading EOF and silently accepting every default.
  **Return `cli/internal/clierr`'s `ErrNoTerminal`** — wrapped, so the message can name the flag or
  environment variable that would have answered — and never a new sentinel of your own. `main` matches that
  one value and maps it to exit code 2, printed without the `norite: ` prefix that makes a usage problem
  read like a crash. Three packages declared their own before this was one value, `main` matched two of
  them by name, and `norite instance bootstrap` shipped exiting 1 with the prefix.
- **`main` adds the `norite: ` prefix, so a message must not carry one.** A `cli.Exit(...)` string that
  includes it prints as `norite: norite: …`. `cliapp`'s `TestUsageMessagesDoNotCarryThePrefixMainAdds`
  walks the tree and catches it.
- **Never echo a secret.** Passwords are read with `term.ReadPassword` and summaries print
  `url.URL.Redacted()`, never the raw DSN (rule 8 applies to the CLI too). Prefer an env-var source over a
  flag for any credential — a flag value is visible in the process list to every user on the machine.
- **Instance config**: TOML, discovery/precedence per `docs/architecture.md` §4 (**environment overrides
  file overrides default**). `contracts/instance-config.toml` is the source of truth listing every key: the
  backend tests that it loads, the CLI tests that the wizard writes nothing outside it. Adding a setting
  means touching that file, `backend/internal/config` (struct field, `envVarFor`, `fileKeyFor`), the
  wizard's template, and `.env.example` — the tests fail if you miss one.
- **`main` owns exit codes, not urfave/cli.** `cliapp.New` sets a no-op `ExitErrHandler` because the
  library's default prints the error and calls `os.Exit` from *inside* `Run` — which would bypass
  `cmd/app/main.go` and make any command that reports through an exit code untestable. Commands still
  return `cli.Exit(msg, code)`; `main` unwraps the `cli.ExitCoder` and exits. Don't remove that handler.

And on the daemon side, from M3:

- **The service is always user-scoped.** systemd *user* unit, launchd *agent*, Windows logon task at
  `/RL LIMITED` — never a system service. Installing must never need elevation, and the daemon must run as
  the account whose tokens it holds. New platform backends follow the same rule.
- **Every external tool call goes through `daemonctl.Runner`.** That interface is what makes all three
  platform backends assertable from one CI machine — a launchd command line is otherwise only ever tested by
  the first macOS user. A non-zero exit is data, not an error (`systemctl is-active` answers with one); use
  `mustSucceed` when it genuinely is a failure, so the error quotes the command and the tool's own output.
- **Exit codes are load-bearing.** Daemon: 0 for a signal-initiated stop (a non-zero would make every
  service manager treat an ordinary stop as a crash and restart it), 3 for "already running", and since M20
  4 for a configuration no restart fixes (an attach socket it cannot open) — both of which the systemd unit
  is told never to retry (`RestartPreventExitStatus=3 4`; launchd has no per-code equivalent and is
  throttled instead). `norite daemon status`: 0 running, 1 stopped, 2 not installed; since M23 "running"
  is a daemon answering or a service the manager calls running, so one started by hand is 0. Changing
  any of these means changing the unit/plist template in the same commit.
- **Report platform differences, don't paper over them.** launchd starts the agent as part of installing it
  and cannot do otherwise, so `Manager.StartsOnInstall()` exists and `install` prints what actually
  happened — rather than the alternative of starting and immediately stopping the daemon to fake parity
  with systemd. When a platform genuinely differs, surface it through the interface and say so in the
  output; a message that is wrong on one of three platforms is worse than three honest messages.
- **Every backend method must be idempotent**, as `Manager` promises: install replaces, start on a running
  daemon succeeds, stop on a stopped one succeeds. The last is easy to get wrong — `launchctl kill` and
  `schtasks /End` both fail on a stopped service — and it matters because `restart` propagates a stop
  failure, so a non-idempotent stop breaks the command people use to recover a crashed daemon.
- **One daemon per OS user, enforced with `gofrs/flock`** on `<state-dir>/daemon.lock`, taken before
  anything else is opened. Never a PID file — it goes stale on a crash and lies after PID reuse. The lock
  stays in the state directory even when the log is redirected elsewhere, and `install` captures
  `XDG_STATE_HOME` into the systemd unit — otherwise the service and a shell-started daemon resolve
  different state directories, take different locks, and both run.
- **Daemon-owned state lives in `daemon/internal/paths`**, `0700`, per-user, never a system-wide path. It
  will hold plugin capability grants and pinned `.wasm` hashes, so treat the mode as a security boundary.

And on the auth side, from M4:

- **Scopes only ever restrict, never grant.** A scope bounds a *delegated* credential below its owner's
  reach; permission resolution still runs on top (rule 1). A user actor passes every scope check by design.
  New scopes are added to `auth.AllScopes` — never invented at a call site, where a typo widens access.
- **Token management is not delegable.** Minting, listing and revoking `api_tokens` require a user actor and
  carry no scope. A credential that can create credentials can escalate itself.
- **Every credential is stored only as a hash — with one stated exception.** Passwords argon2id, opaque
  tokens SHA-256. The exception is M11a's TOTP secret, which verification needs back and which is
  therefore sealed (AES-256-GCM) rather than hashed; it is the only one, and adding a second is a
  decision to argue for, not a precedent to follow. The raw value of
  a refresh or API token exists exactly once, in the response that issued it. Token hashes are unsalted
  deliberately — they are 256-bit random values with nothing to brute-force, and a salt would make the
  indexed lookup that authenticates every request impossible.
- **Credential failures are indistinguishable to the client.** Unknown account, wrong password, and
  OAuth-only account all return the same 401; refresh replay returns the same answer as an unknown token. The
  login path runs argon2id against a dummy hash when no account matched, so timing does not enumerate
  accounts either — don't add an early return that skips it.
- **Refresh rotation is scoped to one `device_id`.** Reuse detection revokes that device's family and never
  another's. `replaced_by_id` is what distinguishes replay (rotation set it) from deliberate revocation
  (logout or a superseding login did not) — conflating the two logs users out of sessions they just created.
- **argon2id runs behind a concurrency gate.** Each hash holds 64 MiB; a distributed login flood would
  otherwise exhaust memory long before any per-IP limit noticed. Any new call site goes through
  `HashPassword`/`VerifyPassword`, which take a context for exactly this reason.
- **The access token never leaves the backend's trust boundary.** It is not presented to the media server or
  the voice-worker (ADR 0023), which is what keeps HS256 correct (ADR 0022). An external verifier appearing
  is a trigger to revisit the algorithm, not to distribute the key.

And on the mail and password-reset side, from M5:

- **Sending never blocks a response.** `internal/mail`'s `Enqueue` cannot block by construction — a full
  queue drops and reports it. That is not only an availability property: it is what makes the reset
  endpoint's always-202 honest, since sending inline would make a registered address take an SMTP
  transaction longer than an unknown one and leak through timing whatever the body said. Any future sender
  goes through the same queue for the same reason.
- **The queue is in memory and delivery is best-effort**, deliberately (§15.7). A message still queued at
  shutdown is drained if it can be, dropped if it cannot, and nothing survives a crash. The upgrade path is
  a Postgres outbox drained by the same worker loop; `Enqueue`'s signature does not change.
- **SMTP is an opt-out, not a requirement.** With no relay the queue reports itself disabled, reset answers
  503 `reset_unavailable`, and everything else works. A disabled queue is still a real object, so nothing
  nil-checks a dependency.
- **STARTTLS is mandatory, never opportunistic.** Opportunistic falls back to plaintext when the server
  says it cannot upgrade, which is indistinguishable from an attacker stripping the capability — so
  "encrypted" would silently ship the relay credential in the clear.
- **Reset guards live in SQL.** Single-use is `ConsumePasswordResetToken`'s `WHERE`, so concurrent confirms
  produce exactly one winner; requesting again spends the earlier token; a token whose account changed
  email is refused. None of it depends on a Go-side check being remembered.
- **A reset revokes sessions *and* API tokens.** `RevokeAllSessionsForUser` is the narrow ancestor of M11's
  general-purpose primitive (rule 17) — M11 widens it to live gateway connections and E2E device trust.
- **`nrp_` is deliberately absent from `LooksLikeOpaqueToken`.** A reset token authenticates exactly one
  endpoint; routing it to the Bearer verifier would be the first step toward it authenticating anything
  else.
- **The reset page is the first HTML this backend serves**, and the API's `default-src 'none'; form-action
  'none'` would render it and then forbid its own form from submitting. `httpx.HTMLPage` overrides that
  per-route — nonce-scoped style, same-origin form post, scripts still denied. Never loosen the global
  policy; M9's device-code page reuses this seam. A page mounted at the root also sits outside the
  `/auth` group, so it needs the stricter rate-limit bucket applied explicitly.
- **`public_base_url` is configured, never derived.** Behind a proxy the Host header is whatever the proxy
  sends, so a link built from it points wherever a request was aimed. Required once SMTP is on.
- **`just dev` ships a real relay.** Mailpit is in the compose stack; its web UI on `localhost:8025` is
  where a reset email lands locally.

And on the OAuth side, from M6 (decisions in ADR 0024):

- **A provider is trusted for exactly one thing**: that whoever completed the flow controls the account
  named by `ProviderUserID`. Everything else it reports is a claim. `EmailVerified` is carried as its own
  field and never inferred — a provider that does not say an address is verified is treated as not having
  said so.
- **An unverified address reaches no account, existing or new.** Both refusals are one sentinel,
  `ErrOAuthEmailUnverified`, with one message carrying both routes forward. Two messages is the obvious
  design and reports whether an address is registered to anyone who can present it unverified at a
  provider — which GitHub permits for any address. Only the log distinguishes the cases. Necessary but not
  sufficient: `POST /auth/register` still answers 409 on a taken address, so the instance stays enumerable
  by a cheaper route until M10 gives it a way to verify addresses itself.
- **An identity is keyed by the provider's user ID, never the email.** An address can be reassigned; the ID
  cannot. After linking, the address is never consulted again.
- **Nothing is written to `users` until a username is chosen.** The continuation token is signed rather
  than stored — the one short-lived value in `internal/auth` that is not a row — because replaying it
  cannot create a second account: `oauth_identities`' unique constraint refuses it, so single-use falls out
  of the schema. Do not add a pending-account row; that cost lands on every future query.
- **The `typ` claim is what separates token purposes.** An access token must never be spendable as a
  signup and vice versa; both directions have a test. `TokenIssuer.sign` and `keyFunc` are shared so the
  `alg` pin exists once, not once per token type.
- **The callback never returns tokens.** It renders a page carrying a single-use exchange code; a redirect
  with a token pair would put credentials in a URL, history, `Referer`, and every proxy log. `device_id`
  arrives at `/auth/oauth/exchange`, from the client that will actually hold the session.
- **PKCE's verifier lives server-side** (`oauth_states`). Putting it in the `state` parameter — the obvious
  stateless design — sends it through the browser and the provider, which is exactly what PKCE prevents.
- **A second verifier binds the client, and it is mandatory.** `/authorize` takes a `flow_challenge` and the
  exchange takes the matching `flow_verifier` — PKCE's construction applied to the client↔Norite hop, which
  `state` does not cover: `state` proves this server issued the request, not that this client made it.
  Without it any browser could complete any outstanding flow, and the code that came back would sign
  whoever opened the link into whichever account consented. Every new consumer of the OAuth endpoints mints
  a verifier first; opening `/authorize` in a browser is not a sign-in path and is not meant to be.
- **Provider errors are redacted before they reach a string.** `x/oauth2`'s `RetrieveError` renders the
  response body, and a misconfigured endpoint that echoes the request would put the client secret in a log
  (rule 8). Only the provider's own error code survives.
- **Both OAuth pages reuse `httpx.HTMLPage`** and the shared `pageStyle`. The callback renders HTML from
  inside `/api/v1`, so it takes the CSP override per-route via `.With()`.
- **Short-lived rows are swept by `auth.RunSweeper`**, started from `cmd/server/main.go` after readiness
  and stopped with the process. Nothing in the roadmap ever swept anything — four comments pointed at
  "M11's cleanup job", and M11 is the session-revocation primitive — so reset tokens, OAuth states and
  exchange codes grew for the life of the instance, two of the three written by unauthenticated endpoints.
  A new table with a TTL adds its delete to `SweepExpired`, and **ships a non-partial index on the column
  the sweep filters by**: a partial index predicated on "not yet consumed" cannot serve a sweep that
  deletes regardless, which is the mistake made on all three of these tables and corrected in `000005`.

And on session revocation, from M11 (decisions in ADR 0030):

- **There is one primitive, `auth.revokeEverything`, and new claims are added to it rather than to a
  caller** (rule 17). It revokes sessions, API tokens, outstanding OAuth exchange codes and approved device
  authorizations, in whatever transaction the caller is already running. That list is not a design; it is
  four milestones each noticing one more outstanding claim the previous ones missed (M4, M5, M6, M9), which
  is exactly why it must not be reassembled per caller.
- **Its two unbuilt steps live inside it as named gaps**, not behind interfaces nothing implements:
  force-closing live gateway connections (M18) and dropping linked-device E2E trust (M101). Two seams whose
  shapes are guesses would be two wrong shapes; one function is one place to add a line. Both milestones'
  roadmap entries now say so.
- **A session is a *device* to a person and a rotating row to the schema.** Every refresh inserts a
  successor and revokes its predecessor, so `GET /users/@me/sessions` collapses each family to its newest
  live record and `DELETE …/{id}` revokes the family. `first_seen` is the family's start; the newest row's
  `created_at` is merely the last rotation. Never expose `device_id` in a path — it is client-chosen text
  that would land in every request log line (M10's invite-code reasoning).
- **The current device must be resolvable from a *revoked* row.** An access token names the session it was
  minted from and lives fifteen minutes; a refresh inside that window revokes that row while the token
  stays valid. `GetSessionByID` returns revoked rows on purpose. Filter them and `logout/all` finds no
  current device, spares nothing, and signs the caller out of itself — the milestone's easiest bug, and it
  has a test that fails without the fix.
- **Session management needs a user actor, never an API token.** The listing enumerates every machine the
  account is signed in on, and the revoke takes the account's other API tokens with it, so a delegated
  credential holding either could lock its owner out. Same rule minting obeys.
- **Access tokens stay stateless and the fifteen-minute residual window is accepted** (§17.10) —
  **except where a signed-out credential would change the account's security state**, which
  `RequireLiveSession` refuses: revoking sessions; minting, listing or revoking API tokens; since M11a,
  enrolling, confirming, disabling or regenerating the second factor; and since M13a, transferring a
  guild's ownership — the one guild route on the list, because it hands layer 2 to somebody and no
  later sign-in takes it back. Written
  first as per-handler checks, it missed `POST /auth/tokens` — and an API token is not session-scoped, so
  one minted inside the window outlives the sign-out for good. **Guards belong in middleware for the same
  reason the revocation list belongs in one function**: a rule written as N call sites has N chances to
  miss one, and the one it missed was the one that mints a durable credential. Liveness is asked of the
  *device*, never the row, because a rotated session is live and its row is revoked.
- **A value a sweep can delete must not be derived from the rows it deletes.** `first_seen` was
  `min(created_at)` over the session family until `000012` started deleting expired rows, which silently
  capped it at the refresh TTL — a device used for a year reporting a rolling month. `000013` carries it
  forward on rotation. The two halves shipped in adjacent parts of one milestone and neither's tests could
  see the other.
- **The sweep deletes by expiry, never by revocation.** A revoked row is the evidence that tells *replay*
  apart from an unknown token, which is what reuse detection revokes a device family on. Past `expires_at`
  nothing can be presented, so there is nothing left to prove.
- **Check every new foreign key for an index, and every sweep index for a partial predicate.** Both bit
  here. `replaced_by_id` was an unindexed `ON DELETE SET NULL` self-reference: deleting 2,000 rows spent
  3,757 ms in its trigger against 9.4 ms in the DELETE. The `expires_at` index was partial on
  `revoked_at IS NULL`, which is 000005's mistake on the one table where the excluded rows are the majority.
  Neither is catchable by a behavior test — a partial index produces a scan, not wrong rows — so
  `sweeper_test.go` asserts the index *shapes*, across every sweep table.

And on the second factor, from M11a (decisions in ADR 0031):

- **The gate is a type, not a check.** `factorProof` is unexported, has unexported fields, and is built
  only by `factorSatisfied` and `proveFactor`. **Two** functions require one — `startSession` *and*
  `issueDeviceApprovalToken` — and both refuse a zero value or one naming another account. The second is
  easy to drop from the summary and is the whole reason the device page is covered: that page never calls
  `startSession`, because approving is a separate request from authenticating, so the gate sits where the
  approval token is minted instead. That is the third time this package has had to make a rule structural —
  after `revokeEverything` and `RequireLiveSession` — and the first time the compiler found the call sites
  instead of a reviewer: adding the parameter located the OAuth exchange and the device page immediately.

  **Two limits, because "cannot forget" decays into folklore otherwise.** The type is package-scoped, so
  any file in `internal/auth` can write the literal — what the unexported fields buy is that nothing
  outside the package can, and that every construction is greppable. And `startSession` is not the only
  session mint: `RedeemDeviceCode` and `Refresh` reach `writeSession`/`issuePair` directly, taking no
  proof. Both are deliberate, but a new sign-in path copying either shape compiles with no factor question
  asked.
- **The factor is asked about strictly after `verifyCredentials` succeeds**, which is what keeps every
  failure path byte-identical to an instance with no factor anywhere. A challenge returned earlier, or a
  different answer for an account that has one, is a fresh account-existence oracle on top of the one M10
  spent a milestone closing. The roadmap's original done-when asked for something stronger and
  unachievable — a 202 and a 200 are visibly different — and was corrected rather than quietly satisfied.
- **A sign-in that owes a factor answers 202 with a challenge**, on `/auth/login` and
  `/auth/oauth/exchange` alike. A distinct status rather than a discriminated body, so a client branches on
  status and oapi-codegen gets one response object per status.
- **`RedeemDeviceCode` is deliberately ungated**, and the device flow's factor is enforced where the
  *approval token* is minted. Approving is a separate request from authenticating, so a proof cannot cross
  that boundary; what crosses is a token whose meaning is "this browser finished proving who it is", and it
  cannot be minted without a proof. The waiting CLI has nobody at it to type six digits.
- **The device page gained a third continuation.** ADR 0028 argued for two because a token with an optional
  user field authorizes before authentication has happened; a browser that has typed a correct password on
  a 2FA account is at neither existing point. `parseDeviceToken` extracts a subject for the two types that
  name an account and not for the entry token — forgetting the new one meant no code could ever pass, which
  the device-flow test caught.
- **The TOTP secret is the only credential here that is encrypted rather than hashed**, because
  verification needs it back. AES-256-GCM under a key derived from the instance signing key by stdlib HKDF.
  Two consequences are written down: a database compromise that also yields `instance.toml` yields every
  secret, and rotating the signing key is a re-enrollment event.
- **Disabling and regenerating require the factor**, not merely a live session — an access token outlives
  its session by up to fifteen minutes (§17.10), and without the step-up a stolen session could remove the
  control standing between an intruder and the account. Disabling revokes the rest through
  `revokeEverything` (rule 17).
- **A password reset does not bypass it**, because `ConfirmPasswordReset` starts no session. Closed by
  shape rather than by a check, and given a test anyway — "closed by construction" stops being true
  quietly.
- **An unconfirmed enrollment is not a factor.** That is what stops a closed tab or a phone with a wrong
  clock locking somebody out, and it is why `GetTOTPForUser` returns unconfirmed rows rather than hiding
  them.
- **Recovery codes are hashed in a `bytea` column**, like every other hash in this schema. It was `text`
  on the first draft and confirming an enrollment answered 500, because a SHA-256 digest is not valid
  UTF-8.

And on the client-auth side, from M7:

- **The credential format lives in `daemon/credentials`, and the CLI imports it** — the repository's first
  cross-module dependency (`cli` → `daemon`, relative `replace`). It follows from ADR 0011: the daemon is
  the sole holder of its account's tokens, so the daemon module owns what a stored credential is. Two
  implementations of one on-disk shape drift, and the failure mode is a login that appears to work and a
  daemon that cannot find it.
- **The keyring is not assumed to exist.** A headless Linux box has no Secret Service, which is exactly
  where this CLI is meant to run, so storage falls back to a `0600` file in the `0700` state directory
  (ADR 0025). The backend is chosen by *writing* a probe entry, never by reading one — a read of a missing
  entry looks identical on a working keyring and a broken one. Never make the fallback silent.
- **Only the refresh token is persisted.** An access token lives 15 minutes, shorter than the gap between
  the restarts persistence would let it survive. The non-secret record beside it is a separate file so
  `LoadRecord` can answer "who is logged in" without opening a keyring, which on a locked one pops a system
  dialog.
- **A `device_id` is per installation, not per login, and a logout keeps it.** Regenerating it strands the
  previous refresh family until it expires and adds a session-list entry each time; rotating it is what
  reuse detection reads as theft. It lives in its own file rather than in the credential record, because
  `Clear` removes the record — and logging out revokes nothing on the instance, so a fresh ID would leave
  the old family live for its full TTL beside a duplicate session. `Store.DeviceID()` is the only way to
  obtain one; it mints on first use and adopts the ID of a record written before that file existed.
- **Untrusted text goes through `daemon/termsafe` before it reaches a terminal** (rule 19) — in the daemon
  module since M19, outside `internal/` so the CLI imports it, and `cli/internal/termsafe` before that. `Text`
  for a value printed inside a line — it removes newlines too, since a one-line value that can contain one
  can forge a line of output — and `Block` for output meant to span lines. Sanitize where foreign text
  *enters* the program, as the API client and `daemonctl.Runner` do, so the value is safe wherever it goes
  afterwards, including into a file; anything read back out of a file a person can edit is foreign again.
  M7 is where this started to bite: `--instance` is a URL somebody hands you, and its answers are printed.
- **The sanitizer removes what acts on a terminal or reorders it, and nothing else** — category `Cc` plus
  the bidi embeddings/overrides/isolates, each removed run leaving one `U+FFFD` so that a name with an
  override never renders as the name it imitates. It deliberately keeps invisible-but-inert characters:
  the `Cf` category that holds zero-width spaces also holds parts of written Arabic and the joiners Persian,
  Indic and composed emoji need, so filtering by "invisible" corrupts real text. Don't widen it to
  categories; widen the *renderer* if legibility is the problem (`docs/architecture.md` §4).
- **A value that also bounds something is rejected, not sanitized.** `ParseInstanceURL` refuses anything
  unprintable because that string becomes a filename and a request target — altering it silently would
  point a password at a host nobody typed, where refusing costs one retry.
- **Never take a password from a flag.** A flag value is in the process list and the shell history —
  `NORITE_PASSWORD` is the scripted path, matching the wizard's rule for the database password. Read
  interactively with `term.ReadPassword`, and refuse an empty answer locally rather than letting the
  instance's deliberately vague 401 read as "wrong password".
- **A daemon with no session is still a daemon.** No credential, an unreachable instance, and a refused
  token are all logged and survived — refusing to start would mean the daemon cannot be installed before
  its first login, and `norite daemon install` deliberately runs first.
- **A refresh writes back with `ReplaceToken`, never `Save`.** The record is read before a network round
  trip and written after, so a `norite login` or `norite logout` landing in that window already owns the
  store; `Save` would take the stale record as truth and delete the token the login just stored. The lock
  makes each operation atomic and says nothing about a read-modify-write spanning two of them.
- **The record names the backend its secret is in** (`Record.SecretBackend`), and reads and deletes go
  there rather than to whatever this process's probe picks. The probe answers "where would a new secret go
  here", which is not evidence about where an existing one is — a desktop login reaches the keyring, a
  systemd user unit starting before it unlocks does not, and an SSH session has no session bus at all.
- **A 2xx is not one answer.** `apiclient.Do` treated every 2xx as success, which was fine until M11a made
  `POST /auth/login` answer 202 with a second-factor challenge where it answers 200 with a pair — and the
  challenge decoded into an empty `tokenPair`, reported as "the instance returned an incomplete token
  pair", i.e. *this is not a Norite API*. `DoStatus` exists so a caller can branch on the status the
  contract makes load-bearing. Any endpoint the contract gives two success statuses needs it; a body-shape
  guess is the thing to avoid, because the two bodies here share `expires_at` and a struct embedding both
  silently decodes neither (encoding/json drops fields that conflict at equal depth).
- **The factor is answered in one place**, `Run`, not once per sign-in flow — the same reasoning that makes
  the backend's `factorProof` a value rather than a check. Both browser-less flows can come back owing a
  factor; the device-code flow cannot, because the browser already proved it. There is **no environment
  variable for the code**, deliberately, where `NORITE_PASSWORD` exists: a static second factor is a
  password wearing a hat, and `--device-code` is the honest answer for an unattended machine.
- **What the store cannot finish, it says through `Notify`** — a credential left in a backend this session
  cannot reach, a previous record too broken to name one. Failing instead would make `norite login`
  impossible over SSH on any machine that once logged in at its desktop; staying silent would hide a live
  token from the only person who can deal with it. The CLI prints it, the daemon logs it.

Three things M7 deferred are closed: revoking a dropped refresh token (M11, which `POST /auth/logout` had
done since M4), a daemon started before its keyring unlocks (M19, by retrying the read), and `termsafe` in
the daemon (M19, moved rather than copied). Two of the three deferrals were wrong about their cause, and were
believed from a comment until somebody read the code.

And on the device-code side, from M9 (decisions in ADR 0028):

- **The verification page offers providers, not only a password**, and that is the milestone. `norite login`
  has done headless password sign-in since M7, so a password-only page would add almost nothing — while an
  account that signs in only with Google or GitHub had no way onto a server at all, because M8's listener
  binds `127.0.0.1` and the phone completing the flow cannot reach it.
- **What protects this flow is a page, not a protocol.** The device-code grant's live risk is somebody
  being *sent* a code and authorizing a stranger's machine, and nothing cryptographic prevents it (§14.21).
  So approval is a separate explicit step that a successful sign-in never implies; the page names the device
  and shows the code back for comparison; a decision that is neither approve nor deny denies; and there is
  no `verification_uri_complete`, because a URL carrying the code makes the whole attack one click. Anything
  that would shorten those screens is reopening that decision. **With one qualification a review found**:
  the page's provider buttons carry a continuation that nothing binds to the browser it was issued to, so a
  link *can* skip the code-entry step — it cannot skip approval, which is where the defense is, and binding
  it needs a browser session this surface has none of until Phase O — **M108**, whose roadmap entry
  now carries it as a done-when rather than leaving it pointing at a phase.
- **The user code is stored in plaintext and the device code is hashed.** The exception is deliberate and
  has two halves, both needed: a user code is not a bearer credential — whoever holds it must still
  authenticate and approve, and what that authorizes is somebody else's machine acting as *their* account —
  and it has to be readable back, because the approval page shows it and the OAuth callback reaching that
  page never saw what anybody typed.
- **The poll is `POST /auth/device/token`.** `architecture.md` sketched `GET /auth/device/code/{code}` and
  both halves are wrong here: the call spends the code and starts a session, which rule 4 forbids a GET
  from doing, and a path is logged where a body is not (rule 8). The document is corrected, not the code
  bent to it.
- **A device flow carries no flow challenge, and that is not the binding going optional.** `/authorize`
  requires exactly one of `flow_challenge` and `device_token`. A challenge makes a *code* redeemable only by
  the client that began the flow; a device flow mints no code, because the waiting client has held its
  credential since before a browser was involved.
- **Which device is being authorized travels inside a signature or inside a consumed row**, never in a form
  body or a callback URL — `oauth_states.device_code_id` across the provider round trip, the `dvc` claim
  across the username form. The same discipline and the same tests M8 built for `client_redirect_uri`.
- **Two continuations on the page, not one — three since M11a.** An entry token says a browser has entered
  a live code; a `device_factor` token (M11a) says it has also given a correct password on an account owing
  a second factor; an approval token says it has finished proving whose account this is. `parseDeviceToken`
  takes the type it wants rather than reporting the type it found, because a single token with an optional
  user field would authorize before authentication happened — and it extracts a subject for the *two* types
  that name an account, not for the approval token alone. Getting that last part wrong is what made M11a's
  factor step unpassable until a test caught it.
- **The fallback is detected and never silent.** `SSH_CONNECTION` is checked before anything platform-
  specific, since a desktop administered over SSH looks local in every other way and macOS would open Safari
  on a screen nobody is at. Detection only ever redirects a sign-in that *already* needed a browser, so a
  password login over SSH is untouched. `--no-browser` keeps M8's meaning; `--device-code` asks deliberately.
- **Values from the instance that direct an action are checked, not sanitized.** The user code is printed as
  an instruction, so a cleaned-up version is worse than none; the verification URI is about to be opened and
  typed into, so it must be the instance that was asked for, scheme and host and port. Rule 8 covers the
  third: the device code never reaches the terminal.
- **`OAuthOutcome.SignedIn()` means "resolved to an account"**, not "has an exchange code". Those stopped
  being the same thing here.

One thing this milestone deliberately leaves: **screen `5a` in `docs/design/tui/` is normative and
specifies a code box with a countdown and a progress bar.** The CLI prints the plain-text equivalent
because it is not the TUI; M55 draws the box. The two are not in disagreement.

And on the instance-administration and registration side, from M10 (decisions in ADR 0029):

- **The operator is a third authority, and the only credential here minted by a client.** An unsubjected
  JWT with `typ: "operator"`, signed with the instance's own key, two-minute life, presented to
  `/api/v1/instance/*`. It concedes nothing new — anyone who can read `instance.toml` can already forge an
  access token for any account — but it *states* the authority, so a bootstrap proves filesystem access
  rather than being trusted for arriving early. There is no window in which whoever reaches a
  freshly-migrated instance first becomes its administrator.
- **`typ` is load-bearing against a token you would not guess.** An access token is refused by the
  empty-subject check, not by `typ`; disabling `typ` leaves that test passing. What it defends against is a
  **device entry token** — same issuer, same key, live expiry, no subject — which without the check would
  turn entering a valid device code into instance-operator authority.
- **The token format lives in `backend/operatortoken`, outside `internal/`**, because Go's `internal/` rule
  makes `backend/internal/auth` unreachable from the CLI and a second copy of a claim shape drifts. The
  authorization decision stays in `internal/auth` where the routes are. Same decision as `daemon/credentials`
  at M7, and it adds the second cross-module edge, `cli` → `backend`. Two tests span it: the `iss` constant
  matching, and a token minted the CLI's way passing the middleware's check.
- **Routes mount unconditionally; middleware may be conditional.** `cmd/server/contract_test.go` walks the
  router built with a nil `AuthSvc`, so a group mounted only when that field is set is invisible to rule 6's
  check — which is how four `/instance` routes went undocumented while the test passed.
  `AuthenticateInstanceAdmin` fails closed on a nil service instead.
- **Bootstrap is a sibling command**, `norite instance bootstrap`, not a step inside the wizard: when `init`
  finishes there is no running server to create an account on. It refuses unless there are zero
  administrators, under an advisory lock — checking a count and acting on it is a read-modify-write, and
  under READ COMMITTED two concurrent bootstraps both read zero.
- **Registration answers 202 identically** whether or not the address is taken, and never returns the
  account. The difference goes to the mailbox: a verification link, or a "somebody tried to register" notice
  that carries **no token and nothing that acts on the account** — it does contain one bare URL, the
  instance's `/reset` page with no token on it, which is a pointer rather than a capability. This line said
  "carries no link" until M14's manual pass read the actual email; the property that matters is that
  nothing in it can be *spent*, and stating it as "no link" made the true claim uncheckable.
  A taken **username** is still 409 — an `@handle` is public, an address is not.
- **`HashPassword` runs before the address check**, and that ordering is the timing half of the guarantee.
  Moving it below makes the taken branch ~1 ms against ~31 ms. The unique-constraint race comes back as the
  same silence, because reporting it would leave the oracle reachable by firing two requests at once.
- **A uniform response is not enough — the two branches must leave the same *state*.** A free address
  commits a row occupying the username; a taken one rolls back and occupies nothing, so `409 username` on a
  second request read the address answer. The branch that creates nothing reserves the username instead
  (`registration_reservations`, 000011). No TTL, because the unverified account it stands in for has none;
  if either gains one, both must. This is the shape to look for whenever a response is made uniform: ask
  what each branch *wrote*, not only what it said.
- **Login refuses an unverified account exactly as it refuses a wrong password**, and mails a fresh link
  instead. A distinct answer reopens the closed oracle in two requests: register an address with a password
  you choose, then log in with it — 403 if the address was free, 401 if it was taken. Measured before the
  test was written. The reminder follows only a *correct* password, so guessing at addresses queues nothing.
- **An unverified provider address takes a detour, and both cases render one page — in every registration
  mode.** If an unknown address showed a username form while a registered one was refused, the browser would
  answer the question ADR 0024's merged message exists to refuse. The form moved behind a mailed link;
  linking to an *existing* account stays refused, that being the takeover direction. The registration-mode
  gate lives *inside* `unverifiedProviderAddress`: ahead of it, a gated instance answered 200 for an
  address with an account and 400 for one without, which is the same oracle by a shorter route.
- **The mailed continuation is unbound, and says so in its signature** (`eml_only`) rather than by carrying
  an empty challenge — the refusal for a token that merely lost its binding has to stay in force. It mints
  no exchange code (nobody is waiting) and the account it creates is verified, opening the link being the
  proof of mailbox control the whole detour exists to gather.
- **Invite redemption is one `UPDATE` with every guard in its `WHERE`**, sharing the account insert's
  transaction. Rewritten as a check-then-update, four of four concurrent racers get in. Both NULLs are
  explicit `IS NULL` branches — `uses < NULL` is NULL, so "unlimited" would otherwise match nothing.
- **An instance with no SMTP relay creates accounts already verified**, and the enumeration hole stays open
  there. Accepted, pinned by a test, and stated in three places, because the failure mode to guard against
  is it becoming quiet. Same absence keeps M6's outright refusal on the provider path.
- **An invite-only instance is password-registration-only.** There is nowhere to carry a code through a
  provider redirect. Fails closed, and it is in the contract rather than left to be discovered.


And on the CLI OAuth side, from M8 (decisions in ADR 0027):

- **The loopback listener receives the code from the *instance*, not the provider's callback.** The
  provider redirects to `{public_base_url}/api/v1/auth/oauth/{provider}/callback`, which is what is
  registered with Google and GitHub and is unchanged from M6; the instance then `302`s to the listener. The
  older reading — that the port is registered with the provider — requires the client secret in the CLI
  binary and is what ADR 0027 exists to correct. Three documents said it and all three are fixed.
- **A client-supplied redirect is validated by host, never by port.** `http` on a loopback IP literal, an
  explicit port, no userinfo, no query, no fragment, ≤256 bytes, and the parser's own re-serialization is
  what gets stored. **`localhost` is refused**: a name resolves through `/etc/hosts` and DNS, an IP literal
  resolves through nobody. A port allowlist was rejected — it would couple two independently-versioned
  modules and stops nothing the host check and the verifier binding do not already stop.
- **The destination is fixed when the flow starts.** It comes out of the consumed `oauth_states` row, and
  the callback never reads `client_redirect_uri` from its own URL — that URL is presented by whoever holds
  the link. Across the sign-up form it rides in the **signed continuation token** (`rdr`), never in a form
  field, for the same reason. Both properties have a test that fails if the handler reads the untrusted
  copy; both were confirmed by making it do so.
- **What crosses the loopback socket is a fixed vocabulary, not prose.** Seven error codes, no
  `error_description`, ever. A listener is a socket any local process can write to, so keeping free-form
  text off it entirely is a better answer to rule 19 than sanitizing it on arrival — the client writes the
  wording. On the CLI side an unknown code is bounded and stripped to `[a-z0-9_]` as well.
- **Bind `127.0.0.1:port` explicitly.** `":port"` binds `0.0.0.0` and puts a sign-in listener on the LAN,
  and it looks identical on a developer's machine. The test prints what it bound when it fails.
- **A busy port is walked past, never diagnosed.** A bind error says something holds the port and nothing
  about what; finding out means speaking HTTP at an unknown local service. The exception is a permission
  error, which is reported, because "free this port and retry" is the wrong advice for it. Safe because a
  squatter receives a code it cannot redeem without the verifier.
- **The listener is bound before the URL is built**, so the redirect names the port actually bound. A bug
  here is invisible until the first fallback and then hangs forever.
- **The sign-in URL is always printed**, whether or not a browser opened. It carries the challenge, which
  is a hash and publishable by construction, and never the verifier. A browser opening the wrong profile is
  the ordinary case, not the rare one.
- **`--no-browser` prints and keeps listening; it does not mean headless.** A machine with no browser at
  all is M9's device code. This refines ADR 0011's sentence rather than reversing it.

And on guilds, permissions and the audit log, from M12:

- **`roles.Resolve` implements ADR 0008's layers 2–5 and deliberately not layer 1.** An Instance Admin is
  not a guild member, holds no roles, and resolves to exactly zero permissions — which is the correct
  answer to the question that package asks. The tier check lives in `guilds.authorize`, which asks it
  *first*: resolving first and checking the tier as a fallback answers "not found" for every guild on the
  instance and never reaches the check. ADR 0008 rejects the synthetic "super role" by name, and this is
  the fourth time a rule here has had to be structural rather than remembered.
- **Every guild route carries a scope, and M12 shipped without them.** `auth` guards even the read-only
  `GET /users/@me` with `RequireScope`; the fifteen guild routes were mounted bare, so an `identify`-only
  API token deleted a guild and answered 204. Reproduced before it was fixed. This is the first milestone
  to put a *mutating* surface within reach of a delegated credential, which is exactly when "scopes only
  ever restrict" stops being free. `guilds.read` and `guilds.write` are separate because the blast radii
  differ and the common bot wants only the first, and **write does not imply read** — a scope bounds a
  credential, and holding one is not a reason to be granted another.
- **`authorize` is the only thing that decides authority, and it exists before the call sites do.**
  `revokeEverything`, `RequireLiveSession` and `factorProof` were each built after finding the one caller
  that forgot. M12 is the first milestone writing its handlers from scratch, so the chokepoint went in
  first — and a table-driven test walks every non-GET route with a non-member and requires 404, which is a
  property no single-endpoint test can assert.
- **`authorizeWith` takes a querier so the check runs on the caller's transaction.** Rule 1 asks for data
  freshly loaded for the request; authorizing on the pool and then opening a transaction to write reads a
  snapshot from before its own `BEGIN`, which is exactly the window a demotion committed in between is
  missed in. Costs a parameter.
- **Two refusals, and the split is anti-enumeration.** A non-member and a guild that does not exist both
  get 404, because guild ids are snowflakes and a 404/403 split turns a list of plausible ids into a map of
  what exists (M11 settled it for session ids). A member lacking a permission gets 403 — they already know
  the guild exists. Neither names the missing bit. **A malformed id in a path answers 404 too**, through
  the same parser, or the oracle comes back through the 400.
- **A role cannot be given permissions the caller does not hold.** Without it `PermManageRoles` is every
  permission: mint a role with `PermAdministrator`, take it, done. It binds creating *and* editing —
  bounding one is the same hole in two steps.
- **A channel is authorized against its own guild, read off the row.** `PATCH /channels/{id}` carries no
  guild, which is why `GetChannel` takes no guild parameter: scoping that read by a caller-supplied guild
  would be trusting the value the check exists to verify. Same shape for a `parent_id`, which must be a
  category *in this guild* or it puts a channel behind another guild's overwrites.
- **The permission bit order is data, not style.** `roles.permissions` stores bit positions, so inserting a
  constant in the middle reassigns every permission every guild has already granted — no migration, no
  compile error, no symptom. A test pins each constant to its literal value. `PermVideoVoice` stays for the
  same reason (rule 10): removing it renumbers the six bits above it.
- **`permAll` stops at the last defined bit** rather than being `^Permission(0)`, so a future bit is not
  already granted to every owner on every instance — and so the value stays positive as a signed `bigint`.
- **Permissions cross the wire as a quoted decimal string**, the decision ADR 0003 makes for snowflakes: a
  63-bit field and a float64 do not mix above 2^53. Fixed now, while nineteen bits are defined and changing
  it is free.
- **Nothing is cached.** `architecture.md` describes `Resolve` as cached per `(guild_id, user_id,
  channel_id)` and invalidated on dispatch — the invalidation signal is a gateway dispatch, and that is
  M18. A cache with nothing to invalidate it is a demotion that takes effect five minutes late.
- **Guild creation writes three rows in one transaction**: the guild, `@everyone`, and the owner's
  membership. A guild without a default role has no permission floor; one without the owner's membership
  looks fine until the member list omits them. Neither is repairable by a client.
- **Deleting a guild is the owner's, not `PermManageGuild`'s.** That bit renames a guild and is one an
  owner delegates; deleting cascades everything with no undo, which ADR 0008 makes layer 2's call. Same
  reasoning refuses removing the owner from their own guild by any route, themselves included.
- **Rule 2 is satisfied by there being no other way to write.** Every mutation goes through `inTx`, and
  `writeAudit` takes that transaction's querier. A mutation committing without its entry is not a state the
  code can reach.
- **A guild's own deletion entry does not survive**, because `audit_log_entries` cascades from `guilds`.
  Rule 2 holds — the entry is written in the transaction — and nothing can read it afterwards. The durable
  record of an instance-level action is rule 14's `instance_audit_log` (M72), not this table. Asserted by a
  test as the state it is, rather than left to look like a bug.
- **Refuse before explaining.** Two endpoints answered a non-member with something other than 404 because
  a public error was evaluated *before* `authorizeWith`: removing the owner returned a 409 naming the
  reason, and a voice-only field on `PATCH /channels/{id}` returned a 400 that reported the row's type.
  Each disclosed the existence of an object the caller could not see, and the owner one disclosed who owned
  it. Validation that reads a loaded row is an authorization question, not an input question — it belongs
  below the check, and nothing in it ever needs to run first.
- **A structural test is only structural over the inputs it sends.**
  `TestEveryMutatingGuildRouteRefusesANonMember` (M14 renamed it `TestEveryGuildRouteRefusesANonMember`
  and derived its route list from `chi.Walk`) walks every mutating route with a stranger and passed
  throughout, because it targets an ordinary member and always sends `{"name": "hijacked"}` — never the
  owner, never a voice-only field. The route table was covered; the branches inside the handlers were not.
  When a test's value is "it cannot miss a route", ask what it cannot miss *within* one.
- **A reserved contract field must exist in the Go struct, not just the document.** `httpx.DecodeJSON`
  calls `DisallowUnknownFields`, so a field the contract calls ignorable is a hard 400 unless the request
  struct names it — M12 reserved `challenge_response` for M67a saying "sending this changes nothing" and
  shipped exactly that. Neither contract test can see it: one compares the route set, the other reads
  responses. **Request-shape drift has no automated check**, which is the thing to know before reserving
  the next field.
- **A read-modify-write needs a lock even when the value looks monotonic.** `max(position) + 1` fixed the
  count-versus-max cause of role-position collisions and left the race: `RunInTx` sets no isolation level,
  so under READ COMMITTED two concurrent creates read the same max and both take it. There is deliberately
  no unique constraint to catch it, and position is the hierarchy M13 enforces over. Slot 3 of the advisory
  lock namespace, keyed per guild.
- **Preserving an unknown bit on read and accepting one on write are different decisions.**
  `PermissionFromInt64` keeps unknown high bits so a row from a newer schema survives an older binary;
  storing one from a *request* means a later milestone defining that bit finds it already granted. The
  escalation check masks it incidentally for a non-admin and not at all for an Instance Admin, whose tier
  short-circuits before any bitfield is consulted — so `roles.Known()` is checked first, on the input.
- **A list that cannot be paginated is bounded at creation instead.** The member list is a cursor capped at
  100; the channel and role lists return everything, because a client needs the whole tree to render a
  sidebar and paginating would make it fetch in a loop (rule 21's chattiness). So the ceiling lives in
  `CreateChannel`/`CreateRole` — 500 and 250 — and it is checked *after* authorization, or "is this guild
  full" becomes one more thing a non-member can measure. Unbounded, the channel list was 782 kB of row data
  and a 740 kB sort at 5,010 channels, against 8 buffers for the capped member list on a 15,000-member
  guild.
- **A polymorphic column cannot cascade, so something has to clean up after it.**
  `permission_overwrites.target_id` names a role or a user depending on `target_type` and therefore cannot
  be a foreign key. Deleting a role or removing a member has to delete their overwrites explicitly — a
  leftover member row is not clutter, because rejoining silently restores a channel-level deny nothing in
  the UI or the audit log explains.
- **A new role's position is `max(position) + 1`, never the role count.** The count collides the moment
  anything has been deleted, and there is deliberately no unique constraint to catch it, so the corruption
  is silent until M13 enforces a hierarchy over it.
- **A permission that is only ever OR'd into a base is not a grantable permission.** `UpdateMember` began
  with `PermManageGuild` and added the moderation bits, which made `PermMuteMembers` and
  `PermDeafenMembers` undeliverable on their own: the only way to let somebody mute was to also let them
  rename the guild. Build `need` from the fields actually present, starting empty.
- **Guards that can be raced live in the statement.** `@everyone` is undeletable by `AND NOT is_default` in
  the `DELETE`, not by an `if` before it — the same discipline as `ConsumePasswordResetToken` and
  `RedeemInstanceInvite`. The Go check exists only to produce a better message.
- **Check every new foreign key for an index, again.** Three in `000015` had the shape M11 found on
  `replaced_by_id`, and the worst — `guild_member_roles.role_id`, which is the *third* primary-key column
  and so serves no lookup alone — cost 4566 ms against 8 ms on 500 role deletions. Nothing in an FK
  declaration hints at it and it only appears once something deletes.
- **A composite index's second column pays on the paginated read, not the unbounded one.** "The index
  answers the `WHERE` and the `ORDER BY` together" is false without a `LIMIT`: Postgres prefers a bitmap
  heap scan and a quicksort, because reading the heap in physical order beats an ordered index scan's
  random access. Add the `LIMIT` a real list endpoint carries and the sort node disappears. Measured, after
  being asserted wrongly first.
- **`oapi-codegen` generates types only**, into `backend/apicontract` — under `internal/` until M19, moved
  out so the daemon and the CLI decode REST and gateway payloads with the same generated types rather than
  hand-written copies — committed with `just contract-check` and a CI step. `chi-server` cannot reproduce
  this router — the fixed middleware order, `/instance` as a sibling, three limiter buckets, one exempt
  path — and a generated router that had to be fought is a worse contract than a hand-written one that
  matches the document.
- **The generated types are imported by no handler, and that is a limit rather than an unfinished step.**
  Handlers declare their own request structs to carry `validate:` tags and their own response types so ids
  marshal as snowflakes. So "a drifted field is a compile error" is *not* what this setup delivers; what it
  delivers is a checked artifact of the contract. Payload drift is caught by a test comparing real
  responses against the declared schemas in both directions, which `additionalProperties: false` plus a
  full `required` list makes exact. Claiming the compile-time version and not having it is how eight
  unlisted error codes accumulated before.
- **The codegen staleness check is inert on a file that has never been committed.** `git diff --quiet`
  compares the working tree to the index and ignores untracked files, so it passed against a deliberately
  drifted contract until the generated file was staged. True of `sqlc-check` too; CI is unaffected because
  it runs against a checkout where the file is tracked. Verify a new check of this shape by drifting its
  input *after* staging its output.


And on hierarchy and overwrites, from M13:

- **Layer 4 has two sentences and M12 implemented one.** Role bits are OR'd, *and* position "governs who
  can manage whom". `roles.Resolve` returns a `Resolution` rather than a `Permission` so standing arrives
  with the permissions — one column on a query that already returned exactly the right roles, against a
  second round trip on every kick, mute, deafen, rename, role edit, assignment and overwrite write. M12's
  authorize.go argued against widening that signature for `owner_id` and the argument does not carry here:
  standing *is* layer 4, so carrying it is the rest of the check rather than a convenience bolted on.
- **`highestPosition` is unexported and read only through `Outranks`.** For an owner it is a meaningless
  zero — and zero is `@everyone`'s position, a real place in the hierarchy, so a comparison that forgets
  to ask about ownership does not fail loudly. It fails by finding the owner at the bottom, unable to
  moderate their own guild, which reads as a permission bug and invites the repair that hands the guild to
  whoever holds the highest role. `Outranks` folds the question in; `OutranksMember` folds in the second
  one, because an owner cannot be reached by comparing standings at all.
- **An administrator has a real standing and the owner does not.** Layer 3 short-circuits *permissions*
  and says nothing about position, so somebody above an administrator can still act on them. That return
  sits after the loop, so the value exists and dropping it would put every administrator on the floor.
- **A new role is created at the bottom**, immediately above `@everyone`, everything else shifting up.
  Created at the top it is a role its own creator cannot edit, delete, assign or reposition. The shift
  *renumbers* rather than increments, which is what keeps positions bounded by the role ceiling instead of
  by the guild's lifetime creation count.
- **Assignment is escalation-checked, and that is the one deliberate departure from Discord.** Discord
  gates assignment on hierarchy alone and relies on powerful roles being placed high; a public instance
  should not rest on that, because a moderator holding `PermManageRoles` and not `PermManageGuild`
  otherwise assigns a low bot role carrying it to an account they control. It needs no owner branch —
  `decision.allows` already exempts layer 1 and the owner.
- **Removing is not the safe direction.** Taking a role off somebody lifts whatever its channel overwrites
  imposed, and the commonest role overwrite anywhere is a `muted` role denying `PermSendMessages`. So
  unassignment is checked like an assignment, `DeleteRole` refuses to remove overwrites whose bits the
  caller lacks, and the escalation check on an overwrite covers **the union of what the request changes** —
  the old row's `allow|deny` or'd with the new — because blanking a row removes what it carried just as a
  delete does.
- **A positional hierarchy cannot protect a restriction placed low**, and a restricting role is placed low
  by definition, so a moderator can still shed one. That is not a hole in the rule; it is why
  `PermModerateMembers` at M74 is a bit on the member rather than a role.
- **Overwrites are resolved per channel, and the resolution knows which.** `InChannel` takes the channel
  id and skips rows for others, because the listing reads every channel's overwrites in one query and
  handing it the whole slice merged every channel's tiers — an allow on one restoring what another denied.
  It resolves from an unexported `base` so calling it twice cannot compound.
- **`RemoveMember` deliberately does not get the overwrite guard the other two deletion paths have.**
  Guarding a kick would let a member become unkickable by holding an overwrite whose bits the moderator
  lacks, trading an escalation for a denial of moderation. The residual — a kick clears a member-tier deny
  — matters once that member can return, which they can since M20a's invites: a rejoin is a clean slate
  (F1), so leave-and-rejoin sheds a deny, accepted and carried to M74 (`docs/security-ledger.md`).
- **Category permissions are copied at creation, never inherited at read time.** A channel does not follow
  its category's later changes, so "sync permissions with category" is a client re-copying through the
  overwrite endpoints rather than a flag anything stores. Without the copy, a channel created inside a
  locked-down category is readable by everyone the moment it exists.
- **A channel the caller cannot view is absent from the listing, and answers 404 elsewhere.** Once the
  listing hides channels, a 403 for a hidden one against a 404 for a nonexistent one is an oracle
  confirming exactly what the filter withholds. Every channel-scoped route refuses that way now; somebody
  who *can* see the channel and merely lacks the permission still gets 403.
- **Four things this milestone measured rather than assumed.** A suspected missing index on
  `DeleteOverwritesForTarget` did not exist — its own `USING channels` join supplies the primary key's
  leading column. The guild-wide overwrite read must pass channel ids rather than join on `guild_id`
  (0.536 ms against 13.682 ms at the channel ceiling). The listing must group those rows once rather than
  hand every channel the whole slice (130 us against 757 us). And the role renumber needs `guild_id` on
  its *outer* relation or it sequentially scans every role on the instance.

And on the audit log, from M14:

- **A permission that is only ever OR'd into a base is not grantable, so the audit log gets its own bit.**
  `PermViewAuditLog` at position 19, not a shape of `PermManageGuild` — folding it in would mean the only
  way to let somebody audit a guild is to let them rename it, which is verbatim the correction M12's
  `UpdateMember` needed. Adding a bit means editing `TestTheBitOrderIsWhatTheDatabaseAlreadyStores` and
  `TestPermAllStopsAtTheLastDefinedBit` **by hand**; that is the mechanism working, not an obstacle, and a
  renumber is the diff those tests exist to put in front of a reviewer.
- **A channel you cannot see is not yours to manage.** M13 taught the listing to hide channels and left
  every channel-scoped mutation gated on its own bit, so the two disagreed about whether a channel
  existed. `authorizeChannel` folds `PermViewChannel` into every caller's `need`. **Adding a filter to a
  listing is a reason to re-examine the *permissions* of every route that names the same object, not only
  their status codes** — M13 did the second half and stopped.
- **The log is bounded by its permission, not by what the reader can currently see.** An entry names a
  channel hidden from its reader, by id and by name. Filtering on present visibility would let somebody
  hide their tracks after the fact by locking a channel down, and half the interesting entries are
  deletions whose target cannot be resolved to a permission at all. In the ledger with its *reopens if*,
  which names M15: a `message.*` action puts content in `changes`, and rule 13 applies from that moment.
- **A diff belongs in the writer, never in the reader.** The prior state exists exactly once — in the row
  being written over — so `changes` is built inside each mutation's transaction. A reader cannot
  reconstruct `from`: the previous entry may have touched other fields or may not exist at all. That is
  why the change touched all sixteen writers rather than one function.
- **Two kinds of key, told apart by the value.** A changed field is an object carrying `from`, `to` or
  both; a context field is a scalar. Context exists because `target_id` is one column and some actions
  name a pair — an overwrite names a channel *and* a role. Rendering context as `{"to": …}` would say it
  had been set to that value. `TestTheAuditDiffShapeIsUniform` enforces it across every action, because a
  rule stated in a comment is one the seventeenth mutation breaks.
- **A field sent with the value it already had is not a change**, and `reflect.DeepEqual` alone cannot
  decide that: it calls two values of different concrete types unequal whatever they hold, so an int32
  column against an int literal would record a change on every request. Go has no generic methods, so the
  comparison widens integer kinds instead of taking a type parameter.
- **Permissions and ids go into `changes` as their own types, never `.Int64()`.** They marshal as quoted
  decimal strings because the field is 63 bits and a browser's number is a float64. M12 and M13 wrote
  numbers here, which put that hazard on the one endpoint whose purpose is to be believed.
- **A cursor is an id, never a timestamp** — not because timestamps collide here (measured: 60 entries, 30
  written concurrently, 60 distinct microsecond values) but because nothing *constrains* `created_at`, and
  a page boundary that loses a row rarely is one nobody will reproduce from a report. The migration's
  first version claimed same-millisecond bursts were normal; writing the test that claim implied is what
  disproved it.
- **An optional bound is `COALESCE`, not `$n IS NULL OR col < $n`.** The obvious spelling cannot become an
  index qual under a generic plan — 1859 buffers against 9, measured. `plan_cache_mode` defaults to `auto`
  and currently rejects that plan, so it is robustness rather than a live bug; it costs one keyword. The
  same trick buys nothing for an *equality* filter, where the rewrite names the column on both sides.
- **A route-surface test must fail on a route it has no case for.** Both walk `chi.Walk` now and fail in
  *both* directions, because a stale case for a deleted route asserts nothing and reads exactly like
  coverage. A route deliberately not exercised declares itself exempt with a reason. Before this, one
  iterated a hand-typed slice and the other covered ten of sixteen actions — the six it missed were the
  whole of M13's surface.
- **A guard is proved by removal, and the removal must itself be proved.** Delete the guard, watch a
  *named* test fail, restore it. On this branch that came back green four separate times while proving
  nothing: `gofmt` had realigned whitespace the string match keyed on, so the edit applied to no lines and
  the silence read as success. Assert the mutation landed before trusting the result. **If a check's
  failure mode is silence, it is not a check** — the same principle that makes `spdx-check` match the
  generated marker strictly rather than grepping the whole file, and that makes a staleness check inert on
  a file nobody has staged.
- **Enumerate call sites; do not spot-check them.** Pinning `go-licenses` removed a CI install step and
  broke `scripts/gen-third-party-notices.sh`, two directories from anything in the diff, because the
  search was `ci.yml` rather than the repository. There were three callers and the edit found two. A
  `git grep` for the thing being changed costs one command and is the difference between a refactor and an
  outage; the same applies to a renamed constant, a moved helper, or a rule whose wording changes.
- **An unknown filter value is refused, not answered with an empty page.** An action nobody writes matches
  nothing, which is indistinguishable from a guild that never did it, so a typo would read as evidence of
  absence. The check lives in the service and not only in the handler, for the reason `authorize` and
  `revokeEverything` do. Same shape for a zero id: `snowflake.Parse` accepts `"0"`, so absence travels as
  a pointer the whole way down rather than as a zero that silently means "no filter".

And on messages, from M15:

- **Authority lives in `guildauth`, and `messages` proves the extraction was the point.** All three
  chokepoint functions were unexported in `guilds`, so a second package literally could not call them.
  Exporting from `guilds` instead would have made it the import root of `messages`, `reports`, `tags` and
  `whispers` — the four the architecture tree gives their own package. Every operation in `messages` opens
  by calling it, so there is no path to a message that skips authorization.
- **There are two channel entry points and the difference is the lock — but the axis is not
  read-versus-write.** `AuthorizeChannel` reads the channel `FOR UPDATE` and closes the diff race M14
  reproduced; `AuthorizeChannelUnlocked` does not. The test for which one you want is **whether the caller
  describes the channel row**: every `guilds` channel mutation writes an audit entry over it and must hold
  it still, while everything in `messages` — including three mutations — reads `type` and `guild_id` off
  it and writes elsewhere. It was called `AuthorizeChannelForRead` at first, on the assumption that the
  lock divides reads from mutations, and that name was false at three of its four call sites **inside the
  same milestone** — `Send`, `Update` and `Delete` all mutate and all belong on the unlocked path. A name
  that lies is worse than a comment that lies, because people read names and skip comments.
  `AuthorizeChannel` keeps the shorter name and the lock deliberately: taking it when you did not need it
  costs throughput, which is measurable and recoverable, while skipping it when you did costs a wrong
  audit entry, which is silent and permanent — so the unconsidered choice is the safe one.
- **Rule 2's narrowing is a property you assert, not one you inherit.** A member posting, editing or
  deleting their own message writes **no** audit entry, and the test asserting that a mutation records
  nothing would have been a bug report before M15. A moderator deleting somebody else's writes one. The
  payload carries ids and never content, so rule 13 is satisfied by there being nothing to exclude rather
  than by an exclusion a later reader has to remember.
- **Only an author may edit, and `PermManageMessages` deliberately does not grant it.** That bit deletes
  other people's messages, which is moderation with a visible outcome. Rewriting somebody's words puts
  them in their mouth under their name, and an audit entry recording that it happened would not make the
  message honest — which is also why there is no `message.edit` verb to record.
- **A verb written in one package and validated in another needs a pin.** `messages` writes
  `message.delete`; the audit-log reader in `guilds` refuses an `action` filter naming a verb outside its
  vocabulary, so a verb missing there makes the log refuse to filter rows it already holds. Neither
  package may import the other, so the value is a literal on the guilds side and
  `TestTheMessageAuditVerbAgreesAcrossPackages` in `cmd/server` — which imports both — is what stops them
  drifting. The two guilds coverage tests name the boundary rather than pretending to reach it.
- **The three message bits compose rather than nest.** `PermViewChannel` puts a channel in your sidebar,
  `PermSendMessages` lets you post, `PermReadMessageHistory` lets you read the backlog. Announcements is
  view+history without send; a support thread opened to a reporter is view+send without history. All three
  are in `@everyone`'s default grant, so withholding history is the deliberate configuration — the
  alternative is a guild whose every channel reads as empty until somebody finds the bit.
- **A message id from another channel answers 404 on every path.** The channel in the route is what was
  authorized, so reaching a message through the wrong one would mean the permission check covered one
  channel while the write landed in another. Same 404 for a `reply_to_id` naming a message elsewhere, and
  the refusal says nothing about whether that id exists — an unvalidated cross-channel reference is a
  disclosure, not merely a data-integrity problem.
- **`last_message_id` is maintained in the send's own transaction.** M12 created the column with no
  foreign key precisely so this could be a cheap write rather than a `max(id)` per channel on every
  channel listing (§15.2's N+1). Nothing maintained it until M15, so a channel's pointer was NULL for
  three milestones.
- **A write must be bounded by the permission that bounds writing, whichever verb performs it.** `Update`
  authorized on the view bit alone, so denying `PermSendMessages` — the mute every guild uses — stopped a
  send and left the author able to rewrite every message they had already posted, one fresh publish each
  and a `MESSAGE_UPDATE` fan-out apiece once M18 lands. M13 spent two decisions stopping a restriction from
  being *shed*; this was the same restriction walked around. Editing now needs `PermSendMessages`; deleting
  deliberately still does not, because removing your own message is redaction and that is the outcome a
  mute wants. **Found by `/security-audit` after the milestone's manual pass had already run**, which is the
  part worth keeping: the pass drove the permission matrix and the audit asymmetry and never denied a bit
  to somebody who already had messages, so the interesting state took a deliberate setup no ordinary happy
  path reaches.
- **Authorization says whether you may act in a channel, never whether the channel holds messages.**
  `guildauth.guildOf` refuses a channel belonging to no guild, which rules out DMs and nothing else, and
  `guilds.isGuildChannelType` is consulted only at *creation* — so a member could post into a category,
  read it back, and advance its `last_message_id`. Reproduced on `GUILD_CATEGORY`, `GUILD_VOICE` and the
  reserved `GUILD_ANNOUNCEMENT`. Not an authorization bypass — the caller holds the bits on that row — but
  a place to park content no client renders, which is invisible to moderation while the API keeps serving
  it. `messages.ChannelGuildText` gates `Send` only, so anything already stored stays readable and
  removable. **Text-only is the reversible direction**: allowing a type later is additive, disallowing one
  later strands whatever was stored in it.
- **The second value written in two places gets the same treatment as the first.**
  `messages.ChannelGuildText` duplicates `guilds.ChannelGuildText` because neither package may import the
  other, and `TestTheTextChannelTypeAgreesAcrossPackages` in `cmd/server` is what stops them drifting —
  the shape `TestTheMessageAuditVerbAgreesAcrossPackages` established one finding earlier. A cross-package
  literal is not a smell to remove here; it is a thing to *pin*, and the pin belongs in the one package
  that imports both.
- **A lock inherited from a caller that needed it is not a lock this caller needs.** `Send` took the
  channel `FOR UPDATE` because it called `AuthorizeChannel`, whose lock exists for the four `guilds`
  mutations that *diff* the channel row. A send diffs nothing — it reads `type` and `guild_id` and writes
  to `messages` — so the lock bought only serialization, and serialization on the product's highest-volume
  write is a per-channel ceiling no horizontal scale lifts: 777 sends/s into one channel against 2,294
  after removing it. Two things the lock was silently doing had to be replaced rather than dropped:
  `last_message_id` monotonicity, now `GREATEST` in the statement (a plain assignment walks the pointer
  backwards — reproduced in psql at pointer 100 with message 101 present), and the channel-overwrite mute
  race, accepted in the ledger because the *role*-assignment mute never had that protection anyway.
- **A guard-by-removal that fails to build has proved nothing**, and neither has one whose test passes.
  Both happened here. Deleting the foreign-key mapping removed its imports, so the package stopped
  compiling and the "failure" was the compiler; making the check never match instead kept it compilable
  and revealed the real problem — the test reached a 404 from the *authorize* path and never exercised the
  mapping at all. The fix was to make the mapping a named function and test it directly. This is the M14
  lesson one layer up: there, the mutation did not apply; here it applied and the test could not see it.
- **A race you cannot provoke is not a race you should test by racing.** The monotonicity test first ran
  eight concurrent senders and passed three times out of three with `GREATEST` removed — the interleaving
  is real but too rare to hit under load. Asserting the *statement's* property instead (a lower id must
  not overwrite a higher one) fails the moment the guard goes. Prove reachability once, by hand, in psql;
  then pin the property deterministically.
- **Two vocabularies live in Go and in the contract, and nothing checked either until M15.**
  `contract_test.go` compares the route *set*; `contract_payload_test.go` validates real responses, which
  sees an enum only if some exercised response happens to carry the value. So `messages.read`,
  `messages.write` and `message.delete` were added to `auth.AllScopes` and `guilds.AuditActions()`, added
  to no enum, and both gates stayed green — a generated client could not request the scopes the message
  routes are gated on, nor decode an audit page containing a moderator deletion. Found by a human reading
  the YAML. `TestTheScopeVocabularyMatchesTheContract` and `TestEveryAuditActionIsInTheContract` now
  assert it against `apicontract`, which is generated from the document and staleness-checked, so
  the comparison is Go-to-Go and needs no second YAML parser. **The error-code enum had this test since
  M11 and nobody generalised it** — when a check exists for one vocabulary, ask what the others are.
- **A bound enforced in two places must measure the same thing in both.** `checkContent` was added so
  `MaxContentLength` stopped being inert, and used `len()` — bytes — while the handler tag it claims to
  agree with is go-playground/validator's `max`, which is `utf8.RuneCountInString`. 4,000 Japanese
  characters are 12,000 bytes and 4,000 emoji are 16,000, so every message near the limit in a non-Latin
  script would have been accepted by the handler and refused by the service. **A test suite written in
  English cannot see this**, which is why the regression test spends its cases on Japanese, emoji and
  Arabic rather than on `strings.Repeat("a", n)`. Duplicating a check is only safe if you know what the
  original measures.
- **A mutating route added to a second package is invisible to the route-surface tests until the test
  router mounts it.** Both mounted only on a non-nil handler, and `newTestRouterWithAuth` builds with nil
  services — so the four message routes existed and neither test saw them. That is the M10 failure
  `contract_test.go`'s own comment warns about, one package over: routes may not be conditional, and a new
  handler has to be added to *both* test routers, the nil-service one and the real-service one.

And on reports, from M16:

- **The reporter is absent from the wire shape, not stripped from it.** `reports.Report` has no
  `reporter_id` field at all, so every response is built without one and there is nothing a handler can
  forget. That is the same structural move as `factorProof` and `revokeEverything`, applied to a
  disclosure rather than to a check — and it is asserted by marshalling each of the three shapes and
  searching the JSON, because what matters is what a client receives rather than which field somebody
  remembered to omit.
- **A one-way door is taken in the reversible direction.** Adding a field later is additive for every
  client; removing one after clients read it is not. Same reasoning M12 used to fix the permission wire
  format while nineteen bits were defined and it was still free.
- **`routed_to` is computed, and the request struct's silence is the enforcement.** There is no routing
  field on `FileInput` or on the request body, and `DecodeAndValidate` rejects unknown fields — so sending
  one is a 400 rather than a value quietly ignored. The thing to not do later is add the field "to return
  a friendlier error", which converts a structural guarantee into a check.
- **Rule 13's exclusion is a join predicate, never an expression.** `LEFT JOIN messages mv ON mv.id = m.id
  AND NOT mv.is_e2e`, with content read only from `mv`. Two reasons that agree: the exclusion is inherited
  by whoever writes the next reader of that join, and sqlc cannot type an expression through a `LEFT JOIN`
  — `CASE WHEN … THEN NULL` generated `interface{}`, and casting it to text generated a non-nullable
  `string` that a NULL fails to scan into at runtime. A plain column types as `*string` correctly.
- **Three states, not two, whenever a joined fact can be absent.** `target_is_e2e` is false for an
  ordinary target, true for a withheld one, and **nil when the target does not resolve at all**.
  Collapsing the last into false reports a vanished message as readable.
- **Content is resolved at read time and never copied into `reports`.** A snapshot would be a second table
  holding message content, a second place rule 13 has to hold, a copy outliving its message, and — the
  part that decides it — a confidently wrong answer for a message edited after it was reported, which is
  the case M16a exists to serve.
- **A negative that rule 2 implies is a test or it is nothing.** Filing writes no audit entry, and
  `TestFilingWritesNoAuditEntry` is the only thing that says so. Before M15's narrowing it would have read
  as a bug report; after it, it is the property keeping member traffic out of the moderation log.
- **A reserved value is refused at the boundary, not accepted and stranded.** Three of four `target_type`
  values and one of four statuses are stored-but-unwritten, held so M61 and M74 do not renumber. Accepting
  one would file a report into a queue that never shows it — M14's unknown-filter lesson pointed at a
  write rather than a read.
- **A cross-package audit verb is pinned, and the exemption that skips it must fail when stale.** M15
  established the literal-plus-pin shape for `message.delete`; M16 needed a third and fourth, which is
  where two hardcoded `if action == …` branches became a map — plus `requireForeignAuditActionsExist`,
  because a list that only ever skips goes stale silently and a stale exemption reads exactly like
  coverage.
- **The route-surface prefix filter is a third place a route can hide.** `guildSurfaceRoutes` matches
  `/guilds` and `/channels`, so `POST /reports` was invisible to both structural tests until the prefix
  list grew. M15's lesson was that a new handler must be mounted in *both* test routers; this is the
  other half of it, and a route mounted outside every known prefix is covered by nothing.
- **A seed that measures nothing looks exactly like a measurement.** Two ways it happened here: ids
  clustered so the planner walked the primary key backwards and every index looked useless, and the guild
  and status distributions sharing one modulus so the heavy guild held every open report. Before trusting
  a plan, check that the seed's conditions are independent and that its ids are distributed the way the ID
  scheme actually produces them.

And on the opt-in recording log, from M16b:

- **Two vocabularies on one subject, pinned *apart* rather than equal.** `messages` writes
  `create`/`edit`/`delete` to `message_audit_entries`; `guilds.AuditActions()` is the audit log's
  vocabulary and must not contain them. Adding them there is the tidy-looking move and breaks two things:
  that slice is what `GET /guilds/{id}/audit-log` validates an `action` filter against, so a verb the
  table never holds makes the filter *validate* and match nothing — M14's "an unknown filter value is
  refused, not answered with an empty page" inverted into a typo that reads as evidence of absence — and
  `TestTheOnlyMessageVerbIsDeleteAndItCarriesNoContent` iterates that slice precisely to stop a message
  verb arriving unreasoned. M15 and M16 each needed a cross-package pin holding two literals *equal*;
  this is the third and the first that holds two sets apart.
- **A guard can be redundant on purpose, and then it needs a three-legged proof.** The writer checks the
  recording flag in Go *and* carries it as a join predicate on the insert. Removing either one alone
  leaves the "records nothing" test passing, which looks like the test being weak and is actually the
  redundancy working — so proving it means three runs: statement guard removed (passes, Go branch
  catches it), Go branch removed (passes, statement catches it), both removed (fails). Only the third
  shows the test can see the property; only the second shows the statement guard is load-bearing.
- **Measure the *off* path before designing the on path.** The design that reads best here — one
  statement, opt-in as a join predicate, no read at all — costs 62–90% of the message insert in every
  guild that has not opted in, against 9% for reading a boolean. A feature nobody has enabled must not be
  paid for by everybody, and the thing that made the expensive version attractive (an unraceable guard)
  turned out not to exist: both shapes read the flag after the message is written.
- **Rule 2 and a recording log disagree about self-deletion, and both are right.** An author deleting
  their own message writes no `audit_log_entries` row, because rule 2 covers authority exercised over
  somebody else. The same deletion *is* recorded here, because a recording that skipped self-deletions
  could be evaded by deleting your own messages — which is the thing the guild switched it on to
  prevent. Two tables answering two questions is the reason they can differ.
- **The first place layer 1 is narrower than layer 2**, and it is deliberately temporary. An Instance
  Admin may not start a guild's recording, because rule 14 has nowhere to record that they did until M72
  builds `instance_audit_log`. The tier still *reads* any recording guild's log. If a second layer-1
  exception ever appears, the ledger entry says the pair needs a mechanism rather than two comments that
  do not know about each other — which is what this codebase has done four times rather than trust to
  memory.
- **`Decision.Owns()` is false for an Instance Admin who genuinely owns the guild.** Layer 1
  short-circuits to a zero resolution, so any ownership check written against the Decision locks an
  instance operator out of a guild they created. Read ownership off the loaded row instead, as
  `RemoveMember` does. The test that catches this asserts the *fact* — that `Owns()` is false for that
  actor — rather than describing it, so the case cannot decay into "an owner may do the thing", which is
  already covered elsewhere.
- **A milestone's central decision can be specified nowhere while a screen has already promised it.**
  §2 settled the toggle's authority and said nothing about the read's; `SCREENS.md` had told members
  their messages are "kept in a log its moderators can read". When a surface has a screen, read the
  screen before deciding what the API allows — it may already have answered on the product's behalf.

And on the gateway and event dispatch, from M18:

- **Publish only through `dispatch.Publisher.Queue`, inside the transaction.** It registers the publish with
  `database.AfterCommit`; there is no other way to publish, so there is no way to publish early (rule 5). The
  payload is encoded once there and copied into every recipient's frame.
- **An event names an audience, never a recipient list.** `Guild` (optionally narrowed to a channel),
  `Users` for events about an account itself, `FormerMembers` only for a deleted guild's `GUILD_DELETE`. Who
  receives it is decided at fan-out by `guildauth.Audience` against fresh rows, so a publisher cannot widen
  an audience by getting a list wrong. When reading the event's object over REST needs more than view, say so
  in `Need` — `MESSAGE_UPDATE` carries `READ_MESSAGE_HISTORY`. An event about a channel whose rows will be
  gone by fan-out carries their snapshot with `Snapshot` set, or it reaches nobody.
- **The gateway must never disclose more than REST.** For a new event, find the REST read of the same object
  and make the audience match it. If the two disagree, the contract decides which is wrong.
- **A new dispatch type goes in `gateway-events.schema.json` in the same commit** (rule 6), and every gateway
  test validates every frame it reads against that schema, so an undeclared field fails the first test that
  sees it. The REST shapes inside it are generated from `openapi.yaml` and held equal by a test; edit the
  OpenAPI document, not the copy.
- **Anything that ends a sign-in closes its connections**, through `revokeEverything` or `endConnections`,
  inside the transaction. Liveness is asked of the sign-in (`SignInOf`, `RequireLiveSignIn`), never of the
  device and never of the row.
- **Ordering is per guild.** Fan-out runs in lanes keyed by guild, so a guild's events arrive in publish
  order and two guilds' may not. Nothing may rely on cross-guild order.
- **Lock order in `internal/gateway`**: the server's `mu` before a session's, and the index's lock after
  either, never before. A session updates the index while holding its own lock.
- **Fan-out queries are written against the generic plan.** They run on every event, so drive them from
  primary keys and add them to `TestTheFanOutsRoleReadCannotScanTheWholeGuild`, which reads the plan pgx
  actually prepared under `force_generic_plan`.
- **`backend/gatewayproto` is outside `internal/` so the daemon can import it**, which made `daemon` →
  `backend` the repository's third cross-module edge at M19. Keep it free of anything the backend alone
  needs.
- **Testing the bus's timing**: `gatedBus` holds revocations until released, and `droppingBus` loses them,
  in `gateway_revoke_test.go`. A race between a commit and its delivery is staged with those, never by
  racing.

And on the daemon as gateway client, from M19:

- **One goroutine owns the refresh token.** Every load, refresh and write-back runs in `session.Source.Run`;
  everything else asks it — `Current`, `Rejected`, `Revoked`, `Reload`. Two presenters of one rotating
  token is what reuse detection reads as theft, and it answers by revoking the device's sign-in.
- **Expiry is judged on the server's clock, never `time.Now()`.** `serverClock` takes samples from HELLO and
  from each refresh's `Date` header and advances them on the monotonic clock. Anything new that compares
  against an instance timestamp goes through it, and its tests inject a `Clock`.
- **A refresh the instance has answered is finished and stored, even while stopping.** The token presented
  is spent the moment the instance answers, and a spent token on disk is what the next start presents —
  reuse detection again. Hence the detached context, and `settleStore` on the way down.
- **The write-back names the token the store holds**, which the source tracks as `stored`, apart from
  `current`. `ReplaceToken` with the wrong one reads as somebody else having logged in. A store that
  refuses a write keeps the session in memory; it is not a sign-out.
- **Every wait in `Run` listens for a reload and a revocation, and re-reads the store before acting.**
  `waitUntilDue`, `waitLive`, `waitForStore` and `throttle` all do. The throttle was the one that did not,
  and a login landing in it waited out the floor and presented a superseded token first.
- **The close-code table is `gatewayclient.Client.after`, and nowhere else.** A new close code or op from
  the server is a row there, not a branch in the read loop.
- **The connection follows the sign-in.** A credential carries its `Generation`; a different one starts a
  fresh session rather than resuming another sign-in's, and `Ended(generation)` closes the connection
  however the sign-in ended. A `Sink` must forget everything on `End`.
- **The instance is not bounded by the server's own validation.** Frames are read-limited, HELLO's interval
  is bounded, a message is charged its whole payload, guild summaries are capped and their names cut.
  Anything new the daemon keeps from the gateway is bounded against a hostile instance, not a correct one.
- **Names are sanitized on arrival; content never is, and is never logged.** Server text that reaches the
  log — close reasons, dial errors, versions, event types — goes through `termsafe.Text` first. No frame is
  logged at any level. Content is the renderer's to make safe (rule 9) and M20's `--json` keeps it
  lossless.
- **A sub-logger adds `subsystem`, never `component`.** The base logger sets `component=daemon`, and
  zerolog appends a field rather than replacing it.
- **The `daemon` → `backend` edge reaches types only**: `gatewayproto`, `apicontract` and, since M28, `voiceproto`. Keep both free of
  anything that decides, or the daemon starts depending on the server's judgement rather than its wire.
- **Testing the session**: `fakeClock` with `advanceUntil`, which steps rather than jumps because `Run`
  arms timers between steps; `settle` before counting refreshes; `waitingWithin` to hold `Run` inside a
  wait. The gateway's fake validates every frame in both directions against
  `gateway-events.schema.json`, and writes its own close reasons — so wording the real server sends is
  checked by a manual pass against the real server, and nothing else.

And on the attach socket and the relayed verbs, from M20:

- **The token never crosses the socket.** An attach client names a method, a path and a body; the relay
  performs the call with the daemon's token and returns the status and body. Nothing in `daemon/ipc` has a
  field that could carry a token, and IDENTIFY is decoded strictly.
- **A field added to any frame on the socket is a MINOR change, never a PATCH one.** Frames are decoded
  strictly, and the version rule lets a PATCH-different daemon and client talk, so a field added in a patch
  release would be refused as malformed instead of answered with "restart the daemon". The version is read
  leniently first, so a MINOR mismatch always reports as one.
- **Frames are encoded with `ipc.Marshal`**, which does not HTML-escape: `json.Marshal` rewrites `<` `>` `&`
  as six-byte escapes inside a relayed body too, which let a hostile instance's answer outgrow the bounds.
- **The relay's reach is a decision per route.** It refuses `/auth/*`, `/instance/*` and
  `/users/@me/sessions`, with one exception since M22: `/auth/tokens` and `/auth/tokens/{id}`, two exact
  shapes, so a first-party client can mint the tokens scripts use (ledger). `TestEveryContractPathIsDecided`
  fails on any path in `openapi.yaml` without an explicit relay-or-refuse entry. A new route gets one in the
  same commit.
- **The fan-out never waits on a client.** Queuing is non-blocking and bounded in frames and bytes; a full
  queue drops the client with 4012. The fan-out runs on the gateway connection's read loop, so a blocking
  write there stops the daemon reading the gateway. The payload is encoded once and shared.
- **A watching client is closed with resync (4100)** whenever the state it was built from is cleared: a
  fresh session, its READY, the sign-in ending. A client that asked for no events is left alone.
- **A verb is ids in, a typed result out.** Ids are digits, checked before anything is attached to. The
  result is a view of the verb's own, filled field by field from `backend/apicontract`'s types, rendered
  by `output.Render` as text or JSON from one value — the shape M48 extends. Its schema goes in
  `contracts/cli-json/` in the same commit (rule 15), and a verb returning no object prints `done`.
- **Every relayed outcome's exit code is decided in `daemonclient.Call`**, never in a verb (since M22 its
  non-2xx half is `daemonclient.Refusal`, which `norite automation request` shares): 4xx is 4,
  except 401 and 429, which are 3 like a stopped daemon, a signed-out one or an unreachable instance; 5xx
  is 1; a 2xx without the body it owed is an error, never an empty object reported as success. Every call
  and every attach is bounded, so a hung daemon is exit 3, not a hang. Usage errors are `clierr.Usage`,
  exit 2, and so is every flag urfave/cli refuses, through the `OnUsageError` the tree walk sets. 2 and 4
  print without the `norite: ` prefix.
- **A verb attaches on its first request**, after every flag check and confirmation, so a usage error is
  exit 2 on a machine with no daemon running.
- **Destructive verbs go through `confirm`** (since M21 a wrapper over `cli/internal/prompt`): `--yes`
  answers it, the question goes to stderr, and with no terminal and no `--yes` it is `ErrNoTerminal`
  naming the flag.
- **Text output passes every instance value through `termsafe`**, ids included; `--json` goes through
  `output.WriteJSON`, which escapes what `termsafe.Block` would remove and is lossless to a parser. Never
  `json.Marshal` straight to stdout.
- **Testing a verb**: add a case to `verbCases`, which `TestEveryVerbHasACase` holds to the tree. The fake
  daemon validates the request and your scripted answer against `openapi.yaml`, so a fixture missing a field
  the instance always sends fails, and the output is validated against its `contracts/cli-json/`
  definition.

And on guild invites and the first client, from M20a:

- **An invite code travels in a body, never a path**, on all three routes that take one (ADR 0029's reasoning:
  a path is in every log line). Unknown, malformed, expired, revoked and used up are one 404. Codes are M10's
  format, from `auth.NewInviteCode`. **Joining is a person's act, never a token's**: redemption takes
  `RequireUserActor`, as an ownership transfer does, since a delegated credential choosing its owner's company
  is an escalation. A token may still create invites.
- **Redemption's order is the lock order**: the account's slot-4 advisory lock, the guild `FOR SHARE`,
  then membership, the joined ceiling and a single-statement spend with every guard in its `WHERE`. A
  member redeeming again spends nothing and writes nothing. Anything new that locks a guild and an invite
  takes the guild first.
- **A rejoin is a clean slate.** `RemoveMember` deletes the departing member's overwrites, so
  leave-and-rejoin sheds a member-tier deny; accepted and carried to M74, with guild bans, in the ledger.
- **A new client action goes through `cli/internal/ops`**, which the verbs use too: the request's shape,
  the instance's bounds and every outcome's exit code exist once. `ops` checks every id it puts into a path,
  whoever supplied it, because the client's ids come from the instance.
- **What the client holds is bounded against the instance**: `maxHomeGuilds`, `maxHomeRows`, and `hold()`
  for a message. Anything new it keeps from an event or a listing gets a bound a correct instance never
  reaches.
- **A sign-in that ends clears the screen.** A sign-out or a different account drops home and the pane,
  draft included (`Model.forget`); a resync rereads the open channel rather than merging into it. The daemon
  forgets on a sign-in's end, and a client that kept drawing would undo it where it matters.
- **An answer names what asked for it.** Every command's result carries the attachment's generation, and a
  send's answer carries its pane, so an answer arriving after the thing it answers has gone is dropped.
- **Draw the tail.** A frame lays out only the newest messages that fill it (`paneModel.tail`); anything
  that needs the whole layout per keystroke is the 59 ms this replaced.
- **A root-only flag is `Local: true`.** urfave/cli v3 hands a root flag to every subcommand, which then
  accepts it and does nothing.
- **Testing the client**: drive `Update` and `View` at a fixed size with the harness in `tui_test.go`
  (teatest has never been tagged), against `daemontest`'s contract-checked fake; play a hostile instance
  with `bounds_test.go`'s stub, which the fake cannot be, since it holds every answer to the contract.
- **The installers in `scripts/` restate what a release publishes**: the archive names, `checksums.txt`,
  and the cosign identity pinned to `release.yml` at the tag. A change to any of those in
  `.goreleaser.yaml` changes `install.sh` and `install.ps1` in the same commit. `install-server.sh` gets its
  programs through `install.sh`, so downloading and checking exist once. Test them against `dist/` served
  over HTTP with `NORITE_RELEASE_BASE_URL`, and **never run `install-server.sh` in a test without `--dir`
  and `--bin-dir`**: its defaults are a real instance and a real `~/.local/bin`, and a run without them
  once registered a second server on a live one's port.
- **A release is proved by `just release-dry-run`** before a tag. Builds run with `GOWORK=off`, there is no
  `go work sync` hook, and the release footer, which carries the `cosign verify-blob` command pinned to
  `release.yml` at the tag, is configuration.

And on the client config and the daemon's own requests, from M21:

- **A setting is a row in `contracts/client-config.toml` and in `daemon/config`'s key table**, in the same
  commit; a test compares the two in both directions. Each key says whether it is portable and which
  milestone reads it. A key with no reader yet is listed, kept by the loader, and read by nothing. No
  secret is ever a key.
- **Anything outside `daemon/config` writes a setting by client, never by path**: `SetFor`, `UnsetFor`,
  `ImportFor`, all through `UpdateFor`, which asks which file the client reads again once it holds that
  file's lock. The path-based writers are unexported for this reason.
- **An edit is a splice, and many edits are one read.** `setAll` and `unsetAll` plan against one parse of
  the document and read the result back once. Nothing re-serializes a config. A new way to change the file
  goes through them, and a batch must write what the same edits one at a time write.
- **The decoder is quadratic in a table's keys.** `MaxFileSize` is 64 KiB because of it, on read and on
  write. Decode a file once and pass what that found; anything that decodes per key or per frame is the
  cubic import again.
- **A config is a regular file, asked before it is opened** (`config.read`, `statefile.Load`). Opening a
  pipe waits for ever.
- **The lock is in the state directory, named for the real directory and the file's own name.** Never
  beside the config, which roams on Windows and is often a link into a dotfiles repository.
- **An editor takes no lock.** Every writer re-reads before its rename, and the toggle re-reads each file
  after its work and redoes it if somebody saved.
- **What a file says is foreign text.** A `Warning` and a `ParseError` are sanitized where they are made;
  a path in an error is the filesystem's, and goes through `termsafe` where it is drawn or printed.
- **The daemon notices the file and interprets nothing.** `configwatch` watches directories, never files:
  the config's, its parent, and a link target's. A client hears `DAEMON_CONFIG_UPDATE`, carrying nothing,
  and reads the file itself.
- **`DAEMON_` is the daemon's namespace for dispatches, and `Server.Dispatch` drops an instance event that
  uses it.** A new local dispatch is a constant in `daemon/ipc`, a row in `daemon-ipc.schema.json`'s
  `LocalDispatch`, and sent through `Server.Local`.
- **A request the daemon answers itself lives under `/@daemon/`** on the relay's own request frame, so no
  frame changes. It is answered signed in or not, with 200 and a body or with `conflict` or `failed`, never a
  status; since M24 also `unreachable`, when the request cannot be carried out yet: the release host could
  not be read, a check is already under way, or the instance has not been heard from. It takes no body and
  carries nothing of the user's in its path, because a daemon older than the prefix relays the path to its
  instance (ledger). Two exceptions, each sent only after a request that says nothing has shown the daemon
  knows these requests: the port number in `/@daemon/automation/enable/{port}` (M22), and the voice join's
  body (M30), which `ipc.DecodeVoiceJoin` decodes strictly. `relay.Target` refuses the prefix, and a test
  holds `openapi.yaml` to never using it. The CLI side is `daemonclient.Local`, `LocalWith` for a body,
  and `LocalRead`.
- **`state.json` is read through `daemon/statefile` and written only through `daemon/internal/statefile`**,
  which nothing outside the daemon module can import. A write puts back the fields it did not understand.
  A step that must follow the state and not be interleaved with the next update goes in `Update`'s `then`.
- **While split, a client's file is read for `[shared]` and its own section.** Its copy of the other
  client's section is stale. Every verb that lists, exports, imports or sets must leave it out or refuse
  it, and unsplit takes each section from its owner's file as written.
- **The toggle sets aside and never deletes**, and never overwrites an earlier copy.
- **The terminal client builds its styles once per config read** (`look`), never per frame, and keeps the
  last config that loaded. `Options.Config` defaults to the user's file, so the package's `TestMain` points
  every root at a temporary directory; a new test package that starts a daemon or a client needs the same.
- **A yes/no question is `cli/internal/prompt`**, for the verbs and for `norite config import` alike.
- **Hold a cost by counting, not by timing.** `config.decodes` counts runs of the decoder, and the tests
  hold an import, an export and a merge to the same count for two keys as for thousands. A three-second
  limit on a merge failed in CI at 3.04, under the race detector, with nothing wrong.
- **Testing the editor**: compare whole files, not the presence of a comment. For a race with an editor
  or another command, stand inside the handler (`Handler.saved`, `Handler.rename`) rather than race it.

And on the automation port and what a token writes, from M22:

- **Two tiers, two packages, one send path.** The attach socket is the account's own programs; the port
  is anything holding its secret, and is lower (rule 16, ADR 0017). `daemon/internal/automation` imports
  no credential source, and a test holds it to that. Both tiers ask the instance through `relay.Perform`,
  so a bound or a header changes for both.
- **Nothing is said before the port secret.** No frame, no reason: a connection that has not presented it
  is closed, or timed out after five seconds. A wrong secret gets one code and one sentence. Connections
  waiting to present it are counted apart from scripts being served.
- **Only a value shaped like an API token is forwarded** (`ipc.LooksLikeAPIToken`), and only to the
  instance the port was turned on for, compared on every request with `ipc.SameInstance`.
- **A script's paths are `relay.ScriptTarget`'s**: the relay's rules with none of its exceptions. An
  exception added to the relay does not reach the port unless somebody adds it there.
- **The rate is charged last**, after every check that costs nothing, so a refused request spends none of
  a budget all scripts share.
- **A connection closed with bytes unread is reset.** Where a script is owed the reason, send the Close
  frame, half-close, and read what it is still writing, within bounds (`closeAfterReading`).
- **A request the port ended says it may have arrived.** So does a timeout. Never tell a script a write
  is safe to send again unless nothing was sent.
- **The port's file is removed before its connections are waited for**, never left by a clean stop, and
  cleaned at start. `Open` leaves a file it did not write alone: it may be a port still serving.
- **The port's control answers from one moment.** `status`, `enable` and `disable` each report under the
  control's lock; an open port is reported as it was opened, and an enabled one that is not open always
  says why. Lock order: the state file's lock, then the control's mutex. `status` takes only the mutex.
- **Not having read the sign-in yet is not being signed out** (`automation.ErrSignInPending`). Anything
  new that asks the session where it is signed in tells the two apart, or it sends somebody to
  `norite login` over a good sign-in.
- **A secret is never printed and never a flag.** The port secret goes into one program's environment
  (`run`) and nowhere else; the API token is read from `NORITE_API_TOKEN`. A token's value is printed
  once, by `token create`, or written to a new `0600` file by `--out`, claimed before the token is minted.
- **`messages.type` is the instance's.** `typeFor(actor)` on send, `GREATEST` on edit, no request field.
  It only rises. Anything new that writes a message passes its actor's type the same way, and anything
  that shows a message shows the tag: `ops.MessageTypeAutomation` in the client, the last word of the
  header in both front ends.
- **Testing the port**: `raw` in `automation_test.go` speaks frames and holds each to the contract;
  `r.silence` asserts nothing was said. `portFixture` in `daemonproc` boots a control on a temp state
  directory; `updateState` is the seam for a state write that fails. A manual pass uses a second server
  process on the same database as "another instance".

And on the daemon's lifecycle and its log, from M23:

- **Where the log is comes from `daemon/logfile`**, for the daemon and for every reader. Never from a
  service definition: the launchd plist passed the path once, and a daemon started by hand logged
  somewhere else. On macOS it is `~/Library/Logs`; elsewhere the state directory.
- **The log is a file anybody can write to.** The reader bounds a line, a read and a batch, opens without
  waiting and checks the handle is a regular file, and counts only names lumberjack writes as rotated
  copies. Whatever prints an entry cleans every piece of it; `--json` is one object per line through
  `output.WriteJSONLine`.
- **Nothing holds the log between reads.** Rotation is a rename, and Windows refuses to rename an open
  file. A follower knows its file by the identity of the handle it read, never by the path.
- **The daemon does not call `Setrlimit`.** The runtime has already raised the soft limit, and a call of
  the daemon's own would be inherited by the voice-worker. What bounds its handles is its caps
  (`ipc.MaxClients`, `ipc.MaxAutomationConns`, the port's `maxPending`), and
  `TestTheDaemonHoldsEveryCapUnderTheLowestDescriptorLimit` holds them to 256 descriptors. A new
  listener or a raised cap is a reason to run it.
- **A crash file is one run's.** Anything in `daemon.crash.log` at start means the run before crashed: it
  is set aside as `.1.log` and the log says so. Nothing compares dates.
- **A stop is asked of the daemon first** (`ipc.PathStop`), then of the service manager, by `stop`,
  `restart` and `uninstall`, and by `install.ps1` using the `norite` already installed, which is the
  running daemon's version. The answer is queued before the stop begins. Anything new that stops or
  replaces the daemon from outside it goes through `socketStop`. M24's updater is inside the daemon and
  ends its own run with the same cancellation.
- **`norite daemon status`: 0 means a daemon is running, however it was started.** It asks the service
  manager and the socket. A daemon started by hand is running and not installed, and anything that offers
  to install must treat it as running.
- **On Windows the daemon keeps its console, and since M24 the console has no window.** A console program is
  told about a logoff and a shutdown through the console, and Go turns each into the SIGTERM the daemon stops
  on. Letting go of the console (`FreeConsole`) was built and removed at M23: without one the daemon is killed
  where it stands at every logoff. The task runs it in `conhost.exe --headless`, which keeps the console and
  shows nothing, seen to stop it cleanly at a sign-out and a shutdown; the updater's successor gets a hidden
  console of its own. conhost exits 0 whatever its program did, which is why the updater watches the successor
  itself. `norite daemon install --show-console` keeps the window, for testing.
- **A daemon is running when something answered, never when a connection could not be made.** A dial
  that fails for any reason but "nothing is listening" says so as a problem and reports no daemon.
- **Every "it has not stopped" has waited.** `askToStop` waits for the process the daemon named, or for
  its socket to go quiet when it named none, before it says so.
- **What "yes" is lives in `prompt.Yes`**, for `prompt.Confirm` and for a question that is not one.
- **Nothing installs or starts a daemon unasked.** `norite login` offers when somebody is there to
  answer, and the default for installing is no. A failed offer never fails the login. `norite setup`
  (easier-setup) asks too, with yes as the default, because running it is asking to be set up.
- **A service of the account stops at its last logout on Linux.** `install` reports lingering and
  `--linger` asks for it, by numeric id. Never the default.
- **`network-online.target` does not exist in a user manager.** Nothing in a user unit orders itself
  after a system target.
- **A test of these commands replaces the socket as well as the service manager**: `answerStop`,
  `answerStatus`, and a `TestMain` that makes the real ones a panic. `logscmd`'s does the same for the
  log's path.
- **A manual pass that needs an init system, a reboot or a login session runs in a real VM, unprivileged,
  or on a machine the author names, and only after asking.** Never in a privileged container, a container
  whose command is an init system, or anything else that can reach the host's ttys, devices, cgroups or
  logind: one ended the author's whole desktop session at M23. Where no VM is to be had, the clause is
  reported as not walked.

And on the updater, from M24:

- **Nothing the listing says is believed until a signature says it too.** The bundle is verified offline
  against `daemon/internal/updater/trusted_root.json` and the identity for the candidate's own tag; then
  the archive's SHA-256; then three programs by exact name (the voice-worker since M28). Any failure fails closed and the version running
  stays. No flag, file or environment variable changes the root or the identity in a release build.
- **The trusted root ages.** `just trusted-root` regenerates it through TUF, and CI fails when the
  committed copy is behind Sigstore's, like govulncheck, on a PR that changed nothing. Read its diff: it
  decides whose signature the daemon believes.
- **A candidate passes four filters**: the channel, higher SemVer precedence (`gatewayproto`), the
  instance's own `gatewayproto.Check`, and not rolled back from here. Signed in to an instance not yet
  heard from, nothing is chosen.
- **Only a receipted install writes a binary** (`daemon/installreceipt`), and it is `auto` by default on
  Linux and Windows. Any other install checks and says so. macOS checks and does not apply.
- **The record is the updater's own** (`<state>/update/state.json`): the mode, the channel, a version on
  trial, versions rolled back from. Not `config.toml`, which the daemon interprets for nobody (M21). What
  it keeps of a host's words is cut short before it is written; a record that cannot be read is off and
  never written over.
- **A version is on trial for ten minutes of being up.** Two unconfirmed starts, or the previous run's
  crash file, put the previous pair back and mark the version bad. A clean stop gives its start back, so
  reboots inside a trial count against nothing. Nothing is installed over a trial.
- **On Unix the daemon replaces itself (`exec`), keeping its PID; on Windows it starts its successor,
  watches it for twenty seconds and exits 0.** The successor runs outside the task until the next logon,
  and `daemon status` says so rather than "started by hand".
- **`norite_testrelease`** is the one way a daemon believes another signer, for the end-to-end test and a
  manual pass; a test holds a release build to containing none of it. Never ship a build with it.
- **Testing the updater**: `newWorld` in `updater_test.go` serves a listing and a Linux archive, so a test
  that sets `GOOS` to Windows stages nothing — assert the decision, not an install. A manual pass on
  Windows runs in a VM, unprivileged, with SSH on loopback; M23's rule for a pass with an init system.

And on voice input, from M25 (ADR 0034):

- **No attach client opens the microphone.** The worker does, and the outcome reaches the clients as call
  state from the daemon. A client explains a refusal and names the setting; it never asks for the device
  to trigger a prompt.
- **On macOS the daemon is the identity.** The worker's use is charged to it, so the worker never
  disclaims its parent. The worker asks AVFoundation for the outcome; samples arriving prove nothing.
- **The daemon stays cgo-free, and push-to-talk does not change that.** Every mechanism is pure Go. On
  macOS, Carbon needs the main thread's run loop, so the daemon's `main` gives it that thread.
- **A global key is registered at join and released at leave**, never held all day, and never before a
  session exists. Failing to register is call state, not a failed call.
- **A Linux app id needs an installed `.desktop` file.** `install.sh` owns it, and uninstall removes it.
- **A pass that captures audio checks its wiring.** `pw-link -l` before and after every capture in a VM,
  because a virtual source's name is a request PipeWire may not honor.

And on packages, from the `package-managers` branch (ADR 0035):

- **The package definitions restate a release too.** `.goreleaser.yaml`'s nFPM packages, casks, Scoop and
  winget manifests and AUR packages name the same archives, programs and notice paths as `install.sh` and
  `install.ps1`; a change to one changes the others in the same commit. `just release-dry-run` checks
  each client package holds its three programs and its marker.
- **A package installs files and starts nothing.** `norite login` offers the service (M23). No package
  script reaches a user's service manager: removal says what to run.
- **A packaged install is its package manager's to update.** The daemon checks and names the command.
  How it knows is `daemon/internal/updater/packaged.go`: the `packaged-by` marker a package installs, or
  the directory Homebrew, winget or Nix installs into. A new channel is a row there and in ADR 0035 §4.
- **A packaged daemon starts the version installed over it**, by watching the path it was started by
  (`updater/replaced.go`) and ending its run as an update does. Only a packaged daemon, never on Windows,
  never during a voice session.
- **A service names the path the manager keeps.** `daemonctl` takes the daemon beside the link first,
  because Homebrew and Scoop replace what the link points at on every upgrade.
- **Each channel is gated on its own secret** in `release.yml`, and skipped while it is unset. Pre-releases
  are never packaged.
- **The GPG key for the apt, rpm and pacman repositories never enters this repository.** It is
  `Alexnex31/packages`' secret, whose workflow verifies the release's cosign signature and checksums
  before signing anything, and never runs on a pull request.

And on the SFU, from M26 (ADR 0023 as amended):

- **The SFU's interface is `voice.Control`**, settled whole because M116 carries it over mTLS: provision
  by connection, and `Evict`, `CloseRoom` and `EvictConnection` each refuse a provision they outran. A
  new method that ends something gets the same memory.
- **No bearer token reaches the SFU.** What admits a connection is `Provision` with the client's offer and
  DTLS against its fingerprint. The offer is checked on structure and never echoed; Pion's own refusals
  are logged, not returned.
- **The forward path never reads a payload** and allocates nothing; `BenchmarkForward` holds it there.
  A slot writes its own header: its SSRC, the sender's source id as CSRC, no extension but the audio
  level re-encoded, and no RTCP crosses a hop.
- **Everything a peer sends is bounded against a hostile one**, and each bound has a test that plays it:
  packets and bits per track, RTCP and messages per connection, data channels, SRTP streams against
  the offer, and the remote candidates ICE adds. A rate past its bound is dropped, and past twice it for
  three seconds closed, through `voice/rate`, which TURN shares.
- **The SFU is ICE-lite** (M27) and sends nothing to any address an offer names. It refuses an offer
  claiming lite, which would select a pair unchecked, and is the DTLS server, so it sends nothing first;
  either alone holds. Its remote filter is a second layer on what it pairs with; a new address class to
  refuse is a row in `reachable`.
- **Every goroutine and Pion callback the package runs defers `conn.contain`**, the room's selection
  contains its own on whatever goroutine runs it, and a lock a contained panic could cross is released
  by defer.
- **A connection takes part in its room only once its hello is accepted**, and is counted before `ready`
  goes out, so a client may send the moment it reads it. Anything new that forwards to a connection or
  from it asks `takesPart`. A test client is joined once it has read `ready`, as a real one is.
- **The slot map and the speaking list are sent after the room's lock**, and each send is of the state
  as it is then. Nothing is sent under a lock a close takes.
- **The data channel is `contracts/voice-datachannel.schema.json`**, decoded strictly; a field added is a
  MINOR change. Every test client's messages are held to it.
- **`voicetest`'s harness is never in a release**, and a test reads `.goreleaser.yaml` to hold that.
  M27 and M29 signal through it until M33.
- **Testing the SFU**: `join` gives a connected Pion client over loopback, `TonePackets` real Opus, and
  `testPanic` a panic or an eviction at a named point. A sender's sequence numbers continue across sends,
  or SRTP drops the burst. A race between an eviction and a provision is staged with cutoffs and
  `s.now`, never by racing.

And on TURN, from M27:

- **The relay reaches the SFU's media address and nothing else**, IP and port, enforced by the relay
  socket's `WriteTo` and `ReadFrom` (`turn/relay.go`). pion/turn's permission check asks of the IP alone
  and is not the guard. TCP relaying (RFC 6062) stays refused; a new relay shape goes through the same
  generator. **A client names the SFU by what it announced**, `turn.Options.Announced`, and the relay
  sends to the media address whichever it named, answering as that address: a STUN response as the one
  its request went to.
- **No pion/turn default is trusted.** Its auth handler, permission handler, listeners and relay
  generator are all this package's; a new pion/turn option is read in its source before it is used.
- **A credential is the account's, minted by `turn.Mint`, and reaches the client before its offer.**
  Expiry is asked of an Allocate alone; nothing else in a credential is time-bound. Nothing is logged per
  request: a username is a stranger's text, and pion's own lines are sampled, ten a minute.
- **An allocation ends when the SFU stops sending through it** (`Limits.Idle`), which is also how it ends
  with its call. Nothing links an allocation to a connection id, and nothing needs to. **One that binds a
  channel to anything but the SFU's addresses at its port ends too**: pion admits a binding on the IP
  alone and walks every binding for each packet relayed.
- **The relay serves both address families when the SFU listens on every interface**: a client reaching
  TURN over IPv6 asks for an IPv6 relay, Pion's included, and gets one on `::1`. With one family alone,
  pion's `StrictAddressFamily` is set, so a client naming none gets IPv4. What the SFU announces counts
  family by family, and a family nothing is announced in is every address of the host's, read every ten
  seconds off the packet path.
- **On every interface, the SFU answers from whichever address the kernel routes a client by.** A machine
  with several public addresses in one family names one in `media_listen`, and `StartStack` warns when it
  sees the case; one socket per address is M96's to decide.
- **The relay's per-packet path allocates nothing**, both ways: it reads with `ReadFromUDPAddrPort` and
  reports a peer by a `net.Addr` made when the peer changes. `TestTheRelayAllocatesNothingPerPacket`
  holds it there, and `BenchmarkRelayRoundTrip` measures it.
- **Every callback given to pion defers `Server.contain`**, which refuses the request and logs once for
  each place. A loop contains per iteration, or one panic ends it.
- **Replies to unauthenticated requests are rate-limited** per destination and in all, in memory, since
  they are counted per packet. A reply that grows reopens the ledger's amplification entry.
- **`[voice]` is checked whether or not it is on**, and `norite-server` binds its ports before migrating.
  A new setting goes in `config.Voice`, which every error names by setting. The relay address is bound
  once at start, so one this host does not have fails there.
- **TURN's Close closes what pion's leaves**: the connections its listeners accepted. The SFU closes
  before TURN, so its goodbye to a relayed client crosses the relay.
- **Testing TURN**: `newFixture` in `turn/server_test.go` stands a socket in for the SFU, so a test sees
  exactly what the relay delivers; pion's own client plays the peer. Test the configuration the defaults
  produce: an SFU on every interface, and one announcing a public address, are what
  `TestARelayedClientReachesAnSFUByWhatItAnnounces` runs. End to end, `newRelayed` in
  `voice/relay_test.go` runs TURN beside a real SFU, and the voice package's `TestMain` trusts a
  certificate for 127.0.0.1 through `SSL_CERT_FILE`, since Pion verifies TURNS against the system's
  roots and nothing else.

And on the voice-worker, from M28:

- **The worker is the daemon's direct child, started by its path beside the daemon's resolved executable**
  (`voiceworker.ProgramBeside`), never through a shell, `launchctl` or `open`: on macOS TCC charges its
  microphone to its parent (ADR 0034). On Windows it shares the daemon's console and is never given one of
  its own, which would show a window.
- **The protocol is `daemon/voiceipc` on two pipes of its own**, the attach socket's framing, and
  `contracts/voice-worker.schema.json`. Never stdin or stdout: the worker's stdin is the null device, and
  its stdout and stderr are one pipe read into the daemon's log, each line cut, cleaned and quoted as one
  field. A field added to a frame is a MINOR change.
- **Everything from the worker is a stranger's.** Frames are bounded (64 KiB, 50 a second), decoded
  strictly and checked (`Check` on each payload, held to the contract by a test refusing each case through
  both); printing is bounded at 256 KiB a second. The first breach kills it. A reason it carries is
  cleaned and cut with `voiceipc.CutReason` wherever it is shown or kept.
- **Its environment is `env.go`'s allow-list.** A capture library's variable is added there by name, and
  nothing else is. **Its descriptors are its pipes and nothing else**: Go opens its own close-on-exec and
  leaves alone what the daemon inherited from whatever started it, so `closeInherited` marks every descriptor
  from 3 up before each start. CI's runner hands two such pipes down, and they reached the worker until M28's
  first CI run.
- **A crash is a closed pipe the daemon did not ask for, or an exit that is not 0**, logged with the last
  twenty lines. Every worker is reaped; one asked to leave, or that closed its pipe and stayed, is killed
  after 3 s. **The worker exits when its control pipe closes**, from a goroutine that does nothing else.
- **One call at a time** (`Voice`): a join leaves first, `Busy` feeds the updater, and `Leave` returns once
  the worker is reaped. Each call ends with the sign-in it was joined under, through the session's own `Ended`
  channel and never the gateway connection's end, which a sign-out made while the instance is unreachable does
  not reach; a join that read a sign-in since ended is refused. The SFU is reached through a `Signaler`; until
  M33 the only one is M26's harness, compiled in under `norite_voiceharness` alone, which no release build
  passes (a test holds both).
- **A join's path carries a channel id**, so a client asks `GET /@daemon/voice` first, M22's answer to
  the ledger's entry on the prefix.
- **The worker ships beside the daemon and updates with it.** It has no release of its own. The updater
  stages and smoke-runs three programs; an install from before M28 has none to keep, and undoing to it
  removes the worker. A worker and a daemon of different versions are refused at the first frame.
- **The worker offers at most `voiceproto.MaxCandidates` candidate lines, each once in the first
  section**, relays first, which is where Pion reads them and what the SFU admits.
- **What the voice server answers is a stranger's too** (from M33 an instance's). The worker's ICE agent
  takes up only the candidates `session.reachable` admits, never this host, its link or a metadata
  address unless the join admits loopback, and at most `maxRemotes`. The pipe's bounds confine nothing: the
  worker runs as the account, and whether the OS confines it is M29's decision.
- **Testing the worker**: `standin_test.go` re-runs the package's test binary as a worker playing each
  hostile case; wait on a stand-in with `drain`, which fails rather than hangs. `e2e_test.go` builds
  `norite-voice` and M26's harness from the checkout and makes real calls. The worker's own loop is tested
  against a fake `Call` over in-memory pipes, every frame held to the contract.

And on the audio pipeline, from M29 (ADR 0036):

- **Opus is `voiceworker/internal/opus`'s `Codec`.**
  - Its default is libopus 1.6.1 in WebAssembly under wazero.
  - The native twin is cgo, from the same `shim.c`, behind `norite_opusnative`. No release passes that
    tag, and the tests run against both.
  - Switching the default is ADR 0036's measurement, `BenchmarkFrame` unpaced, and nothing else.
- **`opus.wasm` is a build of what it claims to be.**
  - `scripts/build-opus-wasm.sh` builds it from wasi-sdk 34 and the libopus tarball, both pinned by hash.
  - It is committed. CI rebuilds it and fails on a different byte; locally that is `just opus-wasm` and
    `just opus-wasm-check`.
  - It imports nothing and runs with no host module, capped at 64 pages. A test holds both.
  - A change to its flags is a change to what every caller runs.
- **One instance per encoder and per decoder.**
  - A trap discards that instance alone.
  - A slot's new sender gets a new instance, never a reset of the old one, which a sender that took the
    instance over could ignore.
  - The compiled module is never cached on disk, since a cache would be native code the account writes
    (ledger).
- **A call into an instance cannot be interrupted.**
  - `WithCloseOnContextDone` cost five times a frame.
  - What bounds a stuck decode or encode is the session's watchdog, on the monotonic clock: past
    `stuckDecode` it ends the call.
- **wazero's compiler is held to its interpreter, byte for byte**, by
  `TestTheCompilerAgreesWithTheInterpreter`, on amd64 and on `test-arm64`.
  - `silk/NSQ_del_dec.c` is built at `-Os` until a wazero release carries its #2508.
  - Anything else compiled to WASM gets the same test.
- **The frame is 20 ms of 48 kHz mono s16 everywhere.** `TestTheFrameIsOneSizeEverywhere` pins the
  constants three packages repeat.
- **The device clock drives both directions**: `Device.Capture(ctx, frame)` and `Device.Play(ctx, fill)`.
  - Each OS has a pure-Go backend: PulseAudio on Linux, WASAPI over COM through `syscall`, and
    AudioQueue through `purego`.
  - A backend nobody has run says so (M23).
  - A capture library's variable goes in the worker's environment allow-list by name, as `PULSE_COOKIE`
    did.
- **No test opens the developer's devices.**
  - The tests use `audiotest`'s device.
  - The daemon's end-to-end test points Linux at no PulseAudio server and uses `norite_noaudio`
    elsewhere.
  - `release_test.go` holds every release to passing neither tag.
- **A device is not the call; the codec is.**
  - A device that fails to open is logged, and the call goes on.
  - A codec failure ends the call.
  - Close is bounded at `closeWithin` as a whole.
  - Devices open outside `devMu`. One opened after close closes itself, outside the lock.
- **The jitter buffer is bounded and answers to the sender**, and `FuzzSlot` plays it.
  - It is a fixed ring of 64. Every sequence and timestamp comparison is a signed difference, so it holds
    across the wrap.
  - A payload is copied out under the lock.
  - It starts from packets that arrived, or are timestamped, within the target delay of the newest, which
    skips a stale DTX update. The timestamp is M31's, for a link slower than the sender.
  - A backlog drains in DTX gaps (M31).
  - Its delay:
    - it starts at `MinDelay`, one frame;
    - it deepens to `fecDelay` on its first concealed loss, which FEC needs;
    - it deepens at once on a late packet;
    - it shrinks after `shrinkAfter` without one;
    - it is capped at `MaxDelay`.
  - It resets only on a change of sender.
- **What is sent:**
  - every packet carries the RFC 6464 audio level, whose id is read once from the answer;
  - a DTX packet of two bytes or fewer is not sent.
- **The notices are each OS's.** `gen-third-party-notices.sh` reads the release targets from
  `.goreleaser.yaml`. It builds each with cgo off, takes the union, and adds the C components in
  `internal/notices/embedded/`. A C library compiled into a module goes there by hand (rule 22).
- **Latency has a budget**: 38–44 ms mouth to ear in process at M29, 69–73 ms with M30's DSP, whose 28.9 ms
  is `dsp.Delay`. `TestMouthToEarStaysWithinItsBudget` fails past 120 ms, and past 160 with loss and
  jitter: M30 raised it from 90 by the DSP's delay, and runs it without `-race`. A change on the path is
  read against it.
  - Its clicks are stamped at their own sample, inside their frame, never at the frame.
  - Its quiet is dithered.
- **Measuring**: `BenchmarkFrame`, `BenchmarkStart` and `BenchmarkReset` in `internal/opus`, and
  `NORITE_MEASURE=1` for what a whole call costs. Pin the run to one core.

And on the audio DSP, from M30 (ADR 0036 as amended):

- **The DSP is `voiceworker/internal/dsp`'s `Module` and `Chain`**, one `dsp.wasm` holding RNNoise 0.2, the
  APM 2.1 and abseil, built by `scripts/build-dsp-wasm.sh` from pinned tarballs and a committed source list.
  - It imports nothing: `wasi_stubs.c` defines every libc wrapper, a clock that never moves included.
    `importsNothing` holds it, with an 8 MiB cap.
  - CI rebuilds it byte for byte (`just dsp-wasm-check`), holds RNNoise's `vec_wasm.h` to plain C byte for
    byte (`just dsp-simd-check`), and the compiler to the interpreter on amd64 and arm64.
  - A change to its sources, patches or flags is a change to what every call runs.
- **The order is the worker's, in Go**: AEC, then RNNoise, then AGC, each a separate export. The APM's own
  chain has no place for RNNoise. `TestEchoIsCanceledBeforeRNNoise` fails when two are swapped.
- **The DSP runs in `send` alone**, and is fed what the speakers played through `refRing`.
  - Each captured frame carries the ring's count, and `send` renders up to it and no further: AEC3 resets
    when a sender behind renders ahead.
  - **Nothing is dropped before the DSP**: AEC3 pairs the two directions by count, and a frame lost
    shifts the rest. A sender behind skips the encode, never twice in a row.
  - The RTP timestamp is the frame's capture number, so a skip is a gap the far end reads as DTX.
- **What fails drops the chain for the rest of the call and never ends it**: a trap, an error, a DSP
  too slow for real time (`slowDSP` frames behind). The reference is a stranger's audio. The frame
  goes as captured.
- **A call into the instance that never returns ends the call**, the chain's construction included,
  through the watchdog's `dspCall`.
- **It compiles at every call start, beside the codec, and nothing waits for it**: `send` takes it up
  with `takeDSP` when it is ready. It is never cached on disk.
- **Its delay is `dsp.Delay`, 28.9 ms**, which the latency budget counts. A change on the path is read
  against `TestMouthToEarStaysWithinItsBudget`.
- **The settings are `voice.*` in `contracts/client-config.toml`.**
  - The client reads them and the join's body carries them, strictly decoded by `ipc.DecodeVoiceJoin`.
  - `voiceipc.Join.Processing` takes them to the worker, and `session.New` reads them from the join.
  - `VoiceStatus.processing` is what was asked, not what runs.
  - All off still runs RNNoise, for the V flag.
- **The level is computed after AGC2, and V follows RNNoise's probability at 0.5.**
- **AGC2 starts from no gain**, not its default 15 dB, which raised the echo canceller's residue while
  nobody spoke. A change to AGC2's settings is read against `TestAGC2StartsFromNoGain`, whose room's delay
  wanders as a real loopback's does.
- **No native twin.** Adding one is a tag and a `native.go`, and it reopens the ledger's confinement entry.
- **wazero's runtimes are made after `wasmrt.Warm`**: v1.12.0 races on its own version cache.
- **Testing**:
  - `audiotest.Room` and `audiotest.Echoing` are a synthetic room and a device that hears its own
    speakers, with a noise floor;
  - `recordings` holds the CC0 and public-domain speech and rain, stored as Opus;
  - `harness.joinWith` sets a call's DSP through its join, as the daemon does.
- **Measuring**: `BenchmarkChain` and `BenchmarkStart` in `internal/dsp`, pinned to one core.

And on adaptive bitrate, from M31:

- **The controller is `voiceworker/internal/congestion`'s, not pion's GCC.**
  - It reads the queue on the uplink from the SFU's transport-wide feedback: each packet's arrival minus
    its departure, against a ten-second minimum.
  - Loss is its second signal: with a queue standing, or past 25% without one.
  - Loss below that sizes the FEC (`Encoder.SetLoss`), never the bitrate.
  - pion v0.1.51's GCC clamps its loss half to 100 kbps and lets a full queue stand. A pion upgrade is not
    a reason to switch back unless it has been measured against the shaper.
- **The sender numbers its own packets.**
  - The transport-cc extension is written in `send`, beside the audio level, and each departure is noted
    there before a test's `Lossy` drops the packet, so the feedback sees the loss.
  - No congestion interceptor is registered.
- **Loss is counted in packets, not feedbacks.**
  - It is judged only once the one-second window holds ten packets, which DTX never fills. That holds for
    the FEC too: it keeps the last loss judged on a full window.
  - The bitrate is cut on the loss since the last cut loss made, never again on what it acted on.
  - A packet counted lost and then reported received was reordered, and is taken back out.
  - The FEC is sized for at least `MinLossPercent`, which equals the encoder's starting 5%: libopus
    turns FEC off at 0%.
  - Symbol 3 (received, no delta) takes no delta, and says nothing of the queue.
- **The bitrate climbs only while packets flow.** Through DTX the feedback carries a comfort-noise update
  every 400 ms, which says nothing about what speech needs. The spike watched GCC climb into a link it did
  not fit.
- **The controller is applied in `send`, read from the controller by polling.** `adapt` sets the bitrate
  and the loss on a change, under `clocked`, as an encode is. Nothing pushes a target from another
  goroutine: GCC's callback ran each change on a goroutine of its own, out of order.
- **The SFU's RTCP is bounded before Pion parses it,** by `session.rtcpGuard`. The guard is first in the
  interceptor chain, so it is the innermost reader. Pion's report interceptors parse every packet in full
  inside the read, and a feedback claiming 8,191 received packets in two bytes allocates a delta for each.
  - **What the guard lets through:**
    - `maxRTCP` packets a second per stream;
    - only feedback that carries the bytes its deltas need.
  - **What the controller reads:** `congestion.MaxStatuses` packets of one feedback, and each departure it
    recorded only once. A departure it did not record is ignored, so forged feedback moves nothing.
  - **A packet Pion cannot parse costs that packet.** `readRTCP` keeps reading.
  - `TestTheGuardIsUnderPionsInterceptors` holds the guard's place in the chain.
- **20 ms frames need about 41 kbps of link at the 16 kbps floor** (architecture.md §17). Nothing here
  serves a slower link.
- **The jitter buffer drains a backlog in DTX gaps, and judges freshness by timestamp as well as arrival**
  (M31's two fixes to M29's buffer). What M31 leaves at the far end is the buffer's depth after a burst of
  late packets: it sheds a frame every ten seconds.
- **Testing:**
  - `congestion_test.go`'s `link` is a token bucket with a drop-tail queue, on the test's clock, whose
    feedback pion's own `twcc.Recorder` builds as the SFU does.
  - End to end, `shaper_test.go` is a UDP hop between one caller and the SFU, reached by rewriting the
    answer's candidate port (`through`).
  - `TestTheBitrateFollowsAConstrainedLink` runs it with the controller and without (`fixedBitrate`), and
    asserts at the far end.

And on the first run, from the `easier-setup` branch:

- **The first run is `norite setup`**, in `cli/internal/login` beside the commands it runs: register, login,
  the daemon, then the invite's guild, with the daemon first when a sign-in is already stored, since it is
  what can say whether the instance still takes it. It calls those flows rather than copies of them, and a
  step already done is said and skipped, a guild by the list of the account's guilds (`ops.InGuild`), since
  redeeming again answers as a first join does. `install.sh` and `install.ps1` run it for their account step
  when there is a terminal to ask on, which `install.ps1` asks of the console and never of
  `[Environment]::UserInteractive`, false over SSH; and a package's note names it. A new first-run question
  goes in it, never back into a script.
- **An invite link is the instance's address, `/join`, and the guild invite code after `#`.** The code is
  a fragment because a browser never sends one, so a link opened in a browser keeps it out of every log.
  `norite invite create` and `list` print it, on the instance that answered (`Response.instance_url`,
  `daemonclient.CallFrom`), never READY's, which names none while the daemon starts; one naming this
  machine alone is printed with a warning.
- **A link is read in one place, `ops.ParseInvite`**, which refuses whatever a link does not need: a
  query, a path not ending at `/join`, a dot segment, a username, and before the `#` any character an
  address is not written with (`linkChar`), since its instance is printed back unquoted in commands to
  paste and `url.Parse` takes `$(…)` in a host. Its instance goes through `ParseInstanceURL`.
- **A code is sent only to the instance its link names, and the daemon checks it.** The request names the
  instance (`ipc.Request.instance_url`, sent by `ipc.Client.DoFor`, which `daemonclient.CallFor` calls for
  `PreviewInvite`, `JoinInvite` and `RevokeInvite`), and the relay compares it with the credential it is
  about to send, after waiting for a daemon still starting, answering `other_instance` with nothing sent
  when they differ. The instance is a parameter, never a value on the context: a connection that cannot
  carry it has no `DoFor`, and `CallFor` refuses it with nothing sent (`ErrCannotNameInstance`), where a
  context dropped on the way would lose it silently. A client checking against READY refused good links
  while the daemon started and could not see a sign-in that changed after it. A bare code in `setup` names
  the instance set up for. Instances are compared by `ipc.SameInstance`, the one comparison, which the
  automation port uses too: scheme, host without case or a final dot, the port a connection uses (`:443`
  is none), and path.
- **Both new frame fields are MINOR changes (M20), and the answer's is on every relayed answer**, so a
  client a PATCH behind fails every request against a daemon carrying it. The release carrying them is
  `v0.2.0`, and until it no PATCH is cut from `main`: one comes from a branch off the last tag (ADR 0033
  §3). **Until `v0.2.0` exists, the installers' account step fails**: they install the newest release,
  which has no `norite setup`. Accepted by the author rather than worked around, and both alpha guides say
  so and give the way round, a checkout's build or setup's steps by hand.
- **A link's instance is confirmed before anything is sent there**, since it decides where a password
  goes. Signing a machine in to another instance than the one stored is asked, default no. A link that
  writes the stored instance, or `--instance`, another way (a final dot, `:443`) is that instance, and
  `setup` then uses the stored or typed spelling: the comparison that skips the question, or accepts the
  pair, is no reason to send a password to an address nobody typed.
- **A guild invite never creates an account on an `invite` instance.** The two codes stay apart, and
  `setup` asks for the instance's when the instance asks for one or refuses the one given
  (`invite_required`, `invite_invalid`).
- **A sign-in straight after registering waits for Enter.** One refused on an unconfirmed account mails a
  new link and spends the one registration sent (M10), so trying at once kills the link the person is
  about to open. After a refusal the account exists, so the way on is `norite login`, never `setup` again.
- **A stored record is not a sign-in the instance accepts.** `setup` asks the daemon, by a relayed
  `GET /users/@me` naming the instance: a 2xx carrying the record's own account id is accepted, a 401 or a
  daemon signed out for a moment (`settle`) refused, and anything else, a 503, a 429, a proxy's 502, a
  portal's 200 the relay dropped as not JSON, another account, or a record naming no account id, which a
  sign-in whose own `/users/@me` failed writes, says nothing, and joins nothing. With no daemon to ask it
  claims nothing either way. Where the join goes is the socket's answer, never the service manager's. The
  join waits on one attachment, asking again while the daemon answers signed out, signed in elsewhere or
  without a session from the instance yet, and while the instance answers anything but the account or a
  401; anything else, or the wait running out, leaves the join for later with the command to run. With
  `--no-daemon` the socket is asked once. **A sign-in the instance refuses reaches the client as signed
  out**, not as a 401: the relay renews on a 401 and answers a renewal refused that way, which is also a
  daemon that has not read a sign-in stored a moment ago. So `standing` asks again for a moment before it
  signs in again, and the wait's message names both causes, and no length of time, since a probe may
  outlast the deadline. A command to join later is the invite as checked (`ops.Invite.Canonical`), a bare
  code as a link to the instance set up for, never the bytes typed.
- **A command to paste quotes what came from a person or a link** with `output.ShellWord`, POSIX quoting
  and on Windows PowerShell's: `norite setup`, `norite register`, `norite instance bootstrap` and
  `norite invite show`, which rebuilds the link it was given from the link's instance and the code the
  instance issues, so a link keeps its instance, and nothing typed is repeated through termsafe. The
  installers quote the link they were given the same way, and refuse one holding what termsafe removes,
  since quoting keeps a link from the shell and not from the terminal. Pasted into Windows PowerShell 5.1
  at the manual pass, every word read back as quoted; 5.1 passes a program a `"` inside a word, or a `\`
  ending one that holds a space, wrongly, which only an email's quoted local part can hold.
- **`GET /join` reads nothing from the request.** It says how to install and run `norite setup`, names the
  instance by `public_base_url` alone, and is mounted unconditionally, with HEAD and `/join/` beside it.
  Its installer commands pin the instance's own release (`releaseTag(meta.Version)`), and fetch the
  scripts at its tag: the newest release is otherwise what they install, before 1.0 an instance refuses
  another MINOR, and a later script on `main` may expect archives the release lacks. Every page shares
  `httpx.PageStyle`.
- **A code the instance would not issue is refused, in `ops`** (`IssuedCode`, `ErrNotAnIssuedCode`):
  `CreateInvite` and `PreviewInvite` refuse the answer, and `ListInvites` leaves the row out and counts it,
  which `invite list` says beside the rows, or on stderr with `--json`, so one bad row hides no good one.
  Sixteen letters of its alphabet, which `openapi.yaml`'s `GuildInvite.code` pattern states and a test on each
  side holds the CLI's copy and the backend's codes to, the two modules being unable to import each other. **A
  link is returned only when `ParseInvite` reads it back** as its instance and its code (`ops.InviteLink`),
  written as it read the instance back, so an instance whose path no link holds gets `link: null`.
- **`ParseInstanceURL` refuses a percent-encoded path, a query and a fragment.** It rebuilt the instance
  from the decoded path, so a `%3F` came back a query: requests went to the path before it, and
  `SameInstance` called the two one instance. A query or a fragment was dropped without a word, so a link
  pasted for an address became an instance at `/join`. A record written before holds the decoded `?` or
  `#`, and `Validate` refuses it when it is next read: the M7 rule for a value that also bounds something.
- **Testing setup**: `setupInstance` in `setup_test.go` registers and signs in over loopback, and its
  `Connect` attaches to `daemontest`'s daemon, which `SignInFrom` makes answer each request as signed in to
  whatever the store holds then, as the real daemon is a moment after a sign-in, checking a named instance
  as the relay does, bound included (`ipc.CheckInstanceURL`, which both ask). `AsOlderDaemon` answers
  without naming the instance, one of the two ways to `link: null`, the other an instance whose address no
  link holds. An answer the contract forbids,
  which the fake refuses to script, is played by a `Caller` of the test's own (`answering` in
  `invite_test.go`). The package's `TestMain` clears `NORITE_INSTANCE` and `NORITE_PASSWORD`.

## Project-specific skills

**Not in the repository** — `.gitignore` excludes `.claude/`, so these live on the maintainer's machine
only. A fresh clone has none of them. They encode conventions that are all written down here and in
`docs/`, so nothing is lost by their absence; treat the list below as a description of the workflows, not
as files to open. Anything a skill enforces that is *not* also stated in this file or `docs/` is a
documentation bug — fix it here rather than relying on an untracked file.

Where they exist, invoke with `/<name>`:

- `/onboard` — **mandatory orientation for a fresh session**, and the one to run before any other. Its
  core is a rule pass that cannot be skipped on the grounds that this file is already in context: having a
  document loaded and having read it are different things, and the failure mode of the first is an agent
  that violates a rule it could quote. It requires all 24 to be restated **in the agent's own words**
  before any work is proposed — paraphrase rather than quotation, because copying proves retrieval and
  paraphrasing proves reading, and because the result is visible to whoever is reading the output. It then
  has the session derive milestone state from `git` rather than from "Milestone status" above, which is
  written at completion and is behind by design mid-milestone; read the authority its task needs rather
  than the ~7,000-line doc set; and run the enumerated-fact self-checks. It gives the authorship rule a
  section of its own, because an agent's harness actively instructs it to add all three forbidden shapes
  and the local hook is a partial, untracked backstop rather than a guarantee. It also sends the session
  to `README.md` — the only outside-in view of the project, and the one place a wrong claim is wrong in
  public. **It deliberately contains no rules of its own** — a second copy in an untracked file is the
  drift this section warns about, so it points at this file and defers.
- `/new-endpoint` — scaffold a new REST route (sqlc query → service → handler → OpenAPI contract → tests).
- `/new-gateway-event` — scaffold a new real-time dispatch event end-to-end (backend publish → schema →
  frontend/daemon-side zod/dispatcher).
- `/db-migration` — add a schema migration with the indexes its queries need, wired through sqlc.
- `/security-audit` — this project's own security checklist (permissions, audit log, token/credential
  hygiene, plugin sandbox boundaries, E2E exclusion, XSS/terminal-escape safety, secrets) — complements the
  generic `/security-review` skill.
- `/security-sweep` — the third security mode, and the one whose durable half is committed. `/security-audit`
  walks the 24 rules and cannot find what no rule covers; `/security-review` finds OWASP-class problems and
  **destroys everything it does not report** — a confidence floor of 8 plus category exclusions that forbid
  reporting unbounded growth and rate limiting, both of which are invariants here. The sweep is that
  discovery with no floor and no exclusions, where **verifying each candidate against the code replaces the
  threshold as the noise control**, and every candidate ends in one of three buckets: fix now, deferred to a
  named roadmap entry, or rejected into `docs/security-ledger.md`.
- `/optimization-review` — the performance counterpart to `/security-audit`, against `docs/architecture.md`
  §15 and rule 7: hot-path identification first, then missing indexes, N+1s, avoidable round trips,
  per-request work that belongs at construction, allocation, backpressure and payload shape. Findings carry
  an evidence tier (Measured / Structural; speculative ones are not reported), and it refuses the
  optimizations that would trade a non-negotiable rule — cached permissions, an async audit write, a
  dispatch moved inside its transaction, a fan-out that skips the `blocks` check.
- `/ship-milestone` — the git workflow above in operational form: commit and PR-title length checks, the PR
  template followed literally, the squash-merge subject/description, and the tag.

## Where to look for more detail

The doc set has one authority per topic — if two files seem to cover the same ground, that is drift and
should be fixed, not tolerated:

- `docs/design/tui/` — **what the terminal client looks like and does.** `SCREENS.md` (27 screens with
  stable ids `1a`…`7a`), `KEYMAP.md`, `TOKENS.md`, and `README.md` (the grid, the responsive rules, and the
  corrections applied to the original handoff). Normative: milestones cite screen ids rather than restating
  them, and `mockups.dc.html` is an illustrative rendering, not authoritative where it disagrees.
- `docs/architecture.md` — **what the system is.** Full Postgres DDL, the permission-resolution algorithm,
  the exact gateway op-code/frame shapes, the daemon/CLI/TUI/GUI design, the voice architecture, the complete
  REST endpoint list, the OAuth linking/PKCE flow, deep dives on security (§14) and performance (§15), and
  the known tensions and accepted limitations (§17). Read it before making an architectural decision this
  file doesn't already cover.
- `docs/roadmap.md` — **what gets built, in what order.** `M0`–`M125`, phase-grouped, with Phase P running
  as an explicitly parallel track. The single source of truth for milestone numbering *and* for what each
  milestone owes — read the entry rather than a summary of it, including this one, because a restatement
  here is a second copy that drifts. `architecture.md` §13 only points here, for the same reason.
- `docs/security-ledger.md` — **every security finding this project rejected, and why.** Not a
  vulnerability list (those get fixed, and the commit is the record) and not a backlog (a finding worth
  doing later goes in the roadmap entry that inherits it). It holds the third case, and it exists because
  a filtered finding is a destroyed one: on the M13 branch a 403-versus-404 oracle was filtered at
  confidence 6, vanished, was re-derived by the next review pass, and was real. Every other kind of
  decision here has somewhere durable — ADRs, roadmap entries, the optimization review's "deliberate
  non-optimizations confirmed" — and security rejections were the one kind with nowhere, so they came
  back. **Read it before reporting a finding**; each entry carries the condition that would reopen it.
- `docs/trying-the-alpha.md` — **how somebody runs an instance and joins it from other machines**, linked
  from the README rather than inside it, with `docs/trying-the-alpha-on-windows.md` for joining from a
  Windows PC. They are the alpha's guides, not self-hosting documentation, which is M96's and replaces
  them. Their commands are ones a stranger will paste, so a change to a flag, a default or a config key
  they name is a reason to walk them again.
- `docs/adr/` — **why the contested calls went the way they did.** Superseded ADRs stay, marked as such in
  both directions. `SECURITY.md` covers vulnerability-reporting
process, not architecture.
