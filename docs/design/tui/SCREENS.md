# Screens

25 screens. Ids match the badges in `mockups.dc.html` (open it and jump to `#1a`, `#2c`, …).
Every screen shares: 120×40 cells, the 4-column chrome (rail 8 / channels 25 / message flex / members 22,
1 blank cell between), and the 2-row status bar (`7a`). Only deviations are noted per screen.

---

## 1 · Core screens

### 1a — Main layout, idle
**Purpose** the default view: read and post in a guild channel.
**Layout** all four columns, one pane, full chrome.
- **Rail (8 cells)**: `@` direct-messages entry, divider, guild initials (2 letters, active = `accent.fill`
  + `accent`), divider, `◎` discover/matchmaking, spacer, then pinned status glyphs at the bottom
  (`●` daemon health, `♪` voice). Guilds are numbered implicitly for `M-1…M-9`.
- **Channel list (25)**: header `Norite` + `M-1`; sections `▾ TEXT`, `▾ VOICE`, `▾ DIRECT`;
  active channel `accent.fill`; unread channel `text.bright` + count in `danger`; footer row
  `● alex` + presence (`deep work` in `warn`).
- **Message area (flex)**: header `# general` bold + `pane 1 · 214 msgs cached` dim; body bottom-aligned;
  day divider `── Tuesday ──`; author-grouped messages; a webhook message with an `AUTO` badge
  (`bg #4fb570`, text `bg`); inline code as `bg #161b1d` + `warn` text; `lune is typing…` in `text.dim`;
  composer with `accent` border and block cursor.
- **Members (22)**: `MEMBERS — 6` label, role groups (`MAINTAINER — 2`, `ONLINE — 3`, `OFFLINE — 1`),
  presence glyph + name (role color for maintainers), footer noting the viewer's presence mode.

### 1b — DM
**Deviations** channel column becomes `Direct`: a `/ find people…` filter row, then `▾ PINNED`,
`▾ RECENT`, `▾ REQUESTS` (new request marked `new` in `warn`).
- Header: `lune` bold + `◈ e2e · verified` in `ok` + `2 devices` dim.
- One dim notice row above history: encrypted end-to-end; the instance stores ciphertext and cannot read it;
  this device holds the decrypted copy it searches. A newly linked device sees only messages sent after
  linking, because it has no keys for the rest (ADR 0014).
- Attachment row: `▤ keystore-bench.txt  4.1 KB · p2p`.
- Right column becomes **PEER**: name, presence, 6-group fingerprint in `ok`, device list with
  verification, shared guilds, friends-since; footer `C-c f verify keys` / `C-c v call`.
- Composer prompt is `◈` in `ok` instead of `›`.

### 1c — Group DM
**Deviations** channel column gains `▾ GROUPS` (`◇ release-crew`). Header shows `◇ group · 4 people` and,
dim, `encrypted in transit and at rest by the instance`.

**No `◈` on this screen.** E2E is `DM`-only (rule 13, ADR 0014): a group has no pairwise session, and
promising one here would be false. The screen says what is true instead — the instance holds these
messages and can read them, exactly like a guild channel.
- Right column **PEOPLE — 4**: presence + name, a legend row, and each person's shared-DM link
  (`RET opens a 1:1 dm`, which *is* E2E-capable).

### 1d — Settings › keybindings
**Deviations** rail keeps a `⚙` slot at the bottom (active). Channel column becomes settings nav,
grouped `▾ YOU` / `▾ CLIENT` / `▾ MACHINE`; `panes & layout` shows a `switcher` hint;
`daemon` shows `ok`.
- Message area: 3-column table (chord 14 cells / action flex / scope 9 cells), active row `accent.fill`,
  chords in `warn`; destructive `C-x C-c` in `danger`. Below it a **REBIND** box (`accent` border):
  "press a chord…" + cursor, `RET save · ESC cancel`.
- Right column **OVERRIDES — 4**: the chords you have rebound, each with the default it replaced, then a
  source tally (defaults 31 / your overrides 4). Plugins never appear here: they register `M-x` commands,
  not chords (ADR 0015, ADR 0026), so a plugin cannot take a binding from you and a conflict between a
  plugin and a default cannot exist. Binding a chord *to* a plugin command is an override like any other.
  Footer `C-c z revert section` — the global revert, applied to what is focused.

