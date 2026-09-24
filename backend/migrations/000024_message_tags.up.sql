-- Milestone M17 — tags a guild puts on its messages.
--
-- **Guild-wide scope, not per-channel**, which is the milestone's one contested decision. §2 cited an ADR
-- for it that was never written — unnumbered, and no ADR in docs/adr/ mentions message tags at all — so
-- the reasoning lives here and in the roadmap entry instead. CLAUDE.md's test for whether an ADR is owed
-- is whether a decision contradicts an existing one, and this contradicts nothing: ADR 0008's hierarchy is
-- guild-scoped, channels are the unit of *visibility* rather than of organisation, and a tag that could not
-- follow a conversation from one channel to the next would be a worse filing system than none.
--
-- Everything measured below is on PostgreSQL 16.14 against 12,800 tags and 95,826 applications over
-- 47,967 tagged messages, inside the 400,000 messages across 2,000 channels that 000020, 000022 and 000023
-- all used — so these numbers sit beside those rather than beside nothing.

CREATE TABLE message_tags (
  id         bigint PRIMARY KEY,                               -- snowflake (ADR 0003)
  guild_id   bigint NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
  name       varchar(50) NOT NULL,
  -- No ON DELETE, matching messages.author_id and audit_log_entries.actor_id: a tag whose creator went
  -- NULL is a tag with the answer removed, and for a *private* tag it is worse than that — created_by is
  -- what makes it private, so nulling it would turn somebody's private tag into an ownerless row that the
  -- visibility filter below can never match again.
  created_by bigint NOT NULL REFERENCES users(id),
  -- A shared tag is the guild's and needs PermManageMessages to create; a private one is its creator's and
  -- needs nothing. The column is the whole of that distinction, which is why the two unique indexes below
  -- are partial on it rather than one index over both kinds.
  is_shared  boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- **Uniqueness, and §2 specified none at all.** Without these a guild can hold two tags named `spam`,
-- which makes a tag list unreadable and a "which one did you mean" question unanswerable.
--
-- Two *partial* indexes rather than one, because the right constraint differs by kind and a single index
-- over (guild_id, name) would get both wrong: it would let one member's private tag block the guild from
-- ever creating a shared tag of that name, which is a private actor vetoing a shared decision.
--
--   shared   unique per guild            — the guild's vocabulary, so two `spam` tags are a mistake
--   private  unique per guild per owner  — my `todo` and your `todo` are different tags and both are fine
--
-- Case-insensitive, via lower(name). A guild holding `Spam` and `spam` is a guild whose tag list is
-- confusing in exactly the way uniqueness exists to prevent — the same call `users.username` makes with
-- citext. The type stays varchar(50) as §2 draws it; the functional index is the smaller deviation.
--
-- **These two turn out to do a third job, which is why there is no separate guild_id index below.** Their
-- predicates are complementary — `is_shared` and `NOT is_shared` partition the table — so together they
-- cover every row by guild_id, and the planner bitmap-ORs them for both the guild's tag listing and the
-- ON DELETE CASCADE from guilds. Measured, because the opposite was assumed first:
--
--   listing a guild's tags     0.171 ms with a (guild_id, id DESC) index *dropped*, 0.332 ms with it —
--                              the planner declines it and reaches for these instead
--   deleting a guild's 640 tags  3.97 / 4.42 ms without it, 6.07 ms with it
--
-- So the index that looked obviously required is not merely unnecessary, it is slightly worse: one more
-- structure to maintain on every insert for a path that never chooses it. That is M13's result on
-- DeleteOverwritesForTarget reproduced — a suspected missing index retired by measuring rather than added
-- by reasoning — and it is the reason rule 7 says the index ships with the query rather than with the
-- table.
CREATE UNIQUE INDEX message_tags_shared_name_idx
  ON message_tags (guild_id, lower(name)) WHERE is_shared;
CREATE UNIQUE INDEX message_tags_private_name_idx
  ON message_tags (guild_id, created_by, lower(name)) WHERE NOT is_shared;

-- The refusing foreign key to users, which needs an index for messages_author_id_idx's reason: deleting an
-- account requires Postgres to prove no tag references it, and proving a negative over an unindexed column
-- is a full scan — inside the transaction rule 17's revoke-everything already holds open.
--
-- The private partial index above leads with guild_id, so it does not serve created_by alone.
--
-- Measured together with the applications index below, 200 deletions of accounts that created and applied
-- nothing, which is the cheapest case and therefore the honest one: 0.2495 ms each with both, 2.2440 ms
-- each with neither. Nine times rather than M16b's five hundred, and the ratio is small for one reason
-- worth stating — these tables are *small*, because the ceilings in `tags.Service` bound them. It grows
-- with the tables, which is why it is here now rather than at the milestone that notices.
CREATE INDEX message_tags_created_by_idx ON message_tags (created_by);

CREATE TABLE message_tag_applications (
  tag_id     bigint NOT NULL REFERENCES message_tags(id) ON DELETE CASCADE,
  message_id bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  applied_by bigint NOT NULL REFERENCES users(id),
  applied_at timestamptz NOT NULL DEFAULT now(),
  -- The pair is the identity: applying the same tag twice is one row, not a Go-side check. Same call
  -- message_reactions makes for (message_id, user_id, emoji).
  PRIMARY KEY (tag_id, message_id)
);

-- **The index §2 omitted that costs the most, and the one hardest to see is missing.**
--
-- message_id is the *second* column of the primary key, so nothing serves a lookup on it alone — which is
-- verbatim M12's guild_member_roles.role_id, where the third PK column served no lookup and 500 role
-- deletions took 4,566 ms. Two separate paths need exactly that lookup:
--
--   * the ON DELETE CASCADE from messages, on every hard message deletion;
--   * "what tags does this message have", which is the only way to render a tagged message at all.
--
-- Measured both ways:
--
--   the cascade, 500 message deletions      23.5 ms with, 961.7 ms without      41x
--   one page of 50 messages' tags (= ANY)   245 buffers with, 837 without       2.1 ms against 10.5 ms
--
-- The read is the half worth reading twice. Without this index the scan is over every application on the
-- instance, so the work grows with total tagging traffic while the indexed form grows with the page — the
-- argument 000020 made for messages_channel_id_id_idx and 000023 made for the recording log's cursor. It
-- is also why the read resolves a whole page's tags in one `= ANY($1)` rather than one query per message:
-- fifty round trips would be §15.2's N+1 on the path every client hits to draw a channel.
CREATE INDEX message_tag_applications_message_id_idx ON message_tag_applications (message_id);

-- The second refusing foreign key, for the reason the first one above has. Measured with it.
CREATE INDEX message_tag_applications_applied_by_idx ON message_tag_applications (applied_by);

-- **What is deliberately not here: a guild_id column on the applications table.**
--
-- A tag knows its guild and a message reaches one only through channels, so nothing in this schema alone
-- stops guild A's tag being applied to guild B's message. That is the shape M16 found when `reports` had
-- no guild_id and M16b's security review found when the recording writer did not tie a message's channel
-- to the guild it was filed under — and the answer here is the same as M16b's rather than M16's: the
-- predicate goes in the *statement* that applies a tag, which joins messages to channels and requires
-- `c.guild_id = t.guild_id`.
--
-- Denormalising the guild onto this table was the alternative and it is worse for a reason M16's own
-- correction does not apply to: M16 needed guild_id on `reports` because the *read* was per guild and the
-- join cost 6.593 ms against 0.098 ms. Nothing reads applications by guild — they are read by message —
-- so a guild_id here would be a column written on every apply, never queried, and free to drift from the
-- channel's actual guild. A predicate that is checked once on write beats a column that can be wrong.
