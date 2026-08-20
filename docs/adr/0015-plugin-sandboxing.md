# ADR 0015: WASM plugin sandbox, headless, capability-gated, daemon-hosted

## Status
Accepted

Extended by [ADR 0026](0026-tui-as-a-first-class-client.md): a plugin may register `M-x` commands,
which is a capability like any other and is granted the same way. It may **not** register keybindings —
that would need a precedence rule against the user's own bindings, and a plugin able to bind `C-c`
something can phish for whatever is typed next.

## Context
Client-side extensibility is real v1 scope, but plugin code is inherently untrusted (whether written by the
user or downloaded from somewhere). It needs a real sandbox boundary, not just "run it and hope," and running
inside the one shared daemon (rather than duplicated per attach-client) means one plugin instance serves both
CLI and GUI.

## Decision
Sandboxed via WASM using `wazero` (pure Go, no cgo — keeps the daemon/CLI/GUI cross-compiling cleanly), with
an explicit host-function capability API: no raw filesystem/network/syscall access unless explicitly granted.
**Plugins are headless by design** — the host-function surface is slash-commands, text-parsing, and
data/message reads only. There is no UI-injection capability and no IPC bridge for painting native CLI/GUI
elements, and none is planned: the CLI (Bubble Tea) and GUI (Gio) use entirely different rendering paradigms,
and a daemon-hosted plugin has no natural way to paint into either safely. A plugin that wants to affect what
the user sees does so only through the data/text it returns from an already-gated host function.

Distribution is local-file-only in v1 (drop a `.wasm` in a plugins folder) — no registry/marketplace. Each
plugin ships a `manifest.toml` declaring needed capabilities; the daemon requires explicit user approval on
first load (browser-extension-style). Approval also pins a SHA-256 hash of the `.wasm` file; every later load
re-verifies against that pinned hash, halting and re-prompting on mismatch, so a file swapped on disk after
approval can never silently run under the stale grant.

Each plugin instance gets enforced CPU (instruction-count/timeout) and memory quotas, plus a separate
per-invocation wall-clock timeout — the CPU/memory quotas alone don't catch a plugin that hangs on a slow
network call without ever tripping them. `wazero`'s instruction-metering imposes a real baseline host-CPU
cost; Phase K benchmarks this against real plugin workloads, and if metering costs more than what it's
measuring, memory quotas plus the wall-clock timeout become the primary safety mechanism instead.

## Consequences
- No plugin code path is exempt from capability-gating "just this once" (`CLAUDE.md` rule 12) — every
  host-function call checks the calling plugin's approved manifest grants.
- TinyGo is the recommended (not required) plugin authoring toolchain, for smaller binaries; `wazero` runs
  WASM from any source language equally.
- Because plugins are headless, there's no accessibility/rendering-trust-boundary question to design for them
  at all — a real simplification traded against not letting plugins visually customize the client.

## Alternatives considered
- **A JSON-schema-based UI payload plugins can return, rendered natively by each client**: considered and
  rejected for v1 — real scope (a rendering contract every client must implement and keep in sync) for a
  capability nothing in the initial feature set actually needs; revisit only if a real use case demands it.
- **Native Go plugin loading (`plugin` package) instead of WASM**: rejected — no real sandboxing, platform-
  and Go-version-fragile, and breaks the cross-compile story.
