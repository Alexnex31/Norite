# Security findings ledger

Every security finding this project has **rejected**, and why.

The two things it is not: a list of vulnerabilities (those get fixed, and the fix's commit message is the
record), and a backlog (a finding worth doing later belongs in the roadmap entry that inherits it, which
is what makes it real). This file holds the third case — candidates raised by a review, verified against
the code, and judged not worth acting on.

## Why this exists

Findings that are filtered out of a review report are destroyed, and a destroyed finding gets re-derived.
Measured on the M13 branch: a 403-versus-404 oracle was filtered at confidence 6, disappeared, was
independently re-raised by the next review pass, and turned out to be a real bug. A missing guild
predicate was filtered at 5 and fixed anyway. In both cases the filter was right that they were not
vulnerabilities and wrong that they were disposable.

Every other category of decision in this repository has somewhere durable to live — ADRs for the contested
calls, roadmap entries for deferrals, "deliberate non-optimizations confirmed" in the optimization review.
Security rejections were the one kind with nowhere, so they came back.

## How to use it

**Read it before reporting a finding.** A candidate already here is not new. It is either noise this file
already answered, or a signal that the reopening condition has arrived — which is the interesting case and
the reason each entry carries one.

A rejection is a judgement about the code as it stands, not a permanent ruling. Most have a condition that
would flip them.

## Format

```
### <short claim>
- **Raised**: <milestone / review that produced it>
- **Verdict**: not a vulnerability | accepted risk | unreachable | superseded
- **Why**: one or two sentences, naming the code that makes it so
- **Reopens if**: the change that would make it real
```

---

## M13 — permission overwrites, role hierarchy, role assignment

### `Resolution.Standing()` re-exports the owner's meaningless zero
- **Raised**: M13, `/security-review`
- **Verdict**: not a vulnerability
- **Why**: `highestPosition` is unexported because an owner's is a meaningless `0` that reads as a valid
  floor position, and `Standing()` hands the same number back through an exported accessor. The comparison
  that matters never uses it: `OutranksMember` refuses by matching the target's user id against `OwnerID`
  and never by comparing standings, so the guard is structural and `Standing()` is report-only.
- **Reopens if**: a hierarchy comparison is ever written against `Standing()` directly rather than through
  `Outranks`/`OutranksMember`, or if `OutranksMember` stops asking about ownership first.

### `GetMemberHighestRolePosition` joins `roles` without a guild predicate
- **Raised**: M13, `/code-review xhigh`
- **Verdict**: **fixed anyway** — recorded because the *rejection reasoning* was sound and the fix was
  taken on a different ground
- **Why**: `guild_member_roles.role_id` references `roles(id)` alone (only `(guild_id, user_id)` is
  composite), so nothing in the schema refuses a row naming another guild's role. Unreachable, because
  `AssignRoleToMember` scopes the role and is the only writer. Scoped anyway, for the principle this
  milestone adopted twice elsewhere: a rule that holds because of who happens to call it is not that rule.
- **Reopens if**: n/a — closed.

### `RemoveMember` deletes a member's overwrites with no escalation check
- **Raised**: M13, `/code-review high`
- **Verdict**: accepted risk
- **Why**: `DeleteOverwrite` refuses a caller who does not hold the bits a row carries, and `DeleteRole`
  and `changeMemberRole` were both given the same guard — but kicking somebody deletes their member-tier
  rows unguarded. Guarding it would let a member become **unkickable** by holding an overwrite whose bits
  the moderator lacks, trading an escalation for a denial of moderation. The residual is that a kick
  clears a member-tier deny, which only matters once that member can return.
- **Reopens if**: a join path exists. That is M57 (invites) and M72a (discovery join), and both entries
  already carry the rejoin question.

### A moderator can shed a low role that restricts them
- **Raised**: M13, escalation audit (gap 13)
- **Verdict**: accepted risk — a limit of the model, not of the check
- **Why**: `UnassignRole` requires `PermManageRoles` and the role strictly below the caller's standing,
  which binds an ordinary member and not a moderator. **A positional hierarchy cannot protect a
  restriction placed low, and a restricting role is placed low by definition.** No stricter role rule
  fixes this; a restriction that must bind a moderator cannot be a role.
- **Reopens if**: never, as stated — but it is the argument for `PermModerateMembers` at M74 being a bit
  on the member rather than a role, and M74's entry carries it.

