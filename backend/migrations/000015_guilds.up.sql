-- Milestone M12 — the guild, its members, its roles and its channels.
--
-- Phase C opens here, and this is the largest schema change since M4. The shape is transcribed from
-- docs/architecture.md §2 rather than reinvented; what this migration adds is the reasoning that belongs
-- next to the columns, and the indexes §2's sketch leaves implicit.
--
-- Two things are deliberately *not* here, and both are decisions rather than omissions:
--
--   * channel_recipients, the DM/group-DM membership table. It belongs to M57 and nothing in M12 reads it.
--     The DM values in channels.type stay in the enum unused, exactly as they already are.
--   * a unique constraint on (guild_id, position) for roles or channels. Reordering is a multi-row swap,
--     and a unique constraint turns every reorder into a dance around it — a temporary negative position,
--     or a deferred constraint, or three statements where one would do. M13 owns hierarchy semantics; this
--     migration stores the column and orders by it.
--
-- # The numbers below
--
-- Every index here was measured rather than argued, on PostgreSQL 16.14 against a seeded instance of
-- 20,000 users, 5,000 guilds, 200,000 memberships, 25,000 roles, 50,000 channels and 400,000 member-role
-- rows. The scale is the point: M11a's recovery-code index looked worthless against a fixture holding one
-- account, because what a sequential scan crosses is the whole instance and a single-tenant fixture cannot
-- see it. A per-guild query benchmarked against a database containing one guild measures nothing.

CREATE TABLE guilds (
  id                bigint PRIMARY KEY,                     -- snowflake (ADR 0003)
  name              varchar(100) NOT NULL,
  -- No ON DELETE. A guild whose owner is deleted is not a guild with a NULL owner and it is not a guild
  -- that vanishes with them — ownership transfer is a real operation with real consequences for everyone
  -- else in it, and the account-deletion path (M66) has to make that decision explicitly rather than have
  -- a cascade make it silently. Until then the FK refuses the delete, which is the honest failure.
  owner_id          bigint NOT NULL REFERENCES users(id),
  icon_hash         text NULL,
  description       text NULL,
  -- The channel system messages go to. Nullable, and the constraint lands after channels exists — see the
  -- ALTER at the bottom, which is why this column has no REFERENCES clause here.
  system_channel_id bigint NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now()
);

