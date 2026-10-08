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
- **Reopened at M20a, and answered**: the join path exists — guild invites moved from M57 to M20a — and
  the rejoin question was decided at its planning (F1): a departure still deletes the member's own
  overwrites, so a rejoin is a clean slate. The kick stays unguarded for the reason above, and the residual
  is now live and accepted: see M20a's "Leaving and rejoining sheds a member-tier restriction" below.
- **Reopens if**: M74 decides that a member-tier restriction must survive a departure, at which point a kick
  that deletes it is the hole this entry describes rather than a residual.

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
- **Reopened at M20a, and answered with the rejoin question**, as this entry asked: nothing is restored on
  a rejoin (F1), so no silently-restored deny exists for the missing bits to explain. What a reader still
  cannot recover from the log is which denies a kick removed; `member.join` now names the invite an
  arrival came through, so the round trip itself is visible.
- **Reopens if**: overwrites ever survive a departure (M74's question), since a restored row is then exactly
  the unexplained deny this entry was about; or a rejoin path appears that writes no `member.join`.

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
  that would actually force it — the same condition the audit-log growth entry names. Also reopens if the
  set of people who can read somebody *else's* prior versions widens past `PermManageMessages`, since the
  exposure this entry accepts is bounded by who can see it.

  **That last clause was reworded at M16a and the original is worth keeping in view**: it said "if M16a's
  disclosure decision widens the reader beyond moderators". M16a's reader does widen — an author reads
  their own history without the moderation bit — and the condition as written fires on it, while the
  exposure it was protecting is untouched, because somebody reading text they wrote themselves learns
  nothing. A condition that fires on a decision it was not written to catch is one the next reader stops
  trusting, which is the failure mode `architecture.md` §16 records for checks. The rewording names the
  property rather than the milestone.

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
  M74) and guild bans plausibly could. This entry said guild bans were M57's; no milestone builds them, and
  M74 owns the question since M20a. Also reopens if `SetChannelLastMessage` stops being
  monotonic, since `GREATEST` is what replaced the lock's other job and a plain assignment would walk the
  channel's unread pointer backwards — reproduced in psql, pointer 100 with message 101 present.

### A 403-versus-404 on edit, delete and edit-history tells a member whether a snowflake names a message here
- **Raised**: M15, twice in one session — `/security-review`'s discovery pass and then `/code-review`,
  independently, within an hour of each other
- **Verdict**: not a vulnerability — the fact it would disclose is published directly, for free
- **Why**: `Update` and `Delete` load the message after authorizing the channel, and answer 403 when the
  caller is neither author nor moderator against 404 when the id names no live message here. Neither
  consults `PermReadMessageHistory`, so a member with view+send in a history-withheld channel can tell a
  live message id from a dead one. The claimed harm is learning that people were talking at time T — and
  `channels.last_message_id` hands exactly that to the same member, continuously, with no probing at all:
  the channel listing filters on `PermViewChannel` alone and the field carries a snowflake's timestamp.
  The history bit withholds *content*, which is what `TestReadingTheBacklogNeedsPermReadMessageHistory`
  asserts, and M18's `MESSAGE_CREATE` fan-out will be view-gated for the same reason.

  The finding is also mislocated in a way that matters for anyone tempted to fix it as described.
  `resolveReply` is the same oracle at the same privilege — 400 against 201 — so patching `Update` and
  `Delete` alone would close two of three doors. And confirming *historical* ids means supplying
  candidates: snowflakes here are 41-bit ms / 10-bit node / 12-bit sequence, and the sequence is shared by
  every entity on the instance, so a scan costs roughly a thousand probes per millisecond of history and
  returns timing only — no content, no author, no count.

  **Recorded rather than dismissed precisely because it came back twice in an hour.** That is the pattern
  this file exists for: the M13 403/404 oracle was filtered at confidence 6, vanished, was re-derived by
  the next pass, and was real. This one is not, and the next reviewer should be able to find that out by
  searching rather than by re-deriving it a third time.
  **M16a added a third surface and widened the fact by one step, which is why the title no longer says
  "live".** `Update` and `Delete` reach their row through `GetMessageForUpdate`, which filters
  `deleted_at IS NULL`, so their oracle distinguishes live messages only. `GetMessageWithHistoryTarget`
  deliberately does not filter — a moderator reads the history of a deleted message on purpose — so the
  history route's 403 additionally means "an id that named a message here which has since been deleted".
  Same audience, same probe cost, no content on the refusal path, and `last_message_id` still publishes
  the timing half for free. Not a reopening; recorded because a reader comparing the two surfaces will
  otherwise notice the difference and re-derive this entry from scratch.
- **Reopens if**: `last_message_id` is ever withheld from members lacking `PermReadMessageHistory`, or
  M18's `MESSAGE_CREATE` dispatch is gated on the history bit rather than the view bit. Either would make
  message *existence* something the bit actually keeps, and at that point all three paths — `Update`,
  `Delete` and `Send`'s `reply_to_id` — must be downgraded together, not one at a time.

---

## M16 — guild-level reports

The two entries below are not rejected *findings*. They are the milestone's two disclosure decisions,
recorded here because each is a deliberate choice to disclose or withhold that a reviewer will otherwise
raise as a finding — which is the same reason M14 put the audit log's visibility decision here, and each
carries the condition that would reopen it.

### A report's reported content is readable by a moderator who cannot view the channel it came from
- **Raised**: M16, at design time
- **Verdict**: accepted, and deliberate
- **Why**: `GET /guilds/{guild_id}/reports/{report_id}` returns the reported message to any holder of
  `PermManageMessages` in that guild, without consulting whether they can currently view the channel it
  was posted in. This is M14's audit-log answer applied to content rather than metadata, and for the same
  two reasons. Filtering on *present* visibility would let somebody hide what they did by locking a
  channel down after the fact, which is precisely the move a report is filed about. And a report about a
  channel the moderator is not a member of is exactly the report that needs reading — a moderation queue
  that silently drops the cases its reader cannot already see is worse than one that discloses, because
  nothing in it says a case was dropped.

  The permission is therefore the boundary, and `PermManageMessages` already means "may act on somebody
  else's message in this guild". The narrower alternatives were considered: gating on channel visibility
  (above), and returning ids only and making the moderator fetch the message through the message
  endpoints. The second fails in exactly the two cases reports exist for — a soft-deleted message, which
  the listing filters, and a channel the moderator cannot open — so it is a triage view that cannot
  triage.

  Bounded in three ways rather than none: the queue listing carries no content at all, so this is one
  route and not two; the content is resolved at read time rather than snapshotted, so nothing is stored a
  second time; and E2E-encrypted content is excluded in the query regardless of any of the above.

  **The first of those three stopped being true at M16a**, and it is recorded in that milestone's own
  entry rather than edited away here. The edit-history read reaches a message's current content from a
  bare message id, with no report involved, so content is no longer one route — and a reader arriving at
  this entry to learn what bounds the disclosure would otherwise take a sentence that has been overtaken.
  The other two bounds hold unchanged.
- **Reopens if**: a guild-scoped surface ever becomes capable of carrying E2E content (the exclusion stops
  being redundant and becomes the only thing standing here), or if `PermManageMessages` is ever granted by
  default rather than deliberately — today it is not in `defaultEveryonePermissions`, which is what keeps
  the boundary meaningful. Also if M16a's edit-history reader chooses a *narrower* gate than this one,
  since two moderation reads over the same message disagreeing about who may see it is a bug in whichever
  came second.

### A guild moderator is never told who filed a report
- **Raised**: M16, at design time
- **Verdict**: accepted, and deliberate — the withholding direction
- **Why**: no route in `reports` returns `reporter_id` to a guild moderator, and the field is absent from
  the wire struct entirely rather than stripped per handler. A guild moderator is not a vetted actor; a
  report system has to survive the case where the moderator *is* the person being reported, and handing
  them the reporter's identity undoes the protection at the one place it matters. `architecture.md` §2's
  account-export asymmetry already encodes the same judgement — an export includes reports you filed and
  excludes reports filed against you, explicitly to protect reporters from retaliation.

  The cost is real and is the reason this is an entry rather than a footnote: a guild moderator cannot see
  that five reports came from one person, which is the signal for report-spam. Two things bound it.
  Migration 000021's partial unique index allows one open report per reporter per target, so the cheap
  form of that abuse is already refused; and M74 owns reporter-history triage by name, for the tier that
  *is* vetted.

  The direction matters more than the decision. Adding the field later is additive for every client;
  removing it after clients read it is not. So the reversible choice is to withhold now.
