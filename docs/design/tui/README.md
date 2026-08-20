# Handoff: Norite TUI client

## Overview
Norite is a voice-and-text chat platform, used mainly through a free publicly-hosted flagship instance,
with self-hosting as a real licensed feature. Clients attach to a pure-Go background daemon, one per OS
user account, which holds the WebSocket gateway connection to the instance, presence and Deep Work state,
in-memory scrollback, and the E2E keystore. This package specifies the **TUI**: a Discord-shaped layout
(guild rail → channel list → message area → member list) with tmux-like pane splitting, driven entirely by
Emacs-style chorded keybindings.

The TUI is a first-class client, not a mode of the command-line tool — it is the in-terminal form of the
same application the native GUI presents natively, and the GUI (Phase J) mirrors this information
architecture. The scriptable `norite` command tree is the *other* front end onto the same command tree; see
`M-x` below and ADR 0026.

Source of truth for architecture: `docs/architecture.md` §4 (CLI) and §4a (TUI),
`docs/adr/0009-cli-and-gui-client-architecture.md`, `docs/adr/0010-client-daemon.md`,
`docs/adr/0026-tui-as-a-first-class-client.md`.

**This file has been corrected against the architecture it was drawn from — see "Corrections applied"
at the end. Where this package and `CLAUDE.md`'s non-negotiable rules disagree, the rules win.**

## About the design files
`mockups.dc.html` is a **design reference written in HTML** — a pixel simulation of a
terminal, not production code and not a web app to ship. Every screen is an HTML approximation of
what the TUI must render in a real terminal.

**The implementation target is a Go TUI**, per ADR 0009: Bubble Tea (`tea.Model`) for the event loop,
Lip Gloss for styling/layout, Bubbles for reusable widgets, talking to the daemon over its local
socket. Translate the HTML as follows:

| HTML in the mock | Real implementation |
| --- | --- |
| `border:1px solid #262c2f` + `border-radius:6px` | `lipgloss.RoundedBorder()` with `BorderForeground` |
| `display:flex` row / column | `lipgloss.JoinHorizontal` / `JoinVertical` |
| `opacity:.45` on an unfocused pane | dim variant of each style (a "dim" palette pair per color) |
| `px` widths (62, 196, 172) | **character columns** — see the Grid section |
| `13px/20px` type | one terminal cell; there is no font sizing in a TUI |
| `font-size:11px` meta text | same cell size, rendered in a dim color instead |
| box shadows on overlays | nothing — overlays are composited by drawing over the cell buffer |

## Fidelity
**High-fidelity.** Colors, glyphs, copy, information hierarchy, and keybindings are final and should be
matched exactly. Pixel geometry is *not* final in the literal sense — it encodes a **character grid**
(below). Match the proportions and the column counts, not the pixel numbers.

## Grid — how to read the pixel numbers
The mocks are drawn at **120×40 cells** inside a 936×800 px card (8px outer / 6px inner padding, so a
920px content box). Type is 13px JetBrains Mono on a 20px line box.

- 1 row = **20px**. Every row of text, every header, every status line is a multiple of 20px.
- 1 column ≈ **7.8px** (920px ÷ 118 usable cells).
- Convert any width: `cols = round(px / 7.8)`.

| Region | Mock px | Cells | Rule |
| --- | --- | --- | --- |
| guild rail (expanded) | 62 | 8 | fixed |
| guild rail (collapsed) | 34 | 4 | fixed, `C-x b` toggles |
| channel list | 196 | 25 | fixed |
| member list | 172 | 22 | fixed |
| settings nav | 196 | 25 | same slot as channel list |
| profile card overlay | 296 | 38 | overlay, right-aligned over the message area |
| message area | remainder | flex | min 40 cells before panes refuse to split |
| gap between columns | 6 | 1 | one blank cell |
| status bar | 2 × 20 | 2 rows | always the bottom 2 rows |

**Below 120 columns, drop in this order.** Decided here rather than by whoever writes the layout code,
because 80 columns is the most common terminal width and the order is not obvious:

