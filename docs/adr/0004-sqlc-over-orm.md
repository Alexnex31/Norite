# ADR 0004: sqlc + pgx over an ORM (GORM)

## Status
Accepted

## Context
The backend needs a DB access layer. Candidates considered: a reflection-based ORM (GORM is the common Go
choice), raw `pgx` with hand-written scanning, or `sqlc` codegen over `pgx`.

## Decision
`sqlc` (compile-time codegen from hand-written SQL) over `pgx/v5`/`pgxpool`. Query files live under
`backend/internal/db/queries/<domain>.sql`, one named query per operation
(`-- name: GetUserByEmail :one`), generating typed Go in `internal/db`.

## Consequences
- Exact SQL is visible and hand-tunable for the performance-sensitive query shapes that matter most here:
  cursor pagination (`messages(channel_id, id DESC)`), multi-join permission resolution, batched `READY`
  hydration (see `docs/architecture.md` Section 8.2 on avoiding N+1 there).
- Zero runtime reflection, and — a security property, not just a style preference — 100% of queries are
  parameterized by construction, since there is no code path that builds SQL from string concatenation
  (`docs/architecture.md` Section 7.1 relies on this).
- Query shapes are static and known at generation time, which also means `pgx`'s prepared-statement caching
  is effective by default (`docs/architecture.md` Section 8.3).
- The tradeoff: every new query needs a `.sql` file entry + `sqlc generate` step (see the `/db-migration`
  project skill), versus an ORM's more dynamic query-building — more upfront ceremony for simple CRUD, in
  exchange for control on the queries that actually matter for correctness/performance.

## Alternatives considered
- **GORM (or another reflection-based ORM)**: rejected — higher N+1 risk by default, obscures the exact SQL
  running for the queries where that visibility matters most, and adds runtime reflection overhead for no
  benefit in a schema this well-understood upfront.
- **Raw `pgx` with hand-written scanning everywhere**: rejected — all the manual-SQL discipline of `sqlc`
  with none of its type-safety/codegen benefit.