- **Reopens if**: guild moderators gain a way to act on a *reporter* rather than on a report — a
  report-spam control at the guild level would need to name somebody — or if M74's instance queue turns
  out to be the only place abuse is visible and guild moderators are left unable to act on a pattern they
  can see the shape of. Either is a reason to expose a stable pseudonym scoped to the guild rather than
  the user id itself.

### Filing a report is a work oracle for "this snowflake names a message"
- **Raised**: M16, `/security-review`'s discovery pass, then verified independently
- **Verdict**: accepted risk — real asymmetry, not practically exploitable
- **Why**: `reports.File` reads the target message before it authorizes the channel, because the channel id
  is only reachable *through* the message row — so the checks cannot be reordered. Both outcomes return a
  byte-identical 404, but the work differs: one query when the id names nothing, four when it names a
  message in a guild the caller is not in, eight for a member who holds no `PermViewChannel` on the
  channel. Using this repository's own measurements for those queries (`guildauth.go`: `IsInstanceAdmin`
  2.85 us, `ListGuildMemberAuthority` 19.42 us) the gap is roughly 50–150 us of indexed lookups — three
  orders of magnitude below the M10 precedent that was worth closing, where `HashPassword` above the
  address check separated ~1 ms from ~31 ms. It is under WAN jitter, and what it yields is only "some row
  exists in `messages` with this id": not the channel, guild, author, content or time, since the attacker
  supplied the id. Snowflakes share one sequence across every entity on the instance, so a scan costs
  about a thousand probes per millisecond of history and returns aggregate message volume.
- **Not the same as the M15 entry above, and the difference is the part worth keeping.** That one is about
  a *member* of the guild, and its whole argument is that `channels.last_message_id` publishes the same
  fact for free. **That argument does not reach a non-member**, which is who this one is about — so this is
  a genuinely wider audience for a strictly narrower fact. Recorded separately rather than folded in,
  because somebody checking whether the `last_message_id` argument covers them needs to find the answer
  "no, and here is why it does not matter anyway".