### 1e — Your own profile
**Deviations** settings nav with `profile` active (`edited`); right column is 27 cells (210px).
- Identity block: 8×3-cell avatar box (`accent` border, 2 initials), then `avatar` / `accent` /
  `handle` rows. Accent picker = five 1-cell swatches, selected one outlined.
- Field rows, label column 10 cells: display name (focused, `accent` border + cursor), pronouns, bio
  (2 rows + `108/240` counter), status (`✦` + text), links, timezone.
- **VISIBILITY** group: bio & links / local time / last seen, each with the chosen scope and the
  alternatives in dim. `last seen: nobody` in `danger` + "the daemon never publishes it".
- Right column **PREVIEW — AS OTHERS SEE YOU**: the exact card from `1f`, live; then
  `HIDDEN FROM THIS CARD: last seen · devices · email`. Footer: `C-c C-p preview as a stranger` and
  "synced to the instance".
- Status bar row 1 carries `2 unsaved fields` in `warn`; nav footer shows `unsaved`.

### 1f — Someone else's profile
**Deviations** the card is a **right-anchored overlay over the message area** (38 cells), not a column;
the chat keeps full width behind it, drawn in the dim palette. Member list stays live, with the
subject's row highlighted.
- Card: 7×3-cell avatar (author color), display name, `kadir#7b02 · they/them`, presence + local time,
  `◈ verified · 2 devices`; bio (2 rows); deep-work status row.
- Groups: **IN NORITE** (roles, joined guild, friends since) · **SHARED WITH YOU** (guilds, group DMs, and the
  DM's gateway round-trip `21ms` — messaging is always client↔instance; see ADR 0016 for the one
  peer-to-peer path, file transfer) · **KEY** (fingerprint + "verified by you on jan 4").
- Action grid, 2×2: `RET message` (accent) / `C-c v call` / `C-c w whisper` / `C-c n note`; below,
  `C-x 3 open dm in a split` and `C-c B block` in `danger`.
- Member list footer: `RET on a name opens their card`.

---

## 2 · Panes & layout

### 2a — Two panes, full chrome
**Purpose** the flagship split: each pane is a complete client.
**Layout** window tab bar (1 row: `1: work*`, `2: logs`, `·`, pane-type picker, `2 panes · full chrome each`),
then two stacked panes, then the status bar.
- Each pane: title bar (`pane 1 · Norite` + `◆ focused` / `C-x o to focus`), then **its own**
  4-cell rail (different guild active per pane), 21-cell channel list, message area with its own
  composer, 18-cell member list. Internal dividers are `border.inner`, not full borders.
- Unfocused pane: neutral border, dim palette, empty composer.
- Status row 2 ends with the rule: `a 3rd pane switches to shared chrome`.

### 2b — Two panes, shared chrome
**Purpose** the same content, one chrome — user's preference, `C-x C-b`.
**Layout** full rail (8) + channel list (25) drawn once; two content panes side by side; one member list (22).
- Channel list marks which pane shows each channel (`# pane-engine  1`, `# daemon  2`); footer explains it.
- Member list header is `MEMBERS — FOCUSED` and follows the focused pane; footer `follows the focused pane ·
C-x o`.
- Status row 2 ends `C-x C-b full chrome per pane`.

### 2c — Three or more panes
**Purpose** a workspace: chat plus non-chat viewports.
**Layout** tab bar with the pane-type picker; **collapsed 4-cell rail** (guild initials, `#` + unread
count, `◈`, vertical `C-x b` hint, health dot); then the pane grid; no channel or member column.
- Mocked as chat (left, 1.1fr) + log pane and shell pane stacked (right, 1fr).
- **log pane**: `HH:MM:SS` dim + level (`INFO` ok / `DEBUG` accent / `WARN` warn / `ERROR` danger) +
  message; footer `filter: level>=debug · / search`. 18px rows — denser than chat, deliberately.
- **shell pane**: `$` in `ok`, command, output in `text.muted`, block cursor; footer `pty · not a chat pane`.
- Status row 2: `M-x pane new  logs · shell · chat`.

---

