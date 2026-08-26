# ADR 0030: Revoking sessions, and what a session is to a person

## Status
Accepted. Implements the primitive [rule 17](../../CLAUDE.md) requires and
[ADR 0013](0013-public-matchmaking-and-community-features.md) assumes; leaves
[ADR 0022](0022-access-token-signing-and-scope-model.md)'s stateless access token untouched, deliberately.

## Context
Rule 17 says a ban or a self-service account deletion must invoke the general-purpose revoke-all-sessions
primitive rather than a second, potentially-incomplete cleanup path. That primitive did not exist.

What existed was one correct implementation of it, inline in the password-reset transaction, revoking four
things. The way it got there is the whole argument for this milestone:

- **M4** revoked sessions, which is the obvious one.
- **M5** added API tokens, because an intruder who minted one keeps it across a password change.
- **M6** added OAuth exchange codes, because a callback page left open still trades its code for a pair.
- **M9** added approved-but-uncollected device authorizations, one milestone later, for the same reason.

Nobody got it wrong. Each milestone added the claim its own feature created, to the one caller that existed.
The next caller — M72's bans, `DELETE /users/@me` — would have started from whichever of those four its
author happened to remember.

## Decision

### One function, and the unbuilt steps live inside it
`auth.revokeEverything` performs all four revocations and takes a `*db.Queries`, so it composes into a
caller's transaction rather than opening its own — which is what the reset path needs, since the password
change and the revocation must land together or not at all.

`architecture.md` §2 defines the primitive as three things, and only one of them can be written today. The
other two are **written into the function as named gaps**, not omitted and not stubbed behind an interface:

- **force-closing live gateway connections (M18)**;
- **revoking every linked device's E2E device-link trust (M101)**.

Two seams whose shapes are guesses would be two wrong shapes. A list of statements with the missing ones
commented in is one place to add a line, which is the property that matters — the same reason
`SweepExpired` collects its deletes in one function rather than distributing them.

The gateway gap is worth stating in its own right, because it looks smaller than it is. Revoking a session
stops the *next* refresh, and an access token expires within fifteen minutes, so the REST surface is
bounded by construction. A WebSocket is not: it authenticates once at IDENTIFY and stays open as long as
the client keeps it. Until M18, a revoked account keeps receiving events.

### A session, to a person, is a device — not a row
The `sessions` table is a rotation chain. Every refresh inserts a successor and revokes its predecessor, so
a client on a fifteen-minute access token produces four rows an hour.

Listing those rows would show somebody a new "session" every quarter of an hour and hand out identifiers
that were stale before they could be clicked. So `GET /users/@me/sessions` collapses each family to its
newest live record, `DELETE /users/@me/sessions/{id}` revokes the family, and `first_seen` is the family's
start rather than the newest row's `created_at` — which on an active machine is at most fifteen minutes ago
and tells nobody anything.

The schema said this before the endpoint did: `sessions_user_device_idx` is on `(user_id, device_id)` and
its comment already named "list my sessions (M11)" as its second reader.

**The id is a snowflake, not the `device_id`.** A `device_id` is client-chosen text, and putting it in a
path would write it into every request log line — the reasoning M10 applied when it moved invite revocation
off `DELETE /instance/invites/{code}`. A snowflake authorizes nothing.

### The current device is resolved through a possibly-revoked row
This is the milestone's sharpest edge and it has a test that fails without it.

An access token carries the `sid` of the session it was minted from and lives fifteen minutes. Any refresh
inside that window revokes the named row while leaving the token perfectly valid. So "which device is this
request from" must be answerable from a **revoked** record — `GetSessionByID` returns them on purpose,
exactly as `GetSessionByRefreshTokenHash` does.

Read through a query that hid revoked rows, `POST /auth/logout/all` would find no current device, spare
nothing, and sign the caller out of itself: the one thing its name promises it will not do. Confirmed by
making it do that.