- **Reopens if**: the gap stops being microseconds. Concretely — `File` gains per-target work on the
  *found* path (M74's whisper break-glass, a link-preview resolution, an E2E check that reads a keystore),
  or the authorization path gains a network hop, which Phase P's Redis event bus would do. Also if message
  existence ever becomes a fact the permission system is supposed to keep, in which case this must be
  downgraded together with the three M15 paths — `Update`, `Delete` and `Send`'s `reply_to_id` — rather
  than alone.

### `reports` grows without bound and nothing sweeps it
- **Raised**: M16, `/security-sweep` (a category `/security-review` cannot report at any confidence)
- **Verdict**: accepted risk
- **Why**: there is no ceiling on rows per guild or per reporter and no retention pass — `auth.RunSweeper`
  does not touch this table. That is the shape `audit_log_entries` already carries an entry for, and the
  argument there rests on the write path being "rate-limited and permission-gated". Here the permission
  half is weak in the way M15 had to correct for the audit log: filing needs only `PermViewChannel`, which
  `defaultEveryonePermissions` grants to `@everyone`.
  What holds it anyway is `000021`'s partial unique index. One open report per reporter per target means
  report volume cannot outrun *target* volume, and today the only target type is a message — itself
  created behind the send rate limit and the same permission. So inflating this table costs an attacker at
  least what inflating `messages` costs, and the row is far smaller. A ceiling would also have to refuse
  reports when full, which is the property that made a ceiling wrong for the audit log: a moderation
  queue you can fill up to stop other people reporting is worse than one that grows.
- **Reopens if**: a target type arrives that is **not** created behind a rate limit — `user` and `channel`
  are both reserved in `reports.target_type`, and a reporter could file one report against every account
  on the instance, which is bounded by the instance's user count rather than by anything they have to pay
  for. M74 owns those types and should weigh a per-reporter open-report cap at the same time as the
  per-user filing limit its entry now carries. Also reopens if the dedupe index is ever dropped or made
  non-unique, since it is the whole of this argument.

---

## M16a — message edit history read surface

### The edit-history read hands a moderator any message's current text, with no report involved
- **Raised**: M16a, `/code-review`, against the finished branch
- **Verdict**: accepted, and deliberate — but it is **this** milestone's decision rather than M16's, which
  is the correction that produced this entry
- **Why**: `GET /channels/{channel_id}/messages/{message_id}/history` authorizes through
  `guildauth.AuthorizeChannelIgnoringVisibility`, so a `PermManageMessages` holder reads it whether or not
  an overwrite currently denies them `PermViewChannel`. That much is M16's answer applied one step later,
  and deliberately so: its own reopening condition says a *narrower* gate here would be "a bug in
  whichever came second".

  What is new is the **reach**. M16's content disclosure is bounded by a report existing — its entry lists
  that as one of three bounds, in the words "the queue listing carries no content at all, so this is one
  route and not two". There are now two, and the second takes a bare message id. So a moderator denied
  view on a channel can read the current text of *any* message in it, one id at a time, with nobody having
  reported anything.

  **The envelope carries more than the text, and saying "content" understates it**: `author_id`,
  `edited_at` and `deleted_at` come back too. For a reported message M16 already disclosed the author
  (`target_author_id`), so the new part is that all of it is reachable for an *unreported* message —
  including who wrote a message in a channel the reader cannot open. Listed explicitly because a reader
  checking this entry against the code should not have to discover it from the struct. `current_content` is the sharp edge rather than the versions: it is the live message,
  and it makes this the single-message read the API otherwise deliberately does not offer.

  Accepted because the alternative reintroduces the failure the field exists to prevent. There is no
  `GET /channels/{id}/messages/{id}`, so dropping `current_content` leaves a moderator holding every
  version a message used to have and no way to learn what it says now — which is precisely the question a
  report about an edited message asks. The permission is therefore the boundary here as it is in M16's
  entry, and `PermManageMessages` is not in `defaultEveryonePermissions`: a guild grants it on purpose, to
  somebody it is already trusting to delete other people's messages in that guild.

  Two things genuinely bound it and are worth stating so a later reader does not assume more. The refusal
  downgrade is intact, so somebody who can neither view nor moderate is answered 404 and learns nothing —
  that has its own test, because lifting the view *requirement* and lifting the *downgrade* look like one
  change and are two. And rule 13's exclusion is in the query, so E2E content is unreachable here
  regardless of everything above.
- **Reopens if**: `PermManageMessages` is ever granted by default, which is the condition M16's entry
  already names and which this route makes sharper, since it is reachable without a report. Or if a
  single-message `GET` is ever added — at that point `current_content` is no longer the only way to answer
  "what does it say now", the argument above dissolves, and this route should be narrowed to versions
  alone rather than left as the widest reader by accident. Or if a guild-scoped surface becomes capable of
  carrying E2E content, which is M16's third bound and is the one thing here that does not depend on the
  permission model at all.

### An author reads the prior versions of their own messages
- **Raised**: M16a, at design time — the roadmap entry asked for it to be decided rather than defaulted
- **Verdict**: accepted, and deliberate — the disclosing direction, for once
- **Why**: `History` lets a message's author read its history without `PermManageMessages`. Refusing
  somebody the prior versions of their own words is hard to defend, and the carve-out discloses nothing to
  anybody new: they wrote every version in it. It is checked after the row is loaded, so it is an
  authorization question rather than an input one (M12's "refuse before explaining"), and it only ever
  *widens* who may read — a bug in that branch cannot turn into a disclosure to a stranger, which is why
  it is the safe direction for a carve-out to fail in.

  Recorded because it is the one place M16a's reader is not a moderator, and because M15's erasure entry
  named "M16a's disclosure decision widens the reader beyond moderators" as its own reopening condition.
  That condition was written before this decision existed and fires on it as literally worded; it is
  reworded in the same commit as this entry rather than left to be argued about later. The exposure that
  entry accepts is unchanged — an author reading their own text is not somebody else reading it.
- **Reopens if**: the carve-out is ever widened past the author — to a channel's members, to everyone who
  can read the backlog, or to a role — at which point M15's erasure entry genuinely does reopen, because
  what nobody can erase would become readable by people who did not write it.

### `message_edit_history` grows without bound, and a moderator cannot remove a row from it
- **Raised**: M16a, `/security-sweep` (the unbounded-growth class `/security-review` cannot report at any
  confidence)
- **Verdict**: accepted risk
- **Why**: nothing bounds how many versions a message accumulates. `Update` carries no edit counter and no
  cooldown, `auth.RunSweeper` does not touch this table, and there is no delete query for it anywhere —
  the only `ON DELETE CASCADE` fires on a *hard* message delete, and M15 made message deletion soft. So
  every edit appends a row that nothing in the product can remove.

  Per request the cost matches a send and the shape does not, which is the part worth recording. An edit
  stores the content the edit *displaced*, so posting 4,000 characters and then editing repeatedly writes
  up to 4,000 characters per request — 16 kB in an emoji-heavy script, since the bound counts runes
  (M15). The same bytes sent as messages would be visible in a channel, enumerable by a listing, and
  deletable by a moderator holding `PermManageMessages`. These are none of those: no surface enumerates
  history rows across messages, and deleting the message leaves them in place because the delete is soft.
  Filing the equivalent volume as *reports* is bounded by `000021`'s partial unique index; nothing
  equivalent bounds this.

  Accepted rather than fixed because both fixes are worse than the thing. An edit ceiling makes a message
  uneditable after N corrections, which is the worse product and — on the one table whose rows can never
  be erased — the worse privacy story. A retention window forgets exactly the version somebody opened the
  history to look at, which is the argument `audit_log_entries` already carries for not being swept; M125
  owns pruning if it is ever wanted. What holds today is the base rate limiter, which is the same thing
  holding `messages` itself.
- **Reopens if**: an automation path reaches `Update` without passing the per-IP limiter — webhooks (M60)
  and bot automation (M22) are both that shape, and both would make edit volume cheap in a way a human
  client is not. Also reopens if message deletion ever becomes hard rather than soft, since the cascade
  would then be a real deletion path and this entry's premise inverts. Also if a per-guild storage quota
  arrives (M125's territory), at which point this table needs to be inside it rather than beside it.

### An Instance Admin reads any message's history on any instance guild, and no record is kept
- **Raised**: M16a, `/security-sweep`
- **Verdict**: not a vulnerability — pre-existing, and outside rule 14 as written
- **Why**: `guildauth.Authorize` short-circuits on ADR 0008's layer 1, so an Instance Admin passes this
  route's gate in a guild they are not a member of and in a channel no overwrite grants them. Rule 14
  requires `instance_audit_log` for "bans, report resolution, license/entitlement changes, admin-tier
  grants" — every one a *mutation*. A read is not named there, and no read is audited anywhere in this
  codebase.

  Not new here: an Instance Admin has read every channel's backlog through `messages.List` since M15 by
  the same short-circuit, so this route widens *what* they reach (prior versions, and content in channels
  hidden from ordinary moderators) rather than *whether* they reach it. Recorded because M16's audit found
  the mutation half of exactly this shape and gave it a tripwire plus a home in M72, and the read half
  would otherwise look covered by that when it is not — M72's instruction says "every guild-scoped path an
  Instance Admin can reach", which reads as though it includes this and means actions.
- **Reopens if**: M72 decides rule 14's "action" includes reads, which is the decision that would make
  this a gap rather than a non-subject — and it is a live question, because an instance operator reading
  private guild conversations unlogged is the case an audit log exists for. Also reopens if the Instance
  Admin tier is ever granted to anyone but the operator, since the whole of this rejection rests on that
  tier being the person who already holds the database.


### A guild's recording log hands its reader every message in the guild, including channels they cannot view
- **Raised**: M16b, at planning — the question the milestone turns on, and nothing in the doc set answered it
- **Verdict**: accepted, and it is the feature rather than a consequence of it
- **Why**: `GET /guilds/{guild_id}/message-audit` returns the content of every message a recording guild
  has produced. There is no per-object step of the kind the two surfaces before it had — M16 bounds
  content by a report existing, M16a takes a bare message id — so this is the widest read in the API, and
  it deliberately does not filter by what the caller can currently see. Filtering on present visibility is
  the tempting answer and fails for M14's reasons one level up: it would let somebody hide what was said
  by locking a channel down afterwards, and a recording a moderator cannot read in the channel that most
  needs reading is not a recording. The boundary is therefore the permission alone, which is why
  `PermViewMessageAudit` is a **bit of its own** (position 21) rather than a reuse. Each of the three
  candidates was rejected for a different reason, recorded in `roles/permissions.go`: `VIEW_AUDIT_LOG`
  reads moderation metadata and its own ledger entry names "the log ever carries message content" as its
  reopening condition, so reusing it would satisfy that condition's letter while doing what it names;
  `MANAGE_MESSAGES` means "delete somebody else's message", and a guild wanting spam removed has not
  decided that moderator reads its private channels; folding it into `MANAGE_GUILD` would mean the only
  way to let somebody read the log is to let them switch the recording off. The bit is granted by default
  to nobody, implied by no other bit, and un-grantable by somebody who does not hold it
  (`refuseEscalation`). `PermAdministrator` confers it, as layer 3 confers every bit in the guild that
  granted it — stated because "implied by nothing" was the original wording here and is not true of the
  one bit whose whole meaning is that it implies the rest.
  Members are told the guild records them — the flag is on the guild payload behind `PermViewChannel`,
  readable by anybody in the guild.
- **Reopens if**: `PermViewMessageAudit` is ever granted by default, implied by another bit, or added to
  `defaultEveryonePermissions` — the same condition M16's entry carries for `PermManageMessages`, and the
  whole of what bounds this. Also reopens if the recording flag stops being readable by ordinary members,
  since the disclosure is accepted on the basis that the people being recorded can find out.

### Members of a recording guild are told nothing until M62a draws the screen
- **Raised**: M16b, at planning — the notice decision its done-when requires
- **Verdict**: accepted risk, with a named home and a gap that is real in the meantime
- **Why**: the notice belongs on screen `6e`, which states the guild's recording status in **both**
  directions and belongs to M62a — settled at M15's planning and recorded in that entry. M16b's own
  obligation is the structural half and is met: `message_audit_enabled` is on the guild payload behind
  `PermViewChannel`, so any member can read it and a client can state either case positively rather than
  letting the absence of a warning carry a meaning nothing guarantees. **The gap is the interval.** No
  client renders that field today, so between this milestone and M62a a guild can record its members with
  nothing telling them. That is stated here rather than discovered at M62a, because a done-when satisfied
  by an API field reads as though the product tells people, and it does not yet.
- **Reopens if**: M62a ships without the negative case, which is the half that makes the positive one
  trustworthy. Also reopens if recording ever becomes settable by anybody other than the guild's owner,
  since "the person who decides is the person who answers for it" is part of why one screen is enough.
  M62a additionally owns the question of whether a member is told at the moment they *join*, which is
  recorded in its entry and will land here with its own reopening condition.

### An Instance Admin cannot start a guild's recording, which is the first place layer 1 is narrower than layer 2
- **Raised**: M16b, at planning
- **Verdict**: deliberate narrowing, not an accepted risk — and temporary by construction
- **Why**: ADR 0008 makes the Instance Admin tier an instance-wide authority that acts on guilds it is not
  in, and `guildauth.Authorize` short-circuits on it everywhere. `guilds.mayFlipMessageAudit` refuses it
  for this one field, because rule 14 requires every Instance Admin action to reach `instance_audit_log`
  and that table is M72's: the tier could otherwise switch recording on for a guild it has never joined,
  read everything said afterwards, and leave nothing anywhere that says it did. The answer was first taken
  the other way — allow it, ledger the gap, as M16a did for the read — and reversed on reversibility:
  lifting a refusal when M72 lands is additive, while withdrawing a capability operators have built around
  is not. **What it buys is narrow and should not be oversold**: `Decision.Allows` still answers for layer
  1 on the *read*, so an Instance Admin reads any recording guild's log, which is M16a's entry above and
  stays open. The line drawn here is between reading what a guild chose to collect and deciding what the
  instance collects about a guild that chose nothing.
- **Reopens if**: M72 lands, at which point the action becomes recordable and the refusal should be
  lifted rather than left as folklore. **Also reopens if a second layer-1 exception appears anywhere** —
  one exception is a documented departure, two that do not know about each other are a rule enforced by
  nobody, and at that point it needs a mechanism the way `revokeEverything`, `RequireLiveSession`,
  `factorProof` and the `guildauth` chokepoint each did.
- **Amended at M16b's `/security-sweep`, verdict unchanged, consequence added.** The refusal is
  **symmetric** and the entry above only argued the "on" direction. `UpdateGuild` is the only writer of
  the column and there is no instance-admin guild surface at all (`/instance` is operator-token bootstrap
  and invites), so an Instance Admin can no more switch a guild's recording **off** than on. An operator
  taking a complaint — "this guild is recording us and we want it stopped" — has no lever short of
  deleting the guild, which destroys every channel and message in it and punishes the members for the
  owner's choice. That is heavier than the situation usually warrants, and it is a real cost of choosing
  the reversible direction. Accepted rather than fixed because the alternative reintroduces exactly what
  the entry withholds: a tier that can reach into a guild's recording setting while rule 14 has nowhere to
  record that it did. M72 should settle both directions together rather than lifting only the one this
  entry names, and the banning and unpublishing levers `guilds.discoverable_locked_at` already models —
  an admin lock the owner's own toggle is refused against — is the shape worth copying if it wants a
  narrower answer than "delete it".

### `message_audit_entries` grows without bound and a recording guild roughly doubles its message storage
- **Raised**: M16b, at planning — the class `/security-review` structurally excludes
- **Verdict**: accepted risk — it is what the guild asked for, and the alternative defeats the feature
- **Why**: a recording guild writes one row per message, one per edit and one per delete, each carrying a
  full copy of the content, on a table nothing sweeps. Growth is therefore faster than `messages` itself
  and unbounded, and it is gated only at the *toggle*: once recording is on, every member with
  `SEND_MESSAGES` adds rows at the send rate limit. That is the shape the `audit_log_entries` entry
  accepts and this one is wider, which is why the roadmap calls it "the expensive choice" and why the
  default is off. A recording you can exhaust, or one that refuses writes when full, is not a recording.
  **Unlike the audit log, this one has a lever**: M125 now names this table in its pruning scope, which
  M16b's planning found it had never been told about despite three documents promising it. A guild that
  switched recording on for itself and can switch it off is not the accountability record
  `audit_log_entries` is, so a retention window is coherent here where it is not there.
- **Reopens if**: M125 ships without this table after all, which would leave the growth with no lever at
  all. Also reopens if a per-guild storage quota arrives, at which point the ceiling belongs beside the
  channel and role ones rather than here — and this table, not `messages`, is what a recording guild fills
  first.

### A recording log's read is a bulk-export primitive and sits in the ordinary rate-limit bucket
- **Raised**: M16b, `/security-sweep`
- **Verdict**: not a vulnerability — consistent with the surface it most resembles, and the bit is the
  boundary
- **Why**: `GET /guilds/{guild_id}/message-audit` returns up to 100 full message bodies per request and
  pages backwards without limit, so a `VIEW_MESSAGE_AUDIT` holder can pull a recording guild's entire
  conversation as fast as the base limiter allows. There is no stricter bucket, and it carries the
  highest-value payload per request in the API. That is deliberate and matches `GET
  /guilds/{id}/audit-log`, which is the same shape — a paginated moderation read in the base bucket whose
  gate is a permission granted to nobody by default. A stricter bucket would throttle the legitimate use
  (an investigation reads a lot, quickly) without bounding the illegitimate one, because the holder can
  simply read slower; what bounds this is who holds the bit, which is the disclosure entry above. Recorded
  because rate limiting is a class `/security-review` cannot report at any confidence and this is exactly
  the surface somebody will raise it about.
- **Reopens if**: the bit is ever granted by default or implied by another, which is the disclosure
  entry's condition and the only thing standing here — or if an export surface arrives that makes bulk
  reads a first-class operation, at which point the per-*user* throttle §14.14 wants for reports is the
  same question asked here.

### "Private" describes the tag, not the message it is on
- **Raised**: M17, at planning — the word needed defining before anything was built
- **Verdict**: not a vulnerability — a naming hazard, closed by making the API say what it means
- **Why**: a private tag is invisible to everybody but its creator: absent from their tag listing, absent
  from the tags on any message they read, and refused as 404 by id. What it does **not** do is make the
  message private. Somebody who privately tags a message `evidence` has annotated something the whole
  guild can still read, and nothing about the tag changes who sees the message. That is the correct
  design — tags annotate, restriction is M61's whispers — and it is written here because "private" is a
  word members will read as stronger than it is. The contract says so in both schema descriptions.
- **Reopens if**: a tag ever gates *visibility* of anything, at which point the privacy of the tag and the
  privacy of what it marks stop being separable and the word has to mean one thing. Also reopens if tag
  names become searchable across a guild, since a private tag's name would then be a channel for its
  creator to leak through without anyone seeing the tag itself.

### Creating a shared tag changes a guild's vocabulary and writes no audit entry
- **Raised**: M17, at planning
- **Verdict**: accepted risk — the alternative costs more than the entry is worth
- **Why**: rule 2 covers guild-scoped *administrative* mutations, and applying or removing a tag
  exercises authority over nobody, so those are outside it the way a message send is. Creating a **shared**
  tag is closer to the line — it adds to something everybody sees and needs `PermManageMessages` — and it
  is still not audited. Two reasons. The entry would carry a tag id and a name and nothing else, which is
  a thin record to page a moderation log for. And the verb would have to join `guilds.AuditActions()`, and
  therefore the vocabulary `GET /guilds/{id}/audit-log` validates an `action` filter against, which M14's
  tripwire exists to make a decision rather than a reflex. Bounded instead: 200 shared tags per guild, and
  the permission is granted to nobody by default.
- **Reopens if**: a tag gains a *consequence* — one that hides a message, routes it, or changes who can
  see it — at which point creating one is an administrative act with an outcome and belongs in the log.
  Also reopens if the shared ceiling is raised far enough that "bounded" stops being the answer to why
  nobody needs to audit it.
- **Amended at M17's `/security-sweep`, verdict unchanged for creation, and its first sentence was too
  broad.** "Applying or removing a tag exercises authority over nobody" holds for your *own* applications
  and not for somebody else's: a moderator taking another member's label off a message is authority over
  that member, and deleting a shared tag cascades away every other member's applications of it. Rule 2
  says administrative "whoever performs it", and the sweep reproduced a moderator removal writing no entry
  while the same moderator deleting that member's message wrote one. Both are now audited — `tag.remove`
  and `tag.delete`, written only when the act reaches somebody else's tagging. Creating a shared tag, and
  deleting one only its deleter ever applied, remain unaudited for the reasons above: vocabulary, not
  authority over anybody.

### `message_tag_applications` grows with tagging and nothing sweeps it
- **Raised**: M17, `/security-sweep`'s category rather than `/security-review`'s
- **Verdict**: accepted risk — bounded per message, unbounded in total, and the totals are small
- **Why**: a row per (tag, message) pair, cascading away with either. The per-message bound is real: a
  message can carry at most the guild's tag count, which the ceilings cap at 200 shared plus 100 private
  per member. The total is not bounded — it grows with messages tagged — but each row is four bigints and
  a timestamp with no content, which is the difference from `message_audit_entries`, where the same
  argument had to accept a full copy of every message. Applying is permission-gated and rate-limited like
  any mutation.
- **Reopens if**: tags gain a payload — a note, a colour, anything that makes a row more than a pairing —
  or if an automation path reaches `Apply` without passing the per-IP limiter, which is M22's and M60's
  territory and the same condition `message_edit_history`'s entry carries.

### The private-tag ceiling is a count and an insert, and two requests can both pass it
- **Raised**: M17, `/code-review` and then `/security-sweep`
- **Verdict**: accepted risk — the overshoot is bounded by one burst, and the ceiling is a nuisance bound
  rather than an authority one
- **Why**: `checkCeiling` counts a member's private tags and `CreateMessageTag` inserts, in one READ
  COMMITTED transaction with no lock, so two creates can each see 99 and both commit. Shown by hand in
  psql: two sessions interleaved with the second committing between the first's count and its insert, both
  counted 94 and both inserted. It did **not** reproduce under load (30 concurrent requests landed exactly
  at 100), which is M15's point about races that are real and too rare to hit by racing. The overshoot is
  bounded by how many requests land inside one window, never cumulative: once the count is at or over the
  cap every later create is refused. What the ceiling defends against is one member writing rows without
  limit, and a burst-sized overshoot does not reopen that. M12's role-position race took an advisory lock
  because position is the hierarchy M13 enforces over, and a collision there is a wrong answer; one tag
  too many is not.
- **Reopens if**: anything reaches `Create` without passing the per-IP limiter (a bulk import, M22's
  automation, M60's webhooks), which widens the window from a burst to a loop; or the ceiling starts to
  mean something beyond nuisance, such as a quota an instance bills or sizes against.

### Near-identical tag names get past the uniqueness indexes
- **Raised**: M17, `/code-review`
- **Verdict**: not a vulnerability
- **Why**: the partial unique indexes key on `lower(name)` and `checkName` trims and normalizes nothing,
  so `spam`, `spam `, ` spam` and an NFD or zero-width-joined `spam` are distinct tags that look the same
  in a picker. That is a legibility problem only among people who can already create shared tags, because
  a **shared** name needs `PermManageMessages`, and a **private** name is seen by nobody but its creator.
  Nobody without the moderation bit can put a lookalike in front of anybody else. Normalizing would also
  mean the instance deciding what counts as the same word in every script, which is the argument
  `checkName`'s comment makes for leaving names alone.
- **Reopens if**: creating a shared tag stops needing the moderation bit, or a private tag becomes visible
  or searchable by anyone else. Both would put a lookalike in front of somebody who did not choose it.

### An ownership transfer does not ask the recipient
- **Raised**: M13a, at planning
- **Verdict**: accepted risk — the refusal at the ceiling bounds it, and consent would double the milestone
- **Why**: `POST /guilds/{guild_id}/owner` makes the recipient owner immediately. Two harms follow from not
  asking. A guild's owner answers for it — its moderation, and whatever it holds — and somebody can be
  made that without agreeing. And ownership counts against the owned-guild ceiling, so handing a victim
  unwanted guilds eats the allowance they would use to create their own. The second is bounded where it
  matters: a recipient at the ceiling is refused, so a transfer can never push somebody past it — true
  because the count is taken under a per-account lock that Create takes too, which /code-review found
  missing on the first draft, where concurrent transfers from different guilds could — and a
  recipient below it can delete or transfer the guild away again — owner-only actions they now hold. An
  offer-and-accept flow closes both, and costs a second endpoint and a table of pending offers with a TTL
  and a sweep, which is roughly the milestone again.
- **Reopens if**: ownership starts to carry a cost the recipient cannot shed by deleting — billing, a
  paid tier, legal responsibility an instance attaches to ownership, a moderation duty with a deadline —
  or if M72a's entitlements make the ceiling large enough that exhausting it stops being the bound it is
  now. Also reopens if transfers show up in abuse reports, which is the evidence this reasoning predicts
  will not arrive.

### A former owner's in-flight request can still act as owner for one transaction
- **Raised**: M13a, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: the transfer holds the guild row against every path that reads `owner_id` off it — `Delete`,
  `Update`, `RemoveMember`, the transfer itself — but role and member management decide layer 2 through
  `guildauth.Authorize`, which resolves unlocked. A grant authorized a millisecond before a transfer
  commits can land after it. That is every demotion's window, and the M15 mute-race entry's reasoning:
  ownership is the strongest demotion, not a new class of race.
- **Reopens if**: something takes a guild *from* an owner acting against it — M72's enforcement
  transferring an abusive owner's guild is the likely one — where the owner racing the transfer is the
  expected case rather than a contrived one.

## M18 — gateway protocol core

### HELLO tells anybody who connects the server's version
- **Raised**: M18, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: HELLO is sent before IDENTIFY and carries `version`, so an unauthenticated socket learns which
  release is running. The same fact is already public by obligation: `GET /meta` answers unauthenticated
  with the source revision, because AGPL §13 owes the source offer to everyone. The version is also what
  lets a client refuse an incompatible server with a message naming the side to upgrade (ADR 0033), which
  the client cannot do without it.
- **Reopens if**: `/meta` stops disclosing the revision, or an instance gains a reason to hide its release —
  at which point HELLO would be the one place still announcing it.

### A close reason tells a token holder that its sign-in has ended
- **Raised**: M18, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: IDENTIFY and RESUME close with 4004 "invalid token" for a token that does not verify and 4004
  "signed out" for a verified token whose sign-in ended. Telling the two apart needs a token the instance
  signed for that account, whose holder learns only what the REST routes behind `RequireLiveSession`
  already answer. Nothing about an account that the caller holds no token for is distinguishable.
- **Reopens if**: the distinction ever depends on something other than a verified token — a reason that
  differs for an unknown account against a known one, say.

### A disconnected session holds a replay buffer for two minutes, sixteen per account
- **Raised**: M18, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: a session keeps up to 512 frames and 1 MiB for `ResumeWindow`, and an account may hold sixteen
  sessions per process, attached or not, so one account can pin 16 MiB for two minutes. The buffer fills
  only with events the account may receive, from guilds it belongs to, so filling it needs busy guilds as
  well as sessions, and each session needs a sign-in, which is rate-limited per address and gated by
  registration. A fresh IDENTIFY from a device supersedes that device's detached sessions, so a reconnect
  loop does not accumulate them.
- **Reopens if**: registration loses its anti-automation (M67a) or its invite gate on the flagship, or M93's
  metrics show replay buffers as a meaningful share of a process's memory.

### The rate-limit store fails open when Redis cannot answer
- **Raised**: M18, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: M18 made the Redis-backed store real, and with it the fail-open `onStoreFailure` has had since
  M1: a store that errors lets the request through, on REST and at IDENTIFY alike. Failing closed would
  turn a Redis outage into a total outage of every rate-limited route, sign-in included. What stays
  bounded without the limiter: argon2id runs behind its concurrency gate, so a login flood during an outage
  costs throughput rather than memory, and nothing an attacker sends can make the store fail.
- **Reopens if**: a client can cause store errors, since that turns the fallback into a bypass; or a limit
  becomes a security control with nothing behind it — a lockout after failed logins, say, or a second-factor
  attempt counter kept only in the limiter.

### A revocation's cutoff trusts the replicas' clocks
- **Raised**: M18, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: a revocation closes connections opened with a sign-in minted before its cutoff snowflake, and
  snowflakes from different replicas compare only as well as the replicas' clocks agree. Skew in one
  direction closes a connection opened just after the revocation, which reconnects at once. In the other it
  misses a connection opened just before, and the periodic liveness check closes that one within
  `LivenessInterval`, because it asks about the sign-in rather than trusting the cutoff.
- **Reopens if**: the periodic check is removed or its interval lengthened past the access token's life, or
  replicas run without synchronized clocks.

### A lost revocation leaves a connection open until the next liveness check
- **Raised**: M18, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: revocations travel on the bus, which is at-most-once, so one can be dropped. The connection it
  should have closed is closed by the heartbeat's liveness check within `LivenessInterval` (five minutes),
  a third of the fifteen minutes §17.10 already accepts for a signed-out token on REST. Checking every
  heartbeat instead would put a database read on every connection every forty seconds.
- **Reopens if**: the interval is raised to or above `AccessTokenTTL`, or the bus drops messages often
  enough that the interval is the ordinary close rather than the fallback.

### A resumed stream replays frames authorized before a permission change
- **Raised**: M18, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: each frame in a session's buffer passed the audience check when it was fanned out, which is the
  moment a connected client would have received it. A client that was disconnected then receives it on
  resume, up to two minutes later. It is what an attached client would already hold, delivered late rather
  than a wider audience.
- **Reopens if**: the resume window grows long enough that "late" means "after a demotion the member was
  meant to feel", or an event type carries something whose authorization is meant to lapse — E2E key
  material being the obvious future case.

### The per-address cap does not bound unidentified sockets across many addresses
- **Raised**: M18, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: the sweep found one address able to hold about four hundred sockets with no credential behind
  them, which is now bounded at sixty-four per address (IPv6 by /64) and ten seconds each. What remains is
  the same attack from many addresses, which is network-scale denial of service: the process cannot tell
  a botnet from a busy instance, and the bound belongs to whatever sits in front of it — the flagship's
  Ingress (M112) or a self-hoster's proxy.
- **Reopens if**: an instance runs with nothing in front of it, or M93's metrics show unidentified
  sockets as a meaningful share of a process's connections.

### A token from an ended sign-in can supersede its device's detached sessions before it is refused
- **Raised**: M18, `/security-review` (excluded there as denial of service)
- **Verdict**: accepted risk
- **Why**: IDENTIFY registers the new session before it asks whether the sign-in is live, which is what
  leaves a concurrent sign-out no gap to fall into, and registering supersedes the device's detached
  sessions (`gateway.newSession`, then `checkLive`). So a token from a sign-in that has ended, presented for
  the same device, is refused only after the device's detached sessions are dropped. The owner loses their
  resume buffer and identifies afresh, losing nothing but a resync. It needs an access token for that very
  device, under fifteen minutes old, held by somebody else, which is a stolen credential, and the token can do
  nothing else here.
- **Reopens if**: a detached session comes to hold something a resync cannot rebuild, or superseding starts
  doing more than dropping buffers. Either would make it worth checking liveness before superseding, which
  means taking the account's session limit without counting the sessions about to be superseded.

## M19 — daemon as gateway client

### An instance served over plain HTTP gets the daemon's tokens over plain WebSocket
- **Raised**: M19, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: `credentials.ParseInstanceURL` admits `http://` for a self-hosted instance behind a proxy on a
  private network, and the gateway URL follows the instance's scheme, so such an instance gets IDENTIFY's
  access token over `ws://`. It already gets the password at login and the refresh token at every renewal
  over the same cleartext. The CLI says so out loud at the login that chose it. The gateway adds a channel
  and no new class of exposure.
- **Reopens if**: `http://` stops being refused for public hosts by convention and starts being chosen for
  them, or the daemon gains a credential the REST path never carries over that connection.

### The instance's HELLO steers how often the daemon refreshes
- **Raised**: M19, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: the daemon judges expiry on the instance's clock (ADR 0010), so a `server_time` far ahead makes
  every token look expired and one far behind makes them look fresh. The instance that sends HELLO is the
  one that issued the token and decides whether it is valid, so it gains nothing it did not have. A forged
  HELLO from anybody else needs the TLS connection. Refreshes are spaced at least `minRefreshGap` apart
  whatever the estimate says, so rotation cannot be driven into a loop.
- **Reopens if**: the gateway and the refresh endpoint can be different parties (a gateway on another
  origin), or the refresh floor is removed.

### A panic in the gateway connection is logged with its value
- **Raised**: M19, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: `converse` recovers a panic and logs `Interface("panic", p)`. Nothing on that path panics with a
  value built from a frame: decoding errors are returned and logged as the event type only, and the state's
  appliers panic on nothing they are given. So the value is a runtime error naming code, not content.
- **Reopens if**: a sink or decoder panics with a value derived from the payload, such as a message's text
  in a formatted panic, at which point the log carries content rule 13's spirit keeps out of it.

### A running daemon adopts whatever credential appears in its state directory
- **Raised**: M19, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: the watcher reloads on any change to `account.json`, and the daemon signs in with what it finds.
  The directory is `0700` and per-user (`paths.StateDir`), so whoever can write that file is the account the
  daemon runs as, which is the trust boundary ADR 0025 draws for the credential itself. They could equally
  run their own daemon.
- **Reopens if**: the state directory's mode is relaxed, or the daemon comes to run as a different account
  from whoever writes the store (a system service, which CLAUDE.md forbids).

### The daemon logs the account's username and display name at every READY
- **Raised**: M19, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: both are public within any guild the account shares, both pass `termsafe.Text` first, and the log
  lives in the `0700` state directory beside the credential it would be less valuable than. The username was
  already logged at every sign-in since M7.
- **Reopens if**: the daemon's log leaves the machine (a crash reporter, a support bundle uploaded on the
  user's behalf), or the line grows to carry something that is not already visible to other members.

## M20 — daemon↔client local IPC

### Any process running as the user can act as the account through the relay
- **Raised**: M20, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: the attach socket is the first-party tier by decision (rule 16, B1): every process running as
  the user can attach, and the relay performs anything the account may do outside `/auth/*`, `/instance/*`
  and `/users/@me/sessions` — deleting a guild, transferring its ownership. Those processes can already read
  the state directory, and on the file backend the refresh token itself; on Linux an unlocked Secret Service
  answers them too. The socket adds no reach that tier lacked, and the credential surfaces stay off it.
- **Reopens if**: the socket admits anything below the first-party tier, or the credential's storage gains
  per-process access control (a keyring that prompts per application) that the relay would then bypass.

### A local process can hold every attach slot, or make the daemon buffer gigabytes of relayed bodies
- **Raised**: M20, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: 64 clients, each holding a slot for the ten-second handshake, deny the CLI the socket; 64 clients
  with 16 requests each against endpoints answering near the 8 MiB body cap make the daemon hold that much
  at once. Both need a process running as the user, which can stop the daemon outright with a signal.
  Every bound that matters against the *instance* — the body cap, the frame caps, the queue drop — holds.
- **Reopens if**: the socket admits another tier, or a daemon component other than the relay starts
  holding per-client memory that is not bounded by the queue.

### A squatted named pipe could impersonate the CLI that connects to it
- **Raised**: M20, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: a pipe server can impersonate a client at the level the client allows, and Windows defaults to
  full impersonation. go-winio's `DialPipeContext` dials with `SECURITY_SQOS_PRESENT` and
  `SECURITY_ANONYMOUS` (`tryDialPipe`), so a squatter learns nothing it can act as, and `ipc.DialAt` then
  refuses a pipe another account owns before writing a byte.
- **Reopens if**: the dial moves to `DialPipeAccessImpLevel` with a higher level, or the owner check moves
  after the first write.

### A relayed request can make the session renew by drawing a 401
- **Raised**: M20, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: the relay reports a 401 with `Rejected`, which renews the token. It needs the instance to refuse
  the daemon's own token, which a live sign-in only meets on a revoked one, and renewals are at least
  `minRefreshGap` apart however often they are asked for.
- **Reopens if**: an endpoint answers 401 to a valid token for a reason of its own, or the refresh floor
  is removed.

### The attach socket briefly has the umask's mode before it is narrowed
- **Raised**: M20, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: `attach.Listen` binds and then `chmod`s to `0600`, so for an instant the socket's mode is the
  umask's. It sits inside the `0700` state directory, which nobody else can traverse, so the window opens
  nothing. The chmod is for a directory somebody loosened.
- **Reopens if**: the socket moves out of the state directory, or the directory's mode stops being
  enforced (`paths.tighten`).

## M20a — first usable client, end to end

### Leaving and rejoining sheds a member-tier restriction
- **Raised**: M20a planning (question F), from the rejoin question M13 routed to the first join path
- **Verdict**: accepted risk
- **Why**: `RemoveMember` deletes the departing member's own overwrites, so a rejoin restores nothing — the
  property M13 wanted, since a silently restored deny is one nothing explains. Read the other way, a
  moderator's member-tier deny ("may not post in #general") lasts until its subject leaves, which needs no
  permission, and redeems any live invite. Keeping the deny's bits across a departure was weighed and not
  taken (F1): it is restrictions that should outlive a departure, and they belong with timeouts and bans,
  which are M74's. `TestARejoinIsACleanSlate` pins the behavior, and `member.join` records each arrival
  with the invite it used, so the cycle is visible in the audit log if not prevented.
- **Reopens if**: M74 lands without settling it, or M72a's direct join lands before M74, which makes the
  cycle free of any invite a moderator could revoke.

### A kicked member can rejoin through any live invite
- **Raised**: M20a planning
- **Verdict**: accepted risk
- **Why**: no guild ban exists. `PermBanMembers` anticipates one and no milestone builds it, so a kick
  removes a member until they hold a live invite, which is also what a kick means on Discord. A guild's
  answer today is to revoke its invites, which `MANAGE_GUILD` can list and revoke.
- **Reopens if**: M74 builds guild bans and they do not gate both invite redemption and M72a's direct join.

### An Instance Admin can create and revoke invites into any guild, and only the guild's log records it
- **Raised**: M20a, part 3
- **Verdict**: accepted risk
- **Why**: `guildauth` passes the tier at layer 1, so invites reach as far for it as for an owner — M17's
  settlement for tags, chosen over a second layer-1 exception. The guild's own audit log names the admin as
  the actor of `invite.create` and `invite.revoke`; rule 14's `instance_audit_log` does not exist until
  M72. The same gap M16a's entry records for the edit-history read.
- **Reopens if**: M72 builds `instance_audit_log` and the tier's guild actions are not routed into it, or a
  tier action ever stops appearing in the guild's own log.

### Any signed-in account can test codes through preview and redemption
- **Raised**: M20a, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: a code is sixteen characters of a twenty-letter alphabet, about 69 bits, so guessing one is
  hopeless at any rate; preview, redemption and revocation also share a 30-a-minute bucket built through
  `internal/platform/ratelimit` (the /64 grouping rule included), and every dead, unknown or malformed
  code is one 404. Preview requires a signed-in account (G1), so the probe is not anonymous either.
- **Reopens if**: codes get shorter, the shared bucket is removed or loosened, or Phase O opens an
  unauthenticated preview without a bucket of its own.

### A preview names a channel to a code holder who cannot see it
- **Raised**: M20a, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: `PreviewInvite` returns the channel's name and id to anyone holding a live code, including a
  member of the guild an overwrite hides that channel from. The invite's creator held view of the channel
  (creation folds `PermViewChannel` in) and chose to hand a way into it to whoever holds the code; the name
  is what a joiner needs to know where they will land. Listing and revoking, which act on invites rather
  than reading one the caller was given, do filter by the channel's view.
- **Reopens if**: a preview starts carrying more of the channel than its name (its topic, its messages),
  or invites can be created by somebody who cannot see the channel.

### A refused redemption holds a share lock on the guild for its transaction
- **Raised**: M20a, `/security-sweep`, against M13a's "refuse on unlocked data, then lock"; re-judged after
  M20a's second `/code-review` strengthened the lock
- **Verdict**: not a vulnerability
- **Why**: `RedeemInvite` locks the guild before it checks membership and the joined ceiling, so a caller
  refused at the ceiling holds the lock for its short transaction. The lock was `FOR KEY SHARE`, which
  conflicts only with deletion, and became `FOR SHARE` so a join cannot answer with an owner a transfer is
  changing: it now also makes a rename, an ownership transfer or a deletion of that guild wait for the
  redemption to finish. It still blocks no insert into a child table, so messages and other joins do not
  queue behind it, unlike the `FOR UPDATE` M13a's lesson was about. Reaching it needs a live code, and each
  attempt is one short transaction inside the 30-a-minute bucket.
- **Reopens if**: the lock taken there becomes `FOR NO KEY UPDATE` or stronger, a path reaches it without a
  live code, or the bucket is removed.

### An invite code typed on the command line is in the process list and the shell history
- **Raised**: M20a, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: `norite invite show|join|revoke <code>` take the code as an argument, as M10's `norite instance
  invite revoke <code>` already does, so another account on the same machine can read it from the process
  list while the command runs, and it stays in the shell's history. A code is meant to be handed to people,
  and what it grants still needs a signed-in account to use. The request carries it in a body, never a
  path, so it reaches no server log (ADR 0029).
- **Reopens if**: Norite targets shared multi-user hosts as a supported client environment, at which point
  the code verbs gain a stdin form, as `message send --content -` has.

### An API token holding `guilds.write` can create invites
- **Raised**: M20a, `/security-sweep`, beside the redemption fix
- **Verdict**: not a vulnerability
- **Why**: creating an invite is bounded by the owner's `PermCreateInvite` on the channel, as every
  delegated guild action is bounded by its owner's permissions, and a bot handing out invites is an
  ordinary use of a guild-management token. Redeeming is where the line is drawn: joining puts the
  account itself into a guild, so since M20a's sweep it needs a logged-in person, as an ownership transfer
  does.
- **Reopens if**: invites gain a power beyond letting an account in (a role granted on joining, say), or
  tokens gain per-guild scoping, at which point a token's invite into a guild it was not scoped to is
  the escalation.

### A message's content can draw lines shaped like another message
- **Raised**: M20a, second `/security-sweep`, against the terminal client
- **Verdict**: not a vulnerability
- **Why**: `termsafe.Block` keeps line breaks, so one message can carry text reading "Alice  12:00". The pane
  draws every content line indented two columns and unstyled (`messageLines`), while an author's line starts
  at column zero in bold, which no content can produce: everything that could move the cursor or set a style
  is removed. A message saying something untrue is a message, as in any chat client.
- **Reopens if**: content is drawn unindented, or M43's renderer interprets markup able to style text the
  way an author's line is styled.

### The composer and the code box draw a pasted bidi override as typed
- **Raised**: M20a, second `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: Bubbles' input sanitizer removes control characters from what is typed or pasted, and leaves
  format characters such as U+202E. What the box shows is the person's own input, which `termsafe` leaves
  out of scope ("a person cannot attack their own terminal by typing into it"). Once sent, the message
  reaches every reader through `termsafe.Block`, which replaces it.
- **Reopens if**: either box is ever filled from somewhere other than the keyboard and the clipboard, for
  example a draft synced from another device or a quoted reply, without passing through `termsafe` first.

### The live-invite ceiling is a count and an insert, and concurrent creates can overshoot it
- **Raised**: M20a, second `/security-sweep`
- **Verdict**: accepted risk
- **Why**: `CreateInvite` counts live invites and then inserts, unlocked, the shape the channel ceiling and
  the private-tag ceiling already take (see "The private-tag ceiling is a count and an insert"). The ceiling
  bounds a listing that is returned whole. A burst-sized overshoot needs `PermCreateInvite`, and does not
  make that listing unbounded.
- **Reopens if**: something starts relying on the ceiling as a guarantee rather than a bound, or creation
  becomes reachable without `PermCreateInvite` on the channel.

### A resync makes the client read its whole home again at once
- **Raised**: M20a, second `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: on resync (4100) the client reattaches without backing off and reloads home: one guild listing,
  then one channel listing per guild. Since the same sweep, that is at most `maxHomeGuilds` listings. An
  instance sending READY repeatedly provokes these requests, and every one goes back to that same instance
  through the daemon, so the cost lands on whoever caused it. The client's own work is paced by the
  instance's frames, and what it holds is bounded.
- **Reopens if**: a resync makes the client fetch from anybody other than the instance (link previews,
  media), or home's guild bound is lifted.

## M21 — config file

### A write through a symlinked `config.toml` reaches whatever file the link names
- **Raised**: M21, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: `config.Update` follows the link on purpose (`atomicfile.Options.FollowSymlink`), because the
  config is somebody's dotfile and a rename over the link would detach it. Planting a link means writing
  the user's own config directory, which is already their account. What can be written is narrow as well:
  a target that exists and is not TOML is refused before anything is written (`valid` in
  `daemon/config/edit.go`), and the bytes written are one validated value for a key the contract lists.
  The credential store and the instance wizard do not follow links at all.
- **Reopens if**: a value written by `set` or `import` can carry text chosen by somebody other than the
  user (an import that is not shown and confirmed first), or a second caller sets `FollowSymlink` on a
  file that holds a secret.

### A config keeps the mode its owner gave it, readable or writable by others
- **Raised**: M21, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: `atomicfile.Options.KeepMode` copies the existing file's permission bits, so a `config.toml`
  published at `0644` stays `0644`, and one left `0666` stays that too. No key holds a secret
  (`TestEveryKeyIsWellFormed` refuses a name shaped like one) and no key runs anything: the live keys are
  five colors and a clock. A new file is created `0600`.
- **Reopens if**: a key can name a command, a path that is loaded, or anything else that acts. That is
  M44's chords, whose roadmap entry now carries the refusal of a file others can write.

### An error can name a symbolic link's target, which is text from a filesystem
- **Raised**: M21, `/security-review`, confirmed by `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: `atomicfile.ErrReadOnly` and the permission-read error quote the resolved target, and a
  repository of dotfiles from a stranger can hold a link whose target is named with escape sequences.
  Every error the command tree prints goes through `termsafe.Block` in `cli/cmd/app/main.go`
  (`errorText`), so the text is inert there. A `ParseError` and a `Warning` are sanitized where they are
  made. The terminal client draws a load error in its hint row since M21's part 7, which is the
  condition below arriving: it passes the whole line through `termsafe.Text` in `applyConfig`, and
  `TestAConfigErrorIsDrawnInert` fails without it.
- **Reopens if**: a client prints an error from `daemon/config` or `daemon/atomicfile` without
  `termsafe`. The GUI's equivalent of that row is the next candidate.

### A crash between writing the temporary file and the rename leaves the temporary file
- **Raised**: M21, `/security-sweep`
- **Verdict**: accepted risk
- **Why**: `atomicfile.Write` removes its temporary file on every return, and a killed process returns
  from nothing. For the credential store that file holds a refresh token. It is `0600` from before its
  first byte, in the `0700` state directory, beside the token file it was about to replace, so it is
  readable by exactly who could already read the token. Both older writers behaved the same way before
  they were unified. For `config.toml` the leftover is a dot-file of settings beside the real file.
- **Reopens if**: a temporary file is created anywhere but the destination's own directory, or a writer
  of a secret sets `KeepMode`.

### Another program of the same user can hold the config's lock and stall every write for five seconds
- **Raised**: M21, `/security-sweep`
- **Verdict**: not a vulnerability
- **Why**: the lock is a file in the `0700` state directory (`lockFor` in `daemon/config/file.go`), so
  only the user's own processes can take it. A writer waits `lockWait` and then fails with `ErrLocked`,
  naming the cause; it never waits without bound, and reading the config takes no lock at all.
- **Reopens if**: the lock moves somewhere another account can open, or a read starts taking it.

### The TOML decoder is quadratic in the keys of one table
- **Raised**: M21, `/security-sweep`
- **Verdict**: accepted risk, after the bound was lowered
- **Why**: measured on a file of nothing but short keys: 6 ms at 16 KiB, 71 ms at 64 KiB, 612 ms at
  256 KiB, 9.99 s at 1 MiB. `MaxFileSize` was 1 MiB and is now 64 KiB, on read and on write, so the worst
  file a client will parse costs about 71 ms a load and about three times that for an edit, which decodes
  three times. The cost is in `go-toml`, not in this repository. Depth is not a problem: the library
  refuses deep nesting outright, in milliseconds.
- **Reopens if**: `MaxFileSize` is raised (`TestTheSizeBoundIsTheOneThatWasMeasured` pins it), the config
  gains a reader that parses on every frame or keystroke, or theme files (M45) are read with a larger
  bound.

### Any program running as the user can flip the same-machine config toggle
- **Raised**: M21, while building the toggle
- **Verdict**: accepted risk
- **Why**: `POST /@daemon/config/split` and `/unsplit` are served to every attach client, and the attach
  socket's tier is first-party and OS-permission-protected (rule 16): whatever can connect runs as the
  account that owns `config.toml` and can rewrite it, and the files beside it, without asking anybody.
  The request adds no authority that account lacks. It takes no body, deletes nothing, and sets aside
  everything it replaces (`internal/toggle`).
- **Reopens if**: the daemon answers a request of its own on the bot-automation port (M22), whose tier is
  a secret and not the account; or a local request gains an effect the account could not have by itself,
  a plugin grant (M89) being the obvious one.

### A daemon older than the toggle sends the request's path to its instance
- **Raised**: M21, while building the toggle
- **Verdict**: accepted risk
- **Why**: a request to the daemon itself reuses the relay's frame under `/@daemon/`, so that no frame
  changes. A daemon from before the prefix existed has no reason to treat it differently and relays
  `POST /api/v1/@daemon/config/split` with its token, as it relays any path. What the instance learns is
  that this account's client asked its daemon to split a config. There is no body. The current daemon
  never relays it: the attach server answers first, and `relay.Target` refuses the prefix on its own
  (`TestTheDaemonsOwnPathsAreNeitherRelayedNorInTheContract`).
- **Reopens if**: a local request carries a body, or a path segment holding something of the user's. An
  older daemon would then deliver that to a stranger's server, and the request needs a frame of its own.
- **Reopened at M22, and answered without a new frame**: `POST /@daemon/automation/enable/{port}` carries
  a port number the user chose. `norite automation enable` sends `GET /@daemon/automation` first, which
  says nothing, and sends the number only to a daemon that answered it. A daemon from before the prefix
  relays that first request and is reported as older; a daemon from M21 answers that it knows no such
  path and relays nothing. Neither is sent the number
  (`TestEnableAsksHowThePortStandsBeforeItSendsAPortNumber`). The condition above stands for anything
  more than that.

### Which file wins a shared key at unsplit is decided by modification times anything can set
- **Raised**: M21, while building the toggle; narrowed by `/code-review`
- **Verdict**: accepted risk
- **Why**: unsplit takes the more recently written of the two client files as its base, read from the
  file's own time, which `touch` changes and a restore from backup resets. The time decides one thing:
  which value a `[shared]` key ends with when both files set it differently. `[tui]` and `[gui]` are each
  taken from their own client's file whatever the times say (`config.MergeClients`), every such shared
  key is listed in the answer, and both files and the replaced `config.toml` are kept as
  `*.before-unsplit`, never overwriting an earlier copy.
- **Reopens if**: unsplit starts deleting what it replaces, or `[shared]` gains a key whose value
  choosing wrongly does harm that reading the answer would not catch.

### The daemon watches the directory its config directory is in, and hears what else is written there
- **Raised**: M21, `/security-sweep` of the finished branch
- **Verdict**: accepted risk
- **Why**: a watch is on an inode, so `~/.config` is watched as well as `~/.config/norite`, or a config
  directory that is removed and restored is never noticed again. Every program that creates or renames a
  file directly in `~/.config` therefore wakes `configwatch`, which compares the name to the config
  directory's and drops the event; nothing is read, logged or sent. With `XDG_CONFIG_HOME` pointed under a
  directory other accounts can write to, those accounts can generate such events, and enough of them to
  overflow the queue is treated as "the config may have changed", at most one notification per 100 ms,
  each costing an attached client about 30 µs to re-read a config that did not change.
- **Reopens if**: an event's name is ever logged or forwarded, or a reload becomes expensive enough for ten
  a second to matter (a theme file read on every reload, M45, is the first candidate).

## M22 — local bot-automation port

### Any program running as the user can mint an API token through the attach socket
- **Raised**: M22, at planning, on reopening M20's refusal of `/auth/*`
- **Verdict**: accepted risk
- **Why**: the relay now reaches `/auth/tokens` and `/auth/tokens/{id}`, so a process that can open the
  attach socket can mint a durable, scoped credential as the account, list the account's tokens, and
  revoke them. M20 refused all of `/auth/*` for that reason and because nothing then needed it. Something
  does now: a script cannot use the automation port without a token, and the only other way to mint one
  was a second sign-in made by hand. What the socket's tier already concedes is larger. A process running
  as the user can read the refresh token itself, from the `0600` file or from a keyring that answers any
  of the user's processes, and that is every scope and a session besides. The instance still requires a
  user actor and a live sign-in on all three routes, a minted token is visible in `norite token list`, and
  a password reset revokes every one. The exception is two exact path shapes
  (`TestOnlyTheTokenRoutesAreRelayedUnderAuth`); the rest of `/auth/*`, `/instance/*` and the session
  routes stay refused.
- **Reopens if**: the attach socket's tier ever admits something that is not the account (a plugin given
  a relay, M88, is the obvious one), a token gains a scope that reaches credential or session management,
  or the refresh token moves somewhere the user's other processes cannot read it, since the argument
  above is a comparison with that.

### A script's token for another account on the same instance is forwarded
- **Raised**: M22, at planning
- **Verdict**: accepted risk
- **Why**: the port forwards whatever API token a script presents to the instance the port was enabled
  for. It does not check that the token belongs to the account the daemon is signed in as, and cannot:
  listing tokens is refused to a token by design (ADR 0022), so no route says whose one is. What the
  script gains is nothing it lacked. It holds that token already and could present it to the instance
  directly; the port adds an address and a local rate limit. The instance still decides everything the
  token may do.
- **Reopens if**: the port gives a script anything that comes from the daemon's own sign-in rather than
  from the script's token (events, state, a cache), since that would then reach somebody else's account.

### Between a killed daemon and the next start, its automation file names a port anything may bind
- **Raised**: M22, `/code-review` of the listener
- **Verdict**: accepted risk, narrowed
- **Why**: `automation.json` is removed when the port closes, and a daemon that is killed removes
  nothing. Until the next daemon starts, the file names a port and a secret, and another account on the
  machine can bind that port and be sent the secret and a script's token by a script that reads the
  file. Narrowed by removing whatever is found before binding and when the daemon starts with the port
  disabled (`automation.Clean`), and by never leaving the file when the port cannot be bound. The window
  that remains needs a second account on the machine, acting while the daemon is dead and a script is
  running. Closing it means the daemon proving it knows the secret before a script sends the token, a
  challenge every script in every language would have to implement.

  **Wider than the file, found by the M22 sweep.** A program started by `norite automation run` holds the
  address and the secret in its environment for as long as it runs, so it needs no file and no killed
  daemon: whenever the port is closed under it (the daemon stopped, the port disabled, or enabled again
  on another number) its next connection goes to whatever holds that port, and its first frame carries
  the token. `norite automation request` cannot fall back on the file to notice, because a script run
  under another account through `run` has only the environment. Same second account, same verdict.
- **Reopens if**: the port is offered on a machine shared by accounts that do not trust each other as a
  supported case, or a script-side library is published, since it could carry the challenge once for
  everybody.

### Another account on the machine can keep scripts waiting on the automation port
- **Raised**: M22, `/code-review` of the listener
- **Verdict**: accepted risk, narrowed
- **Why**: a loopback port accepts connections from every account. Connections that have not presented
  the secret are counted apart from scripts (32 against 16), are told nothing, and are dropped after
  five seconds, so they take no script's place. An account that keeps 32 of them open is still refusing
  every new script its turn to present the secret. It learns nothing and reaches nothing; it denies a
  service on a machine it already runs code on.
- **Reopens if**: the count of waiting connections is ever shared with the scripts being served again, or
  a connection that has presented nothing is sent a frame.