## 3 · Finding & running things

### 3a — Fuzzy switcher, centered (DEFAULT)
**Layout** centered overlay 85 cells wide, 10 rows tall, over the dimmed app.
- Header: `C-c k` accent + query `pane` + cursor, right `6 of 41`.
- Results (left, flex): matched characters highlighted (`accent` on the selected row, `warn` elsewhere),
  source guild in dim, match score right-aligned; mixes channels, DMs (`◈`), voice (`♪`).
- **Preview column (27 cells)**: selected target's name, `4 online · 18 unread`, last two messages.
- Footer chords: `RET open here · C-x 3 open in split · C-n/C-p move · M-g guilds only · ESC cancel`.

### 3b — Fuzzy switcher, minibuffer (ALT)
Selected via `switcher = "minibuffer"` in settings › panes & layout. Bottom-anchored, replaces the
status bar; the app does not move and the highlighted result **previews live in the message pane**
(header shows `previewing · not yet committed`, composer disabled). `RET` commits, `C-x 3` opens in a split.

### 3c — Search
**Deviations** channel column becomes **Filters**: `▾ SCOPE` (this guild / all guilds / dms (local)),
`▾ FROM` (chip with `×`), `▾ IN`, `▾ HAS`, `▾ WHEN`; footer `trgm fuzzy · en dictionary`.
- Header is the query row: `/ from:kadir conpty` + cursor, right `14 hits · 41ms`.
- Results in **two labelled groups**: `SERVER — 12 hits in 4 channels` (2-cell left rule, `accent` on
  the selected hit) and `LOCAL — 2 hits in encrypted dms` + `◈ searched on this machine only`
  (`ok` left rule). A dim notice explains the instance holds only ciphertext and cannot match against it,
  so this group comes from the daemon's own FTS5 index over its decrypted E2E store (ADR 0014) — a
  different machine from the one that produced the group above it, which is why the two are labelled
  rather than merged.
- Right column **BY CHANNEL** tally; footer `C-c s save as filter` / `--json pipe results`.

### 3d — Help overlay
`C-h`. Centered overlay 105 cells wide over a heavily dimmed app (only column outlines visible).
Two columns of chord groups: PANES / NAVIGATION on the left, MESSAGES / VOICE / CLIENT on the right;
chord column 11 cells in `warn`, action in `text`. Footer: override count + `C-c , edit bindings`. `C-h`
again opens the manual; `/` filters.

### 3e — M-x command mode
**Layout** two panes (chat dimmed above, command output focused below) + a bottom command palette.
- Palette: `M-x` accent + partial input + cursor, `4 of 62 verbs`; verb rows with a dim description,
  right-aligned flags (`--json`) and `confirms` in `danger` for destructive ones; footer
  `RET run in this pane · C-x 2 run in a split · TAB complete flags · ESC cancel`.
- Output pane: `M-x output` + `◆ exit 0 · 41ms`; the shell line in dim, then syntax-colored JSON
  (keys `presence`, strings `ok`, numbers `warn`, punctuation dim); footer
  `piped to pane · C-c y yank · C-c > write to file`.

---

## 4 · Voice

### 4a — Full voice view
**Purpose** clicking a live voice channel: the call occupies the message column, member list intact.
**Deviations** the voice channel row in the sidebar expands to list participants inline with level glyphs.
- Header `♪ standup` accent + `4 connected · 03:41`.
- **2×2 speaker tiles**: name + role/state tag, and an 8-segment level meter (`▍` cells, 18px tall max).
  Speaking tile gets the `ok` tint + border; muted shows `mic off` in `danger`; deafened `warn`.
- Three metric cards below: **TRANSPORT** (codec, rtt, jitter, loss, route), **PROCESSING** (denoise, aec,
  worker, device), **LEVELS** (in/out meters + ptt hint). One row per metric, values kept short.
- **CALL LOG** rows at the bottom; then an action row of bordered chips
  (`C-c m mic off` in `danger`, `C-c d deafen`, `C-c v leave`) + `C-x 3 open #general beside this`.
- Member list header `IN VOICE — 4` groups callers first with `♪` glyphs.

