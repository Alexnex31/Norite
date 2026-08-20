# ADR 0009: CLI and native GUI as primary clients, Gio for the GUI, web SPA demoted

## Status
Accepted, and refined by [ADR 0026](0026-tui-as-a-first-class-client.md), which splits what this ADR calls
"the CLI" into two clients: the scriptable command tree, and the full-screen terminal UI. The build order
below is unchanged — terminal clients first, native GUI second, web SPA third — but "the CLI is a separate,
performance-focused, fully scriptable client … with its own custom pane/split TUI engine" describes two
programs, and reading it as one is what left the roadmap with six milestones of TUI capabilities and no
milestone that draws a screen.

## Context
The original plan (see ADR 0001/0002-era design) had a React web SPA as the primary, and only, client. The
revised scope makes a scriptable CLI and a native GUI the primary clients instead — better fits a
power-user/self-hoster/automation-heavy audience, and a terminal is a real requirement for the CLI-native
voice/dev-tools scope (integrated shell, CLI piping, SSH-friendly headless auth). The web SPA's original
design is kept, not discarded, but demoted to a third, lower-priority client built later.

## Decision
Build order: CLI first, native GUI second, web SPA third. The native GUI is built with **Gio**
(`gioui.org`), chosen over Fyne specifically because it's immediate-mode, GPU-rendered, and gives tight
memory control — the "must remain highly optimized" requirement outweighs Gio's lack of a built-in widget
library. The CLI is a separate, performance-focused, fully scriptable client (Unix-style: one action, exit,
pipeable stdin/stdout), built on the Charm stack (Bubble Tea + Lip Gloss + Bubbles) with its own custom
pane/split TUI engine rather than shelling out to real tmux, for identical cross-platform behavior including
Windows.

Keybindings are Emacs-style chorded (not vim-modal) as the shipped CLI default, stored in and fully
overridable via the shared config file. Both CLI and GUI attach to one shared local daemon (ADR 0010) rather
than each holding an independent gateway connection.

## Consequences
- Accessibility (screen-reader support, OS-level accessibility-API integration) is an explicit, documented
  non-goal for v1's GUI — Gio provides no such integration, and building it ourselves on top of an
  immediate-mode toolkit with no component tree is a real, ongoing cost, not a footnote.
- GUI testing relies on golden-image/screenshot comparison for the highest-value, most regression-prone
  surfaces (message list rendering, pane-split layout, voice UI states), with manual QA covering the rest —
  there's no component tree to snapshot-test conventionally.
- CLI testing uses Charm's own `teatest` package (key-press/message simulation, rendered-output assertions).
- The web SPA's REST/gateway contracts must still be periodically sanity-checked against real browser
  constraints (CORS, BFF-auth-compatibility, chattiness) even before it's built, so Phase O doesn't discover
  the contracts were designed assuming native-only capabilities (OS keychains, Unix sockets) — see ADR 0011.

## Alternatives considered
- **Fyne** (the other major pure-Go GUI toolkit): rejected — retained-mode with less direct control over
  rendering/memory behavior than immediate-mode Gio, which matters given the "highly optimized" requirement.
- **Electron/web-view-based desktop GUI**: rejected — pulls in a full browser runtime per client instance,
  directly opposed to the memory-control goal; also breaks the pure-Go/cross-compile story used everywhere
  else in the client stack.
- **Keep the web SPA as the only/primary client**: rejected — doesn't serve the CLI-native power-user/
  automation audience or the SSH-headless use case at all.
