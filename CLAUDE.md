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

**CLI**: Go, Bubble Tea + Lip Gloss + Bubbles (Charm stack), `teatest` for testing, `pelletier/go-toml` v2.

**Native GUI**: Go, Gio (`gioui.org`) — immediate-mode, hand-built widgets, golden-image testing for the
highest-value surfaces.

**Daemon**: Go, `wazero` (WASM plugin sandbox, pure Go/no cgo), `zalando/go-keyring`, `fsnotify`,
`gofrs/flock`, `modernc.org/sqlite` (E2E keystore, pure Go/no cgo).

**Voice-worker**: Go, `pion/webrtc` + `pion/interceptor`, `hraban/opus`/RNNoise/`libspeexdsp` (cgo —
the *only* binary in the stack where cgo is allowed).

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
cli/           The `app` CLI — Bubble Tea/Lip Gloss/Bubbles TUI, pane engine, keybindings
gui/           The native GUI — Gio app, shares the daemon/config model with cli/
daemon/        Shared background daemon — gateway client, dual IPC, plugin host, config/state files
internal/voice/  Pion-based SFU, embedded TURN server (lives under backend/, server-side infra)
contracts/     openapi.yaml (REST), gateway-events.schema.json (WS), CLI --json schemas — source of truth
docker/        docker-compose.yml (postgres, redis, backend hot-reload) — local dev + self-hosted prod option
frontend/      React SPA — the later, tertiary web client (Phase O)
```

## Common commands (once scaffolded)

- `just dev` — run the full local stack (docker-compose: postgres, redis, backend w/ air, frontend w/ vite)
- `just test` — backend `go test ./...` + frontend `pnpm test`
- `just lint` — `golangci-lint` + `staticcheck` + frontend ESLint/`tsc --noEmit`
- `just db-migrate` — apply pending `golang-migrate` migrations
- `just security-scan` — `govulncheck ./...` + `pnpm audit` + `Trivy` (container image scan, once
  Dockerfiles exist)

## Git workflow

**Commits** follow [Conventional Commits](https://www.conventionalcommits.org/): `type: imperative summary`
(lowercase, no trailing period, subject line ≤72 chars), optionally `type(scope): summary` when a scope adds
real clarity (e.g. `feat(gateway): add RESUME handling`). Explain *why* in the body when it isn't obvious
from the diff; reference the relevant milestone as a trailer line (`Milestone: M12`) when the commit is
milestone-scoped. Types: `feat` (new capability), `fix` (bug fix), `docs` (documentation only), `chore`
(tooling/config/repo maintenance, no src impact), `refactor` (no behavior change), `test`, `perf`, `build`
(deps/build system), `ci`.

**Branching**: `main` always reflects the state right after the most recently *completed* milestone — never
a half-finished one. Each milestone (`docs/architecture.md` §13) gets its own branch, named
`m<N>-<kebab-slug>` matching the milestone's title (e.g. `m1-backend-skeleton`). Child branches off a
milestone branch are fine for exploring an approach within that milestone; merge them back into the
milestone branch, not `main`, before the milestone branch itself merges into `main` once its "done when"
criteria are met. This is solo-scoped deliberately — no PR ceremony, no required review, no protected-branch
rules; the milestone branch is a checkpoint for the developer's own sake (a clean rollback point, a clear
diff to review before merging), not a collaboration mechanism. Optionally tag `main` at each milestone
completion (`git tag m12`) — cheap, and makes 118 milestones of history easy to navigate later.

## Milestone status

Nothing implemented yet — repo is freshly initialized, Phase A (foundation) not yet started. Full
dependency-ordered roadmap (`M0` through `M117`, phase-grouped, with Phase P — the flagship Kubernetes
deployment — running as an explicitly parallel track) is in `docs/architecture.md` §13.

## Project-specific skills

`.claude/skills/` has workflows encoding the rules above, invoke with `/<name>`:

- `/new-endpoint` — scaffold a new REST route (sqlc query → service → handler → OpenAPI contract → tests).
- `/new-gateway-event` — scaffold a new real-time dispatch event end-to-end (backend publish → schema →
  frontend/daemon-side zod/dispatcher).
- `/db-migration` — add a schema migration with the indexes its queries need, wired through sqlc.
- `/security-audit` — this project's own security checklist (permissions, audit log, token/credential
  hygiene, plugin sandbox boundaries, E2E exclusion, XSS/terminal-escape safety, secrets) — complements the
  generic `/security-review` skill.

## Where to look for more detail

`docs/architecture.md` has: the full Postgres DDL, the permission-resolution algorithm, the exact gateway
op-code/frame shapes, the daemon/CLI/GUI design, the voice architecture, the complete REST endpoint list, the
OAuth linking/PKCE flow, and dedicated deep-dive sections on security and performance/optimization. Read it
before making an architectural decision this file doesn't already cover. `docs/adr/` has the short-form
reasoning behind the most contested individual decisions. `SECURITY.md` covers vulnerability-reporting
process, not architecture.
