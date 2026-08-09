# ADR 0003: Discord-style Snowflake IDs, not UUIDs or serial

## Status
Accepted

## Context
Every entity (users, guilds, channels, messages, roles, invites use a different scheme deliberately, see
below) needs a primary key. The message table in particular needs efficient `before`/`after` cursor
pagination ordered by creation time, which is the single highest-QPS query shape in the system.

## Decision
Use a custom 64-bit Snowflake (`internal/platform/snowflake`): 41 bits ms-since-epoch, 10 bits node ID,
12 bits per-ms sequence. Stored as Postgres `bigint`, JSON-marshaled as a quoted string (avoids JS
`number`/float64 precision loss past 2^53).

Exception: `invites.code` is a random base62 string, deliberately **not** a snowflake — an invite code
must not leak its creation time to anyone who can decode a snowflake.

## Consequences
- `bigint` (8 bytes) is half the storage/index footprint of a UUID (16 bytes), which matters across every
  foreign key in the schema.
- IDs are naturally time-sortable, so `ORDER BY id` on `messages(channel_id, id DESC)` gives correct
  chronological ordering with a plain index — no need for a separate `created_at` index or offset
  pagination (which degrades under concurrent deletes and on large tables).
- Centralized ID generation is trivial in a single-process monolith (ADR 0001); the 10-bit node ID field is
  unused capacity today but means a future multi-node deployment doesn't need an ID scheme migration, only
  a `NODE_ID` config value per node.
- The generator (`Generator.Next()`) must handle backwards clock movement (NTP adjustment) without emitting
  a duplicate or decreasing ID — this is correctness-critical and needs dedicated concurrent-generation
  tests (see `docs/architecture.md` Section 2).
- IDs are not, by themselves, an access-control mechanism — a guessable-looking sequential ID must still be
  checked for ownership/scope on every request (see ADR 0002's sibling security notes and
  `docs/architecture.md` Section 7.11).

## Alternatives considered
- **UUIDv7**: time-sortable like Snowflake and better for multi-writer/multi-datacenter random generation,
  but that advantage isn't needed in a single-process monolith, and it costs 2x the storage/index size for
  no benefit here.
- **Serial/bigserial**: simplest option, but not time-decodable, trivially guessable/enumerable, and
  provides no pagination advantage over a separate index.
