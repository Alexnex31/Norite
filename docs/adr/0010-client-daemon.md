# ADR 0010: A shared local daemon behind the CLI and GUI, not two independent clients

## Status
Accepted

## Context
With two native clients (CLI, GUI) plus a later web client, each independently holding its own gateway
connection would mean duplicated connection/presence/scrollback logic, two processes racing to claim
"online" presence, and no way for a background voice call or plugin to keep running when both attach clients
are closed.

## Decision
One background daemon process per OS user account owns: the persistent WebSocket gateway connection,
presence/Deep Work state, in-memory scrollback/unread state (not read/unread state, which is durable —
see ADR 0008-adjacent design in `docs/architecture.md`), the WASM plugin host, and the local bot-automation
listener. The CLI and GUI are both thin "attach" UIs over it. Voice is deliberately *not* in the daemon (a
separate voice-worker subprocess, spawned on demand — see ADR 0012).

Two IPC channels, different trust tiers: a Unix domain socket / Windows named pipe for the CLI/GUI
(OS-file-permission-protected, no secret needed, reuses the gateway's own op-code/DISPATCH protocol so both
share one client-side event parser), and a separate localhost-only TCP port with its own per-session secret
for external bot-automation scripts (must not receive first-party trust). Both use the same 4-byte-length-
prefix JSON framing, also reused unmodified for daemon↔voice-worker IPC. The shared HELLO/IDENTIFY handshake
carries a semver field (MAJOR must match exactly; a defined MINOR-version-back window is tolerated).

The daemon auto-installs as a real OS-level service (systemd/launchd/Windows task), running from login. A
single shared, hand-editable TOML config file (`pelletier/go-toml` v2, document-editing mode to preserve
comments) holds theme/keybindings/notification-filter data; a second, daemon-owned state file holds anything
daemon-written-only (plugin capability grants + pinned hashes, the voice-channel breadcrumb). Every writer
uses atomic writes plus `gofrs/flock` locking; the daemon hot-reloads on external changes via `fsnotify`.

## Consequences
- A stalled/frozen attach client can never block the daemon's core loop: the daemon's write path to each
  attach client is asynchronous and bounded (a per-connection writer goroutine + fixed-capacity channel), and
  a client whose buffer fills gets dropped rather than stalling the gateway connection, E2E ratchet
  advancement, or voice control signaling for everyone else attached.
- Scrollback/pane/presence state is in-memory only, lost on daemon restart (the same semantics as tmux); the
  gateway's RESUME mechanism rebuilds it. The one deliberate exception is the "last active voice channel"
  breadcrumb, persisted specifically so voice can auto-rejoin after a crash.
- Cross-client pane splitting is a requirement for all three clients but is never synced across clients or
  devices by default — each client implements its own split-pane engine, and layout lives in the daemon/
  config-file state (CLI/GUI, same machine) or `localStorage` (web, separate codebase). A same-machine
  CLI/GUI toggle can opt into separate config files instead of the default shared one; `app config export`/
  `import` carries preferences across separate daemons/machines manually.
- The daemon proactively raises `RLIMIT_NOFILE` at startup — it's effectively a local server holding many
  simultaneous handles (gateway WS, N attach sockets, bot-automation TCP, voice-worker pipes, SQLite/log
  files) and default OS limits (256 on macOS) are easy to exceed under normal multi-client, active-voice use.

## Alternatives considered
- **Each client holds its own gateway connection independently**: rejected — duplicated logic, presence
  races, and voice/plugins die the moment both attach clients close, defeating "stay in a voice call while
  switching from GUI to CLI."
- **One config file per client instead of one shared file**: rejected as the default — keybindings/theme
  should apply everywhere by default; a same-machine opt-out toggle covers the minority case that wants
  divergence without abandoning sharing as the default.
