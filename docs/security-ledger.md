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
- **Raised**: M14, `/code-review high`
- **Verdict**: accepted risk — the cost of a fix, not a defect
- **Why**: requiring `PermViewChannel` alongside every management permission closed a real bug — a
  moderator administering a channel their own listing hid from them. The price is that denying `@everyone`
  the view bit on a channel now also removes the author's route back: the standing check passes
  (`@everyone` is position 0) and `refuseEscalation` passes (they hold the bit as they write it), and from
  the moment it commits they cannot see the channel or reach any route that would undo it. Before M14 they
  could have reversed their own change, because managing did not depend on seeing — which is exactly the
  property that was wrong. Discord behaves the same way, and its recovery path is the same as ours: the
  owner, an administrator, or an Instance Admin. Pinned by
  `TestDenyingYourselfViewIsAOneWayDoor` so it reads as a decision.
- **Reopens if**: guilds acquire a way to have no owner and no administrator reachable — M13a's ownership
  transfer is what keeps that from happening — or if a self-service "undo my last overwrite" surface is
  ever proposed, which would need a different answer.
