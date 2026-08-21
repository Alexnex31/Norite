# CLAUDE.md

This file gives Claude Code (and any other AI agent) the context needed to work on this repo without
re-deriving decisions already made. The full architecture/design rationale lives in `docs/architecture.md`
(committed in this repo) — **read that file for anything not covered here**; this file is the fast-loading
summary + the rules that must hold on every change. `docs/adr/` has short, focused records of the most
contested individual decisions — read the relevant one before proposing to change any of them.

## What this is

Norite is a voice-and-text chat platform. The primary way to use it is the free, global, publicly-hosted
flagship instance — self-hosting is a real, fully-built, one-time-purchase-licensed feature (aimed at
enterprises and other private groups who want their own instance), not the platform's core identity. Source
visible but under no public license (all rights reserved). **Four clients**: a scriptable CLI (the
command tree — one action, exit, pipeable), a full-screen **TUI** (the in-terminal application: panes,
chords, 25 specified screens), a native GUI mirroring the TUI's information architecture, and a
lower-priority web SPA built later. The CLI, TUI and GUI share one local background
daemon per OS user account; the CLI and TUI share one command tree, so `M-x` in the TUI runs every verb
(ADR 0026). "CLI" here means the command tree only; where it once meant both, that conflation
is what left the roadmap with six milestones of TUI capabilities and none that drew a screen.
Servers ("guilds"), channels, roles/permissions, real-time text and voice
messaging, DMs, presence, invites, public matchmaking, BYOK end-to-end encryption, client-side plugins, and
more all ship as real v1 scope — see `docs/architecture.md` Section 7 for the full list. **No public
license** — default copyright, all rights reserved; self-hosted customers are granted rights individually via
a signed license file, not a public license text. Not AGPL, not open source. See
`docs/adr/0007-licensing-and-project-posture.md`.

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
  (MAJOR must match exactly, a defined MINOR-version-back window is tolerated). REST is used for all CRUD;
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
`alexedwards/argon2id`, `golang-jwt/jwt/v5`, `golang.org/x/oauth2`, `go-playground/validator/v10`,
`golang-migrate`, `oapi-codegen`, `ulule/limiter`, `testify` + `testcontainers-go`, Postgres (`tsvector` +
GIN + `pg_trgm` for search), Redis (activated only for the flagship's horizontal-scale event bus/rate
limiting; self-hosted single-process instances never touch it).

**CLI** (command tree): Go, `urfave/cli` v3 (command tree, flag parsing, `--json`/`--help`, completions —
chosen over `spf13/cobra`), `pelletier/go-toml` v2.

**TUI** (in-terminal client): Bubble Tea + Lip Gloss + Bubbles (Charm stack) with a custom pane/split
engine, `teatest` for testing, `BourgeoisBear/rasterm` for inline images. Screens, keymap and tokens are
specified in `docs/design/tui/` and are normative.

**Native GUI**: Go, Gio (`gioui.org`) — immediate-mode, hand-built widgets, golden-image testing for the
highest-value surfaces.

**Daemon**: Go, `wazero` (WASM plugin sandbox, pure Go/no cgo), `zalando/go-keyring`, `fsnotify`,
`gofrs/flock`, `modernc.org/sqlite` (E2E keystore, pure Go/no cgo).

**Voice-worker**: Go, `pion/webrtc` + `pion/interceptor`, `hraban/opus`/RNNoise/WebRTC APM (AEC3, cgo —
the *only* binary in the stack where cgo is allowed; AEC3 replaces `libspeexdsp` per ADR 0023).

**Web SPA** (later, tertiary client): React, TypeScript, Vite, TanStack Query, Zustand, Zod, Tailwind +
shadcn/ui, `react-hook-form`, Vitest + Playwright.

## Non-negotiable rules

These apply to every milestone, not just a final pass — treat a PR that violates these as incomplete, not
"good enough for now":

1. **Every mutating handler checks permissions before writing.** Resolve effective permissions via
   `roles.Resolve(...)` (or an explicit hierarchy check for role/member management) using data freshly
   loaded for the *specific* guild/channel in the request path. Never trust a client-supplied ID without
   verifying it belongs to the actor's claimed context.
2. **Every guild-scoped mutation writes an audit log entry**, in the same DB transaction as the mutation.
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
21. **Any new REST endpoint or gateway event affecting CLI-observable or browser-observable state must be
    sanity-checked against real browser constraints** (CORS, request chattiness, BFF-auth-compatibility) at
    the time it's added — never deferred silently to Phase O just because the web client isn't built yet.

## Directory layout (see `docs/architecture.md` §1 for full detail)