### 4b — In-call strip
**Purpose** the default while chatting during a call — nothing is hidden.
**Deviations** exactly one extra row: a 1-row `accent`-bordered strip directly above the composer —
`♪ standup · 03:41 · ▍kadir lune mira · 28ms · mic off · C-c m · C-c v`. Chat, members, sidebar all
intact. `C-c V` promotes to `4a`. A system line records `♪ lune joined standup · 09:39` in history.

---

## 5 · Lifecycle & states

### 5a — First run (headless / SSH)
**Purpose** first run on a machine with no usable browser — the device-code path (M9). With a browser and
a free loopback port the TUI uses the browser login instead (M8) and this screen shows that flow's progress
rows without the code box.
**Layout** no chrome; a centered 72-cell column.
- `NORITE` letterspaced + `first run`.
- Checklist rows: `✓` done (config, keystore, daemon installed) with the path/detail in dim,
  `◇` current (waiting for authorization), `·` pending (gateway connect) in `text.dimmest`.
- **Device-code box** (`accent` border): URL, then the code at double size, letterspaced, in `accent`,
  `expires in 4:52`, and a thin progress bar + `polling`.
- Escape hatches: `norite login --token`, `norite instance init`.
- Status bar row 1 shows `◐ daemon starting` in `warn`; row 2 `RET open browser · C-c y paste token · C-c c
cancel`.

### 5b — Empty / first join
**Deviations** rail and both side columns get **dashed** borders and placeholder copy (`+`, `no guilds`);
the channel column explains what will appear where and shows the signed-in identity + `◈ keys generated ·
backed up`.
- Message area: `NO GUILDS YET` + one line of orientation; **REDEEM AN INVITE** box (`accent`): the invite
  URL with the code portion accented, then a resolved-preview row (`✓ Norite · 41 members · invited by kadir
  · expires in 6d`, `RET join`).
