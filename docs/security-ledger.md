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
