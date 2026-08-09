# ADR 0018: Postgres-native full-text search, no dedicated search service for v1

## Status
Accepted

## Context
Message/tag/channel search needs reasonable relevance ranking without asking self-hosters to run and operate
a second stateful service alongside the one Postgres instance they already need.

## Decision
`tsvector` generated columns plus GIN indexes, `pg_trgm` for fuzzy matching, on `messages.content` and
tag/channel-topic text — synchronous on the insert path for v1 at both self-hosted and flagship scale, since
GIN-index maintenance cost is negligible against realistic v1 message volume.

## Consequences
- If the project gains real traction, swapping in a dedicated engine (Meilisearch/Typesense) for better
  relevance ranking is a reasonable future upgrade — not designed for now, and explicitly not v1 scope for
  either deployment shape.
- Similarly, an async-queue-decoupled indexing path (batching `tsvector` updates off the insert transaction's
  critical path) is a documented future upgrade specifically for the flagship Kubernetes deployment if/when
  message fan-out latency under real load justifies it, not built now.
- E2E-encrypted DMs are excluded from this system entirely (ADR 0014) — their mandatory local FTS5 index on
  the daemon's keystore is a separate, client-local mechanism, not a Postgres one.

## Alternatives considered
- **A dedicated search engine from day one**: rejected — real operational cost (a second stateful service)
  for a relevance-ranking improvement v1 doesn't need yet.
- **Async-decoupled indexing from day one**: rejected for v1 — added complexity (a queue, a worker) for a
  performance problem that doesn't exist at v1 scale; kept as a documented, not built, upgrade path.