```
backend/       Go modular monolith — cmd/server, internal/{config,platform,auth,users,guilds,channels,
               roles,messages,gateway,presence,voice,db}, migrations/
cli/           The `norite` binary — the scriptable command tree (internal/cliapp) *and* the TUI
               (shell, panes, chords, screens); one binary, two front ends onto one command tree
gui/           The native GUI — Gio app, mirrors the TUI's screens; shares the daemon/config model
daemon/        Shared background daemon — gateway client, dual IPC, plugin host, config/state files
internal/voice/  Pion-based SFU, embedded TURN server (lives under backend/, server-side infra)
contracts/     openapi.yaml (REST), gateway-events.schema.json (WS), CLI --json schemas — source of truth
docker/        docker-compose.yml (postgres, redis, backend hot-reload) — local dev + self-hosted prod option
frontend/      React SPA — the later, tertiary web client (Phase O)
```

## Common commands

- `just dev` — run the full local stack (docker-compose: postgres, redis, backend w/ air; frontend w/ vite
  joins at Phase O)
- `just test` — every Go module's tests. **Needs a running container runtime**: the backend's integration
  tests start a real Postgres via `testcontainers-go`. Frontend tests join at Phase O.
- `just test-short` — unit tests only, skipping everything container-backed. Fast inner loop, not a
  substitute for `just test` before pushing.
- `just lint` — `go vet` + `golangci-lint` per module (frontend ESLint/`tsc --noEmit` at Phase O)
- `just db-migrate` — apply pending migrations (runs the server's own `-migrate-only` mode, so it takes the
  exact same advisory-lock-guarded code path a real startup does)