| Width | What changes |
| --- | --- |
| < 120 | member list drops first (it is the most redundant — the same presence is in the message headers) |
| < 96 | channel list drops; navigation falls back to `C-c k` (switcher) and `M-1…M-9` |
| < 90 | `C-x 3` (side by side) refuses — two panes cannot both hold the 40-cell minimum below this |
| < 80 | guild rail collapses to 4 cells |
| < 24 rows | `C-x 2` (stacked) refuses — splitting rows, not columns, is what this one costs |
| < 40 × 12 | render a single line explaining the minimum, and nothing else |

The two split thresholds differ because the two splits cost different things: `C-x 3` divides the columns,
so it needs twice the message area's 40-cell minimum plus the chrome around it; `C-x 2` divides the rows
and leaves width alone. A single "panes refuse below N columns" rule would be wrong for one of them.

The 90 is arithmetic, not a round number: below 96 the channel list is already gone and the rail is still
expanded, so a side-by-side split costs `8 + 1 + 40 + 1 + 40`. Collapsing the rail first would buy four
cells, but the rail collapses at 80 and the split is refused above that, so the case never arises.

A dropped column is *dropped*, never squeezed: a 12-cell member list is unreadable and costs the message
area the width that makes it readable. What is dropped stays reachable by chord, and the status bar keeps
its two rows at every width — truncating segments (counts first, then the clock) rather than wrapping.

## Screens
25 screens, grouped into 7 sections. Each carries a stable id used throughout this package
(`1a`, `2c`, …) and shown as a badge in the mock. Full per-screen specs: **SCREENS.md**.

| Section | Ids |
| --- | --- |
| 1 · Core screens | 1a main · 1b DM · 1c group DM · 1d settings · 1e own profile · 1f other's profile |
| 2 · Panes & layout | 2a two panes, full chrome · 2b two panes, shared chrome · 2c three+ panes |
| 3 · Finding & running | 3a switcher (default) · 3b switcher (minibuffer) · 3c search · 3d help overlay · 3e M-x |
| 4 · Voice | 4a full voice view · 4b in-call strip |
| 5 · Lifecycle & states | 5a first run · 5b empty · 5c disconnected · 5d deep work · 5e whisper |
| 6 · Trust & admin | 6a device verify · 6b plugins · 6c admin reports |
| 7 · Shared components | 7a status bar |

## The three load-bearing decisions
Everything else follows from these. Get them right first.

**1. Chrome mode is a function of pane count.**
- **1 or 2 panes** → each pane MAY be a *complete client*: its own rail, channel list, message area,
  member list, composer, showing a different guild per pane (`2a`). Two panes may also share one
  chrome (`2b`) — user's choice, toggled with `C-x C-b`.
- **3+ panes** → chrome is drawn **once** for the window (one collapsed rail, one channel list, one
  member list that follows focus); panes hold content only (`2c`). There is no room for per-pane
  chrome and forcing it produces unreadable columns.
- `C-x 1` (back to one pane) always restores full chrome.

**2. A pane is any viewport, not just a conversation.** Pane types: `chat`, `log`, `shell` (pty),
`peers`, `scratch`. `peers` is **file-transfer sessions** — the opt-in, consent-gated P2P transfers of
ADR 0016, which are the only peer-to-peer thing in the architecture — not message routing, which is always
client↔instance. The type picker lives in the window tab bar; `C-x c` cycles it, `M-x pane new <type>`
creates one. Pane scroll offsets and layout tree live in the **daemon**, not the view — detaching and
reattaching restores them, and they are in-memory, so a daemon restart loses them (ADR 0010).

**3. Security state is always on screen, and never overclaims.** `◈` = verified E2E,
`▲` = unverified device, `○` = offline/disconnected.

