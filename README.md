# Norite

A voice-and-text chat platform. The primary way to use Norite is the free, global, publicly-hosted flagship
instance — self-hosting your own instance is a real, fully-built feature, not the platform's core identity:
useful for enterprises and other private groups who want their own instance, available via a one-time
license purchase. Source is visible here but under no public license (see License, below).

Four clients: a scriptable CLI (the `norite` command tree — one action, exit, pipeable), a full-screen TUI
(the in-terminal application: panes, chords, 25 specified screens), a native GUI mirroring the TUI's
information architecture, and a lower-priority web SPA built later. The first three attach to one local
background daemon per OS user account, which holds the real connection to whichever instance you're using
and does the real work; the clients are thin UIs over it. The CLI and the TUI ship in one binary and share
one command tree, so every verb is runnable from the TUI's `M-x`.

## Status

Early implementation — Phase A (foundation), `M0` through `M9` done and `M10` next. What exists: the
monorepo and CI (`M0`), the backend skeleton — chi router, pgx pool, advisory-lock-guarded auto-migration,
structured logging, rate limiting, `/healthz` (`M1`), the `norite` command tree and instance setup wizard
(`M2`), the user-scoped background daemon's lifecycle (`M3`), accounts with argon2id, device-scoped refresh
families and scoped API tokens (`M4`), transactional email and password reset (`M5`), OAuth sign-in with
Google and GitHub (`M6`), `norite login` with the credential the daemon starts with (`M7`), the OAuth
loopback login — a system browser and a localhost callback (`M8`) — and the headless fallback for a machine
with no browser at all — a code completed in a browser on another device (`M9`). `M10` finishes
`norite instance init` and hardens registration.

No product features exist yet — no guilds, channels, messages, or voice; those begin at `M12`. See
`docs/architecture.md` for the full architecture and `docs/roadmap.md` for the milestone sequence (`M0`
through `M125`, dependency-ordered, phase-grouped). This is realistically multi-year,
systems-engineering-team-sized work; the roadmap is a long-term critical path, not a near-term promise.

### Running the backend locally

```sh
just dev          # Postgres + Redis + the backend with hot-reload, via docker-compose
just test         # every Go module's tests (needs a container runtime for the backend's)
just test-short   # unit tests only, no container runtime required
just lint
```

The backend is configured entirely through `NORITE_*` environment variables — see `.env.example` for the
full list. It applies its embedded migrations on startup, holding a Postgres advisory lock so two processes
never race, and `GET /api/v1/healthz` returns 200 only once that has completed.

## Features (planned, v1 scope — none of this is a "later" phase)

- **Servers ("guilds"), channels, roles & permissions** — real-time messaging over a WebSocket gateway
  modeled on Discord's real protocol (op-codes, HELLO/IDENTIFY/READY handshake, heartbeats, RESUME).
- **Voice calling on every client**, the terminal ones included — a self-hosted, custom-built SFU
  (Pion/Go), embedded TURN server, noise suppression/echo cancellation/AGC, adaptive bitrate.
  Video/screen-share is deferred but architected now so it's additive when it lands, never a rework.
- **BYOK end-to-end encryption**, opt-in, `DM` channel type only — a Signal-style double ratchet
  (`go.mau.fi/libsignal`) plus a custom device-linking flow, gated behind a hard external-cryptographic-review
  release gate before it's trusted with real conversations beyond the developer's own test accounts.
- **Public matchmaking**: guild-less, ephemeral topic channels (voice-and-text pairs) plus a "recently met"
  list, a mutual friend-request system, and a personal block/mute system.
- **Chat power features**: Deep Work status (with an `@urgent` bypass and an optional offline email
  fallback), message tagging, in-channel whispers, regex notification filters, bandwidth/network toggles.
- **Per-guild custom emoji** and **incoming webhooks** — real, free v1 scope on every instance.
- **Dev tools and extensibility**: code block copy/fold, a shell pane running your own shell, local bot
  automation, shell piping, GitHub-aware and generic link previews, a client-side WASM plugin system
  (sandboxed, capability-gated, headless by design — a plugin registers commands, never keybindings).
- **P2P (WebRTC) file transfer** — explicit opt-in per transfer, consent-gated before any IP address is
  exposed.
- **The Instance Admin tier**, platform-wide bans, a unified reports/moderation system, and instance-level
  registration gating.
- **Self-hosting support**: transactional email (SMTP) and built-in automatic HTTPS are both real, both
  deployment-time opt-outs — a self-hosted instance runs fine without either configured.