- `just sqlc-generate` / `just sqlc-check` — regenerate the committed sqlc layer / fail if it's stale
- `just security-scan` — `govulncheck ./...` (+ `pnpm audit` and `Trivy` once frontend/ and Dockerfiles
  exist)

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
noise. Together the merge commit, the PR and the tag are how history stays navigable: **`git log main
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

Practical constraint, settled at M1: the `gh` CLI is **not** installed on the dev machine, so PRs are
opened and squash-merged through the GitHub web UI. An agent can prepare the branch, push it, and draft the
PR title/body, but cannot open or merge the PR itself — hand that off rather than trying to automate it.
Install and authenticate `gh` if you want that to change.

## Milestone status

**Phase A (foundation), through M9.** Full dependency-ordered roadmap (`M0` through `M125`, phase-grouped,
with Phase P — the flagship Kubernetes deployment — running as an explicitly parallel track) is in
`docs/roadmap.md`.

- **M0 — monorepo scaffolding**: done (tag `m0`).
- **M1 — backend skeleton**: done (tag `m1`). `internal/config` (typed, env-bound, validated at startup),
  `internal/platform/{logging,httpx,database,ratelimit}`, `internal/db` (sqlc-generated, pgx/v5),
  `migrations/` (go:embed'd; `000001_init` is intentionally empty), and the `cmd/server` composition root.
- **M2 — CLI skeleton and `norite instance init`**: done (tag `m2`). Scope was reshuffled with M3 (settled
  2026-08-10): the `norite` command tree landed here, since the wizard needs it, and M3 is the daemon
  lifecycle stub alone.
- **M3 — daemon lifecycle stub**: done (tag `m3`). `daemon/internal/{daemonproc,paths}` (single-instance
  flock, `RLIMIT_NOFILE` raise, lumberjack log, clean shutdown) and `cli/internal/daemonctl` (the
  `norite daemon` command group over a systemd-user / launchd-agent / Windows-logon-task `Manager`).
- **M4 — backend auth core**: done (tag `m4`). `internal/platform/snowflake` (IDs), `internal/platform/dbtest`
  (the shared container harness every domain package's tests use), migration `000002_auth`, and
  `internal/auth` — argon2id, HS256 access tokens, device-scoped refresh families, scoped `api_tokens`, the
  Bearer middleware. Decisions recorded in ADR 0022; the voice-connection reasoning that settles the signing
  algorithm is ADR 0023. First milestone merged with a merge commit rather than a squash, which is what
  settled that as the default going forward — `main` carries its seven sub-commits directly.
- **M5 — transactional email and password reset**: done (tag `m5`). `internal/mail` (a `wneessen/go-mail`
  sender behind a bounded queue whose `Enqueue` cannot block), migration `000003_password_reset`, the
  always-202 request and single-use confirm endpoints, and the server-rendered `/reset` page — this
  codebase's first HTML surface, which is why `httpx.HTMLPage` exists. The same PR carried nine fixes from
  a repo-wide review of already-merged M1/M2/M4 code.
- **M6 — OAuth backend flow**: done (tag `m6`). `golang.org/x/oauth2` with PKCE for Google and GitHub,
  migration `000004_oauth` (`oauth_identities`, plus `oauth_states` and `oauth_exchange_codes` — both
  deliberate additions), the callback's server-rendered pages, and `auth.RunSweeper`, which expires the
  short-lived rows M5 and M6 create because no milestone ever did. Decisions in ADR 0024: a provider is
  trusted for one thing, nothing is written to `users` until a username is chosen, and a sign-in is bound
  to the client that started it. Two review passes ran against the branch and everything they found was
  fixed on it.
- **M7 — CLI `norite login`, password plus keychain**: done (tag `m7`). `daemon/credentials` (the stored
  session: a keyring-or-file secret, the non-secret record beside it, and this installation's device
  identity in a third file), `cli/internal/login`, `cli/internal/termsafe`, and the daemon's startup
  sign-in. Decisions in ADR 0025: the keyring where the machine has one and a `0600` file in the `0700`
  state directory where it does not, chosen by *writing* a probe rather than reading one, and never
  silently. The repository's first cross-module dependency (`cli` → `daemon`) starts here, because the
  daemon owns what a stored credential is. Rule 19's sanitizer landed here too rather than at M43, since
  this is the first command that prints a name a stranger's instance chose.
- **M8 — CLI OAuth loopback flow**: done (tag `m8`). `cli/internal/login`'s loopback listener, browser
  launcher and flow binding, plus the backend half M6 did not build: an optional `client_redirect_uri` at
  `/authorize`, so the callback returns the exchange code to a listener instead of rendering it (migration
  `000006`). Decisions in ADR 0027, which also corrects four documents that described the loopback port as
  registered *with the provider* — a design requiring the client secret in the CLI binary. What is
  registered is the instance's own callback, unchanged since M6.
- **M9 — CLI headless device-code fallback**: done (tag `m9`). Migration `000007`, `internal/auth`'s
  `device.go`/`devicetoken.go`/`devicepage.go`/`devicehttp.go`, the verification page at `/device`, and
  `cli/internal/login`'s `headless.go` and `devicecode.go`. Decisions in ADR 0028: the completion page
  offers providers and not only a password (which is what makes the milestone worth its size), approval is
  a separate explicit step, and the user code is the one credential-shaped value stored in plaintext.
- **M10 — `norite instance init` finish, and registration hardening**: next.

What exists on the backend today, and the conventions the next milestone should follow rather than
re-derive:

- **Startup order** (`cmd/server/main.go`): config → pool (verified with a real round trip) → HTTP listener
  → blocking advisory-lock-guarded migration → *then* readiness. `/api/v1/healthz` answers 503
  `{"status":"starting"}` until migrations finish, then 200. `-migrate-only` runs migrations and exits —
  that's what `just db-migrate` and, later, the flagship's Helm pre-upgrade Job use.
- **Middleware chain** (`cmd/server/router.go`), fixed by `docs/architecture.md` §2:
  SanitizeInboundRequestID → RequestID → EchoRequestID → RealIP *(mounted only when
  `NORITE_TRUST_PROXY_HEADERS=true`)* → Recoverer → SecureHeaders → StructuredLogger → RateLimit.
  `AuthenticateBearer` slots in below RateLimit at M4. SanitizeInboundRequestID and RealIP are one decision
  made twice — whether a client-supplied forwarded header may be believed — and both take it from that one
  setting, at the top, where nothing below re-opens it. A request ID is not cosmetic: it is echoed to the
  client, written to every log line, and returned in every error body. Domain routers mount in the
  rate-limited group inside `/api/v1`; `/healthz` sits outside it on purpose.
- **Errors**: return `httpx.ErrNotFound` / `ErrForbidden` / … (or `httpx.Errorf(sentinel, …)`) from
  services and let `httpx.WriteError` map them. 5xx detail is logged, never returned. Every response
  carries `X-Request-Id`; every error body carries `request_id`.
- **Transactions**: `database.RunInTx`. Mutation + audit-log write go in one `fn` (rule 2); publish gateway
  events *after* it returns nil (rule 5).
- **Rate limiting**: always build limiters through `internal/platform/ratelimit` — it is what enforces the
  global "IPv6 groups by /64" rule (rule-adjacent, `docs/architecture.md` §11). Give a new stricter limit
  its own `Bucket`.
- **sqlc**: add `.sql` files under `backend/internal/db/queries/`, run `just sqlc-generate`, commit the
  output. CI's `codegen` job fails if the committed output is stale.
- **Logging**: `logging.FromContext(ctx)` inside handlers. The request logger deliberately omits query
  strings, headers, and bodies (rule 8) — don't add them back.
- **Toolchain pinning**: `golangci-lint` (config in `.golangci.yml`) and `sqlc` are pinned to exact
  versions in *two* places that must move together — `.github/workflows/ci.yml`'s `env:` block and the
  justfile's variables. `just lint` warns when your local golangci-lint differs. Also note golangci-lint
  must be built with Go >= the workspace's highest `go` directive (1.25.0), which is why the lint action
  is v9/golangci-lint v2 rather than the v6/v1 pair M0 started with.

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
  `instanceinit.ErrNotATerminal` is the shape to follow; `main` maps it to exit code 2.
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
  service manager treat an ordinary stop as a crash and restart it), 3 for "already running" — which the
  systemd unit is told never to retry (`RestartPreventExitStatus=3`; launchd has no per-code equivalent and
  is throttled instead). `norite daemon status`: 0 running, 1 stopped, 2 not installed. Changing any of
  these means changing the unit/plist template in the same commit.
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
- **Every credential is stored only as a hash.** Passwords argon2id, opaque tokens SHA-256. The raw value of
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
- **Untrusted text goes through `cli/internal/termsafe` before it reaches a terminal** (rule 19). `Text`
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
- **What the store cannot finish, it says through `Notify`** — a credential left in a backend this session
  cannot reach, a previous record too broken to name one. Failing instead would make `norite login`
  impossible over SSH on any machine that once logged in at its desktop; staying silent would hide a live
  token from the only person who can deal with it. The CLI prints it, the daemon logs it.

Three things this milestone deliberately leaves for the milestone that can do them properly:

- **A dropped refresh token cannot be revoked yet.** When a login lands mid-refresh, the daemon discards
  the token it just obtained; it stays valid at the instance until it expires. Handing it back needs M11's
  revoke-a-session primitive, and reaching for it from the daemon needs M19's gateway connection.
- **The daemon never re-probes for a keyring that unlocks later.** The backend is chosen once per process
  (`sync.Once`), so a daemon that started before the session keyring was unlocked keeps reading the file
  path for its whole life. Correct today because the record names the backend; worth revisiting when the
  daemon becomes long-lived and reconnecting at M19.
- **`termsafe` lives in the CLI module and the daemon cannot import it.** Fine while every value the daemon
  logs was sanitized by the login that stored it. At M19 the daemon fetches names of its own, and the
  function has to move somewhere both modules reach — not be copied.

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
  that would shorten those screens is reopening that decision.
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
- **Two continuations on the page, not one.** An entry token says a browser has entered a live code; an
  approval token says it has also proved whose account this is. `parseDeviceToken` takes the type it wants
  rather than reporting the type it found, because a single token with an optional user field would
  authorize before authentication happened.
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

## Project-specific skills

**Not in the repository** — `.gitignore` excludes `.claude/`, so these live on the maintainer's machine
only. A fresh clone has none of them. They encode conventions that are all written down here and in
`docs/`, so nothing is lost by their absence; treat the list below as a description of the workflows, not
as files to open. Anything a skill enforces that is *not* also stated in this file or `docs/` is a
documentation bug — fix it here rather than relying on an untracked file.

Where they exist, invoke with `/<name>`:

- `/new-endpoint` — scaffold a new REST route (sqlc query → service → handler → OpenAPI contract → tests).
- `/new-gateway-event` — scaffold a new real-time dispatch event end-to-end (backend publish → schema →
  frontend/daemon-side zod/dispatcher).
- `/db-migration` — add a schema migration with the indexes its queries need, wired through sqlc.
- `/security-audit` — this project's own security checklist (permissions, audit log, token/credential
  hygiene, plugin sandbox boundaries, E2E exclusion, XSS/terminal-escape safety, secrets) — complements the
  generic `/security-review` skill.
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

- `docs/design/tui/` — **what the terminal client looks like and does.** `SCREENS.md` (25 screens with
  stable ids `1a`…`7a`), `KEYMAP.md`, `TOKENS.md`, and `README.md` (the grid, the responsive rules, and the
  corrections applied to the original handoff). Normative: milestones cite screen ids rather than restating
  them, and `mockups.dc.html` is an illustrative rendering, not authoritative where it disagrees.
- `docs/architecture.md` — **what the system is.** Full Postgres DDL, the permission-resolution algorithm,
  the exact gateway op-code/frame shapes, the daemon/CLI/TUI/GUI design, the voice architecture, the complete
  REST endpoint list, the OAuth linking/PKCE flow, deep dives on security (§14) and performance (§15), and
  the known tensions and accepted limitations (§17). Read it before making an architectural decision this
  file doesn't already cover.
- `docs/roadmap.md` — **what gets built, in what order.** `M0`–`M125`, each with scope, dependencies, and a
  checkable "done when". The single source of truth for milestone numbering; `architecture.md` §13 only
  points here.
- `docs/adr/` — **why the contested calls went the way they did.** Superseded ADRs stay, marked as such in
  both directions. `SECURITY.md` covers vulnerability-reporting
process, not architecture.
