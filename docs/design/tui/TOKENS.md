# Tokens & component recipes

## The theme model

Everything below is a **default**, not a fixed appearance. Norite is meant to be riced.

**The shipped default is your terminal's own palette.** The roles map to ANSI 0–15, so Norite inherits
whatever scheme you already run and looks like the rest of your setup on first launch. The hex palette in
the next section is a named theme (`norite-dark`) you opt into with `theme = "norite-dark"` — it is what
the mockups draw, and it is what to match when judging whether a screen looks right.

Why that way round: a user with a tuned 16-colour terminal has already made these decisions, and an app
that overrides them by default is the thing ricers complain about. A truecolor theme is one line of config
away for anyone who wants the drawn look.

**Where themes live.** `~/.config/norite/themes/<name>.toml`, selected by name from `[tui]` in
`config.toml`. A theme is a shareable file — that is the point — so it is treated as untrusted input:

- Colours parse as ANSI index, `#rrggbb`, or a role reference; anything else fails the load with the line
  number, and the theme does not partially apply.
- **Glyph overrides are validated for display width before use.** The grid assumes one cell; a two-cell
  glyph (most emoji, some Nerd Font ranges) silently shears every row it appears in. A glyph that is not
  exactly one cell wide is refused, named in the error.
- Every string in a theme passes `termsafe` before it reaches the terminal (rule 19). A theme file is text
  from someone else, arriving over the internet, being written to your terminal — precisely the shape the
  sanitizer exists for.
- A theme sets appearance only. It cannot bind keys, run commands, or reach the network.

**What is overridable**: the palette roles below; border style (`rounded` / `square` / `heavy` / `none`);
message density (`compact` / `cozy`); timestamp format; the author-colour set; the glyph table; and the
fixed column widths (rail / channels / members). Column widths are clamped to what stays legible and to
the responsive rules in `README.md` — a 6-cell member list is refused rather than drawn.

**What is not**: the information architecture. Which columns exist, what the status bar's two rows mean,
and the chrome-by-pane-count rule are the design, not preferences.

## Palette — the `norite-dark` theme (16-color classic, near-black base)
| Token | Hex | Used for |
| --- | --- | --- |
| `bg` | `#0b0d0e` | terminal background |
| `bg.panel.head` | `#101314` | unfocused pane title bar |
| `bg.panel.head.focus` | `#0f1c1f` | focused pane title bar, voice pane header |
| `bg.status.keys` | `#0e1112` | status bar, second row |
| `bg.overlay` | `#0e1113` | switcher, help, M-x, profile card |
| `bg.log` | `#0d1011` | log/peers pane body |
| `border` | `#262c2f` | every panel and pane border |
| `border.inner` | `#1c2224` | dividers inside a panel |
| `text` | `#c8d0d2` | message bodies, values |
| `text.bright` | `#eef2f3` | names, active headers |
| `text.muted` | `#7d8a8e` | secondary, inactive channels |
| `text.dim` | `#5b6669` | labels, timestamps, hints |
| `text.dimmest` | `#4a5457` | disabled, empty placeholders |
| `accent` | `#2fb6c4` | focus, selection, active route, cursor |
| `accent.fill` | `#12262a` | selected row background |
| `ok` | `#4fb570` | online, verified, INFO, speaking |
| `warn` | `#d0a437` | chord hints, idle, WARN, unverified, queued |
| `danger` | `#cf5c5c` | mentions, muted mic, ERROR, offline, destructive |
| `presence` | `#a97fd0` | deep work, whispers, one author color |

**Semantic backgrounds** (banner tints, always border + fill together):
- ok `#0e1512` / `#2f4a39` · warn `#161206` / `#3a3320` · danger `#160f10` / `#3a2020`
- presence `#120f17` / `#2c2438` · presence fills `#241f2c`, `#1a1424`

**Author colors** (deterministic hash of user id, never random per render):
`#a97fd0` `#2fb6c4` `#cf5c5c` `#d0a437` `#4fb570`. Bots/webhooks always `#4fb570` with an `AUTO` badge.