E2E is **`DM`-only** — never group DMs, never guild channels, never whispers, never voice (rule 13,
ADR 0014). So the **per-conversation E2E badge** — `◈` in a channel header, a composer prompt, or beside a
person — appears on a 1:1 DM and nowhere else. The same glyph is also the *keystore/E2E status* indicator,
which is a different statement and appears anywhere that status is shown: the status bar's health block on
every screen, `5b`'s setup checklist, `5c`'s "keystore ok", `2c`'s collapsed rail, `3c`'s local-search
group, and `6c`'s note that E2E limits what a report can show. Read it as "this conversation is E2E" only
where it sits on a conversation. A group DM is encrypted in transit and at rest by
the instance like any other channel, and says so; a whisper is server-stored plaintext, deliberately, so
that the Instance Admin break-glass path always has something to act on for whisper-vector abuse reports
(ADR 0013). Drawing `◈` on either would promise a guarantee the system does not make.

The server holds DM **ciphertext** and cannot match against it, so `3c` shows server hits and DM hits as
**separate, labelled groups** — they come from different machines with different guarantees. The DM group
is served by the daemon's own mandatory local FTS5 index over its decrypted E2E store, encrypted at rest
under the keystore master key (ADR 0014). `6c` (admin reports) states that only reporter-attached content
is visible. Never draw a UI that implies the server can read plaintext.

## Screens are drawn finished; milestones are not

Every screen here is drawn in its completed state. That is the right way to specify a design and the wrong
way to read a schedule: the roadmap assigns each screen to the milestone that can *first* draw it, and a
screen almost always arrives before every guarantee it depicts.

The large instance is E2E, which is Phase M (`M97`–`M104`), near the end — while the screens carrying
keystore or verification state land twenty to fifty milestones earlier: `2c`'s collapsed rail, `5a`'s setup
checklist, `5c`'s `◈ keystore ok` footer, `5b`'s "keys generated · backed up", `1b`'s verified-device header
and peer fingerprint, `1f`'s KEY group, `3c`'s LOCAL result group, and `6c`'s note about what a report can
show. **Until `M99` every one of those is absent, not faked** — no `◈`, no fingerprint, no local-search
group, no checklist row for a keystore that does not exist. Drawing an indicator that reports a guarantee
the build cannot make is precisely the failure decision 3 above exists to prevent; a missing indicator is
the better failure, because it is visibly missing.

The same rule covers the smaller cases: `1a`'s `◎` discover entry until `M66`, its `AUTO` webhook badge
until `M60`, `1d`'s plugin-command tally until `M89`, and the `peers` pane's contents until `M94` — the
pane type can be cycled to from `M46` and shows an empty state until then.

## Interactions & behavior
- **Keybindings**: Emacs-style chords, two prefixes — `C-x` for panes/windows, `C-c` for app
  actions; `M-x` for command mode; `M-1…M-9` jump to guild N. Full table: **KEYMAP.md**.
- **Prefix feedback**: when a prefix is armed, the status bar shows it (`C-x` + "prefix armed") and the
  next keystroke is captured. Unknown chord → status-bar error, no modal.
- **Focus**: exactly one pane is focused. Focused = accented border (`#2fb6c4`) + accented pane title
  bar (`#0f1c1f` bg) + `◆` marker. Unfocused = neutral border, dimmed content (mocked as
  `opacity:.45–.55`; implement as a dim palette, not real alpha).
- **Overlays**: `3a` (switcher) and `3d` (help) are centered overlays over a dimmed app; `3b`
  (minibuffer) and `3e` (M-x) are bottom-anchored and shift nothing; `1f` (profile card) is a
  right-anchored overlay over the message area. All close on `ESC`.
- **Live regions** that must redraw without a full repaint: voice level bars (`4a`), log follow
  (`2c`), typing indicators, queued-message state (`5c`), deep-work countdown (`5d`).
- **No animation** beyond a block cursor and level meters. This is a terminal; motion is noise.

## State
Client-side state the TUI owns:
- `layout`: tree of panes per window + focused pane id + zoom flag (mirrored to the daemon).
- `windows[]`: name, pane tree, active pane. Tab bar renders this; `*` marks the active window.
- `focus.route`: guild / channel / dm / voice id — drives header, member list, status bar line 1,
  and the contextual chords on status bar line 2.