### Overwrites embedded on the channel payload disclose a guild's permission configuration
- **Raised**: M13, escalation audit
- **Verdict**: not a vulnerability
- **Why**: the listing they ride on is already filtered to channels the caller can view, so a member sees
  the configuration only of channels they can see. Discord exposes the same. The guild-wide read that
  feeds the filter is a server-side input and is never returned as-is.
- **Reopens if**: the channel listing stops being filtered, or the array is added to a response that is
  not view-scoped.

### A member may hold every role in the guild, and the authority query pays for it
- **Raised**: M13, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: nothing caps roles per member. Measured on a 254-role guild with one member holding all of
  them, `ListGuildMemberAuthority` returns 254 rows and 1,032 buffers at 0.230 ms, against a handful of
  buffers and 0.019 ms for an ordinary member — roughly twelve times, on the query that runs before every
  guild mutation and every listing. Bounded by the 250-role ceiling, requires `PermManageRoles` and roles
  below the caller's standing to construct, and Discord accepts the same shape. A per-member cap would put
  a check on the hottest path in the guild surface for a configuration a guild has to build deliberately.
- **Reopens if**: the role ceiling is raised substantially, or the authority resolution stops being
  per-request (a cache lands at M18, which changes what this costs).

### The member listing's payload scales with roles held
- **Raised**: M13, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: a page is 100 members and each may hold every role, so the worst case is 25,000 role ids —
  about 513 kB of ids before the member rows. That is §15.2's "payload that scales with something the
  client did not choose", except that it *is* bounded, by the same ceiling and for the same reason as the
  entry above.
- **Reopens if**: the role ceiling is raised substantially, or the member page size stops being capped
  at 100.

### Role creation renumbers up to 250 rows in the default limiter bucket
- **Raised**: M13, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: every create renumbers the guild's live non-default roles, so create-and-delete in a loop
  rewrites up to 250 rows per request at the global REST rate. This project has precedent for stricter
  buckets — `auth` at 20/M, `device-poll` at 120/M — and this does not earn one: the caller is
  authenticated, holds `PermManageRoles` in a guild they are already in, and the work per request is
  bounded by the ceiling.
- **Reopens if**: role creation becomes reachable without guild membership, or the renumber grows beyond
  the ceiling's bound.

### A self-targeted overwrite is a one-way door for its author
- **Raised**: M13, `/code-review xhigh`
- **Verdict**: not a vulnerability — the escalation check working
- **Why**: a moderator who denies themselves a permission in a channel cannot then blank or delete that
  row, because the union check requires holding the bits the removal would restore. That is precisely
  gap 12: removing a deny grants what it denied. The same shape lets a moderator write an `@everyone`
  overwrite denying `PermManageRoles` on a channel and put it permanently beyond every non-administrator,
  themselves included — a denial of management rather than an escalation, repairable by the owner, an
  administrator or an Instance Admin, and the same footgun Discord has.
- **Reopens if**: the union check is ever narrowed to the value being written, which would make the door
  swing both ways and reopen gap 12 with it.

### The per-channel overwrite ceiling is a read-then-insert with no lock
- **Raised**: M13, `/code-review xhigh`
- **Verdict**: accepted risk
- **Why**: two concurrent writes both read 49 and both insert, so the channel lands at 51. Consistent with
  the precedent this codebase already set for the channel, role and guild ceilings, whose comment says the
  consequence is "one over a soft limit, not a corrupted ordering". Role *position* takes an advisory lock
  because a collision there corrupts an ordering; a ceiling overshoot does not.
- **Reopens if**: the overwrite ceiling ever becomes load-bearing for something other than bounding work —
  a fixed-size buffer, or a payload guarantee a client relies on.

### Assignment pays two overwrite queries for a check that is a superset there
- **Raised**: M13, `/code-review xhigh`
- **Verdict**: not a vulnerability, and the cost is the check rather than the sharing
- **Why**: `refuseRemovingOverwritesFor` runs on assignment as well as removal, and the review proposed
  skipping it when assigning. That would remove a real check: a role whose overwrite *allows* a permission
  grants it to whoever is given the role, so the caller must hold it. The query is what answers that
  question, so it is not saved by splitting the function — only by dropping the check. What is fairly
  criticised is that the comment called the *strictness* theoretical without mentioning the two round
  trips, which is now stated.
- **Reopens if**: bulk role sync becomes a real workload (a bot doing hundreds of grants), at which point
  the answer is to resolve the role's overwrites once per batch rather than per grant.