## Type
One cell, one size. The mock's `13px/20px` = the terminal's cell; `11px` = the same cell rendered in
`text.dim`. Weight: bold for names, headers, active rows; regular everywhere else. Section labels are
UPPERCASE with `letter-spacing:.06em` in the mock → in a TUI, just uppercase in `text.dim`.

## Glyphs
| Glyph | Meaning |
| --- | --- |
| `◈` | E2E verified |
| `▲` | unverified device / warning |
| `●` `◐` `○` | online / idle / offline |
| `✦` | deep work / busy |
| `♪` | voice |
| `◇` | group DM, pending step |
| `◷` | queued, will send on reconnect |
| `▍` | audio level segment |
| `▤` | file attachment |
| `⚑` `⚙` `@` | reports, settings, direct-messages rail entry |
| `◎` | discover / public matchmaking rail entry |
| `◆` | focused pane marker |
| `›` | composer / list cursor prompt |
| `▾` | expanded sidebar section |

No emoji. A color emoji is double-width in some terminals and single in others — it breaks the grid.

**One cell each — a claim to measure, not assume.** Most of the table (`◈ ▲ ● ◐ ○ ♪ ◇ ▍ ▤ ◆`) is East Asian
Width **Ambiguous**: one cell in a Western locale, *two* in a terminal told to treat ambiguous characters as
wide. That is not exotic — it is the default in CJK-locale terminals and a switch in tmux, VTE and Windows
Terminal. Such a glyph shears every row it lands in, which is the emoji failure arriving from the direction
the emoji ban does not cover. So the renderer resolves width the way the terminal will
(`mattn/go-runewidth`'s East-Asian mode, which Lip Gloss already depends on) and refuses a two-cell glyph
exactly as it refuses a two-cell theme override — one implementation, both paths. **The shipped table
therefore needs a one-cell alternative for each ambiguous glyph**, chosen at M45 with the theme validator
rather than by whoever hits the problem first.

## Component recipes
**Panel** — rounded border `border`, header row (1 row, bold title left, dim meta right), body, optional
footer row separated by `border.inner`.

**Pane, focused** — border `accent`, title bar `bg.panel.head.focus` with `accent` bold title +
`◆`. **Unfocused** — border `border`, title bar `bg.panel.head`, body drawn in the dim palette.

**Sidebar row** — 1 row; active = `accent.fill` bg + `accent` text; unread = `text.bright` + right-aligned
count in `danger`; muted/inactive = `text.muted`; section label = 1 row, `▾ NAME` in `text.dim`.

**Message group** — header row (author in its author color, bold + timestamp in `text.dim`), then 1–n
body rows. Consecutive messages from one author within a short window share the header. Day dividers
are a centered `── Tuesday ──` in `text.dim`. System lines are a single dim row with a glyph.

**Composer** — 1-row bordered box, prompt glyph, text, block cursor (`accent`, 1 cell). Border is
`accent` when the pane is focused, `border` when not, dashed `warn` when messages are queueing (`5c`),
`presence` when composing a whisper (`5e`).

**Status bar (`7a`)** — always the bottom 2 rows.
Row 1 (state): route in `accent` bold · context counts in `text.dim` · `·` · unread in `warn` →
spacer → daemon health in `ok`/`danger` · E2E `◈` · voice · presence · clock.
Row 2 (chords): `bg.status.keys` background, chord in `warn` + label in `text.dim`, contextual to the
focused surface, ending with `C-h all keys`. **Both rows never wrap** — truncate the least important
segment first (counts, then clock).

**Overlay** — `bg.overlay`, `accent` border, header row (title + dim hint), body, footer row of chords.
Centered (`3a`, `3d`) or bottom-anchored (`3b`, `3e`) or right-anchored over the message area (`1f`).
Background app is drawn in the dim palette.

**Metric list** (voice, peers, plugins) — label in `text.dim` left, value right, 1 row each, value colored
by health. Keep values short; these columns are 20–28 cells wide.