## Clients

Build order is the terminal clients first, native GUI second, web SPA third (demoted from primary to a
lower-priority later client). They are all thin "attach" UIs over one shared local daemon per OS user
account, which owns the actual gateway connection, presence, scrollback, the plugin host, and local bot
automation. Voice is deliberately not in the daemon — a separate voice-worker subprocess, spawned on
demand, owns the whole audio pipeline so a media bug can never take down messaging or presence.

The **CLI** is the scriptable half: Unix-style, one action then exit, `--json` on every data-printing verb.
The **TUI** is where a person actually spends time — a Discord-shaped layout (guild rail → channel list →
message area → member list) with tmux-like pane splitting and Emacs-style chorded keybindings, specified
screen by screen in `docs/design/tui/`. They are two front ends onto one command tree rather than two
programs, which is what keeps the scriptable surface and the interactive one from drifting apart.

## Tech stack

**Backend**: Go, `chi` (router), `sqlc` + `pgx` (database access, no ORM), a WebSocket gateway
(`coder/websocket`), PostgreSQL, Redis (reserved for the flagship's horizontal-scale event bus and rate
limiting; self-hosted single-process instances never activate it).

**CLI** (the command tree): Go, `urfave/cli` v3 — flag parsing, nested subcommands, `--json`/`--help`,
shell completions.

**TUI** (the in-terminal client): Go, Bubble Tea + Lip Gloss + Bubbles (Charm stack) with its own
pane/split engine, Emacs-style chorded keybindings by default, and inline images where the terminal
supports them.

**Native GUI**: Go, Gio (`gioui.org`) — immediate-mode, GPU-rendered, hand-built widgets (no built-in widget
library), for tight memory control.

**Daemon**: Go, `wazero` for the WASM plugin sandbox, `zalando/go-keyring` for credential storage,
`pelletier/go-toml` v2 for the shared config file.

**Voice-worker**: Go with cgo bindings confined to this one binary — `hraban/opus`, RNNoise, and WebRTC's
Audio Processing Module (AEC3) — the only place cgo is allowed anywhere in the stack.

**Web SPA** (later, third-priority client): React, TypeScript, Vite, TanStack Query, Zustand, Tailwind +
shadcn/ui, with its own BFF-style httpOnly-cookie auth exchange layer.

See `docs/architecture.md` for the full rationale behind each choice, and `docs/adr/` for short, focused
records of the most contested individual decisions.

## Commercial model

Two independent deployments of the same codebase, no shared infrastructure between them. **The free,
publicly open-registration flagship instance (Kubernetes/Helm, optional paid per-user subscription perks) is
the primary product** — the one most people use. Self-hosted instances, sold via a one-time license purchase
(offline, cryptographically-signed license file, no phone-home), are a real secondary offering, not a
lesser-effort one — pricing is a flat one-time purchase regardless of who's buying, though it's expected to
be most attractive to enterprises and other private groups who want their own instance. There's no "Platform
Operator" tier and no federation — every instance, flagship or self-hosted, is Instance-Admin-managed and
stands alone.

## Documentation

- `CLAUDE.md` — fast-loading project summary and non-negotiable engineering rules, for both human
  contributors and AI coding agents working in this repo.
- `docs/architecture.md` — the full architecture: data model, permission system, daemon/CLI/TUI/GUI design,
  voice architecture, gateway protocol, REST API, security and performance deep dives, and the known
  tensions this design accepts.
- `docs/roadmap.md` — the dependency-ordered milestone sequence (`M0`–`M125`), each with a checkable
  "done when" condition.
- `docs/design/tui/` — the terminal client's normative design: 25 screens with stable ids, the keymap, the
  design tokens, and a visual HTML mock of each screen.
- `docs/adr/` — Architecture Decision Records for the most contested individual choices.
- `.claude/skills/` — repo-specific workflows (`/new-endpoint`, `/new-gateway-event`, `/db-migration`,
  `/security-audit`) encoding the conventions above.

Found a security issue? See `SECURITY.md` rather than opening a public issue.

## License

**No public license — all rights reserved.** The source is visible here for self-hosting trust and
transparency, but no rights to use, copy, modify, distribute, host, or sell it are granted by default; under
default copyright law, "all rights reserved" is what applies to unlicensed code. Self-hosted instances are
run under an individually-issued, cryptographically-signed license file granted directly to that customer
(see the commercial model above), not under a public license text. See
`docs/adr/0007-licensing-and-project-posture.md` for the full reasoning.