- Two cards side by side: `start a guild` (`M-x guild create`) and `add a friend` (`C-c a`, "DMs need no
guild").
- Bottom: three orientation chords. Right column **SETUP** checklist + the keystore-backup warning
  (`~/.local/share/norite/keystore.db` — cannot be recovered), `M-x key export`.

### 5c — Disconnected
**Deviations** rail border turns `danger`-tinted, guild initials go dim, health dot is `○` in `danger`.
Channel list header shows `stale`; voice channels read `unavailable`; footer `○ alex · offline`.
- Header: `cached · last synced 09:41`. Below it a 1-row `danger`-tinted banner:
  `○ gateway disconnected · retry in 8s (attempt 4) · C-c C-r retry`.
- History above the cut is dimmed; a divider reads `connection lost 09:41` in `danger`; messages sent
  since carry `◷ queued` in `warn`; a held file shows `held · p2p peer unreachable`.
- Composer: **dashed** `warn` border, `◷ will send when reconnected`, right `3 queued`.
- Right column: `MEMBERS — CACHED`, all `○`, "presence unavailable while disconnected", footer
  `◈ keystore ok · reading local cache`.

### 5d — Deep work
**Deviations** rail border and active guild use the `presence` palette; sidebar shows per-channel
`muted` / `✦ allowed` / `held`; footer `✦ alex · until 12:00`.
- Header right: `✦ deep work · 2h 11m left`.
- **Rule strip** (presence tint): `allow` lines (mentions of you, a regex, a specific DM sender) and a
  `hold` line (`41 msgs · 2 dms · voice invites`).
- A divider marks when the session started; the one message that got through has a `presence` left rule
  and states the rule that let it in (`✦ broke through · rule: dm from kadir`).
- A dim row offers `C-c h` to review the 41 held messages.
- Right column **SESSION**: countdown + progress bar, tallies (held, broke through, presence shown as,
  auto-reply), and the auto-reply text others see.

### 5e — Whisper composer
**Deviations** the composer expands into a `presence`-bordered block (5 rows) inside the normal channel:
- Row 1: `whisper` + `in #general to` + recipient chips (`presence` fill, `×` to remove) + partial name +
cursor.
- Rows 2–4: candidate list — selected row `presence` fill with matched chars underlined and presence per
  person, and a disabled row for someone **not in this channel**
  (`can't whisper here`).
- Row 5: the message body with the ordinary `›` prompt; then a chord row (`RET send`, `C-c a → group dm`,
  `ESC cancel`).
- History shows a received whisper: `presence` left rule, a `whisper → you` tag, and a dim line
  "only the people named above see this in #general".
- Right column marks `SELECTED — 2` with `✓`, then `IN CHANNEL`; footer states that a whisper is delivered
  to a private recipient set but is **stored by the instance like any other message** — it is not E2E
  (rule 13, ADR 0014), deliberately, so abuse reports about whispers remain actionable (ADR 0013).

---

## 6 · Trust & administration

### 6a — Device verification
**Deviations** message area (`accent` border) is the verification flow; DM sidebar marks the affected
conversations with `▲`.
- Header `verify mira's new device` + `"desktop" · added 2h ago`; a dim instruction line about
  comparing out of band.
- **Two fingerprint cards side by side**: yours (neutral border) and theirs (`ok` tint + `matches`),
  each 3 rows of 3 groups, letterspaced, in `ok` at ~1.2× cell emphasis.
- Two info cards: **MIRA'S DEVICES** (per-device verified date, the new one `▲ verifying now`) and
  **AFFECTS** (which DMs/groups and how many messages).
- Decision row (`ok` border): `RET they match` (ok) / `C-c x they differ` (danger) / `ESC later`;
  below, a dim warning about what to do if they differ and what blocking means.
- Right column **TRUST**: tallies, your own key, and a note that rotating forces everyone to re-verify.

### 6b — Plugin manager
**Deviations** settings nav with `plugins` active (`4`); nav footer `wasm · wasi p2 sandbox`.
- Four plugin cards, one per state:
  - **enabled + focused** (`accent` border): name, version, size, `enabled` in `ok`; `caps` line
    (granted cap in `ok`, denied list in dim); `sha256 … pinned` + `adds 2 M-x commands` in `text.dim`.
  - **enabled** (neutral border).
  - **needs approval** (warn tint): `wants` line listing requested caps in `danger`; action row
    `RET grant · C-c n deny net, load anyway · DEL remove`.
  - **blocked** (danger tint): `sha256 mismatch`, expected hash, "the file changed since you pinned it — not
  loaded".
- Footer: `M-x plugin add` / `C-c i inspect manifest`.
- Right column **CAPABILITIES**: per-cap counts, `keystore: never`, a wrapped note that no plugin can
  read plaintext DMs or keys, then the enforced quotas — a wall-clock timeout and a memory cap **per
  invocation** (rule 12). Plugins
  do not run in the render loop, so there is no per-frame budget.

### 6c — Instance admin, reports
**Deviations** channel column becomes **Admin** nav (`▾ MODERATION` with a report count in `danger`,
`▾ GUILD`, `▾ INSTANCE`); footer `role: maintainer`.
- Header `reports — 7 open` + `oldest 2d · only reported content is visible`.
- Table: 5-column header (ID 6 / SUBJECT 12 / REASON flex / REPORTS 9 / AGE 7 cells).
  The selected report expands (`accent` border + tint) into: the row, a quoted excerpt with the URL
  defanged (`norite-pro[.]xyz`) and `posted 6× in 4 channels`, then **two rows** of actions
  (`RET open thread · C-c b ban · C-c C-t timeout` / `C-c C-d delete posts · DEL dismiss`). The two
  destructive verbs sit on `C-c C-…` because `C-c t` is tag and `C-c d` is deafen, and a global chord is
  never reinterpreted by a screen (KEYMAP.md, "Scope and precedence").
- One collapsed row is annotated `◈ e2e — you can only see what the reporter chose to attach`.
- Footer: `C-n/C-p move · M-x report export` + `actions are signed & logged`.
- Right column **SUBJECT**: joined, invited by, message count, priors, devices (`▲ 1 unverified`), then
  **RECENT ACTIONS** (audit trail). Footer `C-c b ban subject` (danger) / `C-c C-a audit log`.

---

## 7 · Shared components

### 7a — Status bar
The reference strip, shown in isolation above a stub message pane. Two rows, always the bottom two rows
of the terminal, present on **every** screen above with screen-specific content. Full recipe in
`TOKENS.md → Status bar`. Per-screen row-2 chord sets are listed in `KEYMAP.md`.