- `overlay`: `none | switcher | minibuffer | help | mx | profile` — mutually exclusive.
- `connection`: `connected | reconnecting(attempt, next_retry_s) | offline` + `gateway_rtt_ms`,
  `resume_seq`. Drives `5c` and the status bar health block.
- `outbox[]`: queued messages with `◷` markers while offline; flushed on reconnect, in order. Held in the
  daemon's memory like everything else here — a daemon restart before reconnection loses them, and `5c`
  must not imply otherwise.
- `deep_work`: `{active, until, allow_rules[], held_count, broke_through[]}`.
- `voice`: `{channel, joined_at, participants[{name, speaking, muted, deafened}], rtt, loss, mic, deaf}`.
- `trust`: per-peer `{fingerprint, devices[{name, verified_at}], route}`.
Everything else (messages, members, presence, profiles) is read from the daemon and cached **in memory**;
the TUI never treats its cache as authoritative except when `connection != connected` (then it labels the
view "cached", as in `5c`). For guild channels and group DMs "cached" means the recently-loaded window the
daemon is holding, and scrolling past it re-fetches from the instance (ADR 0010). **E2E DMs are the
exception**: the instance stores their ciphertext, and the daemon additionally keeps a decrypted local
store with an FTS5 index, because it is the only place they can be searched at all (ADR 0014). A *newly
linked* device still sees only messages sent after linking — it has no keys for the rest.

## Design tokens
Full list with usage notes: **TOKENS.md**. Summary — a 16-color-classic palette on near-black:

| Role | Hex |
| --- | --- |
| background | `#0b0d0e` |
| panel border | `#262c2f` · inner divider `#1c2224` |
| text | `#c8d0d2` · bright `#eef2f3` · muted `#7d8a8e` · dim `#5b6669` · dimmest `#4a5457` |
| accent / focus (cyan) | `#2fb6c4` · fill `#12262a` · panel header `#0f1c1f` |
| ok (green) | `#4fb570` |
| warn / chord hint (yellow) | `#d0a437` |
| danger (red) | `#cf5c5c` |
| presence / deep work (purple) | `#a97fd0` |

Type: JetBrains Mono (any mono the user's terminal provides — the design assumes nothing but a
monospace cell and box-drawing glyphs). Borders: rounded Unicode. Glyphs: `◈ ▲ ○ ● ◐ ✦ ♪ ◇ ◷ ▍ ▤ ⚑ ⚙ ◎ @
◆ › ▾` — no emoji anywhere, and every one of them must measure a single cell on the terminal actually in
use, which most of them do not do unconditionally (`TOKENS.md → Glyphs`).

## Assets
**The client ships no assets.** No images, no icon fonts, no logos; avatars are 2-letter initials in an
accent-colored box, and the real client uses whatever monospace font the terminal provides.

**The mock is not dependency-free**, which matters if you try to open it offline. `support.js` fetches
React, ReactDOM and Babel from `unpkg.com` at load, and the page pulls JetBrains Mono from Google Fonts —
so `mockups.dc.html` needs network access to render, and cannot be opened in an air-gapped environment.
It is a generated bundle from the design tool, committed for reference only: nothing in the product builds
against it, and it must not become a dependency of anything. If offline access matters, read `SCREENS.md`,
which is the normative document anyway.

## Files in this bundle
- `README.md` — this file
- `SCREENS.md` — per-screen specification, all 25 screens
- `TOKENS.md` — palette, grid, glyph set, component recipes
- `KEYMAP.md` — every chord, with scope and target screen
- `mockups.dc.html` — the visual reference (open in a browser; ids are anchors, e.g. `#2c`)
- `support.js` — runtime needed to render the HTML reference

## Suggested implementation order
1. **Shell + grid**: one window, one pane, full chrome, static data. Get the 20px-row discipline and
   the rounded-border columns right before anything else.