### "Log out all other devices" revokes API tokens too
It is the whole primitive minus one device, not a session-only action.

The alternative — sessions only, tokens left alone — is friendlier to the person kicking a lost laptop and
wrong for the person this feature is actually reached for by. That person thinks somebody else has their
account, and a token an intruder minted outlives a signed-out session. One action that cannot under-clean
beats two that have to be run in the right order.

The cost is real: somebody's bots stop. So it is said rather than discovered — the response reports
`api_tokens_revoked` separately, which is the number a client needs in order to explain why. This is the
treatment M5 gave the same cost on the reset page.

**A user actor only.** An API token may not call it, nor list sessions. A delegated credential that can
revoke its owner's sessions — and, being the whole primitive, its owner's other tokens with them — is a
credential that can lock its owner out. Same rule minting already obeys.

### Access tokens stay stateless, and the residual window is accepted
Not reopened here. `architecture.md` §17.10 records it: an already-issued access token is not checked
against session state, so it keeps working until it expires, at most fifteen minutes, and cannot be renewed
because the session behind it is gone.

The alternative is a database lookup on every authenticated request — on the hottest path in the API, to
close a fifteen-minute window on a credential that is already the shortest-lived thing here. M11 states the
window in the contract instead, on both endpoints, so a client can decide what to tell somebody.

### Sessions are swept, by expiry and never by revocation
Nothing had ever deleted from this table. It grows with *traffic* rather than with sign-ins — about
ninety-six rows a day per active device — and `RunSweeper`, which prunes six other tables, did not know it
existed.

The sweep deletes past `expires_at` only. A revoked row is still evidence: `replaced_by_id` is what lets a
presented token be recognized as *replay* rather than as merely unknown, and replay is the signal reuse
detection revokes a whole device family on. Deleting revoked rows while their tokens could still be
presented would make a stolen token unrecognized and quietly disable that detection — the table would look
tidier and a security property would be gone. Past expiry nothing can be presented, so the evidence has
nothing left to prove.

Migration `000012` carries the measurements. Two of them are worth repeating here:

- the `expires_at` index was **partial** on `revoked_at IS NULL` — 000005's mistake recurring, and worse
  here, because the excluded rows are the rotated-away ones and rotation is this table's dominant write;
- `replaced_by_id` was an **unindexed self-referencing FK** declared `ON DELETE SET NULL`, so every deleted
  row ran a trigger that scanned the table. Deleting 2,000 rows out of 50,004 spent **3,757 ms in that
  trigger against 9.4 ms in the DELETE**. With the index, 21 ms.

A behavior test cannot catch either: a partial index produces a scan, not wrong rows, which is precisely
why 000005's mistake survived three migrations. The index *shapes* are asserted directly instead, across
every sweep table rather than this one.

## Consequences

**The dropped-token deferral was closed here, not at M19, and the claim behind it was wrong twice.** M7
recorded that when a `norite login` lands mid-refresh the daemon drops the pair it obtained, and that
handing it back needed M11's single-session revoke reached through M19's gateway connection. `POST
/auth/logout` has revoked exactly one session by presenting its refresh token since M4, and the daemon
holds the HTTP client to call it with at the line that dropped the token. The roadmap repeated the claim
because it was written from the code comment. Both are corrected; the struck-through note is kept in
`CLAUDE.md` because being believed twice is the cautionary part.

**`POST /auth/logout` and `DELETE /users/@me/sessions/{id}` are different operations**, and the difference
is the one this ADR is about. Logout revokes the record a token names. The session endpoint revokes the
device family a record belongs to. On a rotating chain those diverge within fifteen minutes.

**`GET /users/@me/sessions` discloses IP addresses to the account that owns them.** Its own, and the point
of the screen is recognising a machine that is not yours. Nothing else reads that column.

**M72 and account deletion now have one thing to call**, which is the entire purpose. When M18 lands, the
connection close goes inside `revokeEverything` and every caller gets it without being edited.