-- Deleting a user checks every table referencing users(id) for rows that would be orphaned, and does it
-- with whatever access path exists. Without this index that check is a sequential scan of every guild on
-- the instance, on a path that already holds a transaction open (rule 17's revoke-everything runs there).
--
-- It also serves the read it looks like it serves — "which guilds does this account own" — which the
-- ownership-transfer and account-deletion paths both need.
--
-- The smallest win in this migration, and it is quoted rather than rounded up: 0.115 ms and 8 buffers
-- against 0.284 ms and 49. Two and a half times, on a table of 5,000 rows — because guilds is the one
-- table here that grows with *guilds* rather than with memberships or messages. The shape is what earns
-- the index, not today's ratio: a seq scan costs the size of the table and an index scan costs the rows
-- returned, so the gap widens for the whole life of the instance while the index does not.
CREATE INDEX guilds_owner_id_idx ON guilds (owner_id);

CREATE TABLE guild_members (
  guild_id  bigint NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
  user_id   bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  nickname  text NULL,
  joined_at timestamptz NOT NULL DEFAULT now(),
  -- Server-side mute and deafen, set by a moderator and distinct from the self_mute/self_deaf a client
  -- sets on itself (voice_states, M25). Active v1 functionality per rule 10, not reserved columns.
  deaf      boolean NOT NULL DEFAULT false,
  mute      boolean NOT NULL DEFAULT false,
  -- (guild_id, user_id) rather than a surrogate id, because the pair *is* the identity: a person is in a
  -- guild once. It also gives guild_member_roles' composite foreign key the unique index Postgres requires
  -- on the referenced columns, without a second constraint to keep in step.
  PRIMARY KEY (guild_id, user_id)
);

-- The member list is one of the three hot paths rule 7 names, but it reads by guild_id, which the primary
-- key's leading column already serves. This index is the other direction: "which guilds is this account
-- in", which is the first query every client makes after READY (§15.2) and the one the cascade above runs
-- on account deletion.
--
--   with the index    0.121 ms,   15 buffers   Bitmap Index Scan
--   without           8.120 ms, 1471 buffers   Seq Scan
--
-- Sixty-seven times, on the query that runs once per client connection.
CREATE INDEX guild_members_user_id_idx ON guild_members (user_id);

CREATE TABLE roles (
  id          bigint PRIMARY KEY,                           -- snowflake (ADR 0003)
  guild_id    bigint NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
  name        varchar(100) NOT NULL,
  color       integer NOT NULL DEFAULT 0,
  -- The permission bitfield, and the reason internal/roles/permissions.go transcribes its iota order from
  -- §2 rather than choosing one. What is stored is bit positions; renumbering them silently reassigns
  -- every permission every guild on the instance has already granted.
  --
  -- bigint, signed, so the top bit is unavailable and 63 permissions is the ceiling. That is stated rather
  -- than discovered at bit 64: Postgres has no unsigned integer type, and the alternatives (numeric, a
  -- second column, a bit varying) all cost more than the ceiling is worth against 40-odd defined bits.
  permissions bigint NOT NULL DEFAULT 0,
  position    integer NOT NULL,
  hoist       boolean NOT NULL DEFAULT false,
  mentionable boolean NOT NULL DEFAULT true,
  -- The @everyone role. Created in the same transaction as the guild, so there is no instant at which a
  -- guild exists without the floor permission resolution starts from. One per guild by convention rather
  -- than by constraint; the guild-creation path is the only writer, and M13's role endpoints refuse to
  -- delete or reposition it.
  is_default  boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Serves the role list, which reads by guild and returns in position order. Its leading column also serves
-- the ON DELETE CASCADE from guilds, and it is what makes the role-deletion measurement below a fair one.
--
-- On the ordering half, see channels_guild_id_position_idx: the second column earns its place on the
-- paginated read and not on the unbounded one, and the plan says so.
CREATE INDEX roles_guild_id_position_idx ON roles (guild_id, position);

CREATE TABLE guild_member_roles (
  guild_id bigint NOT NULL,
  user_id  bigint NOT NULL,
  role_id  bigint NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  PRIMARY KEY (guild_id, user_id, role_id),
  -- A composite foreign key rather than two separate ones, so a row cannot name a (guild, user) pair that
  -- is not a membership — which two independent FKs would happily allow, giving a person roles in a guild
  -- they had left. Postgres requires a unique index on the referenced columns and guild_members' primary
  -- key is exactly it.
  FOREIGN KEY (guild_id, user_id) REFERENCES guild_members(guild_id, user_id) ON DELETE CASCADE
);

-- role_id is the third column of the primary key, which serves a lookup on (guild_id, user_id, role_id)
-- and on its leading prefixes — not on role_id alone. So deleting a role runs its cascade with no usable
-- index, and this is M11's lesson recurring exactly: sessions.replaced_by_id was an unindexed
-- self-referencing FK, and deleting 2,000 rows spent 3,757 ms in the referential-integrity trigger against
-- 9.4 ms in the DELETE itself. Nothing in an FK declaration hints at the cost, and it appears only once
-- something finally deletes.
--
-- The largest measured difference in this migration. Deleting 500 roles:
--
--                              total        the cascade trigger
--   with the index             8.395 ms        8.066 ms, 500 calls
--   without                 4566.650 ms     4566.108 ms, 500 calls
--
-- Five hundred and forty times, and note where the time is: the DELETE itself costs 0.3 ms either way.
-- The whole cost is the trigger scanning 400,000 rows once per deleted role, so it is quadratic in the
-- table — which is why the number to look at is not the ratio but the fact that it grows.
CREATE INDEX guild_member_roles_role_id_idx ON guild_member_roles (role_id);

CREATE TABLE channels (
  id              bigint PRIMARY KEY,                       -- snowflake (ADR 0003)
  -- NULL for a DM or group DM, which belong to no guild. That is why this column is nullable and why
  -- PATCH /channels/{id} cannot authorize against a guild id from its own path — it has none. The handler
  -- loads the channel and reads *its* guild_id (rule 1).
  guild_id        bigint NULL REFERENCES guilds(id) ON DELETE CASCADE,
  -- 0 GUILD_TEXT, 1 DM, 2 GUILD_VOICE, 3 GROUP_DM, 4 GUILD_CATEGORY, 5 GUILD_ANNOUNCEMENT (reserved),
  -- 6 GUILD_STAGE_VOICE (reserved), 7 PUBLIC_MATCHMAKING.
  --
  -- GUILD_VOICE is active v1 functionality; GUILD_STAGE_VOICE is the deferred-but-seamed value rule 10
  -- forbids removing. M12 creates only GUILD_TEXT, GUILD_VOICE and GUILD_CATEGORY; the rest are reachable
  -- schema for the milestones that own them.
  type            smallint NOT NULL,
  -- The category a channel sits under. ON DELETE SET NULL, so deleting a category orphans its children to
  -- the top level rather than deleting them — losing a category must not lose the conversations in it.
  parent_id       bigint NULL REFERENCES channels(id) ON DELETE SET NULL,
  name            varchar(100) NULL,
  topic           text NULL,
  position        integer NOT NULL DEFAULT 0,
  nsfw            boolean NOT NULL DEFAULT false,
  -- Denormalized pointer for the unread computation, with no REFERENCES clause on purpose: messages does
  -- not exist until M15, and a foreign key here would make deleting a message rewrite the channel row on
  -- the hottest write path in the product.
  last_message_id bigint NULL,
  -- Active for GUILD_VOICE (rule 10), NULL for every other type.
  bitrate         integer NULL,
  user_limit      integer NULL,
  -- Generated rather than maintained by a trigger, so it cannot drift from the column it summarizes.
  -- 'english' is the fixed configuration §2 specifies; making it per-guild would change the meaning of an
  -- already-built index.
  topic_search    tsvector GENERATED ALWAYS AS (to_tsvector('english', coalesce(topic, ''))) STORED,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);

-- The channel list: rule 7's second named hot path, read by guild in position order. Its leading column
-- also serves the cascade from guilds.
--
--   with the index    0.250 ms,   18 buffers   Bitmap Index Scan
--   without           4.417 ms, 2114 buffers   Seq Scan
--
-- The second column needs a correction, because the obvious claim for it is wrong and the plan output is
-- what caught it. "The index answers the WHERE and the ORDER BY together, so there is no sort" holds only
-- for the *paginated* read. Unbounded, Postgres prefers a bitmap heap scan and a quicksort even with this
-- index available — a bitmap scan reads the heap in physical order, which beats the random access an
-- ordered index scan would do, and sorting ten rows afterwards is free. Add the LIMIT a real list endpoint
-- carries and the plan switches to an Index Scan in index order with no sort node at all, measured on a
-- 510-channel guild. So the ordering earns its place on the query the endpoint actually makes, and not on
-- the one that is easier to type into psql.
CREATE INDEX channels_guild_id_position_idx ON channels (guild_id, position);

-- The same shape as guild_member_roles.role_id above, and the same reason: an ON DELETE SET NULL
-- self-reference whose referential-integrity trigger has no index to use. Deleting a category would scan
-- every channel on the instance. This is sessions.replaced_by_id's shape down to the ON DELETE SET NULL.
--
-- Deleting 100 categories, the channels_parent_id_fkey trigger alone:
--
--   with the index      7.395 ms
--   without           129.322 ms
CREATE INDEX channels_parent_id_idx ON channels (parent_id);

-- Channel search (M63). The index ships with the column rather than with the query, because a GIN index
-- built later on a populated table is a very different operation from one built on an empty one.
CREATE INDEX channels_topic_search_idx ON channels USING GIN (topic_search);

-- Deferred to here because it references channels, which does not exist when guilds is created. §2's DDL
-- already orders it this way; the ordering is the whole reason the column above carries no REFERENCES.
--
-- ON DELETE SET NULL: deleting the system channel leaves the guild without one, which is a supported
-- state, rather than refusing the delete or taking the guild with it.
ALTER TABLE guilds
  ADD CONSTRAINT fk_system_channel FOREIGN KEY (system_channel_id) REFERENCES channels(id) ON DELETE SET NULL;

-- Third instance of the unindexed-FK shape in this one migration, and the one most likely to be missed
-- because the constraint is declared a hundred lines from the column rather than beside it. Deleting any
-- channel checks guilds for rows pointing at it.
--
-- Deleting 100 channels, the fk_system_channel trigger alone: 2.212 ms with the index against 16.924 ms
-- without. The smallest of the three cascade wins, because guilds is the smallest table involved — and the
-- one that would be easiest to talk yourself out of, which is why it is measured rather than reasoned
-- about.
CREATE INDEX guilds_system_channel_id_idx ON guilds (system_channel_id);

CREATE TABLE permission_overwrites (
  channel_id  bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  -- 0 role, 1 member. Two columns rather than two nullable FKs, because an overwrite targets exactly one
  -- of them and a pair of nullable references would permit a row targeting both or neither.
  target_type smallint NOT NULL,
  target_id   bigint NOT NULL,
  allow       bigint NOT NULL DEFAULT 0,
  deny        bigint NOT NULL DEFAULT 0,
  -- The primary key's leading column is what roles.Resolve reads by: every overwrite on one channel, in
  -- one lookup, sorted in Go by the precedence ADR 0008 fixes. No separate index is needed, and the plan
  -- confirms it — Index Scan using permission_overwrites_pkey, 0.092 ms, 6 buffers.
  PRIMARY KEY (channel_id, target_type, target_id)
);

-- Nothing here is swept, and unlike the other tables that statement covers it is not even a close call:
-- none of these has a TTL. A guild is as durable as the instance. Written down because "which tables the
-- sweeper knows about" is the question M11 found nobody had been asking, and the answer for a new table
-- should be a decision rather than a silence.