2. **7a status bar** — two rows, contextual line 2. Every later screen feeds it.
3. **1a → 1b → 1c**: message rendering (author grouping, day dividers, system lines, whisper styling).
4. **Chord dispatcher + 3d help overlay** — prove the prefix model before adding more surfaces.
5. **Pane engine**: `2a` (full chrome), then `2b`/`2c` (shared chrome + the count rule), then
   non-chat pane types. This is the riskiest piece; ADR 0009 keeps it in-house for a reason.
6. **3a/3b switcher, 3c search, 3e M-x**.
7. **5a/5b/5c/5d/5e** — the state screens. `5c` (disconnected) is the one users will see most.
8. **4a/4b voice**, **6a trust**, **1e/1f profiles**, **6b plugins**, **6c admin**.

---

## Corrections applied to this handoff

The package was drawn from `docs/architecture.md` and ADRs 0009/0010 as they stood on 2026-08-17, and it
reached in a few places past what the architecture actually promises. It is normative now, so those places
are corrected rather than left for a reader to trip over. Each entry says what changed and what forced it.

The visual mock (`mockups.dc.html`) has been hand-corrected to match, as a text-only edit — no tag was
added, removed or renamed; the exact counts are under "What changed in the mock" below. It remains
**illustrative** — where it and this markdown disagree, the markdown wins. It also needs the network to
render (see Assets), so the markdown is the only offline-readable source.

| # | Was | Now | Forced by |
| --- | --- | --- | --- |
| 1 | `1c` group DM header `◈ e2e · 1 unverified device`; per-person `◈`/`▲` | No E2E on group DMs. The header states instance-side encryption; per-person glyphs show presence only | Rule 13; ADR 0014 — a group needs N² sessions or a group-key scheme, rejected for v1 |
| 2 | `5e` whisper: `◈` prompt, "not stored server-side", per-recipient E2E | Whispers are ordinary server-stored messages with a private recipient set; the composer keeps its `presence` styling and loses `◈` | Rule 13; ADR 0014 excludes whispers *deliberately* so ADR 0013's break-glass moderation has plaintext for whisper-vector abuse |
| 3 | Overview: daemon does "P2P WebRTC routing"; `1f` DM route `p2p direct · 21ms` | Messaging is always client↔instance over the gateway. P2P is opt-in, consent-gated **file transfer** only | ADR 0016 (P2P scoped to file transfer, client-owned); architecture §6 (voice is an SFU, "not a plain P2P mesh") |
| 4 | `1b` "history is local only" | The instance stores DM **ciphertext** as well; it simply cannot read it. `3c`'s local FTS5 group and the overview's "local SQLite storage" were **correct** and are kept — ADR 0014 requires that index, because it is the only way an E2E DM is searchable at all. It covers E2E DMs only: guild channels and group DMs stay in-memory scrollback (ADR 0010) | ADR 0014; ADR 0018's exclusion note |
| 5 | `1e` "profile is signed, then synced" | "synced". Every other field on that screen — pronouns, timezone, links, accent, per-field visibility — is adopted as-is | No ADR covers profile signing; adopting the fields costs nothing, adopting the cryptography is a separate decision |
| 6 | `1d` CONFLICTS panel counting **plugin** chord conflicts; `3d` plugin-conflict tally; `6b` "1 chord conflict" | Plugins register **`M-x` commands**, never chords, so a plugin chord conflict cannot exist. The panel counts *your* overrides against defaults | ADR 0015 has no keybinding capability; a plugin that could bind `C-c` anything is a phishing surface |
| 7 | `6b` "cpu budget 2ms/frame" | Per-invocation wall-clock timeout plus a memory cap, as specified | Rule 12; plugins do not run inside the render loop |
| 8 | Overview: "sovereign, zero-bloat chat architecture" | The flagship-instance-first framing `CLAUDE.md` uses | `CLAUDE.md`, "What this is"; ADR 0007 |
| 9 | `5a` implied device-code is *the* first run | Loopback-browser login is primary (M8); `5a` is what a headless or SSH first run looks like (M9) | Roadmap M8/M9 ordering |
| 10 | `2c`/`C-x c` `peers` pane, undefined | `peers` = file-transfer sessions (ADR 0016) | Follows from #3 — there are no routing peers to show |
| 11 | Responsive pass "not mocked — an implementation decision" | Specified: the drop order, the minimum, and what happens below it (see Grid) | 80 columns is the most common terminal width; leaving it open invites the worst version |
| 12 | `KEYMAP.md`: bindings in `[keys]` | `[tui.keys]`. The client sections are `[shared]`, `[tui]`, `[gui]` — there is no `[cli]`, because the scriptable command tree has nothing to style | The `[cli]` section in `architecture.md` was written when "CLI" meant both things; that conflation is what this session set out to fix |
| 13 | `TOKENS.md` read as a fixed appearance | The palette is a named theme (`norite-dark`); the **default is the terminal's own ANSI 0–15**, and themes are shareable files under `~/.config/norite/themes/` | The design should be riceable; a tuned terminal palette should not be overridden by default |

