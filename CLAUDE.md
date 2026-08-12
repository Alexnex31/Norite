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
visible but under no public license (all rights reserved). Three clients: a
scriptable CLI, a native GUI, and a lower-priority web SPA built later — all sharing one local background
daemon per OS user account. Servers ("guilds"), channels, roles/permissions, real-time text and voice
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
  state). The same protocol is reused, unmodified, over the local daemon↔CLI/GUI socket.
- **IDs**: Discord-style Snowflakes (`bigint`, time-sortable), not UUIDs, not serial — see
  `internal/platform/snowflake`. Always JSON-marshaled as a quoted string (avoid JS float64 precision loss).
- **Auth transport**: token-based (Bearer access + refresh tokens, `device_id`-scoped families, OS-keychain
  storage via `zalando/go-keyring`, scoped `api_tokens`) is primary and exclusive for the CLI/GUI — the
  daemon is the sole holder of its account's tokens, never an attach client. Cookie/CSRF is retired entirely
  for the CLI/GUI/daemon REST surface; it returns, decided but not yet built, as a BFF-style httpOnly-cookie
  exchange layer in front of the same token API once the web SPA is built (Phase O).
- **E2E encryption**: the daemon — never the CLI/GUI independently — holds the E2E keystore/ratchet state
  and performs decryption, relaying plaintext to attach clients over the already-trusted local IPC socket.
  Opt-in, `DM` channel type only.

## Tech stack

**Backend**: Go, `chi` (router), `sqlc` + `pgx/v5` (DB access — no ORM), `coder/websocket`,
`alexedwards/argon2id`, `golang-jwt/jwt/v5`, `golang.org/x/oauth2`, `go-playground/validator/v10`,
`golang-migrate`, `oapi-codegen`, `ulule/limiter`, `testify` + `testcontainers-go`, Postgres (`tsvector` +
GIN + `pg_trgm` for search), Redis (activated only for the flagship's horizontal-scale event bus/rate
limiting; self-hosted single-process instances never touch it).

**CLI**: Go, `urfave/cli` v3 (command tree, flag parsing, `--json`/`--help`, completions — chosen over
`spf13/cobra`), Bubble Tea + Lip Gloss + Bubbles (Charm stack, the interactive TUI layer only), `teatest`
for testing, `pelletier/go-toml` v2.

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
   token-authenticated CLI/GUI/daemon REST surface has no CSRF exposure at all (no ambient browser
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
16. **Any new local IPC surface** (daemon↔CLI/GUI socket, the local bot-automation port) **must state its
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
cli/           The `norite` CLI — Bubble Tea/Lip Gloss/Bubbles TUI, pane engine, keybindings
gui/           The native GUI — Gio app, shares the daemon/config model with cli/
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

**Phase A (foundation), through M4.** Full dependency-ordered roadmap (`M0` through `M117`, phase-grouped,
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
- **M5 — transactional email and password reset**: in progress.

What exists on the backend today, and the conventions the next milestone should follow rather than
re-derive:

- **Startup order** (`cmd/server/main.go`): config → pool (verified with a real round trip) → HTTP listener
  → blocking advisory-lock-guarded migration → *then* readiness. `/api/v1/healthz` answers 503
  `{"status":"starting"}` until migrations finish, then 200. `-migrate-only` runs migrations and exits —
  that's what `just db-migrate` and, later, the flagship's Helm pre-upgrade Job use.
- **Middleware chain** (`cmd/server/router.go`), fixed by `docs/architecture.md` §2: RequestID →
  EchoRequestID → RealIP *(only when `NORITE_TRUST_PROXY_HEADERS=true`)* → Recoverer → SecureHeaders →
  StructuredLogger → RateLimit. `AuthenticateBearer` slots in below RateLimit at M4. Domain routers mount
  in the rate-limited group inside `/api/v1`; `/healthz` sits outside it on purpose.
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

- `docs/architecture.md` — **what the system is.** Full Postgres DDL, the permission-resolution algorithm,
  the exact gateway op-code/frame shapes, the daemon/CLI/GUI design, the voice architecture, the complete
  REST endpoint list, the OAuth linking/PKCE flow, deep dives on security (§14) and performance (§15), and
  the known tensions and accepted limitations (§17). Read it before making an architectural decision this
  file doesn't already cover.
- `docs/roadmap.md` — **what gets built, in what order.** `M0`–`M117`, each with scope, dependencies, and a
  checkable "done when". The single source of truth for milestone numbering; `architecture.md` §13 only
  points here.
- `docs/adr/` — **why the contested calls went the way they did.** Superseded ADRs stay, marked as such in
  both directions. `SECURITY.md` covers vulnerability-reporting
process, not architecture.
