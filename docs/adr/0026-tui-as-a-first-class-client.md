# ADR 0026: The TUI is a first-class client, and shares one command tree with the CLI

## Status
Accepted. Refines [ADR 0009](0009-cli-and-gui-client-architecture.md), which named the primary clients "a
scriptable CLI and a native GUI" and folded the terminal UI into the first of those. Extends
[ADR 0015](0015-plugin-sandboxing.md) with a plugin capability. The screens this describes are specified in
`docs/design/tui/`.

## Context

ADR 0009 says "CLI first, native GUI second". Read closely it means two different things by "CLI": a
Unix-style command tree — one action, exit, pipeable — and an immersive full-screen terminal application
with panes, chords and a message list. It builds the second out of the first's budget, as one bullet
("its own custom pane/split TUI engine"), and the roadmap inherited that: pane engine, keybindings,
markdown renderer, image rendering, logging, tests. Six milestones of *capabilities*, and no milestone that
builds an application. Nothing in the plan ever drew a guild rail, a channel list, a status bar, or a
message. The *CLI pane engine* milestone's "done when" — panes each pointed at a different channel —
assumes a chat view that no milestone creates. (Cited by title rather than number on purpose: this decision
renumbered the phase it describes, so the number that milestone carried now belongs to one of the
milestones written to replace it.)

That is not a scheduling oversight, it is a category error, and it had a consequence: the design work in
`docs/design/tui/` specified twenty-five screens against an architecture that had described none of them,
and drifted where it had nothing to check against.

The two are genuinely different programs. `norite login`, `norite config get`, `norite instance init` are
things you run from a shell and pipe into `jq`. The TUI is where a person spends an evening. They share a
daemon, a protocol, a config file and a sanitizer — and almost no user interface.

## Decision

**The TUI is a client in its own right**, alongside the scriptable CLI, the native GUI and the later web
SPA. It ships inside the same `norite` binary because that is convenient to distribute, not because it is
the same program. `docs/architecture.md` gets a section of its own for it (§4a) and the roadmap gets a
phase of its own, ordered by screen rather than by capability.

**One command tree, two front ends.** Every verb in the `urfave/cli` tree is invocable from the TUI's `M-x`
command mode and renders into a pane. This is the property that makes the split safe rather than a fork:
the scriptable surface cannot drift from the interactive one, because there is only one of them. Three
things follow, and they are the cost of the decision:

- Every verb must return a **structured result**, not printed text, so that one caller can format it for a
  terminal and the other for a pane. `--json` and the schemas in `contracts/cli-json/` stop being a
  late nicety and become the shape every command is written against.
- The tree must be **invocable in-process**. It already is: `cliapp` exists so the tree can be built and
  exercised without spawning a process.
- Verbs that are interactive by construction — `norite instance init` is a sequential stdin conversation
  that refuses to run without a TTY, deliberately, so it works over SSH and in `docker exec` — run in a
  **pty pane**. They get a real terminal and behave exactly as they do outside. There is no second
  implementation of any prompt flow.

**A pane is any viewport.** `chat`, `log`, `shell` (pty), `peers` (file-transfer sessions, ADR 0016),
`scratch`. Panes live in named windows with a tab bar. The layout tree and scroll offsets belong to the
daemon (ADR 0010), so detaching and reattaching restores them.

**Chrome is a function of pane count.** One or two panes may each be a complete client — own rail, own
channel list, own member list — showing a different guild each. Three or more panes draw chrome once for
the window and hold content only. Forcing per-pane chrome at three panes produces columns too narrow to
read, and the rule is what keeps the pane engine from having to negotiate that case.

**Plugins register commands, never keybindings.** A plugin adds `M-x` verbs, which appear alongside the
built-in ones; binding a chord to one is the user's override like any other. The alternative — plugins
declaring chords — needs a precedence rule, a conflict-resolution UI, and an answer for a plugin that binds
`C-c` something and phishes for whatever the user types next. Registering a command is the same capability
with none of that.

**The GUI mirrors this information architecture.** Same layout, same vocabulary, same screens, presented
natively. The TUI is the in-terminal form of one application, not a different product that happens to share
a backend.

## Consequences

- **The roadmap's client phase is rebuilt around screens.** Shell and grid, then the status bar, then
  message rendering, then the chord dispatcher, then the pane engine, then the finding/voice/state/trust
  surfaces. The old capability milestones fold into it.
- **`--json` moves early and becomes load-bearing.** A verb added after this without a structured result is
  a verb the TUI cannot run.
- **A pty lands earlier than planned.** The integrated shell was a late milestone; it is now on the
  self-hosting onboarding path, because it is what `norite instance init` runs inside. It carries a real
  trust boundary — the same one a terminal emulator has — and that arrives with it.
- **The GUI phase is no longer free to invent its own information architecture**, which removes a large
  design question from it and adds the constraint that a terminal-derived layout has to survive being
  rendered natively.
- **ADR 0009's build order is unchanged**: terminal clients first, native GUI second, web SPA third. Only
  the count of primary clients and the naming change.

## Alternatives considered

- **Leave the TUI inside the CLI, expand the existing milestones**: rejected. It keeps the vocabulary that
  caused the gap — every milestone named "CLI …", a config section called `[cli]` that is really about
  chords and colours — and it keeps the ordering capability-first, which is what produced a plan with no
  screens in it.
- **A curated subset of verbs in `M-x`**: rejected. The subset is a judgement call somebody has to
  maintain, and the first verb left out is the first place the two front ends diverge.
- **TUI-native forms for interactive verbs**: rejected for now. It is a second implementation of every
  prompt flow, and it contradicts the wizard's own design, which is that it must work as plain stdin over
  SSH and inside `docker exec`. A pty pane gets that for free.
- **Plugins may bind chords**: rejected, above.
- **A separate `norite-tui` binary**: rejected. Two binaries to install, version-match and document, for a
  separation that matters to the architecture and not to the person typing `norite`.
