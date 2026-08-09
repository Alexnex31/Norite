# ADR 0001: Modular monolith, not microservices

## Status
Accepted

## Context
This is a Discord-like platform that will eventually need auth, guilds/channels/roles, real-time messaging,
presence, and (later) voice/video. Discord's own production architecture is microservices-based. We need to
pick a starting architecture for a project beginning from zero code and, initially, a single/small
contributor base.

## Decision
Single Go binary ("modular monolith"), with well-separated `internal/` packages per domain (`auth`, `users`,
`guilds`, `channels`, `roles`, `messages`, `gateway`, `presence`, `voice`). Each domain owns its own
service/handler/model/events files and depends on narrow repository interfaces it defines itself, backed by
one shared `internal/db` (sqlc) package and one Postgres instance.

## Consequences
- Much simpler to build, test, deploy, and reason about at the current scale (one process, one DB, one
  `docker compose up`).
- Production deployment is a single binary + Postgres (+ Redis once needed) — see ADR 0002-adjacent
  discussion in `docs/architecture.md` Section 1 on embedding the frontend build via `embed.FS`.
- The explicit cost: if the project ever needs true independent scaling/deployment of, say, the gateway vs.
  REST API, today's package boundaries make that a plausible extraction, not a guarantee — some rework will
  still be needed (e.g. today's in-process event bus assumes a single process; see `docs/architecture.md`
  Section 8 on the `EVENTS_BACKEND=redis|nats` seam).
- We deliberately avoid paying the operational cost of multiple services (service discovery, distributed
  tracing, N connection pools) before there's real load or team-size pressure that justifies it.

## Alternatives considered
- **Microservices from day one** (mirroring Discord's real architecture): rejected as premature complexity
  for a project with no production load yet and a small contributor base; would slow every early milestone.