Two things the design got *more* right than the plan did, kept as drawn and now reflected upstream:

- **`3c` separating server hits from DM hits** is exactly what rule 13 requires, expressed as a UI rather
  than as a caveat. The plan had the rule; this package had the shape.
- **`6c` stating "only reporter-attached content is visible"** on the admin screen matches ADR 0013's
  break-glass posture more honestly than any prose in `architecture.md` did.

### What changed in the mock

Text only, verified by comparing the tag structure of every line before and after: 2,163 lines in and out,
1,614 `<div>` open/close either side, 1,809 `<span>` open/close either side, and the same parse result.
Fifty lines differ from the original handoff. Twenty-three of them are one character — the rail's
direct-messages glyph, `U+FF20` FULLWIDTH COMMERCIAL AT, which is two cells wide and so broke the one-cell
rule `TOKENS.md` enforces on everyone else; it is a plain `@` now. The remaining twenty-seven are the
claims tabled below, four of which also change a colour, because what they carried was a warning and its
replacement is not:

| Screen | Was | Now |
| --- | --- | --- |
| `1b` | `2 devices · local history only` | `2 devices · decrypted on this device` |
| `1c` | `◈ e2e · 1 unverified device` (warn) | `encrypted by the instance · not e2e` (dim) |
| `1c` | `4 people · local history only` | `4 people · server-stored` |
| `1d` | `CONFLICTS — 1`, `also bound by plugin “vim-keys”`, `plugins 2` (warn) | `OVERRIDES — 4`, `yours · default was C-c V`, `plugin commands 2` (muted) |
| `1e` | `profile is signed, then synced` | `profile is synced to the instance` |
| `1f` | `dm route · p2p direct · 21ms` | `dm latency · 21ms via instance` |
| `3d` | `1 conflict with plugin “vim-keys”` | `2 commands added by plugins` |
| `5e` | `◈ whisper → you`; `◈` on candidate rows | `whisper → you`; `●` presence glyphs |
| `5e` | `only you and lune can read this · not stored server-side` | `only the people named above see this in #general` |
| `6b` | `1 chord conflict` (warn) | `adds 2 M-x commands` (dim) |
| `6b` | `cpu budget 2ms/frame` | `cpu budget per invocation` |
| `5e` | side panel `whispers are e2e per recipient`; status bar `◈ e2e per recipient` | `whispers reach only the named recipients`; `whisper` |
| `1c` | screen label `mixed verification states, no server-side history` | `instance-encrypted, not e2e` |
| `1d` | status bar `1 conflict · 4 overrides` | `4 overrides` |
| `4a` | TRANSPORT `route: p2p` | `route: sfu` — voice is an SFU, never a mesh (§6) |
| `5a` | `sovereign chat · first run` | `first run` |
| `6c`, `1d`, `1e`, `5a` | `C-c t` timeout, `C-c d` delete, `C-c a` audit, `C-c r` reset, `C-c p` preview, `C-c t` paste | `C-c C-t`, `C-c C-d`, `C-c C-a`, `C-c z`, `C-c C-p`, `C-c y` — screen-local verbs moved off global chords |

`3c`'s local-search group is **unchanged**: it was correct as drawn, and correction 4 above says why.
