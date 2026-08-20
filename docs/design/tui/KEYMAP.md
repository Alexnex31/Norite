# Keymap

Two prefixes: `C-x` = panes & windows, `C-c` = app actions. `M-x` = command mode.
When a prefix is armed the status bar says so. Bindings live in **`[tui.keys]`** in
`~/.config/norite/config.toml`, hot-reloaded, comments preserved on write (`1d`).

Plugins never appear in this table. A plugin registers `M-x` commands; binding a chord to one is your
override like any other (ADR 0015, ADR 0026), so no plugin can take a binding from you.

## Scope and precedence

Every chord is **global** or **screen-local**, and the rule between them is one-way:

> A global chord means the same thing on every screen and **cannot be shadowed**. A screen that wants a
> verb of its own takes a chord no global uses.

Panes and windows (`C-x …`), voice (`C-c v m d V`, `C-space`), navigation, and the client-wide verbs are
global — they are the ones held in muscle memory, and they are the ones you reach for without looking at
what is focused. That is exactly why they must not be reinterpreted: `C-c d` is deafen, so an admin screen
may not spend it on "delete this person's posts". A maintainer in a call who opens a report and reaches for
deafen must not delete anything.

Screen-local verbs therefore live on `C-c C-<key>` where the plain chord is taken, and two *different*
screens may share a screen-local chord, since they are never focused at once. Where a screen-local verb is
genuinely the same verb as a global one applied to what is in front of you — `C-c s` saving a search
filter, `C-c z` reverting an unsaved settings section — it keeps the global chord deliberately.

## Panes & windows — `C-x`
| Chord | Action | Scope |
| --- | --- | --- |
| `C-x 2` | split pane below | global |
| `C-x 3` | split pane right | global |
| `C-x 1` | back to one pane (restores full chrome) | global |
| `C-x o` | focus next pane | global |
| `C-x ←→↑↓` | focus by direction | global |
| `C-x z` | zoom / unzoom focused pane | global |
| `C-x x` | close pane | global |
| `C-x }` | grow pane | global |
| `C-x t` | new window (tab) | global |
| `C-x b` | toggle collapsed sidebars | global |
| `C-x c` | cycle pane type (chat / log / shell / peers / scratch — `peers` is file-transfer sessions) | global |
| `C-x C-b` | toggle shared vs. full chrome (2 panes only) | global |
| `C-x C-c` | detach — daemon keeps running | global |

## Navigation
| Chord | Action | Scope |
| --- | --- | --- |
| `C-c k` | fuzzy switcher (`3a` default, `3b` if `switcher = "minibuffer"`) | global |
| `M-1` … `M-9` | jump to guild N | global |
| `M-n` / `M-p` | next / previous channel | global |
| `C-c u` | next unread | global |
| `M-<` | oldest unread | global |
| `/` | search this guild (`3c`) | global |

## Messages
| Chord | Action | Scope |
| --- | --- | --- |
| `C-c w` | whisper composer (`5e`) | global |
| `C-c r` | reply to selected | global |
| `C-c t` | tag message | global |
| `C-c e` | edit last | global |
| `C-c p` | send file (opt-in p2p transfer, ADR 0016 — both sides consent before any address is exchanged) | global |
| `C-c a` | add person (→ group DM) | global |
| `C-c q` | view outbox queue (`5c`) | global |
| `C-c C-r` | retry connection now (`5c`) | global |

## Voice
| Chord | Action | Scope |
| --- | --- | --- |
| `C-c v` | join / leave voice | global |
| `C-c m` | toggle mic | global |
| `C-c d` | toggle deafen | global |
| `C-c V` | promote in-call strip to full voice view (`4b` → `4a`) | global |
| `C-space` (hold) | push to talk | global |

## Client & trust
| Chord | Action | Scope |
| --- | --- | --- |
| `M-x` | command mode — every `norite` verb, plus plugin-registered commands (`3e`, ADR 0026) | global |
| `C-h` | keybinding overlay (`3d`); again for the manual | global |
| `C-c ,` | settings (`1d`) | global |
| `C-c D` | toggle deep work (`5d`) | global |
| `C-c h` | review held messages (`5d`) | global |
| `C-c f` | verify keys / devices (`6a`) | global |
| `C-c i` | inspect plugin manifest (`6b`) | global |
| `C-c l` | plugin logs | global |
| `C-c s` | save (settings, profile) | global |
| `C-c z` | revert unsaved | global |
| `C-c B` | block user (`1f`) | global |

## Moderation (`6c`, maintainer role only)
| Chord | Action | Scope |
| --- | --- | --- |
| `C-c b` | ban subject | `6c` only |
| `C-c C-t` | timeout 24h (was `C-c t`, which is tag) | `6c` only |
| `C-c C-d` | delete subject's posts (was `C-c d`, which is deafen) | `6c` only |
| `DEL` | dismiss report | `6c` only |
| `C-c C-a` | audit log (was `C-c a`, which adds a person) | `6c` only |

## Screen-local chords

Verbs belonging to one surface. None shadows a global; two of them deliberately reuse a global chord
because they are the same verb applied to what is focused.

| Chord | Action | Scope |
| --- | --- | --- |
| `C-c x` | the fingerprints differ — refuse the device | `6a` |
| `C-c n` | add a private note about this person | `1f` |
| `C-c n` | deny the network capability, load the plugin anyway | `6b` |
| `C-c C-p` | preview your profile as a stranger (was `C-c p`, which sends a file) | `1e` |
| `C-c z` | revert the unsaved section — the global revert, applied here | `1d` |
| `C-c s` | save the search as a filter — the global save, applied here | `3c` |
| `C-c y` | yank: copy `M-x` output, and paste a token on first run | `3e`, `5a` |
| `C-c >` | write `M-x` output to a file | `3e` |
| `C-c c` | cancel the first-run flow | `5a` |
| `M-g` | restrict the switcher to guilds | `3a` |

## Conventions
`RET` = primary action on the selection · `ESC` = close overlay / cancel · `TAB` = next field or accept
completion · `C-n`/`C-p` = move in a list · `DEL` = remove/dismiss.
Destructive verbs confirm in the status bar, never in a modal.