### An idempotent no-op still reads the member back
- **Raised**: M13, `/code-review xhigh`
- **Verdict**: accepted risk
- **Why**: a repeat grant skips the audit write but still runs `GetGuildMember` and `ListMemberRoleIDs` to
  build the response, inside the transaction. Two indexed reads on a cold path, and the response has to
  carry the member either way — a `PUT` that returned nothing on a repeat would make the endpoint's shape
  depend on whether it had been called before.
- **Reopens if**: the same bulk-sync workload above makes the repeat case the common one.

## M14 — guild audit log

### A moderator can lock themselves out of a channel and not undo it
- **Raised**: M14, `/code-review high`. The reasoning below was **corrected by `/security-sweep` on the
  same branch**, which is why it is worth reading rather than skimming: the finding was right and its
  explanation was wrong, and the explanation is the half a later reader acts on.
- **Verdict**: not a vulnerability — the escalation check working, and older than the milestone that
  reported it
- **Why**: reported as a cost M14 introduced by requiring `PermViewChannel` alongside every management
  permission. It is not. A `PermManageRoles` holder who denies `@everyone` `PermViewChannel` could never
  undo it: `DeleteOverwrite` runs `refuseEscalation` over the bits the removed row carried, and restoring
  the view bit means holding it *in that channel* — which the row being removed has just taken away.
  Reproduced against `main` at `c8ec10b`, where the author's own delete answers `403 a role cannot be
  given permissions you do not hold yourself`. That is the self-targeted-overwrite entry above,
  generalised from `PermManageRoles` to `PermViewChannel`. What M14 changes is the **status code**, 403 to
  404, because the channel is now hidden at the route rather than refused at the check — and it closes the
  one escape that did work: pre-M14 the author could still delete the channel outright, which was
  confirmed to succeed and is the divergence this milestone opened with rather than a recovery path.
  Discord behaves the same way, and its recovery path is ours: the owner, an administrator, or an Instance
  Admin. Pinned by `TestDenyingYourselfViewIsAOneWayDoor`.
- **Reopens if**: the union check is narrowed to the value being written — the same condition the M13
  entry above carries, and the accurate one, because that check is what holds the door shut and the view
  requirement is not. A self-service "undo my last overwrite" surface would still need its own answer.

### The audit log shows channels the reader's own listing hides
- **Raised**: M14, while designing the read surface — the question the milestone turns on
- **Verdict**: accepted risk, and the alternative is worse
- **Why**: `GET /guilds/{id}/audit-log` does not filter entries by what the caller can currently see, so a
  `PermViewAuditLog` holder denied `PermViewChannel` on a channel reads that channel's entries by id and,
  in `changes`, by name. Three options were weighed. Filtering by present visibility is the tempting one
  and fails twice: half the interesting entries are deletions whose target no longer exists and cannot be
  resolved to a permission at all, and whether a channel is visible *today* is not the question a log
  answers about an action taken last month — filtering on it would let somebody hide their tracks after
  the fact by locking a channel down. Withholding the surface from anyone below administrator was the
  other, and it makes the log useless for the delegated-moderator case it exists for. So the permission is
  the boundary: it is not granted by default, is not implied by `PermManageGuild`, and cannot be handed
  out by somebody who does not hold it (`refuseEscalation`). Stated in the contract, in `ListAuditLog`'s
  own comment, and pinned by `TestTheAuditLogDoesNotFilterByChannelVisibility`.
- **Reopens if**: the log ever carries message content, where the exposure stops being metadata about
  moderation and becomes the conversations themselves — M15 puts `message.*` actions within reach, and
  rule 13 already forbids reading E2E DM content on any server-side path. It would also reopen if a
  future action recorded a channel's *members* rather than its id and name, which is a different kind of
  disclosure than this entry weighed.

### `audit_log_entries` grows for the life of the instance with no ceiling and no sweep
- **Raised**: M14, `/security-sweep`
- **Verdict**: accepted risk — the design, and the alternative defeats the feature
- **Why**: rule 2 puts a row here inside every guild-scoped mutation and migration `000016` states the
  table is never swept. There is no retention window and no per-guild ceiling, so a guild's log grows
  without bound and a determined member with `PermManageRoles` can inflate it by editing a role in a loop.
  An audit log you can empty by waiting, or that refuses writes when full, is not an audit log — and the
  write path is rate-limited and permission-gated like every other mutation, so the cost of inflating it
  is bounded by both. M14 gave the table a reader but did not change its growth.
- **Reopens if**: an instance needs a retention policy for legal reasons rather than technical ones, which
  is the shape that would actually force this — or if a per-guild storage quota arrives, at which point
  the ceiling belongs beside the channel and role ones rather than here. Note that a sweep would also need
  a second index: `000018`'s reasoning assumes nothing deletes by age.
- **Re-examined at M15 (2026-09-15), verdict unchanged, premise corrected.** The "permission-gated" half
  of the *why* above was about to stop being true: rule 2 read "every guild-scoped mutation", which would
  have made every message send an audit row, and `guilds.defaultEveryonePermissions` grants
  `PermSendMessages` to `@everyone` — so inflating any guild's log would have cost an attacker nothing but
  the send rate limit, and the ordinary member doing nothing wrong would have inflated it faster. M15
  narrowed rule 2 to administrative mutations instead, which keeps both halves of this entry's argument
  intact. The opt-in at M16b writes to `message_audit_entries`, a different table, so it does not reopen
  this entry — but it inherits the whole question, and unlike this table it may take a retention policy,
  because a record a guild switched on for itself is not the accountability record this one is.

### `auditDiff.context` accepts a value that is not a scalar
- **Raised**: M14, `/security-sweep`
- **Verdict**: not a vulnerability — a renderer contract enforced by a test rather than by a type
- **Why**: `changes` distinguishes a diffed field from a context field by whether the value is an object,
  so passing a map to `context` would make it indistinguishable from a diff carrying neither `from` nor
  `to`. Go has no type for "not a map", so the signature takes `any` and
  `TestTheAuditDiffShapeIsUniform` asserts the rule across every action every writer produces. The blast
  radius is a client rendering a field oddly; nothing reads `changes` for an authorization decision, and
  nothing ever should.
- **Reopens if**: anything starts *parsing* `changes` rather than displaying it — a moderation tool that
  branched on a diff's shape would turn a rendering bug into a logic one, and the type would then need to
  carry the distinction the test currently does.

### Deleting a category re-parents its children and the log says nothing about them
- **Raised**: M14, `/code-review xhigh`
- **Verdict**: accepted risk — a real gap in the record, deliberately not closed here
- **Why**: `channels.parent_id` is `ON DELETE SET NULL` (migration `000015`), so deleting a category moves
  every channel inside it to the top level. The single `channel.delete` entry names the category and
  nothing else, so an operator asking "why is this channel suddenly at the top level" finds no entry that
  mentions it. M14 gave deletions a payload and this is the one deletion whose effects reach objects it
  does not name. Closing it means either listing the affected ids as context — unbounded, up to the
  500-channel ceiling — or emitting a `channel.update` per child, which multiplies one moderation action
  into 500 entries in a table nothing sweeps. Both are decisions about entry *volume* rather than about
  the diff shape this milestone settled.
- **Reopens if**: a client renders the channel tree from the audit log rather than from the channel
  listing, or a moderation surface starts answering "what happened to this channel" by query rather than
  by an operator reading — either makes the missing rows load-bearing rather than merely absent.
- **Not reopened by**: changing the deletion to cascade instead. That was weighed at M14 — it would remove
  the re-parenting this entry is about — and rejected on Discord parity and on irreversibility: a cascade
  makes an ordinary tidying action destroy content nobody meant to touch. Migration `000015` now carries
  that reasoning next to the `ON DELETE SET NULL` it applies to.

### A kick or a role deletion destroys permission overwrites and records none of their bits
- **Raised**: M14, `/code-review xhigh`
- **Verdict**: accepted risk, and it inherits a question already routed elsewhere
- **Why**: `RemoveMember` calls `DeleteOverwritesForTarget` and records only the nickname; `DeleteRole`
  removes the role's overwrites on every channel and records name, permissions and position.
  `DeleteOverwrite`'s own comment makes the case against this — "removing a deny grants whatever it
  denied, so an operator reading this entry needs the bits" — and a kick does that N times over. It is not
  closed here for the same reason as the entry above: the count is unbounded, and the residual only
  *matters* once the target can return, which is the rejoin question M13 routed to M57 and M72a. Note the
  asymmetry is already guarded in the direction that would be an escalation: `DeleteRole` refuses to
  remove overwrites whose bits the caller lacks, while `RemoveMember` deliberately does not, so a member
  cannot become unkickable.
- **Reopens if**: a join path exists (M57, M72a) — at which point the rejoin question and this one are the
  same question and should be answered together, since what makes a silently-restored deny dangerous is
  exactly that nothing in the log explains it.

## M15 — core messaging CRUD

### `messages.content` is unbounded `text` with no length constraint
- **Raised**: M15, `/security-sweep`
- **Verdict**: not a vulnerability — and the precedent that prompted it does not exist
- **Why**: the candidate was raised on the belief that this schema already CHECK-constrains text length,
  citing `notification_filters.pattern text NOT NULL CHECK (length(pattern) <= 200)`. That line is in
  `architecture.md`'s *planned* DDL for M62 and is in no migration: `grep -c 'CHECK.*length('` over
  `backend/migrations/` returns zero. The built pattern is `varchar(n)` for short identifiers and bare
  `text` for free-form fields bounded at the validator — `channels.topic` is bare `text` with
  `validate:"omitempty,max=1024"` — so `content` matches its nearest neighbour exactly. A CHECK here would
  also be the expensive kind to change: raising a message-length cap is a product decision, and behind a
  constraint it becomes an `ALTER TABLE` validation scan over the largest table in the product.
- **Reopens if**: a write path reaches `messages` without passing the handler's validator — a bulk import,
  a webhook ingest (M60), or a bot-automation path (M22) — at which point the validator stops being the
  only door and the bound belongs where every door passes. Also reopens if a length CHECK is ever added to
  any other table, since the argument here is consistency with a schema that has none.

### A user cannot erase what they edited out, or what they posted before deleting their account
- **Raised**: M15, `/security-sweep`
- **Verdict**: accepted risk — it is the feature, and the alternative defeats it
- **Why**: `message_edit_history` has no deletion path. Its only `ON DELETE CASCADE` fires when a message
  row is *hard*-deleted, and message deletion is soft (`messages.deleted_at`), so in practice nothing
  removes a prior version. Account deletion does not either: settled at M15, a deleted account's messages
  survive attributed to "Deleted User", and `messages.author_id` carries no `ON DELETE` precisely so that
  stays true. So somebody who posts something by mistake, edits it out, and then deletes their account has
  removed none of it. That is what an edit history *is* — one that can be edited is not a history, for the
  reason `audit_log_entries` is never swept — and M16a gates who may read it behind `PermManageMessages`
  rather than making it public. Recorded because it is a privacy expectation somebody will raise as a bug.
- **Reopens if**: an erasure obligation arrives that is legal rather than technical, which is the shape
  that would actually force it — the same condition the audit-log growth entry names. Also reopens if
  M16a's disclosure decision widens the reader beyond moderators, since the exposure this entry accepts is
  bounded by who can see it.

### Dropping the channel row lock lets one message land just after a channel-overwrite mute
- **Raised**: M15, `/optimization-review`
- **Verdict**: accepted risk — the protection was partial and incidental, and the cheaper mute path never
  had it
- **Why**: `Send` read the channel `FOR UPDATE` through `guildauth.AuthorizeChannel`, and
  `guilds.SetOverwrite`/`DeleteOverwrite` take the same lock — so a mute written as a channel overwrite
  blocked on an in-flight send and no message could commit after the deny was durable. Measured: the
  moderator's `SELECT ... FOR UPDATE` waited 1,480 ms behind a send that was deliberately held open.
  Removing the lock leaves a window equal to one send transaction — a couple of milliseconds — in which a
  send that read permissions before the deny committed still inserts.

  What makes that acceptable is that **the other mute path never had the protection at all**.
  `AssignRole`, `UpdateRole` and `DeleteRole` authorize at guild level with `channelID = 0` and take no
  channel lock, and a `muted` role carrying a channel deny is the mute this project's own M13 notes call
  the commonest overwrite anywhere. Reproduced: with a send holding the channel lock open for two seconds,
  the role-assignment mute committed in 2 ms without blocking and the message landed after it. So the lock
  covered one of the two ways to silence somebody and the code read as though it covered muting. Removing
  it makes the two paths consistent rather than introducing a new class of race, and it buys about 3x on
  the product's highest-volume write — a ceiling that is per channel and that horizontal scale cannot lift,
  since it is one row lock in one database.

  Note the lock never protected rule 1's freshness: `guildauth.Authorize` reads `guild_members`, `roles`
  and `permission_overwrites` unlocked in both the locking and non-locking variants.
- **Reopens if**: the role-assignment path is ever made to serialize against sends — at which point the
  two paths agree again and this one becomes the odd one out — or if a moderation feature arrives whose
  correctness depends on "no message exists after this timestamp", which timeouts (`PermModerateMembers`,
  M74) and guild bans (M57) plausibly could. Also reopens if `SetChannelLastMessage` stops being
  monotonic, since `GREATEST` is what replaced the lock's other job and a plain assignment would walk the
  channel's unread pointer backwards — reproduced in psql, pointer 100 with message 101 present.
