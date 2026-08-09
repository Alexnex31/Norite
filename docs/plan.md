# v2 Architecture & Milestone Plan

## 1. Overview

This document is the complete architecture and milestone plan for the project: a self-hosted-first,
source-available chat platform (voice and text) with three clients — a scriptable CLI, a native GUI, and a
lower-priority web SPA built later — all sharing one local background daemon per OS user account. It
supersedes all prior planning content in this repository (`CLAUDE.md`, `docs/architecture.md`, `docs/adr/`,
`README.md`) — those files are rewritten from this specification, per Section 12. Nothing described here is
implemented yet; the codebase is pre-implementation. This document is the target architecture and the
dependency-ordered build sequence to reach it.

The full scope described here is realistically multi-year, systems-engineering-team-sized work. The milestone
roadmap in Section 13 is a long-term dependency-ordered critical path, not a near-term promise. No scope
described in this document is removable; the roadmap is ordered so the most foundational pieces land first.

## 2. Known tensions and accepted limitations

These are permanent, deliberate properties of the design. They must never be treated as gaps or oversights
during implementation, and must be documented plainly wherever the relevant subsystem is described in
`docs/architecture.md` and the relevant ADR:

- **Voice-worker isolation.** Voice/audio media (capture, encoding, the SFU connection, DSP) runs in an
  isolated voice-worker subprocess, spawned on demand by the daemon and torn down when a voice session ends —
  never inside the daemon process itself. A crash in the voice-worker must never take down messaging,
  presence, or plugins.
- **Mic-permission handoff is unverified until Milestone M25 completes.** The design intent is that a
  foreground CLI/GUI client triggers the OS permission prompt on first voice use, then hands audio capture off
  to the voice-worker subprocess once granted. OS mic-permission grants (especially macOS TCC) are typically
  tied to whichever binary actually opens the audio device, which may end up being the voice-worker process
  rather than whichever attach client displayed the prompt. Milestone M25 is a throwaway prototype/spike that
  determines the real per-OS answer before the real voice milestones are designed in further detail.
  Milestone M25 also determines whether a headless daemon process can register an OS-wide global hotkey (for
  push-to-talk) on each target OS, including the macOS Input Monitoring entitlement — the same category of
  "which OS-level binary actually holds this capability" question as the mic-permission handoff.
- **Self-hosting simplicity and voice-in-v1 are in real, unreconciled tension.** Postgres-only self-hosting
  keeps the database story simple, but the custom SFU and embedded TURN server mean self-hosters must still
  handle UDP port ranges and NAT/firewall traversal — a materially bigger operational burden than pure
  text-only self-hosting. Voice is real v1 product functionality, but is a deployment-time opt-out
  (Milestone M37) specifically because of this burden.
- **cgo is confined to the voice-worker binary.** The pure-Go, cgo-free constraint used everywhere else in the
  stack (daemon, CLI, GUI, backend) does not extend to the voice-worker: Opus (`hraban/opus`), RNNoise, and
  `libspeexdsp` are all cgo bindings, because no mature pure-Go equivalents exist for production-grade audio
  codec/DSP work. This exception is contained entirely to the isolated, opt-out-gated voice-worker binary; the
  daemon, CLI, and GUI stay pure Go and cross-compile cleanly regardless.
- **E2E encryption carries compounding, not merely additive, cryptographic risk.** The feature depends on two
  independent custom protocol surfaces: the device-linking protocol (fully custom, no off-the-shelf
  equivalent) and the correct integration of the `go.mau.fi/libsignal` library into this project's own
  key-management and multi-device model. Either surface can silently break forward secrecy or device-trust
  guarantees with no visible symptom. Both require the dedicated external cryptographic security review at
  Milestone M95 before E2E is enabled for any account beyond the developer's own test accounts — enforced by a
  build/instance-level flag, not a documentation policy. This is a hard release gate, not optional polish.
  No history-transfer mechanism exists for a newly linked device, either: a device linked via Milestone M92
  sees only messages sent after linking, matching the no-backup, permanent-loss-on-device-loss philosophy
  already accepted below. This is a deliberate limitation, not an oversight — it adds zero new custom-crypto
  surface to a protocol already carrying the two risks above.
- **The SFU's codec-agnostic track model is necessary but not sufficient for video.** Section 6 deliberately
  keeps Pion's internal track/participant model track-kind-agnostic now, specifically so a video track type
  is additive later rather than a redesign — but that agnosticism only covers whether the SFU *can* forward
  a track, not whether it forwards it *well* under real-world bandwidth constraints. Simulcast/SVC (dropping
  spatial/temporal layers for a participant on a poor connection) is real, separate engineering work,
  deliberately scoped into Phase N (Milestones M97–M99) rather than assumed to fall out for free from the
  agnostic track model. Stated explicitly so it is never mistaken for scope the current design already
  covers.
- **Gio's engineering cost is real, not just a toolkit-choice footnote.** Gio provides no built-in widget
  library, no OS-level accessibility/screen-reader integration, and no component tree to snapshot-test. Every
  GUI surface — message virtualization, voice/video call UI, pane splitting, device-linking flows, plugin
  extension points, settings, theming — is hand-built from primitives. Accessibility support is an explicit,
  documented non-goal for v1, not a silently dropped feature. GUI testing relies on golden-image/screenshot
  comparisons for the highest-value, most regression-prone surfaces (message list rendering, pane-split
  layout, voice UI states), with manual QA covering everything else.
- **Public-matchmaking voice abuse has no evidence to review, by design.** Call recording is a permanent
  non-goal across the whole platform, for privacy reasons — this holds even for public matchmaking voice
  channels, which are the one voice context with no guild owner to trust. A voice-abuse report filed against a
  public-matchmaking voice channel therefore has nothing for an Instance Admin to review. Moderation of
  public-matchmaking voice is necessarily corroborating-multi-report-based only, never evidence-based. This is
  accepted as a permanent limitation.
- **E2E encryption is text-only, permanently, for this plan's scope.** Voice audio relies solely on standard
  WebRTC transport encryption (DTLS-SRTP), which protects against network eavesdroppers but not against the
  server/SFU operator. True end-to-end voice (frame-level encryption the SFU forwards without decrypting) is
  out of scope entirely.

## 3. Licensing and project posture

The project is source-available, not open source. The license is a custom, restrictive, non-OSI-approved
license in the BSL/SSPL style: the code is visible for self-hosting trust and transparency, but forking,
reselling, or standing up a competing hosted offering is forbidden by the license terms. The exact legal
license text requires real legal review before any external distribution — it is not something to draft
unreviewed.

The audience starts as personal use (the developer, optionally a small invited circle) but the architecture
must support a commercial future without a rewrite: nothing is designed to assume "no billing, no external
users, no ToS will ever exist," but no billing or team-accounts system is built now. This is a seam — present
in the data model and permission surface, inert until activated — the same treatment voice previously
received before this plan activated it.

Required repository scaffolding changes:
- Remove `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, and `CODEOWNERS` — there is no external contributor
  community to serve. These can be re-added if collaborators are ever brought on.
- Keep `SECURITY.md`, with its "open source project" framing removed. The vulnerability-reporting process
  itself is retained, since it remains useful once the project has any non-personal users.
- Replace `LICENSE` with the new custom source-available license text once legal review is complete.
- Rewrite `README.md` in full. It must describe: the CLI and native GUI as the primary clients (the web SPA
  is a later, lower-priority third client); voice (audio) as real v1 functionality, not a later phase; BYOK
  E2E encryption (opt-in, `DM`-only); public matchmaking channels and friends; Deep Work, message tagging,
  whispers, regex notification filters; client-side plugins; per-guild custom emoji; incoming webhooks; P2P
  file transfer; the Instance Admin/reports moderation system; and the two-deployment commercial model (a free
  flagship instance plus sold self-hosted licenses). The "Tech stack" section must list the CLI (Bubble
  Tea/Lip Gloss/Bubbles), the native GUI (Gio), the daemon, and the voice-worker alongside the existing
  backend stack, with React listed as the later tertiary client. The milestone summary must match Section 13.

## 4. Clients and authentication

Three clients exist in this plan: a CLI, a native GUI, and (later, lowest priority) a web SPA. Build order is
CLI first, GUI second, web SPA third — the web SPA is demoted from primary client to a lower-priority third
client, built only after the CLI and GUI land.

**Native GUI.** Built with Gio (`gioui.org`), chosen over Fyne specifically because it is immediate-mode,
GPU-rendered, and gives tight memory control — the "must remain highly optimized" requirement takes priority
over Gio's lack of a built-in widget library. Buttons, lists, and the virtualized message view are built from
primitives rather than off-the-shelf components. Accessibility (screen-reader support, OS-level
accessibility-API integration) is an explicit non-goal for v1. Testing uses golden-image/screenshot comparison
for the highest-value, most regression-prone surfaces (message list rendering, pane-split layout, voice UI
states) — rendered headlessly and compared pixel-for-pixel against a reference image — with manual QA covering
everything else.

**CLI.** A separate, performance-focused, fully scriptable client: Unix-style (run one action and exit,
pipeable stdin/stdout).

**Web SPA.** The existing React web SPA design is kept but demoted to the third, lowest-priority client, built
only after the CLI and GUI exist.

**Authentication model.** Neither the CLI nor the native GUI has a browser cookie jar, so authentication for
these two clients is fully token-based: a short-lived access token plus a refresh token, Bearer-style, stored
in each OS's secure credential store via `zalando/go-keyring`. Cookie/CSRF/double-submit authentication is
retired entirely for the CLI and GUI. Personal API tokens support scopes (e.g. a `messages:send`-only token
for automation/bots), not just one full-access token per user.

**Credential ownership.** The daemon is the sole holder of its account's access and refresh tokens — one
keychain entry, one process. The CLI and native GUI never independently store a token copy of their own;
every authenticated action they trigger is relayed through the daemon over the existing local IPC socket
(Section 5), the same as messaging. Refresh tokens are scoped per `device_id`: rotating the token on one
device's daemon must never invalidate another device's session or refresh-token family, since a user may run
a daemon on more than one machine (e.g. a desktop and a laptop) under the same account. Scoped `api_tokens`
minted for bots/local automation are a separate, deliberate exception — they are secrets for external
scripts, not the daemon's own session, and are not subject to this single-holder rule.

**CLI login (`app login`).** Supports direct password login (an in-terminal prompt) and, for OAuth (Google,
GitHub), a system-browser-plus-localhost-callback loopback flow: the CLI opens the user's default browser to
the provider and briefly listens on a local port to catch the redirect and extract the resulting token — the
same pattern `gcloud`/`aws`/`gh` use. The exact callback URL is registered with both providers as a **fixed
local port** (e.g. `http://127.0.0.1:51763/callback`), the same port for both providers for consistency and
predictability, because GitHub OAuth Apps require an exact, pre-registered callback URL with no port
flexibility (unlike Google's more flexible loopback-redirect support for installed apps). A small documented
fallback list of 2–3 alternate registered ports is tried in order if the primary port is already bound
locally; if every registered port is occupied, `app login` fails with a clear "free up port X (or one of Y/Z)
and retry" error rather than hanging or silently picking an unregistered port.

**CLI headless/remote fallback.** Since the CLI is explicitly meant to run over SSH on remote servers,
`app login` detects a headless context (no local browser reachable, or an explicit `--no-browser` flag) and
falls back to a device-code flow: it displays a short code and URL that the user completes on any device with
a browser — the same dual-mode pattern `gh`/`gcloud`/`aws` support. This is backed by a `device_code` table
(code, expiry, resulting token once completed) and a minimal, unstyled, server-rendered auth-completion page
(enter the code, complete the password/OAuth login) that ships as part of the backend, entirely separate from,
and not blocked on, the web SPA. Additional scoped tokens for automation/bots can be minted from either the
CLI or the GUI once logged in.

**Web SPA auth (later).** When the web SPA is eventually built, it gets its own BFF-style httpOnly-cookie
exchange layer in front of the token API, so it never holds a raw Bearer token in JS.

**Contract review against browser constraints.** Because the web SPA is built last (Section 3), `openapi.yaml`
and `gateway-events.schema.json` are sanity-checked against real browser constraints — CORS, BFF-compatible
auth flow, request chattiness — on an ongoing basis starting the moment each contract becomes load-bearing
(`openapi.yaml` at Milestone M12, where `oapi-codegen` first generates code from it; `gateway-events.schema.json`
at Milestone M18) and continuing through every later contract change, up to and through Phase O. This
prevents native-client-only assumptions (OS keychains, direct filesystem access, persistent Unix sockets)
from quietly baking into the contracts before anyone is checking them against a browser's sandboxed model.

**Voice.** Voice (audio) ships as real, working functionality in v1, on every client (CLI, native GUI, and the
later web client) — see Section 6 for the full design.

## 5. Client daemon architecture

The CLI and GUI are not two independent programs that each hold their own gateway connection — they are both
thin "attach" UIs over a shared local background daemon.

**Process model.** One daemon process per OS user account (on a shared multi-user system, one daemon per OS
account, each with its own config/keychain/socket; on a typical personal single-user device this is
functionally the same as "per machine"). The daemon owns: the persistent WebSocket gateway connection,
presence/Deep Work state, in-memory scrollback state (read/unread state is durable and Postgres-backed
instead — see "Read state vs. scroll state" below), the WASM plugin host, and the local bot-automation
listener. Voice/audio is deliberately not in this process (Section 6).

**Lifecycle.** The daemon is auto-installed as a real OS-level service (a systemd user unit on Linux, a
launchd agent on macOS, a startup task on Windows), running from login rather than only when a client happens
to be open. On startup, before opening any IPC/network/subprocess handles, the daemon proactively raises its
own `RLIMIT_NOFILE` (via `syscall.Setrlimit`) to a safe ceiling (e.g. 4096) — the daemon is effectively a
local server holding a persistent WebSocket, multiple attach-client Unix-socket connections, the
bot-automation TCP listener, voice-worker subprocess pipes, and SQLite/log file handles simultaneously, and
default per-process file-descriptor limits (notoriously as low as 256 on macOS) are easy to exceed under
normal multi-client, active-voice-call use.

**Daemon↔attach-client IPC.** A Unix domain socket / Windows named pipe connects the CLI TUI and native GUI to
the daemon — OS-file-permission-protected, no secret needed since only the owning OS user account can open it.
This is a different, more trusted channel than the local bot-automation port. The daemon reuses the real
gateway's exact op-code/DISPATCH event protocol over this local socket: it holds the actual WebSocket
connection and relays the same envelopes locally, and accepts the same client-to-server op-codes for the local
handshake. `contracts/gateway-events.schema.json` and one client-side event parser serve both the real gateway
and local daemon-attach, so CLI and GUI share the same event-handling code path. Message framing on the Unix
socket is a 4-byte length prefix followed by that many bytes of JSON payload (a Unix socket is a raw byte
stream, unlike a WebSocket, so explicit framing is required; the length prefix avoids relying on "JSON never
contains a literal newline"). The wire format is JSON, not a binary format like MessagePack: JSON is
human-readable in a packet capture/debug session with zero-friction tooling everywhere, and for a
chat/voice-control-plane protocol (actual voice audio bypasses this path entirely via its own RTP stream to
the SFU) parsing overhead is not where CLI responsiveness lives — rendering is. This same 4-byte-length-prefix
JSON framing is reused, unmodified, for the daemon↔voice-worker subprocess IPC described in Section 6, so only
one framing scheme is documented and implemented across the whole project. The daemon's write path to each
attach client is **asynchronous and bounded**: a per-connection outbound queue/channel with a fixed capacity,
fed by its own writer goroutine (Section 5's "Concurrency model" below). If a frozen or suspended CLI/GUI
(e.g. a terminal caught by `Ctrl+Z`) lets its socket buffer fill, the daemon drops that local connection
rather than blocking on the write — a stalled attach-client UI must never be able to stall the daemon's real
gateway connection, since that would also stall E2E ratchet advancement and voice control signaling for
every other attached client. The dropped client simply resyncs its state on reattach.

**Clock synchronization.** The gateway HELLO handshake includes the backend's current server time. The daemon
computes a local offset from it and applies that offset to all local JWT-expiry checks and E2E ratchet
timestamping, rather than trusting the OS clock directly — desktop machines routinely wake from sleep with a
clock skewed by minutes to days, which would otherwise cause silent auth drops or spuriously "expired"/
"replayed" E2E messages.

**Initial sync payload.** The gateway's initial READY/DISPATCH sync is kept small at the source: guild/channel
metadata is sent upfront, but full member lists and other per-guild bulk state are deferred until a guild is
actually opened by the user (lazy per-guild loading), rather than the backend eagerly shipping everything for
every guild an account belongs to. The daemon additionally stream-decodes this payload (`json.Decoder`) rather
than buffering it fully before parsing. Together these keep first-connect latency and daemon startup CPU/GC
pressure bounded for accounts in many guilds, instead of scaling linearly with total guild count.

**Protocol version compatibility.** The shared HELLO/IDENTIFY-equivalent handshake — used for both the real
daemon↔backend gateway connection and the local CLI/GUI↔daemon socket — carries a semver version field. The
MAJOR component must match exactly; a mismatch blocks the connection outright. The backend supports a defined
N-minor-version-back window for the MINOR component (the last 2–3 minor versions), so a slightly-behind
client/daemon still connects, perhaps with a "please update" warning, rather than being hard-blocked for every
small protocol addition.

**Local bot-automation port.** A separate, localhost-only TCP listener with its own per-session secret
(written to a `0600` file or passed via an environment variable at launch), kept separate from the
daemon-attach IPC because external scripts in arbitrary languages need a plain TCP/HTTP surface, not a
Unix-socket client library, and must not receive the same trust level as first-party clients. It is
authenticated via scoped `api_tokens`.

**Daemon state persistence.** Scrollback/panes/presence state is in-memory only, not disk-persisted across a
daemon restart or reboot — the same semantics as tmux (state survives terminal-close/reattach, not a machine
reboot); the gateway's RESUME mechanism rebuilds state after a daemon restart. The one deliberate exception:
the daemon persists a small "last active voice channel" breadcrumb to disk, specifically so it can auto-rejoin
voice after a crash/restart (Section 6).

**Read state vs. scroll state.** These are two deliberately separate concepts, not one. Scroll/viewport
position stays exactly as designed above: in-memory, daemon-only, lost on restart. Read state — whether a
channel has unread messages — is a different, durable concern: a Postgres-backed `channel_read_states`
watermark table (per-user, per-channel) tracks it server-side and syncs via its own gateway dispatch event, so
opening any client on any machine (not just the one that was open when a message arrived) shows accurate
unread state. A channel is marked read automatically when the client's viewport reaches the latest message,
debounced so rapid scrolling doesn't spam writes to the watermark — no separate mark-as-read action is
needed. Without this split, a channel viewed on one daemon would show as unread everywhere else — including a
second daemon on a different machine, which the ephemeral scroll-state design was never meant to cover.

**CLI multiplexing.** A custom in-app TUI with its own pane/split engine, built on the Charm stack — Bubble
Tea (event loop/architecture) + Lip Gloss (styling) + Bubbles (viewport/text-input/list components) — rather
than shelling out to a real installed tmux, so behavior is identical cross-platform including Windows. Testing
uses Charm's own `teatest` package, built specifically to simulate key presses/messages and assert on rendered
output. Logging: the CLI, GUI, and daemon write to a log file, never stderr — Bubble Tea takes over the entire
terminal (alternate screen buffer) while the TUI is active, so writing to stderr during that time would
visibly corrupt the display. An `app logs tail` command views the log file. Log rotation uses
`natefinch/lumberjack` (size/age-based rotation with compression of old logs), wired in as an `io.Writer`
wrapper around the existing structured-logging output, reused by the daemon, CLI, and GUI alike.

**Cross-client pane splitting.** Tmux-like splitting is a requirement for all three clients, not CLI-only: the
native GUI and the (later) web app each also support splitting their window into multiple independent
panes/viewports. Each client implements its own split-pane engine appropriate to its rendering model (CLI: TUI
layout; GUI: native widget-based tiling; web: CSS grid/flex-based resizable panes) — one shared design
pattern, realized three separate times, not one shared implementation. Pane layout is independent per client,
never synced across clients or devices: opening the GUI does not restore the pane layout used in the CLI, and
vice versa. The GUI's layout lives in the shared local daemon/config-file state (same machine as the CLI, so
it reuses that model directly); the web app's layout is browser-local only (`localStorage`), since the web
client is a separate codebase with no access to the local daemon or config file. Pane content is fully
flexible in every client: any pane is a viewport that can be pointed at any channel/DM/server independently,
not a fixed structural layout.

**Keybinding scheme.** Emacs-style chorded (Ctrl/Meta combinations), not vim-modal, is the shipped default for
CLI navigation and pane management. Keybindings are stored in and fully overridable via the shared config
file's `[cli]` section, the same as every other customizable surface (theme, notification filters) — Emacs-
chorded is the default the CLI ships with, not a hardcoded, unchangeable scheme.

**Config file.** A single local TOML config file (e.g. `~/.config/<app>/config.toml`), with the CLI exposing
`app config get/set`-style subcommands as a scriptable interface to the same file. The native GUI reads and
writes the exact same file/schema (namespaced sections, e.g. `[cli]` / `[gui]` / `[shared]`), so
keybindings/theme/notification-filter data defined once apply everywhere they conceptually overlap. The config
file stays plain-text and directly hand-editable at all times — it must be possible to open it in any editor
(e.g. Emacs) and edit it by hand at any time; the daemon never becomes the sole writer. Concurrency safety is
achieved by every writer (CLI, GUI, daemon) using atomic writes (write to a temp file, then rename over the
original), which avoids corruption from an interrupted write, *plus* `gofrs/flock`-based file locking around
each read-modify-write cycle as an extra safety net — atomic writes alone prevent corruption but not a lost
update between two genuinely concurrent writers (e.g. a GUI theme change landing at the same instant as a
daemon-issued OAuth token refresh); the lock closes that gap outright rather than accepting it as an edge
case. The daemon watches the file via `fsnotify` and hot-reloads on external changes, including a hand-edit
made in a text editor while the daemon is running, so manual edits take effect without a restart. The TOML
library is `pelletier/go-toml` v2, used in its document-editing mode (edits the specific key in place) rather
than parsing into a struct and fully re-serializing — a naive re-serializing library would silently destroy a
user's hand-written comments/formatting the first time any process ran `app config set`.

**Config file split.** `config.toml` above covers only what a user should freely hand-edit — theme,
keybindings, notification filters, pane-layout preferences. A second, daemon-owned state file holds anything
daemon-written-only: plugin capability grants together with the pinned SHA-256 hash of each approved `.wasm`
file (Section 8.12), the "last active voice channel" breadcrumb, and the same-machine config-toggle setting
described next. The daemon exclusively writes this second file; it is not meant for hand-editing, and it is
never included in `app config export`/`import` (below) since it is machine-local by nature — exporting a
plugin's approved hash or a voice breadcrumb to a different daemon would be meaningless. The same
`gofrs/flock` + atomic-write discipline applies to both files.

**Same-machine CLI/GUI config toggle.** An app-settings toggle lets CLI and GUI on the *same* machine diverge
into separate `config.toml` files instead of always sharing the one file under the one shared daemon. Default
off (shared, as designed above). The toggle setting itself lives in the daemon-owned state file, not
`config.toml`, avoiding a chicken-and-egg problem over which copy of the toggle is authoritative once the
files are actually split. Flipping the toggle on copies the current shared `config.toml` to both CLI and GUI
files as the starting point, so no existing customization is lost; flipping it back off reconciles onto one
shared file via simple last-write-wins — a documented one-time step, not a real merge algorithm.

**Config export/import.** `app config export` / `app config import` subcommands produce/consume a single
portable file covering the hand-editable `config.toml` scope only (theme, keybindings, pane-layout
preferences) — never the daemon-owned state file. This is how a user carries preferences between separate
daemons/machines (e.g. a desktop GUI and a laptop's CLI, which are different daemons with no automatic sync
between them), and later, the web client's own local settings. Import merges key-by-key into the target's
existing config — touching only keys present in the imported file and preserving everything else already
customized on the target machine — matching how `app config set` already behaves, rather than wholesale
replacing the target's `config.toml`.

**Theming.** One shared theme spec (named roles: background/accent/danger/muted/etc.) is defined once in
config; the CLI maps it to terminal ANSI/24-bit color, and the GUI maps the same roles to native rendering.

**Account model.** One account per daemon: the daemon holds a single logged-in identity (no
multi-account/workspace switcher); switching accounts means logging out and back in.

**CLI structured output.** Every CLI command that prints data supports a `--json` flag emitting stable,
parseable output (matching `gh`/`kubectl`/`docker` conventions), defaulting to human-readable text. These JSON
schemas are formally versioned and documented in `contracts/` — a third source-of-truth artifact alongside the
existing `openapi.yaml` (REST) and `gateway-events.schema.json` (WS) — with the same rule already required for
the other two contracts: schema changes ship in the same commit as the code change that causes them.

**CLI image/attachment rendering.** Terminal image-rendering capability (Kitty graphics protocol / iTerm2
inline images / Sixel) is detected via the `BourgeoisBear/rasterm` library; images render inline when
supported and fall back to a filename/link otherwise. This is the hook point for the "disable image loading"
network-performance toggle (Milestone M55), which suppresses this rendering path specifically; that toggle
does not suppress custom-emoji rendering, since emoji are core to message meaning rather than a heavyweight
attachment preview.

**CLI markdown rendering.** A small custom renderer implements only the allow-listed subset (bold, italic,
code, links, mentions, custom-emoji shortcodes) — not Charm's `glamour` (a full markdown-to-ANSI renderer).
Restricting a general-purpose renderer down would be a larger trusted surface to audit than building only the
narrow thing actually needed, matching the security posture used for message content everywhere else on the
platform.

**CLI terminal-escape-sanitization.** A blanket sanitization function strips/escapes ASCII control characters
and ESC sequences from all untrusted text (usernames, message content, link-preview titles, plugin manifest
descriptions, webhook display names, and any other user- or third-party-controlled text) at the single point
where it meets terminal output. This risk is specific to the CLI — it does not exist for the GUI/web
renderers — because the CLI renders untrusted text directly into a terminal, where a malicious string
containing raw ANSI/terminal escape sequences could otherwise manipulate the user's terminal.

**Concurrency model.** The daemon's design deliberately leans on a handful of Go-idiomatic concurrency
patterns rather than incidental goroutine use, called out here as one family: a bounded per-connection writer
goroutine (fed by a fixed-capacity channel) for each attach client, so one stalled client can never block the
daemon's main event loop; a single dedicated writer goroutine, fed by a buffered channel, serializing all E2E
keystore SQLite writes (Section 8.17/Milestone M90) so the WS event loop never blocks on disk I/O and a burst
of concurrent incoming encrypted messages never produces a `database is locked` error; the daemon's main loop
itself as a `select`-based multiplexer over the gateway WebSocket, the local IPC listener, the voice-worker's
subprocess pipe, and `fsnotify` config-file events; and `context.Context`-based cancellation for the
per-invocation wall-clock timeout every plugin call gets (Section 8.12). These share one shape: isolate
anything that can block or run unboundedly behind a goroutine and a bounded channel, so the daemon's
core loop stays responsive regardless of what a slow client, a busy disk, or a misbehaving plugin is doing.

## 6. Voice architecture

Voice is real, working audio calling on every client in v1. Video/screen-share is deliberately deferred but
architected now so it requires no rework later.

**Media server.** A self-hosted, custom-built SFU on Pion (Go), not LiveKit, not a plain P2P mesh — stays in
the all-Go ecosystem and gives full control to scope it exactly to audio-now/video-later.

**Client scope.** Audio is universal (CLI, GUI, web); video/screen-share is GUI+web only, never CLI — a
terminal cannot meaningfully render video, so this is a permanent client-capability distinction.

**Voice-worker subprocess.** A separate subprocess, spawned on-demand by the daemon via `os/exec` when joining
a voice channel and torn down when leaving (not a persistent idle process), owns the entire audio media
session: capture/encode/send, receive/decode/play, plus noise suppression, echo cancellation, and automatic
gain control. It is shared by CLI and GUI — both send control commands (join/leave/mute/status) through the
daemon, which relays them to the worker. Daemon↔worker IPC uses the inherited stdin/stdout pipes of the child
process (free, requiring no socket/port allocation or stale-file cleanup; the pipe closing is itself an
immediate crash/exit signal), framed with the same 4-byte-length-prefix-plus-JSON scheme used for the
daemon↔CLI/GUI Unix socket (Section 5). The worker holds its own direct WebRTC connection to the SFU — actual
RTP audio data never flows through the daemon process, only control signaling does, which is what makes the
fault isolation real: a media-pipeline bug can only crash the worker, never messaging/presence/plugins, and
cannot be corrupted by unrelated daemon logic in the data path. Staying connected to voice survives closing
either attach client, since the worker keeps running independently once spawned. This is audio-only,
permanently — video is never routed through the daemon or its voice worker.

**Codecs and DSP.** Opus via cgo bindings (`hraban/opus`), RNNoise (cgo) for noise suppression, and
`libspeexdsp`'s echo-canceller and AGC modules (cgo) for echo cancellation and automatic gain control — the
three distinct DSP concerns the audio pipeline needs. This cgo usage is contained entirely to the voice-worker
binary (see Section 2); the daemon, CLI, and GUI stay pure Go and cross-compile cleanly regardless. The
pipeline runs in this exact, non-negotiable order: **Mic Capture → AEC → RNNoise → AGC → Opus Encode**.
Order matters here — running RNNoise before AEC would non-linearly distort the mic signal, breaking the
linear echo-correlation assumption AEC depends on to recognize and cancel the far-end signal, producing a
feedback loop for other callers instead of cancelling it.

**Adaptive bitrate / congestion control.** `pion/interceptor`'s REMB/TWCC support provides receiver-side
bandwidth-estimation feedback, which feeds back into `hraban/opus`'s runtime bitrate control so the encoder's
bitrate measurably drops and recovers in response to real network conditions rather than staying fixed
regardless of packet loss or bandwidth constraints. This is audio-only, reactive bitrate adaptation — no
simulcast, and no video-specific adaptation, both of which are deferred to the video/screen-share phase since
v1 has no video track to adapt. The Prometheus voice/SFU metrics (packet loss, bitrate, jitter) feed this
control loop directly; they are not merely a dashboard.

**Video/screen-share (deferred, seamed now).** Video and screen-share are owned directly by the GUI/web client
itself — a second, separate WebRTC connection to the SFU for the video track, opened by whichever client has
it, never piped through the daemon (screen/camera selection is inherently a foreground-interactive operation
the daemon has no UI to perform anyway). This means no daemon rework is needed when video ships — only the
GUI/web client gains a second media connection. The voice-join/identify payload carries a `supports_video:
bool` client-capability flag from day one (CLI always sends `false`; GUI/web send `true` once video exists),
so the SFU/permission UI can gate video controls correctly per client type without a protocol version bump
when video ships. The SFU's internal track/participant model is kept codec/track-kind-agnostic (not hardcoded
to "one audio track per participant") so adding a video track type — and screen-share as effectively a second
video track — is additive, not a redesign. This agnostic track model is necessary but not sufficient for
video, however (see Section 2): it covers whether the SFU can forward a track at all, not whether it forwards
it well under real bandwidth constraints. Simulcast/SVC layer-switching for participants on a poor connection
is real, separate engineering work, explicitly scoped as a concrete done-when item for Milestone M99 rather
than assumed to arrive for free.

**TURN (NAT traversal).** An embedded Go TURN server (`pion/turn`), bundled into the backend (server-side
infrastructure, not the client daemon), rather than requiring self-hosters to separately stand up `coturn`.
`pion/turn` already answers plain STUN binding requests too (TURN is a superset of STUN), so no separate
STUN-only server needs to be deployed alongside it.

**Voice deployment opt-out.** Voice is a deployment-time opt-out, not a mandatory instance requirement: TURN
and SFU voice need a reachable public IP and forwarded UDP port range, a real networking burden many home
self-hosters behind routine NAT/routers cannot easily satisfy. An Instance Admin can disable voice entirely via
config; the SFU/TURN never start, and voice+text channel pairs gracefully degrade to text-only. When disabled,
voice-related UI is hidden entirely in CLI/GUI — no voice option in the channel-type picker, no
disabled/grayed-out voice controls — rather than the pre-v1 "coming soon" disabled pattern, which would risk
confusing "not available on this instance" with "not implemented yet."

**Existing seams activated.** `PermConnectVoice`/`PermSpeakVoice`/`PermVideoVoice`/`PermMuteMembers`/
`PermDeafenMembers` permission bits, the `GUILD_VOICE`/`GUILD_STAGE_VOICE` channel types, and the
`voice_states` table are activated for the audio-related bits now; `PermVideoVoice` and stage-channel behavior
stay reserved-but-unused until video ships. Public matchmaking channels (specced as a voice-and-text pair)
ship with a real, working voice half.

**No call recording, ever.** A permanent non-goal, consistent with the project's privacy-forward framing (E2E
encryption, opt-in-only telemetry, self-hosted, no tracking) — nothing is captured server-side or by the SFU.
This is documented explicitly because the SFU is custom-built and could otherwise accidentally grow
stream-tapping capability without a deliberate decision behind it. Public-matchmaking voice abuse reports have
no evidence to review as a direct consequence — see Section 2.

**Active-speaker indication and self-defense controls.** Because public-matchmaking voice abuse leaves no
recorded evidence (above), regular users are the first line of defense against it, and the voice UI must make
that defense actually usable. Both CLI and GUI voice surfaces show a highly visible, real-time active-speaker
indicator — a highlight/ring around whoever is currently transmitting — and expose two separate actions, each
its own keybind/click rather than one combined action: a local-only mute (silences a participant for this
user alone, no effect on others) and a report action (files a report against that participant). A user can
mute without reporting, report without muting, or do both as two deliberate steps.

**Mic permission flow.** The CLI/GUI (whichever is foreground) triggers the OS permission prompt on first
voice use, then hands capture off to the voice-worker subprocess once granted — not the worker requesting its
own OS permission independently. This is unverified pending Milestone M25's spike (see Section 2).

**Voice input mode.** Voice-activity-detection is the default (works with the noise-suppression/AGC pipeline
already described, no extra plumbing needed). Push-to-talk is available as an option and must work as a true
OS-wide global hotkey via `golang.design/x/hotkey`, transmitting/muting correctly even when the CLI/GUI isn't
focused (e.g. alt-tabbed into a game). The daemon — not either attach client — owns global-hotkey registration
(registered once, when voice is active), since it is the single persistent process either attach client
shares; this avoids duplicate-registration/double-trigger conflicts if both CLI and GUI are attached
simultaneously. Whether a headless daemon process can actually register a global hotkey on each target OS
(including the macOS Input Monitoring entitlement) is verified as part of Milestone M25's spike.

**Auto-rejoin on daemon crash/restart.** If the daemon crashes or restarts while actively in a voice channel,
it automatically attempts to rejoin the same voice channel on startup, using the persisted "last active voice
channel" breadcrumb (Section 5) — the daemon respawns the worker and has it rejoin the last active channel,
minimizing disruption from a transient crash during an active call. This is also why the client auto-update
mechanism (Section 10, Milestone M24) must defer applying a downloaded update while the daemon is tracking an
active voice session, applying it only once the call ends — auto-update forcing a binary swap and daemon
restart mid-call would otherwise silently drop it for the duration of the reconnect, an unforced regression
auto-rejoin exists specifically to avoid. Since M24 ships before Phase E in milestone order, this guard
activates once Phase E exists, the same way Milestone M85's voice metrics are phrased.

## 7. v1 feature scope

All of the following ship as real v1 scope — none of it is deferred roadmap:

- Voice (audio) calling on every client (Section 6).
- Chat power features: Deep Work status, message tagging/grouping, in-channel whispers, regex notification
  filters, bandwidth/network toggles.
- Public matchmaking: ephemeral topic channels (voice-and-text pairs) plus a "recently met" list.
- A mutual friend-request/accept system.
- A personal block/mute system.
- Per-guild custom emoji.
- Incoming webhooks (channel-level integrations).
- Dev tools and extensibility: code block copy/fold, an integrated shell, local bot automation, CLI
  piping/local port forwarding, GitHub-aware and generic link previews, a client-side WASM plugin system.
- Self-hosting emphasis, including transactional email (SMTP) and built-in automatic HTTPS.
- P2P (WebRTC) large-file transfer.
- BYOK end-to-end encryption (opt-in, `DM` channel type only).
- The Instance Admin tier, platform-wide bans, the unified `reports`/moderation system, and instance-level
  registration gating (invite codes) — infrastructure that public matchmaking's abuse-reporting story and
  whisper break-glass moderation both depend on existing.
- An integrated whiteboard — GUI-only, for solo local quick-drafting, the lowest-priority item in the entire
  GUI milestone phase.

## 8. Feature-level design decisions

### 8.1 Deep Work

Deep Work is a real, server-known presence status, not an in-memory/no-schema client concept: presence is
persisted (Milestone M38) and read/written through the daemon. While Deep Work is active, the backend
withholds non-`@urgent` notifications/pushes; an `@urgent` mention bypasses the suppression and is delivered
normally.

**Offline `@urgent` fallback.** With no mobile client and no third-party push service, a user whose machine is
asleep or powered off would otherwise miss an `@urgent` mention entirely, undermining the reliability an
`@urgent` bypass is supposed to guarantee. An optional, per-user, opt-in email fallback closes this: if SMTP
is configured (Section 8.15's existing opt-out seam) and the account has no live gateway connection at the
moment an `@urgent` mention is dispatched, the backend sends an email instead. This is unconditional on Deep
Work specifically — the fallback fires for any offline `@urgent` mention, not only while Deep Work is
active — and is independent of the OS-desktop-notification path (Section 8.19), which fires precisely when a
client *is* running; the two conditions are mutually exclusive.

### 8.2 Public matchmaking channels

A genuinely new guild-less top-level public channel type (not a special auto-guild), shipped as a
voice-and-text pair. Instance-level toggle, defaulting ON — an Instance Admin can disable public matchmaking
entirely for a deployment that wants to stay fully private/invite-only, but it ships enabled by default since
it is advertised as a core v1 feature.

Moderation uses a fixed, platform-level ruleset (no custom roles, no ownership) — abuse is handled via
reporting and rate-limiting, enforced by the Instance Admin tier (Section 8.3). There is no "channel
moderator" concept for these channels by design: there is no owner to grant that role in the first place.

Anti-abuse: joining or creating a public matchmaking channel requires a minimum account-age/verification
threshold (verified email plus an account older than a configured threshold) on top of the existing
per-user/IP rate limiting already in place for REST/gateway — including that limiting's `/64`-subnet IPv6
grouping (Section 10's "Rate limiting" paragraph), since a griefer trivially controls an entire `/64` block
and this matchmaking gate is one direct consumer of that global rule. No CAPTCHA or third-party service is
used, to keep the self-hosted/privacy-first framing intact. The email-verification half of this gate is
unavailable on an instance that has not configured SMTP (Section 8.15) — the account-age threshold alone
still applies in that case.

The "recently met" list is server-side, with a short retention window (7–30 days), user-visible, and covered
by the existing account data export/deletion story.

Voice-side abuse in public matchmaking channels has no recorded evidence, by design (Section 2) — Instance
Admins can only act on corroborating multi-report patterns for the voice half of these channels, never on
recorded evidence.

Message retention for reporting: a public channel's history is kept for a short window (48 hours) after it
empties/unlists, queryable only by Instance Admins investigating a report, then permanently purged — without
this, the reporting/enforcement system would have nothing left to review by the time most reports are filed.
Whispers exchanged inside a public matchmaking channel follow this same 48-hour window, tied to the channel's
own lifecycle rather than a second, independently-tracked retention timer.

### 8.3 Instance Admin tier and platform-wide bans

A boolean/flag-based tier that supports multiple admins per instance (not a single-owner concept) — the
self-hoster by default, with no technical limit on granting it to others (e.g. a co-operator). It sits
outside any single guild's role hierarchy entirely; it is not resolved via `roles.Resolve`. See Section 9 for
its position in the full authority hierarchy.

An Instance Admin can: issue full-account `instance_bans` (with the last-admin safety rail described below);
review reports, both the guild-level and instance-level halves (Section 8.9); manage the license/entitlement
configuration; break-glass into a reported whisper (Section 8.4); and proactively intervene in any guild's
content without a filed report, subject to the mandatory-justification rule below.

An `instance_bans` table makes a platform ban stick, instead of an abusive account simply rejoining a fresh
public channel seconds later. Scope: an instance ban is a full account suspension — it blocks everything on
the instance (public matchmaking, every guild the account belongs to, DMs, all of it), not just public-channel
access. Existing guild-level bans stay separate as the lesser, guild-scoped tool for guild-specific issues. An
instance ban supports an optional expiry (`expires_at`, nullable), so Instance Admins can issue either a
permanent ban or a timed suspension (e.g. a 24-hour cooldown for a first offense).

A safety rail blocks demoting or removing the last remaining Instance Admin (by themselves or another admin),
so an instance cannot accidentally end up with zero admins from a routine action.

**Enforcement mechanism.** Issuing a ban force-closes the account's live gateway connection(s) and revokes its
refresh token(s) and any scoped API tokens (all DB-backed and instantly revocable) — the real-time experience
(messaging, presence, voice) stops immediately. An already-issued short-lived access token (~15 minute TTL) is
not individually invalidated; it simply cannot be renewed, and expires naturally within its existing short
window. "Immediate" applies to the real-time session and to renewal, with a short, already-accepted-elsewhere
tail on any already-issued access token — this keeps the stateless-JWT performance design intact rather than
adding a revocation check to every request. This revoke-all-sessions mechanism is a general-purpose primitive
(Milestone M11), also exposed as a normal user-facing "log out all other devices/sessions" account-security
feature and invoked by self-service account deletion, reusing the same underlying revocation path in every
case. Revoking a device's session also revokes its E2E device-link trust, connecting the two previously
separate systems so revoking a stolen device's session also cuts that device off from being trusted in
ongoing encrypted conversations.

**Audit logging.** Every Instance Admin action — bans, whisper break-glass access, license/entitlement
changes, admin-tier grants — is recorded in a dedicated `instance_audit_log`, separate from the per-guild
`audit_log_entries` table, giving the platform's most powerful tier the same accountability rigor required of
guild mutations.

**Proactive intervention.** An Instance Admin can proactively intervene in any guild's content without a
filed report (e.g. for legal/compliance takedown requests), not just react to reports or issue account-wide
bans. Every such report-less action requires a mandatory logged justification (e.g. "legal takedown ref #X,"
"proactive review — reason") in `instance_audit_log`, in a field that clearly distinguishes it from
report-triggered entries.

**Lockout recovery.** A server-side recovery CLI command (`app instance grant-admin <email>`), runnable only
with direct server/filesystem access (the same trust level as direct DB access), lets a self-hoster regrant
Instance Admin access if they are ever locked out.

**First-run setup.** `app instance init` is a full first-run setup wizard, not just admin bootstrap: it walks
through the Postgres connection, storage backend choice (local disk vs. S3), voice/TURN networking
configuration (including the voice opt-out), transactional-email/SMTP configuration (including the email
opt-out, Section 8.15), the public-matchmaking toggle, and registration mode (below), finishing with
first-admin-account creation. It offers a quick-start default path (`app instance init`) — public matchmaking
on, registration gated with an auto-generated instance invite code, ACME on if a domain is given, voice on if
the network looks reachable, SMTP prompt skipped by default — that gets someone running in under a minute, and
a full walkthrough (`app instance init --full`) that prompts for every option explicitly, including SMTP,
for an operator who wants full control. An operator who skipped SMTP at quick-start can configure it later via
`app config set` or by re-running `--full`.

**Registration gating.** Registration is gated by default: a distinct instance-level invite code (separate
from per-guild invite codes, which grant membership in a specific guild after an account already exists) is
required to create an account at all. An operator who wants open signup can disable the gate. The very first
Instance Admin is created via the setup wizard/CLI command described above, run once when standing up a fresh
instance, before normal registration is even open.

**License/entitlement seam.** This tier is the natural owner of the license/entitlement seam (Section 10).
License validation is an offline, cryptographically-signed license file, validated locally by the backend —
no phone-home to any server the developer operates.

### 8.4 Whispers

Fully private like a DM: not guild-audit-logged, not moderator-visible by default. One narrow break-glass
exception: content becomes visible to the Instance Admin tier only when attached to a specific, already-filed
report naming that whisper, and that access is itself audit-logged — never a general "admin can browse
whispers" capability. Whispers are explicitly excluded from the E2E-encryption opt-in scope (Section 8.16),
specifically so this break-glass moderation path always has plaintext to fall back on for whisper-vector abuse
reports — this is a deliberate, real user-facing inconsistency ("DMs can be E2E, whispers can't") that is
documented plainly, not hidden.

### 8.5 Friends

A real mutual friend-request/accept system: a `friend_requests`/`friendships` table pair, either party can
send a request, the other must accept before they are "friends." Friend status does not gate DMing — any
account can already DM any other account regardless of friend or guild-sharing status; friend status is
purely an organizational/contacts-list label (e.g. a "friends" section in the UI), not an access-control
mechanism. This means DMs stay open to unsolicited contact from strangers, including from public matchmaking —
a deliberate choice rather than adding DM-gating friction. Section 8.6 (Blocks) is the mechanism a user has to
protect themselves from unwanted contact.

### 8.6 Blocks

A full, server-enforced, unilateral (not mutual, unlike friends) restriction one account places on another. A
new `blocks` table (`blocker_id`, `blocked_id`, `created_at`).

**Enforcement scope.** Blocking prevents DM and whisper delivery, and hides presence visibility, in both
directions. It also reaches into shared guild channels: gateway dispatch itself skips a blocked author's
guild-channel messages, presence, and `@mention` notifications for that specific recipient's connection — the
gateway never sends a blocked author's guild-channel content to the blocker's connection at all. This is
enforced server-side at fan-out time, not merely as a client-side rendering filter, both because purely
client-side filtering would still ship the blocked author's content over the wire to the blocker (wasted
bandwidth) and because a modified or hostile client build could otherwise simply ignore a client-side filter
and display the content anyway. The fan-out check uses a cached per-connection block-set, not a
query-per-message-per-recipient, so it does not regress message fan-out latency — a block or unblock action
updates the affected connection's cached set immediately, not on a delay. Blocking never touches guild
membership, permissions, or authority (that remains the job of guild kicks/bans, Section 9) — the blocked
account is still a fully normal guild member from everyone else's point of view; only what gets dispatched to
the blocker's own connection changes.

**Failure mode.** A blocked account's DM, whisper, or guild-channel-mention attempt never produces an
explicit "you are blocked" signal — it simply does not arrive, or does not generate a notification. This
reduces the harassment feedback loop: a blocker's block is never confirmed to a harasser.

**Side effects.** Blocking auto-removes any existing friendship and cancels/rejects pending friend requests
between the two accounts, and removes the blocked account from the recently-met list, preventing future
recently-met entries between the pair while the block stands. Scoped API tokens and local bot automation must
also respect blocks — they cannot be used to route around one.

**Non-interaction with E2E.** No E2E-specific action is triggered by a block — it only gates future
delivery/visibility; existing message history (server-side or locally-decrypted) is untouched.

**Data export.** A user's account data export includes accounts they have blocked (their own action) but
excludes who has blocked them, protecting the blocker from retaliation — the same reasoning applied to the
reports data-export asymmetry (Section 8.9).

### 8.7 Regex notification filters

Evaluated server-side (consistent across clients/daemon, and works for offline push), using Go's standard
library `regexp` package (the RE2 engine). RE2 guarantees linear-time matching with no catastrophic-
backtracking vulnerability class, unlike PCRE-style backtracking engines — the primary ReDoS mitigation is the
engine choice itself. A simple pattern-length cap is kept as cheap extra defense-in-depth. No third-party
regex library is used, since some implement PCRE-style backtracking for extra features, which would silently
reintroduce the exact vulnerability class being guarded against.

### 8.8 Message tagging

Guild-wide in scope: a tag belongs to a guild and can be applied to any message in any of that guild's
channels, not scoped to a single channel — matching an "advanced pinned messages" framing (e.g. a "decisions"
tag spanning multiple channels). Private (solo) tags need no special permission; creating a shared tag
requires `PermManageMessages` (no new permission bit needed).

### 8.9 Reports

A real `reports` table: reporter, target type/id (message, whisper, channel, or user), a reason category plus
free-text detail, and a status workflow (open → under review → resolved/dismissed). Any user can file a report
against any message, whisper, channel, or user.

This is one unified system serving every scope, with no content type falling through a gap: a report filed
inside a normal guild routes to that guild's moderators (permission-gated via `PermManageMessages`); a report
on public-matchmaking or whisper content routes to Instance Admins (there is no guild owner to escalate to);
and a report on a plain DM or Group DM (which is likewise not guild-scoped and has no owner to escalate to)
also routes to Instance Admins.

Report filing is rate-limited per user (reusing the existing REST/gateway rate-limiting infrastructure) to
deter report-spam/false-reporting. The triage view surfaces a reporter's own report history (count filed,
count resolved-as-valid) so whoever reviews a report can weigh the reporter's credibility.

**Data export asymmetry.** A user's own account data export includes reports they filed (their own authored
content) but excludes reports filed against them by others — this protects reporters from retaliation, since
seeing "who reported you and what they said" would have a real chilling effect on people's willingness to
report abuse at all.

**No push notification on filing.** Reports are queue-only — filing one does not trigger an OS notification or
a Deep Work bypass for reviewers, who check the triage queue on their own schedule rather than being
interrupted in real time.

### 8.10 Per-guild custom emoji

Real, free v1 scope on every instance, self-hosted or flagship. A `guild_emojis` table (`guild_id`, `name`, an
image reference via the existing attachment-storage interface, uploader, `created_at`) and a new permission
bit, `PermManageEmojis` (a regular, OR'd-across-roles bit, the same model as every other permission — no
special-cased authority tier needed).

**Upload validation.** Custom emoji images are static-image-only for v1 (no animated GIF/WebP support — the
CLI cannot render animation regardless of what is uploaded; animated support is a clean later addition on the
same schema). Uploads are validated beyond the generic attachment content-type sniffing already used for
regular attachments: a resolution/dimension cap, a format allow-list, and a decompression-bomb guard. This
stricter validation is necessary because, unlike a regular attachment a user opens deliberately, emoji render
automatically and repeatedly across every client and every viewer, so a malicious upload is a real
repeated-decode denial-of-service vector against every viewer, not just the uploader's own session. An
oversized, malformed, or animated-format upload is rejected with a clear error, never silently accepted.

**Rendering.** Each client's message renderer (the CLI's allow-listed markdown subset, the GUI, and the web
client) resolves `:emoji_name:`-style shortcodes to the stored image — a small, allow-listed addition to each
renderer, not a new rendering trust boundary.

**Relationship to the paid flagship perk.** The flagship instance's paid "custom emoji anywhere" perk (part of
the per-user `user_entitlements` seam, Section 10) specifically means using your emoji in guilds you do not own
or have not uploaded them to — a real upsell layered on top of a complete, free, per-guild base feature, not a
paywall on the base feature itself.

### 8.11 Incoming webhooks

Per-channel webhook integrations, the standard "let a third-party service post into a channel" pattern (CI
results, GitHub pushes, monitoring alerts). A `webhooks` table (channel-scoped secret, creator, default
name/avatar) backs a dedicated, unauthenticated-except-by-secret REST endpoint,
`POST /webhooks/{id}/{token}`, matching the well-understood Discord-webhook shape. A new permission bit,
`PermManageWebhooks`, gates creation and deletion; creating or deleting a webhook is a guild mutation and
produces an audit-log entry per the standard rule.

**Per-message overrides.** A webhook's POST payload can override its display name and avatar per message,
matching the common CI/integration-tool expectation that one webhook URL can post as differently-labeled
sources per event type.

**Content handling.** Messages sent via a webhook reuse the same `messages.type` "sent via automation"
reserved value already used for local bot automation, so they are visually tagged the same way — one
mechanism serving both integration paths, not two. Webhook-sourced content passes through the exact same
allow-listed markdown rendering, CLI terminal-escape sanitization, and SSRF-protected link-preview fetcher as
any other message — arriving over the webhook endpoint instead of a normal authenticated session grants it no
elevated trust.

**Security.** Because this is a new secret-bearing, unauthenticated-except-by-token endpoint, the token is
high-entropy (the same generation path as scoped API tokens), stored hashed like refresh tokens, and is
regenerable/revocable without deleting and recreating the whole webhook. The post endpoint carries its own
per-webhook rate limit, independent of the creating user's normal REST rate limit, so a leaked-but-not-yet-
revoked token cannot flood the channel.

### 8.12 Client-side plugins

Sandboxed via WASM using `wazero` (pure Go, no cgo, so the daemon/CLI/GUI keep cross-compiling cleanly), with
an explicit host-function capability API — no raw filesystem/network/syscall access unless explicitly
granted. Plugins run inside the daemon (one host, available to both CLI and GUI) rather than being duplicated
per attach-client.

**Plugins are headless by design.** The host-function API surface is slash-commands, text-parsing, and
data/message reads only — there is no UI-injection capability and no IPC bridge for painting native CLI/GUI
elements, and none is planned. This is a deliberate non-goal, not a gap: the CLI (Bubble Tea) and GUI (Gio)
use entirely different rendering paradigms, and a daemon-hosted plugin has no natural way to paint into either
one safely. A plugin that wants to affect what the user sees does so only through the data/text it returns
from an already-capability-gated host function, rendered by the client the same as any other content.

**Distribution.** Local files only in v1 — a user drops a `.wasm` file in a plugins folder the daemon reads;
no registry/marketplace/discovery mechanism now. The recommended plugin authoring toolchain is TinyGo (not
mainline Go's own WASM/WASI target), since TinyGo produces meaningfully smaller, more embedded-appropriate
WASM binaries; `wazero` runs WASM from any source language equally, so this is a recommendation for plugin
authors, not a technical restriction.

**Capability manifest.** Each plugin ships a companion `manifest.toml` (matching the main config file's TOML
convention) declaring the capabilities it needs (e.g. "reads message text," "network access to
api.example.com"). The daemon shows this to the user and requires explicit approval on first load before the
plugin runs — the same trust model as browser-extension/mobile-app permission prompts. Revoking a grant is
simply removing it. Approval also pins a SHA-256 hash of the `.wasm` file at that exact moment, stored
alongside the capability grant in the daemon-owned state file (Section 5); every subsequent load re-verifies
the file against that pinned hash, halting and re-prompting for approval on any mismatch. Without this, a
`.wasm` file swapped on disk after approval (by a malicious script or a compromised secondary app) would
silently run under the stale approval's capabilities.

**Resource limits.** Each plugin instance gets enforced CPU (instruction-count/timeout) and memory quotas, so a
runaway or malicious plugin cannot spin a CPU core or exhaust memory and degrade messaging/presence for that
user — WASM sandboxing already contains crashes/security escapes, so only resource exhaustion needs a
dedicated answer, the same underlying principle as isolating voice into its own subprocess, applied here as
resource quotas instead of a separate process. Each plugin invocation also gets a wall-clock timeout (e.g. a
few hundred milliseconds for a hook call), separate from the CPU/memory quotas — a plugin that hangs (a slow
network call within its granted capability, or a sleep-heavy loop that never trips the CPU quota) would
otherwise block message rendering indefinitely without ever exceeding a resource limit. `wazero`'s
instruction-metering (how the CPU quota above is enforced) works by injecting context checks and counters into
compiled WASM functions, which imposes a baseline host-CPU cost on every plugin call; Phase K benchmarks this
overhead against real plugin workloads (e.g. a chat-filter running on every incoming message), and if metering
costs more than the work it's metering, memory quotas plus the wall-clock timeout above become the primary
safety mechanism instead of strict instruction counting.

### 8.13 Local bot automation and CLI piping

Uses scoped API tokens. Messages sent via automation are visually tagged in the UI, reusing the existing
`messages.type` reserved-values seam — the same mechanism reused by incoming webhooks (Section 8.11).

### 8.14 Integrated shell

Spawns the user's actual shell — the same trust boundary as a real terminal, with no extra sandboxing layer.

### 8.15 Link previews and transactional email

**Link previews.** Generic, server-side-fetched OpenGraph previews (title/description/image, sanitized before
rendering) for any pasted URL, with GitHub URLs additionally getting a richer repo/PR/issue-aware preview.
GitHub-specific previews use the real GitHub REST API via `google/go-github` (with a configured API token, to
avoid the low unauthenticated rate limit) rather than scraping GitHub's page HTML — more reliable, and exposes
data (like PR merge status) not necessarily present in OpenGraph tags at all. All other URLs use the generic
OpenGraph path, parsed with `PuerkitoBio/goquery` (a jQuery-like API over `golang.org/x/net/html`) for
extracting `<meta property="og:...">` tags.

**SSRF protection.** Fetching arbitrary user-pasted URLs server-side is a classic SSRF vector (a pasted URL
could point at the server's own internal network, localhost, or a cloud metadata endpoint like
`169.254.169.254`). The fetcher's HTTP client uses a custom `DialContext` that resolves the hostname and
rejects the connection if the resolved IP falls in a private/loopback/link-local range — checked at actual
connect time, not by string-matching the URL, since DNS rebinding can make a public-looking hostname resolve
to a private IP between the check and the connection. This protection is a standing requirement for any
future feature that fetches a user-supplied URL server-side, not just this one. The fetcher also enforces a
strict request timeout and a response-size cap (`io.LimitReader`), preventing a malicious or
slow/uncooperative server from tying up a fetch worker indefinitely or flooding memory with an oversized
response.

**Transactional email (SMTP).** The platform sends password-reset and email-verification messages via a
generic SMTP relay configured by the Instance Admin (host, port, credentials, from-address) through
`app instance init` — no managed third-party email API, consistent with every other infra choice in this plan
(Postgres-only, S3-compatible-not-AWS-specific, offline license validation). Email is itself a deployment-time
opt-out, the same pattern as voice and ACME (below): if SMTP is not configured, password-reset-via-email and
the email-verification half of the public-matchmaking anti-abuse gate (Section 8.2) are simply unavailable —
the account-age threshold alone still applies, and the instance still runs. `app instance init` quick-start
skips the SMTP prompt by default; `--full` prompts for it explicitly; an operator who skipped it can configure
SMTP later via `app config set` or by re-running `--full`.

The email library is `wneessen/go-mail` (modern, pure-Go, no dependencies, good STARTTLS/auth support) —
`net/smtp` is too bare, and a full templating engine is unnecessary for the one or two plain emails needed;
Go's standard `text/template`/`html/template` is enough.

**Password reset.** A `password_reset_tokens` table (hashed, single-use, expiring, per the platform's rule
that reset tokens are never stored in plaintext). The reset-request endpoint responds identically whether or
not the submitted email exists, to avoid an account-enumeration leak, and reuses existing REST rate-limiting
to prevent inbox-bombing a target. Email sending is asynchronous/backgrounded, never synchronous in the
request path — a slow or unresponsive SMTP relay must never stall the HTTP response for a password-reset or
verification request.

### 8.16 Self-hosting, attachment storage, and P2P file transfer

**Self-hosting.** Stays Postgres-only (no SQLite) — elevated self-hosting quality means packaging, one-command
setup, and documentation quality, not a database-engine change.

**Schema migrations.** The backend auto-runs `golang-migrate` against the configured Postgres instance on
startup, guarded by a Postgres advisory lock so two accidentally-concurrent processes (e.g. a self-hoster
briefly running two instances of the binary) never race on the same migration. Startup **blocks** on this —
`/healthz` stays unavailable until the migration and the rest of startup complete — giving the self-hosted
single-process path the same "never serves against a not-yet-migrated schema" guarantee the flagship's Helm
`pre-upgrade` Job hook (Section 11) already gives that deployment. This is the same `golang-migrate` tooling
in both deployment shapes; only the trigger differs — auto-run-on-startup here, a dedicated Helm Job there —
for the same reason the built-in `certmagic` ACME path is self-hosted-only and `cert-manager` handles TLS on
Kubernetes instead (below/Section 11): every replica independently trying to do the same one-shot operation on
its own would race.

**Automatic HTTPS.** The backend includes ACME/Let's Encrypt integration via `caddyserver/certmagic` (point it
at a domain, it provisions/renews its own certificate, Caddy-style), rather than requiring self-hosters to run
a separate reverse proxy just for TLS termination — using `certmagic` rather than the lower-level `autocert`
package for its more robust renewal/retry/storage-backend handling. A self-hoster who wants their own reverse
proxy in front can disable this and terminate TLS themselves. ACME is itself a deployment-time opt-out,
mirroring the voice opt-out pattern exactly: a self-hoster running purely on a LAN/Tailscale/local network with
no public domain can disable ACME and serve plain HTTP (or a self-signed cert) instead.

**Backup/restore.** No dedicated backup tooling for self-hosted instances: `pg_dump`/`pg_restore` plus copying
the attachment storage directory, documented explicitly noting that E2E key material lives client-side, not
server-side, so a server backup alone does not preserve access to encrypted conversations — that is the
device's own durable keystore (Section 8.16.1).

**Attachment storage.** A pluggable interface (local disk default, S3-compatible for production): self-hosted
instances use local disk by default (zero added complexity), while the flagship instance uses S3-compatible
storage for real scale/redundancy. The S3-compatible client is `minio-go` — the reference client for MinIO
(the self-hosted S3-compatible store self-hosters are far more likely to actually run than real AWS), fully
compatible with real AWS S3 too, and lighter than pulling in the full `aws-sdk-go-v2` for one bucket
read/write path.

**P2P (WebRTC) file transfer.** Explicit opt-in per transfer, not automatic by size threshold — both sender
and recipient must consent each time, given the IP-address exposure inherent to a direct P2P connection. The
default upload path stays server-relayed storage under existing size limits. The initiating attach client
(CLI or GUI) owns the transfer's WebRTC negotiation directly — the daemon is not involved, the same rule
applied to video ("the daemon is only the always-on audio session; every other, one-off/interactive media use
is client-owned"). Consent is enforced as a real three-way handshake, not just a UI convention: the initiator
sends a lightweight, server-relayed "Intent to Transfer" payload first; only after the recipient explicitly
accepts do both clients initialize `RTCPeerConnection` and begin exchanging SDP offers/ICE candidates. Without
this ordering, the initiator's local/public IP — carried in the ICE candidates bundled into the SDP offer —
would already have reached the recipient's machine before they had consented to anything, defeating the point
of opt-in.

### 8.17 BYOK end-to-end encryption

Opt-in, restricted to the `DM` channel type only — actual 1:1 direct messages, never `GROUP_DM`, and never any
guild channel regardless of member count. A `DM` is a fixed, non-growable 1:1 relationship in the schema
(adding a member turns it into a different channel type, `GROUP_DM`, which is never eligible for E2E), so
there is no "what happens when a member is added" transition to design at all — E2E is simply a property of
the `DM` channel type, full stop. Whispers are explicitly excluded from this scope (Section 8.4), so the
Instance Admin break-glass path always has plaintext to fall back on.

The pairwise-vs-group scaling limitation is why `GROUP_DM`/guild channels are excluded: the Signal-style
double ratchet is inherently a pairwise protocol and does not scale to multi-member groups (it would need
either N² pairwise sessions or a genuinely separate group-key scheme like Signal's Sender Keys). Rather than
add a third independent custom crypto protocol on top of the already-flagged ratchet and device-linking risks
(below), E2E is simply not offered for anything but 1:1 DMs.

E2E is text-only for v1 — voice audio relies on standard WebRTC transport encryption (DTLS-SRTP) alone, which
protects against network eavesdroppers but not against the server/SFU operator, unlike E2E text; true E2E
voice is a distinct, later effort, out of scope here.

**Cryptographic base.** A Signal-style double ratchet (forward secrecy, per-message keys, device
verification/safety numbers) is provided by integrating `go.mau.fi/libsignal` — a mature, pure-Go port of the
actual Signal protocol, maintained by the mautrix project and used in production by their bridges. This
satisfies the project's pure-Go, cgo-free constraint while using a battle-tested implementation rather than a
from-scratch protocol built on raw primitives. **Before this library is integrated in code, its license must
be read and confirmed compatible with the project's restrictive custom license** (Section 3) — if the library
turned out to be copyleft (GPL/AGPL-family), embedding it could force the same terms onto the whole binary,
directly undermining the licensing model; this check is a blocking prerequisite of Milestone M89, not an
assumption.

**Device linking.** A device-linking flow (Signal-style "link new device"): the primary device authorizes a
new device (CLI on one machine, GUI on another, under the same account) to inherit access to ongoing encrypted
conversations, instead of each device being a fully separate identity requiring per-conversation
re-verification. This protocol has no off-the-shelf equivalent and is fully custom — it is a second, real
piece of custom crypto protocol, on top of the library-integration risk above, and both are in scope for the
external cryptographic security review below. For a CLI context specifically, safety-number/device-link
verification is text/code-based (no camera/QR needed): comparing a short code or word-string via another
channel, the same underlying concept as Signal's safety numbers, terminal-appropriate. No history-transfer
mechanism exists for the newly linked device: it sees only messages sent after linking, matching the
permanent-loss framing already used below for a lost device — this is a deliberate limitation (Section 2),
adding zero new custom-crypto surface to a device-linking protocol already flagged as one of this feature's
two compounding risks.

**External cryptographic security review.** A real external audit of the `go.mau.fi/libsignal` integration
(correct use of the library, key-material handling around it) and the fully-custom device-linking protocol is
a hard release gate, not optional polish: **E2E encryption must be technically disabled for any account beyond
the developer's own test accounts — enforced by a build/instance-level flag, not a documentation policy —
until this review is complete and passes.** Property-based/fuzz testing (Go's built-in `go test -fuzz`)
targets the integration/wrapper code and the device-linking protocol's message handling (malformed,
out-of-order, replayed inputs) as a concrete, cheap, complementary layer alongside the external audit, not a
replacement for it.

**No key-loss backup, permanent, by design.** Losing a device (or reinstalling without a working device-link)
means permanent, unrecoverable loss of that device's encrypted history, matching Signal's actual model — keys
never leave the device, and no server-side or passphrase-backup escrow mechanism exists.

**Key material storage.** Unlike the rest of the daemon's in-memory-only state, long-term identity keys and
ratchet/session state live in a durable, locally-encrypted keystore (surviving daemon restarts — losing
ratchet state would break decryption of past messages), separate from the ephemeral scrollback/pane state.
Backed by `modernc.org/sqlite` (a pure-Go, transpiled SQLite implementation, no cgo needed), encrypted with a
master key stored in the OS's native credential store via `zalando/go-keyring` (the same library the daemon
uses for its own auth-token storage — Section 4's credential-ownership rule applies equally here: the daemon
holds this keystore exclusively, no attach client holds a copy). SQLite is single-writer; a burst of
concurrent incoming encrypted messages across different channels would otherwise contend on the same file and
risk a `database is locked` error, or worse, block the daemon's WS event loop on disk I/O. The daemon avoids
this by routing every keystore write (ratchet advances, new sessions) through one dedicated writer goroutine
fed by a buffered channel (Section 5's "Concurrency model") — the network layer only ever enqueues a write and
moves on, never blocking on it directly. Client-side search over this keystore is a mandatory requirement, not
optional: `modernc.org/sqlite` FTS5 indexes the decrypted local message store, encrypted at rest via the same
keystore master key, since E2E-encrypted DMs are otherwise excluded from server-side search entirely (below)
and a user should never lose the ability to search their own conversations just because they're encrypted.

**Device revocation.** Revoking a device's session (the general-purpose revoke-all-sessions primitive,
Section 8.3) also revokes its E2E device-link trust, so a revoked device is cut off from being trusted in
ongoing encrypted conversations, not just its normal API session.

**Feature tradeoffs.** Encrypted conversations lose server-side search, moderation-content-visibility,
audit-diffing, and link-preview generation for their content; everything else on the platform keeps full
server-side functionality. For any server-side capability that depends on reading message content — search,
moderation visibility, audit-diffing, edit-history, link-preview generation, account export — an
E2E-encrypted DM is simply excluded from that capability server-side, full stop; the daemon (see below —
it, not the CLI/GUI, holds the decryption keys) may independently implement a local-only equivalent for its
own view — its FTS5 search index above being the mandatory example — but that is never a server capability.

**Key boundary: the daemon holds the keys.** The daemon owns the E2E keystore and ratchet state end to end
and performs all decryption itself, consistent with "Key material storage" above (surviving daemon restarts
implies the daemon, not an attach client, is what's persisting session state) and with Section 4's
daemon-exclusive credential-ownership rule. CLI and GUI receive plaintext over the already-trusted local IPC
socket, exactly the same as every other DISPATCH event — they never independently hold key material or
perform decryption themselves, which would otherwise mean passing raw ciphertext over local IPC and
duplicating ratchet-session handling per client type for no benefit, since the socket is already trusted.

**Account data export.** The server-side export endpoint covers everything it can actually see. For
E2E-encrypted conversations, the daemon performs the local decrypt-and-export step itself (not the CLI/GUI
independently, per the key boundary above) and produces a **standalone `local_e2e_export.zip`**, presented to
the user alongside the server-side account export — never merged into it. A seamless merge would mean
downloading a potentially multi-gigabyte server archive, unpacking it fully to disk, injecting decrypted local
content, and re-zipping, a real memory/CPU/disk bottleneck especially for the CLI; producing two separate
files avoids that mechanical problem entirely rather than trying to solve it.

### 8.18 Message, tag, and channel search

Postgres-native full-text search (`tsvector` plus a GIN index, `pg_trgm` for fuzzy matching) — stays inside
the one Postgres instance self-hosters already run, no new service. This is a deliberate v1-scale choice: if
the project gains real traction later, swapping in a dedicated search engine (Meilisearch/Typesense) for
better relevance ranking is a reasonable future upgrade, not something to design for now.

Updating `tsvector` synchronously as a generated column on every message insert is the deliberate v1 choice
for self-hosted scale, where GIN-index maintenance cost is negligible against realistic message volume. An
async-queue-decoupled indexing path (batching `tsvector` updates off the insert transaction's critical path)
is a documented upgrade path specifically for the flagship Kubernetes deployment if/when message fan-out
latency under real load justifies it — the same "documented future upgrade, not built now" treatment as the
Meilisearch/Typesense swap-out above, not v1 scope for either deployment shape.

### 8.19 OS-level desktop notifications

The daemon/GUI fire real native OS notifications (toast/notification-center, via `gen2brain/beeep`) for
`@urgent`-during-Deep-Work and regular mentions — required for the Deep Work override to actually mean
something, not just an in-app badge.

## 9. Guild/channel authority hierarchy

Six layers, top to bottom, plus several parallel concepts that deliberately sit outside the hierarchy rather
than inside it:

1. **Instance Admin** — sits outside any guild's role hierarchy entirely; not resolved via `roles.Resolve`.
   Can issue full-account `instance_bans` (with the last-admin safety rail), review reports (both scopes),
   manage the license/entitlement config, break-glass into a reported whisper, and proactively intervene in
   any guild's content without a filed report (mandatory logged justification required). Multiple admins per
   instance are supported.
2. **Guild Owner** — one per guild, bypasses all permission checks within that guild (`AllPermissions`
   immediately).
3. **A guild role with `PermAdministrator`** — the same short-circuit, scoped to that one guild.
4. **Regular guild roles** — permission bits OR'd across all roles a member holds; role `position` governs
   who can manage whom (never a role positioned above your own highest).
5. **Permission overwrites** — `@everyone` → role overwrites → per-member overwrite, in that order, most
   specific wins.
6. **"Guild moderator" is not a separate concept** — it is any role holding `PermManageMessages` (or similar),
   which is also who guild-level reports route to.

**Parallel concepts that deliberately sit outside this hierarchy:**

- **Public matchmaking channels** — no guild, no owner, no custom roles; a fixed platform-wide ruleset applies
  uniformly, and the only enforcement lever is the Instance Admin tier via reports (there is no in-between
  "channel moderator," by design — there is no owner to grant that role in the first place).
- **Whispers** — a message-visibility restriction within a channel (readable only by selected recipients), a
  layer below channel-level permissions, not a new authority tier.
- **Friends** — a personal, mutual, non-hierarchical label between two accounts; grants no permission (DMing
  is already open to anyone regardless of friend status).
- **Blocks** — a personal, unilateral (not mutual, unlike friends) restriction between two accounts. It is a
  gateway-dispatch/notification-routing filter that reaches into guild channels without ever touching guild
  membership, permissions, or authority — enforcement is server-side at fan-out time (Section 8.6), and the
  blocked account remains, from every guild-authority perspective, a fully normal member. Block is the one
  concept in this list that touches guild-channel *content delivery* while deliberately staying outside guild
  *authority*.
- **E2E-encrypted DMs** — a privacy layer, not an authority layer; does not interact with the guild hierarchy
  at all, since DMs were never guild-scoped to begin with.
- **Guild-to-guild relationships** — none exist. Guilds are pure independent peers on an instance, exactly like
  real Discord: an account can belong to many unrelated guilds, and there is no parent/child or inheritance
  relationship between them. The only things that ever span guilds are an account itself, its friends list,
  its block list, and (if the account is an Instance Admin) the instance-wide tier above all of them.
- **Instances themselves** are also pure independent peers — the flagship instance and every self-hosted
  instance have zero relationship to each other, consistent with the no-federation stance (Section 10), so
  there is no cross-instance hierarchy to design either.

## 10. Business posture, platform scope boundaries, and operations

**Commercial model.** There is no shared multi-tenant architecture at all. The actual model is two independent
deployments of the exact same single-instance codebase, with no relationship between them:

- **A flagship global instance**, operated by the developer/company, free to use, publicly
  open-registration (the operator sets the registration-gating toggle described in Section 8.3 to "open" for
  this deployment specifically — no new mechanism needed, just a different configuration choice on the same
  seam). It is monetized via optional per-user subscription features (Nitro-style: custom emoji usable across
  guilds you don't own — see Section 8.10 — larger upload limits, profile flair/supporter badge, etc.),
  detailed feature design deferred; the seam is a `user_entitlements` table/structured blob, per-user (not
  per-instance), inert and unused by any v1 code path, mirroring the per-instance entitlement blob below. The
  flagship instance runs on Kubernetes/Helm from the start (Section 11) — self-hosted instances stay on the
  simple single-binary story throughout; this divergence is intentional. Running multiple backend replicas on
  Kubernetes requires activating the gateway's fan-out mechanism (`events.Bus`, the RESUME ring buffer) for
  horizontal scale, using Redis Pub/Sub (already reserved in the stack for exactly this purpose) — self-hosted
  single-process instances never activate this at all, and the Redis dependency stays reserved-but-unused for
  them. Self-hosted customers get both a bare-metal/systemd path and a docker-compose production option
  (extending the existing local-dev `docker-compose.yml`), since many self-hosters (Unraid/Portainer/
  Synology-style setups) are far more comfortable with container-based deployment even for a single private
  instance.
- **Self-hosted instances**, sold via a one-time license purchase — a customer buys a license, runs their own
  completely separate instance on their own infrastructure, and is that instance's own Instance Admin, exactly
  as designed in Section 8.3. No tenant-isolation architecture is needed, because self-hosted instances never
  share infrastructure with the flagship instance or each other in the first place.
- **No "Platform Operator" tier exists.** The flagship instance simply has its own Instance Admin(s) — the
  developer/company's own team — using the exact same Instance Admin tier designed for any self-hosted
  instance. Instance Admin's proactive-intervention-without-a-filed-report capability (Section 8.3) is most
  operationally relevant on the flagship instance, which carries the most legal exposure as a real public
  service; a self-hosted Instance Admin already has full DB access to their own private instance anyway, so
  this capability changes little there.
- **Per-instance entitlement seam.** A lightweight, inert license/entitlement seam for the self-hosted-license
  path — a simple binary unlock for v1 (`{licensed: bool, license_key: string}`-shape, unused/unchecked by any
  v1 code path), represented as a structured `entitlements` blob rather than a single flag, so adding named
  feature-tier keys later does not require a schema migration. License validation stays an offline,
  cryptographically-signed license file (no phone-home), and it is a one-time purchase with no expiry. The
  license file format reuses the JWT tooling already in the stack (`golang-jwt/jwt`): an Ed25519-signed
  JWT-like structure (claims: `license_id`, `issued_to`, the `entitlements` blob, no expiry claim), verified
  locally with an embedded public key. **This scheme is deliberately separate from, and different from, the
  release-binary signing scheme below**: a license file encodes a specific customer's purchase/entitlement
  data, and publishing every issued license file's signature to a public transparency log (as the release-
  binary scheme below does, by design) would leak customer purchase records — the wrong property for this use
  case. The license-issuance/checkout mechanism itself (a small storefront/payment page that generates these
  signed license files) is effectively separate infrastructure from the chat platform's own runtime; it does
  not need to be part of the backend's own codebase.

**Federation.** Explicitly out of scope — each instance is an island (like real Discord, not Matrix/
ActivityPub). This is a deliberate non-goal, documented in its own ADR, not a silent gap.

**Mobile clients.** Out of scope for v1, with no dedicated client planned. Since the token-auth model is
already device/OS-keychain-based (not assuming a browser), no extra seam work is needed beyond ensuring the
REST/gateway contracts do not quietly assume a desktop-only client. This is a deliberate non-goal, revisited
only if the project grows.

**Auto-update.** Client-side only: the end-user daemon (plus the CLI/GUI binaries it manages) self-updates,
periodically checking a version endpoint, downloading, verifying a signature, and swapping its own binary.

Release-artifact signing for CLI/GUI/daemon/voice-worker binaries uses **Sigstore/cosign** — keyless signing
backed by an OIDC identity plus the public Rekor transparency log, so a compromised signing key's malicious
release becomes publicly detectable rather than silently trusted client-side. `goreleaser` (already the
chosen release tool) has built-in cosign support, so this is a release-pipeline configuration, not new
tooling. Verification on the client side uses self-contained, offline-verifiable Sigstore bundles (the
signature plus an embedded Rekor inclusion proof) — never a live call to Sigstore's public infrastructure at
every update check — both for reliability (verification still works if Sigstore's own services are down) and
to avoid introducing a phone-home dependency into a client-side mechanism, which would sit at odds with the
project's self-hosting-independence framing (offline license validation, no third-party telemetry, both
above and below).

Additional hardening, applied regardless of the signing scheme: the updater refuses to install a validly-
signed *older* version than what is currently running unless explicitly forced (anti-downgrade protection); a
failed signature verification means the current version keeps running, never a fallback to an unverified
binary (fail-closed); and the release-signing identity/key has a documented rotation runbook.

**Rollback safety.** The daemon keeps the previous binary after an update and auto-rolls-back if the new
version crashes repeatedly on startup, flagging the failed update to the user (surfaced in CLI/GUI) instead of
leaving always-running background infrastructure stuck in a crash loop.

**Backend/server updates stay manual.** The backend/server binary a self-hoster runs is explicitly not
auto-updated — unattended updates to a server holding other people's data/messages is a materially bigger
risk than a user's own local client updating itself. Instead, the backend checks the same version endpoint and
surfaces a passive "update available" notice to Instance Admins, never applying it, keeping operators informed
without taking the update decision out of their hands.

**Code signing.** Self-signed/unsigned for now — personal/early use accepts OS security warnings
(Gatekeeper/SmartScreen) or a one-time manual approval per machine. Real paid signing (Apple notarization plus
a Windows code-signing certificate) is a documented, known gap to close before any external/commercial
distribution, not something to set up during the personal-use phase.

**Build/release tooling.** `goreleaser` — the standard Go-ecosystem tool for cross-compiling and packaging
multiple binaries (CLI, GUI, daemon, voice-worker, backend) across multiple OS/arch targets, versioning, and
checksums, rather than hand-rolling a build matrix in CI; it is also the natural place the Sigstore/cosign
signing above and any future real code signing slot in.

**Container image scanning.** `Trivy` is added to CI, closing a gap the original planning docs already
anticipated before any container images existed. Now that the flagship's Kubernetes deployment (Section 11)
has real Dockerfiles and a Helm chart, `just security-scan` gains a Trivy step alongside the existing
`govulncheck`/`pnpm audit`.

**Observability.** A Prometheus-style metrics endpoint, built now, using `prometheus/client_golang` — connection
counts, message throughput, and voice/SFU call-health metrics (packet loss, bitrate, jitter), since voice-call
quality problems are notoriously hard to debug from logs alone and voice is real v1 functionality worth being
able to operate/tune properly from day one; these voice metrics feed the adaptive-bitrate control loop
directly (Section 6), not just a dashboard. The `/metrics` endpoint requires authentication (an Instance Admin
token) or is localhost-only bound — not the typical unauthenticated Prometheus convention, since this
project's self-hosted threat model means an operator could unintentionally expose it to the public internet.
Metric labels stay aggregate-only (total connection count, total message throughput, overall voice
packet-loss) — never per-guild/per-channel identifiers, avoiding both an information-disclosure surface and
unbounded label-cardinality growth as guild/channel counts scale.

Structured logging uses `zerolog` (chosen over both `zap` and stdlib `log/slog` for allocation-free
performance headroom on the message/gateway hot path, accepting the extra dependency as worthwhile here,
unlike the wazero/pure-Go choice elsewhere where the dependency tradeoff runs the other way).

**Telemetry.** Opt-in crash reports only, no usage/behavioral analytics — consistent with the project's
privacy-forward framing (E2E encryption, self-hosting, no cookie-based tracking) elsewhere. Crash reports post
to a simple, self-operated endpoint (the developer's own infrastructure) rather than a third-party service
like Sentry — a crash report can contain stack traces with local file paths/usernames, and routing that to a
third-party APM vendor would be a real tension with the privacy stance taken on every other
telemetry-adjacent decision in this plan (no license phone-home, no third-party analytics).

**Rate limiting.** `ulule/limiter` is the base REST/gateway rate-limiting library from Milestone M1 onward —
in-memory store for self-hosted single-process instances, Redis-backed store for the flagship (Section 11)
once multiple replicas are in play. All IP-based limiting this library enforces, anywhere in the system (login
attempts, general REST/gateway limits, webhook posting, the public-matchmaking anti-abuse gate in Section 8.2,
everywhere else it's used), groups IPv6 traffic by **`/64` subnet**, not exact address: a single bad actor is
trivially assigned an entire `/64` block, so exact-address matching would let IPv6 traffic route around every
one of these limits for free. This is a global property of the base wiring, not a per-feature opt-in.

**Database connection management.** `pgx` pool sizing per backend replica is kept intentionally small and
documented, rather than left to default configuration — standard Postgres treats connections as relatively
heavy processes with typically low default limits (often 100), and an always-on daemon-per-OS-user model means
a self-hosted instance's real connection count can grow with both instance popularity and how many devices a
given user runs a daemon on simultaneously. Self-hosters whose instance grows past what a small direct pool
comfortably serves are pointed at PgBouncer to multiplex connections efficiently, rather than the backend
holding a large direct pool itself.

**Data retention.** A configurable pruning seam covers `audit_log_entries` and `instance_audit_log` only
(auto-prune entries older than a configured N days), wired into `app instance init`/`app config set`. Default:
**pruning disabled** — audit logs are compliance-sensitive, so no silent data loss happens by default; a
self-hoster opts in and sets the window explicitly. This is deliberately narrow: message history and reports
are **not** covered by this seam and stay permanent by design — both are core product data users expect to
keep indefinitely (message history the same way real Discord does; reports as the moderation evidence trail),
so a self-hoster short on disk is expected to add storage or offload attachments to S3 (already pluggable via
Section 8.16's attachment-storage interface), not have the product silently delete either.

## 11. Flagship instance Kubernetes deployment architecture

The flagship instance is the one deployment in this plan that is not single-binary-simple, so it gets its own
dedicated architecture rather than being folded into the general self-hosting story.

**Component breakdown:**
- **Backend API/Gateway pods** — a standard Kubernetes `Deployment`, multiple replicas, behind a normal
  Ingress/LoadBalancer over TCP. This fits ordinary Kubernetes networking without special-casing — the only
  reason it can run as multiple replicas at all is the Redis pub/sub fan-out (Section 10), since any replica
  can now serve any client (gateway events cross replicas via Redis instead of living only in one process's
  memory).
- **TURN/SFU pods** — deliberately separate from the API pods, with different networking. WebRTC media
  fundamentally does not fit the typical ClusterIP/Ingress abstraction (ICE candidates need to reflect a real
  reachable IP:port; media is UDP, not HTTP). These pods run with `hostNetwork: true` — the pod uses the
  node's network directly, the same well-established pattern real-world Kubernetes WebRTC deployments
  (LiveKit, Jitsi) use, rather than fighting Kubernetes's default overlay networking or requiring a
  UDP-port-range-capable LoadBalancer (e.g. MetalLB).
- **Stateful dependencies run self-managed in-cluster via operators, not managed cloud services**, consistent
  with the project's self-hosting-independence ethos applied to its own flagship deployment:
  - **Postgres**: a Postgres operator (e.g. CloudNativePG) for HA within the cluster, not RDS/Cloud SQL.
  - **Redis**: a Redis Helm chart/operator in-cluster — the activation of the horizontal-scale event bus
    described in Section 10; self-hosted single-process instances never activate this at all.
  - **Object storage**: self-hosted MinIO in-cluster (via `minio-go`), not real AWS S3.
  - This deployment also doubles as the reference "how would someone actually run this at real scale"
    deployment for any advanced self-hoster who wants to go beyond a single bare-metal/docker-compose
    instance.
- **TLS termination** uses `cert-manager` plus Ingress, not the backend's own built-in `certmagic` ACME logic.
  `certmagic` (Section 8.16) is the right answer for a single bare-metal/systemd/docker-compose self-hosted
  instance, but the wrong answer for multiple Kubernetes replicas — every replica independently trying to
  manage the same Let's Encrypt certificate would race on issuance and hit rate limits. In this deployment
  specifically, the backend's built-in ACME code path is disabled entirely; `cert-manager` issues/renews the
  certificate as a Kubernetes Secret, mounted into the Ingress, and the backend just serves plain HTTP behind
  it. Self-hosted instances keep using built-in `certmagic` exactly as designed in Section 8.16.
- **Deployment tooling**: Helm charts package the whole multi-component release (API pods, TURN/SFU, Postgres
  operator config, Redis, MinIO, Ingress, cert-manager) as one versioned, parameterized chart, and doubles as a
  reusable artifact for any self-hoster who later wants to run their own instance on Kubernetes instead of
  bare-metal/docker-compose.
- **Graceful rolling updates** reuse an existing protocol op-code rather than inventing new shutdown behavior:
  a `preStop` hook on the API pods sends the gateway's already-existing `Reconnect` op-code to every
  locally-held WebSocket connection before the pod actually terminates (`terminationGracePeriodSeconds` gives
  it time to do so). Clients get a clean signal to reconnect to a different replica — already possible now
  that gateway events cross replicas via Redis — instead of experiencing an abrupt drop and detecting failure
  themselves via a dead socket/timeout. The `preStop` hook staggers its `Reconnect` sends across the available
  `terminationGracePeriodSeconds` window rather than firing all of them at once, and the daemon implements
  randomized exponential backoff on receiving a `Reconnect` — without this, a single replica holding thousands
  of connections disconnecting them all simultaneously would thundering-herd the auth/DB layer on the
  remaining replicas the instant they all reconnect together.
- **Database migrations** run via a Helm `pre-upgrade`/`pre-install` Job hook — a one-shot Job runs the
  existing `golang-migrate` tooling unchanged, completing before the new Deployment's pods start. This is what
  prevents multiple replicas racing to apply the same migration concurrently during a rollout, and guarantees
  no replica ever starts against a not-yet-migrated schema — the same guarantee, same tooling, that Section
  8.16's self-hosted auto-run-on-startup-with-advisory-lock path gives a single-process deployment; only the
  trigger differs by deployment shape.
- **Observability stack**: minimal for now — the Prometheus metrics endpoint exists (Section 10), but no
  scraper/dashboard (e.g. `kube-prometheus-stack`, Grafana, Loki) is deployed yet; debugging relies on
  `kubectl logs` and manually querying `/metrics` until real operational need justifies standing up the full
  stack.
- **Secrets management**: plain Kubernetes Secrets, applied via the Helm chart — JWT signing keys, the license
  Ed25519 private key, Postgres/Redis/MinIO credentials, OAuth client secrets. No Sealed Secrets or external
  secrets manager (Vault, cloud secrets service) for now, revisited only if a real multi-cluster or compliance
  need arises later.
- **Autoscaling**: CPU/memory-based HPA now (via the built-in `metrics-server`, no Prometheus needed);
  connection-count-based scaling is a documented future upgrade once the observability stack (above) actually
  exists to feed a Prometheus adapter.
- **Namespace isolation**: TURN/SFU pods run in their own dedicated namespace under the "privileged" Pod
  Security Standard (required for `hostNetwork: true`); API/backend/Postgres/Redis/MinIO pods stay in a
  separate namespace under the stricter "restricted" policy — keeping the blast radius of the one genuinely
  privileged capability this deployment needs contained to exactly the pods that need it.
- **NetworkPolicies** restrict pod-to-pod traffic to what is actually needed (e.g. only API pods can reach
  Postgres; TURN/SFU pods have no network path to the database at all) — baseline hardening defined upfront in
  the Helm chart, since network segmentation is cheap to define now and meaningfully limits blast radius if
  any one pod — especially the customer-facing, media-handling TURN/SFU pods — is ever compromised.
- **Rate limiting** uses `ulule/limiter`'s Redis-backed store for the flagship instance specifically — the
  same base library introduced in Section 10's "Rate limiting" paragraph (including its global `/64` IPv6
  grouping rule), just backed by Redis instead of an in-memory store here. With multiple flagship replicas
  behind a load balancer, in-memory counters would be per-replica, meaning a bad actor could effectively get
  `(intended limit) × (replica count)` just by spreading requests across replicas. Since Redis is already
  deployed for gateway fan-out, switching the flagship's rate limiter to the Redis-backed store `ulule/limiter`
  already supports reuses infrastructure that is already there. Self-hosted single-process instances keep the
  simpler in-memory store exactly as designed, since they never have multiple replicas to race across.
- **Flagship Postgres backups** use CloudNativePG's native continuous backup/WAL-archiving to the in-cluster
  MinIO — real point-in-time recovery via continuous WAL-archiving plus scheduled base backups, rather than
  bolting a separate manual dump/cron job onto an operator that already solves this properly. Self-hosted
  instances keep the simpler `pg_dump`/`pg_restore` documentation from Section 8.16, since they do not run
  CloudNativePG at all.
- **Deployment trigger**: a simple CI-triggered `helm upgrade` (e.g. a GitHub Actions step on merge to main),
  not GitOps (ArgoCD/FluxCD) — a GitOps controller's extra guarantees (drift detection, automatic
  reconciliation) are not yet justified by this deployment's actual scale/team size.
- **Testing infrastructure** extends, rather than replaces, the standard `testify` plus `testcontainers-go`
  pattern: the flagship-specific paths that only exist because of the multi-replica architecture
  (Redis-backed pub/sub fan-out, Redis-backed rate limiting) get their own integration tests spinning up real
  Redis via `testcontainers-go`, the same way Postgres integration tests already do. Self-hosted
  single-process code paths (in-memory event bus, in-memory rate limiter) keep their existing simpler test
  coverage, since they never touch Redis at all.

## 12. Files to update and how

### 12.1 `CLAUDE.md`

- **"What this is"**: describe the CLI and native GUI (plus the later, lower-priority web SPA) as the client
  story; reference the licensing ADR (Section 12.3) instead of AGPL.
- **"Architecture at a glance"**: rewrite the "Auth transport" bullet — token-based (Bearer, OS keychain,
  scoped tokens) is primary for the CLI/GUI; cookie/CSRF is retired for those two clients, and
  deferred-but-decided (BFF cookie exchange) for the future web SPA. Add a "Client daemon" bullet describing
  the daemon/attach-client split and its two IPC channels (Unix socket/named pipe for CLI+GUI, localhost TCP
  plus secret for bot automation).
- **"Tech stack"**: add the CLI (Go, Bubble Tea + Lip Gloss + Bubbles), the native GUI (Go, Gio), the daemon
  (Go, `wazero` for the WASM plugin sandbox), and note Postgres `tsvector`/GIN/`pg_trgm` for search; keep the
  React stack listed, marked as the later tertiary web client.
- **"Non-negotiable rules"**: rule 4's CSRF note now applies only to the future web SPA's BFF layer, not the
  token-authenticated CLI/GUI/daemon REST surface. Rewrite rule 10 in full: voice (audio) is real v1
  functionality now; video/screen-share is the part still deferred-but-seamed (`PermVideoVoice`,
  `GUILD_STAGE_VOICE`, and the `supports_video` capability plumbing are never removed/restructured even
  though video itself isn't built yet). Add new numbered rules:
  1. Voice/audio media (capture, encode, the SFU connection) must run in the isolated voice-worker subprocess,
     spawned on-demand by the daemon — never in the daemon process itself. A crash there must never take down
     messaging/presence/plugins.
  2. Every WASM host-function exposed to plugins must be capability-gated against that plugin's approved
     manifest grants; every plugin instance gets enforced CPU/memory quotas and a wall-clock timeout per
     invocation — no plugin code path is exempt "just this once."
  3. Any server-side feature that reads message content (search, moderation visibility, audit-diffing,
     edit-history, link-preview generation, account export) must explicitly exclude E2E-encrypted DMs — never
     assume plaintext is available; E2E is never offered for `GROUP_DM`, guild channels, whispers, or voice —
     `DM` only, full stop.
  4. Every Instance Admin action (bans, report resolution, license/entitlement changes, admin-tier grants)
     writes to `instance_audit_log` in the same transaction, with the same rigor required for guild-scoped
     audit logging.
  5. New gateway dispatch types and REST endpoints that affect CLI-observable state must also update the
     CLI's `--json` output schema in `contracts/`, in the same commit — it is a versioned source-of-truth
     contract, not best-effort output.
  6. Any new local IPC surface (daemon↔CLI/GUI socket, the local bot-automation port) must state its trust
     tier explicitly in its design — OS-permission-protected (first-party clients) vs. secret-protected
     (external scripts) — never assume the two are interchangeable.
  7. A ban or self-service account deletion must invoke the general-purpose revoke-all-sessions primitive
     (force-close live connections, revoke refresh/scoped tokens, revoke linked-device E2E trust) — no
     separately-implemented, potentially-incomplete cleanup path for either.
  8. Any server-side feature that fetches a user-supplied URL (link previews today, anything similar later)
     must route through the SSRF-protected dialer (rejects private/loopback/link-local resolved IPs, checked
     at actual connect time), with a strict request timeout and a response-size cap (`io.LimitReader`) — never
     a plain, unbounded `http.Get` on a user-controlled URL.
  9. Any untrusted text rendered by the CLI (usernames, message content, link-preview titles, plugin manifest
     descriptions, webhook display names, anything else user- or third-party-controlled) must pass through
     the terminal-safe sanitization function first — never written to the terminal raw.
  10. Any server-side feature that gates delivery or visibility to a user (DM send, whisper send, friend
      request, presence, notification dispatch, and guild-channel gateway fan-out specifically) must check the
      `blocks` table. This includes the real-time gateway dispatch path, not only REST mutation handlers.
  11. Any new REST endpoint or gateway event affecting CLI-observable or browser-observable state must be
      sanity-checked against real browser constraints (CORS, request chattiness, BFF-auth-compatibility) at
      the time it's added to `openapi.yaml`/`gateway-events.schema.json` — never deferred silently to Phase O
      just because the web client isn't built yet.
- **"Milestone status"**: replace with a short pointer to Section 13's roadmap and its current phase.
- Point to the ADR list in Section 12.3.
- **"Common commands"**: update `just security-scan` to add the Trivy container-image-scanning step.

### 12.2 `docs/architecture.md`

- **Context**: backend plus CLI plus native GUI (plus later web SPA); source-available, not open source.
- **§1 Monorepo Layout**: add `cli/`, `gui/`, a `daemon/` (or fold into `cli/` as a subcommand/mode — an
  implementation detail for whoever executes this), and an `internal/voice`/`media/` area for the Pion-based
  SFU plus embedded TURN server; demote `frontend/`.
- **§2 Data model DDL**: add every new table/column introduced by this plan: `presence_status` (including the
  Deep Work value), `public_channels` (guild-less), `recently_met`, `message_tags` (plus its join table),
  `whispers`/recipients, `notification_filters` (with the complexity/timeout cap noted), `api_tokens` (scoped,
  hashed, replacing the plain refresh-cookie model for CLI/GUI, carrying `device_id`-scoped refresh-token
  families so rotation on one device never invalidates another device's session), E2E key material (point to
  the E2E ADR rather than fully speccing inline), `instance_bans` (platform-wide, full-account-suspension,
  keyed by user, not guild-scoped, with an optional `expires_at`), `instance_audit_log` (separate from
  per-guild `audit_log_entries`), `reports` (reporter, target type/id, reason category plus free text, status
  workflow), an instance-admin flag/table distinct from any guild's role hierarchy, a `device_code` table for
  the CLI's headless OAuth fallback, an `instance_invites` table (distinct from per-guild `invites`), a
  `friend_requests`/`friendships` table pair, a per-instance `entitlements` table/blob, a per-user
  `user_entitlements` table/blob, a new `messages.type` reserved value for "sent via automation," `tsvector`
  generated columns plus GIN indexes (plus `pg_trgm`) on `messages.content` and on tag/channel-topic text, a
  `blocks` table (`blocker_id`, `blocked_id`, `created_at`), a `password_reset_tokens` table (hashed,
  single-use, expiring), a `guild_emojis` table (`guild_id`, `name`, image reference, uploader, `created_at`),
  a `webhooks` table (channel-scoped secret, creator, name/avatar), two new permission bits
  (`PermManageEmojis`, `PermManageWebhooks`), a `channel_read_states` watermark table (per-user, per-channel,
  synced via its own gateway dispatch event — distinct from the daemon's ephemeral in-memory scroll state), a
  configurable data-retention/pruning field set scoped to `audit_log_entries`/`instance_audit_log` only
  (default disabled), and the instance-level SMTP configuration fields (in whichever config surface
  `app instance init` writes to — not a new Postgres table). Note that E2E key material is scoped specifically
  to the `DM` channel type, never `GROUP_DM` or guild channels.
- **§2 Permission system**: note that public matchmaking channels intentionally bypass this system (fixed
  ruleset, documented exception); add the Instance Admin tier as a concept sitting above/outside the
  per-guild role hierarchy entirely (not resolved via `roles.Resolve`), boolean/flag-based, supporting
  multiple admins per instance, with a safety rail blocking removal/demotion of the last remaining admin. Add
  a clear, consolidated diagram/write-up of the full authority hierarchy exactly as specified in Section 9
  above (Instance Admin → guild owner → `PermAdministrator` role → regular roles → overwrites, plus the
  parallel non-hierarchical concepts: public matchmaking's fixed ruleset, whispers as message-visibility not
  authority, friends as a non-gating mutual label, blocks as a unilateral delivery-layer filter, E2E as a
  privacy not authority layer, and the confirmed absence of any guild-to-guild or instance-to-instance
  relationship).
- **§2 Real-time gateway**: the presence op-code/dispatch carries the new persisted status values including
  Deep Work; server-side notification-filter evaluation sits in the dispatch path; the daemon is the actual
  gateway client now, not each attach UI. Op-code 4 (Voice State Update) and the `VOICE_STATE_UPDATE`/
  `VOICE_SERVER_UPDATE` dispatch events go from inert stub to real, active signaling — `MediaCoordinator` gets
  a real `PionMediaCoordinator` implementation, and the voice-join payload gains the `supports_video`
  capability field. Add the block-aware fan-out mechanism as a concrete design: each connection's block-set is
  cached (never queried per message), consulted at DISPATCH fan-out time for guild-channel
  message/presence/mention events, so a blocked author's content and mentions never reach the blocker's
  connection; a block or unblock action updates the affected connection's cached set immediately, not on a
  delay.
- **§2 Auth design**: replace the cookie/CSRF description with the token-based design; point to the auth ADR.
- **§2 Account lifecycle: deletion & data export**: note that server-side export cannot cover E2E-encrypted
  conversation content; add the daemon-performed local decrypt-and-export step (the daemon, not the CLI/GUI
  independently, per the E2E key-boundary rule) producing a standalone `local_e2e_export.zip` alongside —
  never merged into — the server-side account export, per the E2E ADR. Add a "log out all other
  devices/sessions" self-service action, built on
  the general-purpose revoke-all-sessions primitive the Instance Admin ban mechanism also uses. Self-service
  account deletion must invoke that same primitive (revoke every session/token, mark every linked device's E2E
  identity revoked). Add the block-list export asymmetry (blocked-by-you included, blocked-you excluded)
  alongside the reports-filed export asymmetry (filed-by-you included, filed-against-you excluded), stated
  together in the same place since they follow the same rule.
- **§3 "Frontend (React SPA)"**: retitle as "the (later) web client." Add substantial new sections for the
  daemon (lifecycle, IPC channels reusing the gateway protocol, state ownership), the CLI TUI (Bubble
  Tea/Lip Gloss/Bubbles pane engine, the user-remappable keybinding scheme, config/theme sharing), and the
  native GUI (Gio, attaches to the daemon, shares config/theme, plus the GUI-only, lowest-priority,
  solo-local-drafting-only integrated whiteboard). Note explicitly that split-pane multiplexing is a
  requirement for all three clients, each with its own independent, non-synced pane-engine implementation and
  layout storage (CLI/GUI: local daemon/config state; web: browser `localStorage`).
- **§3 Voice seam**: rewrite in full, describing real functionality: the supervised voice-worker subprocess's
  audio pipeline (capture/encode/send, receive/decode/play, noise-suppression/AGC, spawned/monitored by the
  daemon rather than living in it), the adaptive-bitrate control loop (REMB/TWCC feedback into Opus's runtime
  bitrate control), the CLI's minimal voice status/control surface (join/leave/mute/deafen via keybinding plus
  a status line, no visual call UI since it's a terminal), the GUI's voice UI (participant list, device
  selection, mute/deafen controls), the active-speaker indicator plus separate local-mute and report actions
  shipped in both, and the forward-looking video/screen-share design (GUI/web own it directly
  via a second WebRTC connection to the SFU, the `supports_video` capability flag, the codec-agnostic SFU
  track model) even though video itself isn't built yet. Note the mic-permission-handoff design and the
  global-hotkey-registration-from-a-headless-daemon question as both flagged-unverified pending Milestone
  M25's spike.
- **§4 Contracts & Dev Workflow**: add CLI `--json` output schemas as a third versioned contract artifact in
  `contracts/`, alongside the existing `openapi.yaml` and `gateway-events.schema.json` — the same
  same-commit-with-the-code-change rule. Add `oapi-codegen` to generate Go server-side request/response types
  and routing scaffolding directly from `openapi.yaml`, so a spec change that isn't reflected in code fails to
  compile rather than silently drifting into a documentation lie nobody notices.
- **§7 Security deep dive**: add the token-scope model; the two-tier IPC trust model (OS-socket for
  first-party clients vs. secret-protected TCP for external automation); the WASM plugin sandbox boundary
  (capability manifest, first-load approval, CPU/memory/wall-clock quotas); the P2P opt-in/IP-exposure
  disclosure; the E2E threat model and what-the-server-loses-visibility-into list (including the
  `DM`-channel-type-only restriction, the text-only-not-voice scope, the license-compatibility gate on
  `go.mau.fi/libsignal`, and the narrowed audit scope covering the library integration and the device-linking
  protocol specifically); the ban/deletion session-revocation mechanism and its reconciliation with the
  stateless-JWT access-token design (real-time kill is immediate, already-issued access tokens have a short
  natural-expiry tail); the link between device revocation and E2E device-link trust; the `/metrics`
  endpoint's auth requirement and aggregate-only labeling; the reports system's rate-limiting and
  reporter-history triage, plus the filed-against-you/filed-by-you data-export asymmetry; the ACME/HTTPS
  opt-out for LAN-only deployments alongside the voice opt-out; the block-enforcement checkpoints, including
  the server-side gateway-dispatch guild-channel filter and notification-suppression path; the password-reset
  anti-enumeration and rate-limiting design; the OAuth loopback fixed-port/fallback-port design; and the
  auto-update hardening list (anti-downgrade, fail-closed-on-verify-failure, key-rotation runbook).
- **§8 Performance**: note that server-side regex-filter evaluation uses Go's RE2-based stdlib `regexp`
  (linear-time by construction, no catastrophic-backtracking class of risk) with a pattern-length cap as cheap
  extra defense-in-depth. Add the Prometheus metrics endpoint (connection counts, message throughput,
  voice/SFU packet-loss/bitrate/jitter) as a real v1 operability requirement. Add the adaptive-bitrate control
  loop as closing a real optimization gap in the custom SFU, framed as load-bearing functionality, not a
  nice-to-have.

### 12.3 `docs/adr/`

- **Amend `0002-cookie-based-auth.md`**: mark Superseded by the new auth ADR for CLI/GUI; keep the historical
  rationale intact (still correct for a browser client); note the future web-SPA BFF-cookie-exchange plan.
- **Amend `0005-agpl-license.md`**: mark Superseded by the new licensing ADR.
- **Supersede `0006-voice-deferred-with-seams.md` entirely** (not a light amend): the core decision (defer
  real media to a later phase) is reversed. Keep the historical record but mark it Superseded, and write a
  full new ADR with the actual v1 voice architecture.
- **New ADRs** (numbering starting at 0007):
  - Licensing and project posture: the source-available custom license; the two-deployment commercial model
    (flagship free public instance plus sold self-hosted licenses, no shared multi-tenancy, no Platform
    Operator tier); the per-user `user_entitlements` seam for future flagship subscription features.
  - Guild/channel authority hierarchy: the consolidated write-up from Section 9 (Instance Admin, guild owner,
    roles, overwrites, and the parallel non-hierarchical concepts including blocks).
  - CLI and native GUI client architecture: the Go-native GUI toolkit choice, build order, web SPA demotion,
    and the user-remappable keybinding scheme.
  - The client daemon: lifecycle/autostart, the dual IPC model, state ownership, why CLI and GUI share it, the
    plain-hand-editable config file plus atomic-write plus `fsnotify` hot-reload model, and the semver-based
    version-compatibility policy for the shared handshake.
  - Token-based client auth: Bearer access/refresh, OS-keychain storage, scopes, the future web-SPA BFF plan,
    and the fixed-port/fallback-port OAuth loopback design.
  - Voice in v1 (supersedes `0006`): the custom Pion SFU; the on-demand-spawned voice-worker subprocess (not
    the daemon itself) holding its own direct SFU connection while the daemon relays control signaling only;
    the adaptive-bitrate control loop; the embedded TURN server; GUI/web-owned video (deferred but seamed);
    the noise-suppression/AGC pipeline; the P2P-file-transfer/voice media-ownership consistency rule; the
    voice auto-rejoin-on-crash behavior; and the Prometheus voice/SFU metrics requirement.
  - Public matchmaking channels: the guild-less type, fixed-ruleset moderation, recently-met retention, the
    mutual friend-request/accept system, the personal block/mute system, the new Instance Admin tier and
    platform-wide `instance_bans` as full account suspension with immediate session revocation, the dedicated
    `instance_audit_log`, the server-side admin-lockout-recovery command, and the unified `reports` system
    serving guild-level, instance-level, and DM/Group-DM moderation with rate-limiting and reporter-history
    triage. Also covers per-guild custom emoji and incoming webhooks as v1-scope community features closely
    coupled to this same ADR's subject matter.
  - E2E encryption: opt-in, restricted to the `DM` channel type only; the pairwise-scaling limitation as the
    deliberate reason `GROUP_DM`/guild channels are excluded; whispers and voice audio explicitly excluded;
    the `go.mau.fi/libsignal` integration choice and its mandatory license-compatibility gate; the fully
    custom device-linking protocol as a second flagged audit-risk item; the narrowed external-audit scope
    (library integration plus device-linking); the no-backup/permanent-loss policy; the durable local
    keystore requirement; and the feature tradeoffs given up for encrypted content.
  - Plugin sandboxing: WASM/`wazero`, the host-function API, daemon-hosted execution, enforced per-instance
    CPU/memory quotas, the wall-clock timeout per invocation, and the self-declared capability manifest plus
    first-load user approval model.
  - P2P file transfer: opt-in-per-transfer, the IP-exposure tradeoff, and the client-owns-the-negotiation rule.
  - Local automation security: scoped tokens, the dual IPC trust tiers, and the integrated-shell trust
    boundary.
  - Search: Postgres `tsvector`/GIN/`pg_trgm` now, explicitly noting a dedicated search engine as a possible
    future swap if the project scales, not a v1 concern.
  - Platform scope boundaries and the commercial model: federation and mobile as deliberate non-goals; the
    flagship free public instance versus sold self-hosted licenses as two independent deployments of the same
    codebase, with no shared multi-tenancy and no Platform Operator tier; the per-instance entitlement seam
    (offline signed license file, no phone-home, one-time purchase); and the per-user `user_entitlements`
    seam for future flagship-instance subscription features.
  - Operations: the client-side-only auto-update flow (Sigstore/cosign signing with offline-verifiable
    bundles, anti-downgrade protection, fail-closed verification, key-rotation runbook, and
    rollback-on-crash-loop), the explicit separation between that scheme and the license file's own
    Ed25519-JWT scheme, the deferred-to-commercial-distribution code-signing timeline, the built-in
    ACME/Let's Encrypt HTTPS for self-hosted instances (with its LAN-only opt-out), the transactional-email
    (SMTP) design (with its own opt-out and async-sending requirement), and telemetry (opt-in crash reports
    to a self-operated endpoint only, no third-party service, no analytics).
  - Flagship instance Kubernetes deployment architecture: the Helm-packaged multi-component deployment — API
    pods behind normal Ingress, TURN/SFU pods on `hostNetwork` for real UDP/ICE reachability, in-cluster
    self-managed Postgres (operator)/Redis/MinIO rather than managed cloud services, `cert-manager` plus
    Ingress for TLS, and the Redis-backed horizontal-scale event bus this deployment specifically requires.

## 13. Milestone roadmap

Read as a long-term, dependency-ordered critical path, not a near-term v1 promise — the accumulated scope
(custom SFU, custom crypto, native GUI, plugin sandbox, licensing infrastructure) is realistically multi-year
work. Every milestone is scoped to one coherent deliverable, states what it depends on when that is not simply
"the previous milestone," and ends with a concrete, checkable "done when" condition. Milestones are numbered
continuously across the whole roadmap; Phase headers are for orientation only, not separate tracks, except
Phase P (the Kubernetes deployment track), which is explicitly parallel — see the dependency notes at the end
of this section.

#### Phase A — Foundation

- **M0 — Monorepo scaffolding**: create the directory layout (`backend/`, `cli/`, `gui/`, `daemon/`,
  `internal/voice`, `contracts/`, `docker/`), Go module setup, CI skeleton (lint, build, and test jobs, no
  content yet), and a `goreleaser` config skeleton (build/package multiple binaries, no signing wired in
  yet). Done when: a `just dev`-equivalent boots an empty backend, and CI runs green on an empty test suite.
- **M1 — Backend skeleton**: `chi` router, `sqlc` plus `pgx` wiring, Postgres connection pooling (sized per
  Section 10's "Database connection management" guidance), `golang-migrate` wiring (an empty initial
  migration) that auto-runs on startup guarded by a Postgres advisory lock and blocks `/healthz` until it
  completes (Section 8.16), `zerolog` structured logging wired in, base `ulule/limiter` rate-limiting
  middleware (in-memory store, global `/64` IPv6 subnet grouping per Section 10's "Rate limiting" paragraph),
  a `/healthz` endpoint. Done when: the backend binary starts, blocks on a pending migration until it
  completes, connects to Postgres, and `/healthz` returns 200; a burst of requests from one IPv6 `/64` block
  is throttled as a single source.
- **M2 — `app instance init`, infrastructure config only**: the setup wizard's DB-connection,
  storage-backend (local disk vs. S3/MinIO), ACME on/off (plus the LAN-only opt-out), and
  registration-gating prompts — writing a valid config file. Explicitly does not include first-admin-account
  creation yet (no `users` table exists until M4) — that step is added at M10. Both quick-start and `--full`
  modes are stubbed now, fleshed out as later milestones add more prompts (the SMTP prompt at M5, the voice
  opt-out at M37, the public-matchmaking toggle at M58). Done when: running the wizard produces a valid,
  loadable instance config.
- **M3 — CLI skeleton and daemon lifecycle stub**: the `app` CLI binary builds and runs, command scaffolding,
  `--json`/`--help` flag plumbing (no real commands yet), and a daemon stub that installs itself as an
  OS-level service (systemd/launchd/Windows task) but does nothing beyond starting and stopping cleanly. Done
  when: `app` installs and starts a daemon that appears as a running OS service, with no functional commands
  yet.

#### Phase B — Auth

- **M4 — Backend auth core**: `users` table, `argon2id` password hashing, JWT access-token issuance
  (`golang-jwt`, 15-minute TTL), refresh-token issuance/hashing/rotation scoped per `device_id` (rotating one
  device's token must never invalidate another device's refresh-token family), the `api_tokens` (scoped) table
  plus validation middleware. Done when: a REST call with a valid password can obtain an access-plus-refresh
  token pair, a scoped `api_tokens` row can authenticate a request restricted to its granted scope only, and
  rotating one device's refresh token leaves a second device's own token family valid.
- **M5 — Transactional email (SMTP) and password reset**: `app instance init` gains the SMTP prompt
  (skipped by default in quick-start, prompted explicitly in `--full`), the `password_reset_tokens` table, and
  reset-request/confirm endpoints with anti-enumeration and rate-limiting, using `wneessen/go-mail` for
  sending. Email sending is asynchronous/backgrounded, never synchronous in the request path. Depends on M4
  (the `users` table). Done when: a password reset completes end-to-end via a real SMTP relay; a reset request
  for a non-existent email returns a response identical to one for a real email; and email sending never
  blocks the HTTP response even against a deliberately slow SMTP relay in a test.
- **M6 — OAuth backend flow**: `golang.org/x/oauth2` wiring for Google and GitHub, an account-linking table,
  callback handling. Done when: completing a Google or GitHub OAuth flow against the backend issues a valid
  token pair.
- **M7 — CLI `app login`, password plus keychain**: a direct in-terminal password prompt, storing the
  resulting tokens via `zalando/go-keyring`. Done when: `app login` with a password succeeds, and the daemon
  can use the stored token on next launch without re-prompting.
- **M8 — CLI OAuth loopback flow**: the system-browser-plus-localhost-callback loopback login, using the
  fixed local port (with its documented fallback-port list) registered as the exact callback URL with both
  providers. Done when: `app login` opens a browser, completes Google or GitHub OAuth via the fixed
  registered port, and stores the resulting token via the same keychain path as M7; and if the primary port is
  occupied, the CLI falls back to the next registered port and only fails with a clear "free this port and
  retry" error once every registered fallback is exhausted.
- **M9 — CLI headless device-code fallback**: the `device_code` table, the minimal unstyled server-rendered
  auth-completion page, and CLI headless-context detection plus polling logic. Depends on M6 (OAuth) and M8
  (loopback, to detect when to fall back from it). Done when: `app login --no-browser` (or an auto-detected
  headless context) displays a code, and completing it on a separate device with a browser finishes the login
  on the original CLI session.
- **M10 — `app instance init`, finish**: adds the first-admin-account-creation step (now that M4 exists) and
  wires up instance-level registration gating (the `instance_invites` table plus enforcement at
  registration). Depends on M2 and M4. Done when: a fresh instance can only be bootstrapped via the wizard,
  ending with one working admin account, and normal registration requires a valid instance invite code if
  gating is on.
- **M11 — Session revocation primitive**: the general-purpose "revoke all sessions/tokens for account X"
  mechanism (force-close live gateway connections, revoke refresh plus scoped tokens), exposed now as a
  self-service "log out all other devices" account-security feature. Ban-triggered use of this same primitive
  comes later at M64, once Instance Admin exists — the primitive itself is built now. Done when: a user can
  log out all their other sessions from one account action, and previously-issued refresh tokens immediately
  stop working.

#### Phase C — Guild/channel/permission core

- **M12 — Guilds/channels/roles schema plus CRUD**: the core guild/channel/role tables and REST endpoints.
  `oapi-codegen` against `openapi.yaml` is wired up starting here — every REST endpoint from this point on is
  generated, not just documented. Done when: a guild, its channels, and its roles can be created, read,
  updated, and deleted via the REST API, matching the generated types.
- **M13 — Permission engine**: `roles.Resolve`, the permission bitfield, overwrite resolution
  (`@everyone` → role → member), role `position` hierarchy enforcement. Done when: the permission-resolution
  algorithm's documented test cases (owner bypass, `PermAdministrator` short-circuit, overwrite precedence,
  position-based role-management limits) all pass.
- **M14 — Guild audit log**: `audit_log_entries`, written in the same transaction as every guild-scoped
  mutation, `GET /guilds/{id}/audit-log`. Done when: every guild mutation type produces exactly one audit
  entry, atomically.
- **M15 — Core messaging CRUD**: send/edit/delete REST endpoints for channel messages, permission-checked via
  the engine from M13 and audit-logged per the mechanism from M14. Depends on M13 and M14. Done when: a
  permitted member can send/edit/delete a message via the REST API, an unpermitted one is rejected, and each
  mutation produces an audit entry.
- **M16 — Guild-level reports**: the `reports` table (reporter, target type/id, reason category plus free
  text, status workflow), a file-a-report endpoint, and a guild-moderator triage view gated by
  `PermManageMessages`. This is the guild-scoped half of the eventual unified reports system — the
  Instance-Admin-facing half does not exist until M66. Report filing is rate-limited now (reuses existing
  REST rate limiting). Depends on M15 (a message must exist to report). Done when: a guild member can file a
  report against a message, and a `PermManageMessages` holder can see and resolve it.
- **M17 — Message tagging**: `message_tags` (plus its join table), guild-wide scope (not per-channel),
  private/solo tags need no permission, shared tags require `PermManageMessages`. Depends on M15. Done when: a
  tag created in one channel can be applied to a message in a different channel of the same guild, and
  permission gating on shared-tag creation is enforced.

#### Phase D — Real-time gateway and daemon

- **M18 — Gateway protocol core (backend)**: op-codes, the HELLO/IDENTIFY/READY handshake (carrying the
  backend's current server time for client clock-offset calculation), heartbeat, RESUME, DISPATCH, backed by
  `coder/websocket`. The initial READY payload sends guild/channel metadata upfront but defers full member
  lists/bulk per-guild state until a guild is actually opened (lazy per-guild loading). Done when: a raw
  WebSocket client can complete the handshake, receive DISPATCH events for guild activity, and RESUME after a
  disconnect without losing events; an account in many guilds gets a bounded-size initial payload rather than
  one that scales linearly with total guild count.
- **M19 — Daemon as gateway client**: the daemon holds the persistent WS connection to the backend, maintains
  in-memory scrollback/presence state, computes and applies the HELLO clock offset to local JWT-expiry checks,
  and stream-decodes (`json.Decoder`) the initial sync payload rather than buffering it fully before parsing.
  Done when: the daemon alone (no CLI/GUI attached) stays connected and accumulates state correctly; a
  deliberately skewed system clock does not cause spurious auth failures.
- **M20 — Daemon↔CLI/GUI local IPC**: the Unix domain socket / named pipe, 4-byte-length-prefixed JSON
  framing, reusing the gateway's op-code/DISPATCH shape and one shared client-side event parser, and the
  semver MAJOR-must-match/MINOR-window version-compatibility handshake. The daemon's write path to each
  attach client is asynchronous and bounded (a per-connection outbound channel with fixed capacity, fed by its
  own writer goroutine); a client whose socket buffer fills gets dropped rather than blocking the daemon.
  Done when: a CLI-side test client attaches to the daemon's socket and receives the same DISPATCH events the
  daemon itself gets from the real gateway; a deliberately frozen test client gets dropped without stalling
  delivery to a second, healthy attached client.
- **M21 — Config file**: the shared TOML config (`pelletier/go-toml` v2, document-editing mode for
  comment-preserving programmatic writes), atomic writes (temp file plus rename) plus `gofrs/flock`-based
  locking around each read-modify-write cycle from every writer, the `fsnotify`-based hot-reload in the
  daemon, and the shared theme spec (named roles mapped to ANSI/native color, defined here, consumed later by
  the CLI at M42 and the GUI at M73). Also: the config-file split (hand-editable `config.toml` vs. the
  daemon-owned state file for plugin grants/hashes and the voice breadcrumb), the same-machine CLI/GUI
  separate-config toggle (living in daemon state, copying shared state on enable and reconciling via
  last-write-wins on disable), and `app config export`/`app config import` subcommands (covering
  `config.toml` scope only, import merging key-by-key). Done when: hand-editing the config file in a text
  editor while the daemon runs takes effect without a restart; `app config set` never destroys hand-written
  comments elsewhere in the file; two near-simultaneous writers no longer race thanks to the file lock;
  flipping the same-machine toggle on/off preserves existing customization; and `app config export` on one
  machine followed by `app config import` on another correctly carries over theme/keybindings/pane-layout
  without disturbing the target's own existing settings.
- **M22 — Local bot-automation port**: the separate localhost-only TCP listener with a per-session secret
  (`0600` file or environment variable), authenticated via scoped `api_tokens` (M4), messages sent through it
  tagged via the `messages.type` "sent via automation" reserved value. Done when: an external script with a
  valid scoped token can send a message via the local port, and it renders visually tagged as automated.
- **M23 — Daemon lifecycle polish**: OS-service auto-install across all three platforms, a startup
  `RLIMIT_NOFILE` raise (`syscall.Setrlimit`, to a safe ceiling such as 4096) before any IPC/network/subprocess
  handle is opened, log-file-not-stderr logging with `natefinch/lumberjack` rotation, `app logs tail`. Done
  when: the daemon survives a full reboot and comes back up automatically; its log file rotates instead of
  growing unbounded; and a test forcing many simultaneous attach-client/voice-worker/log handles open does not
  hit the OS's default file-descriptor ceiling.
- **M24 — Client auto-update mechanism**: version-check endpoint polling; Sigstore/cosign signature
  verification, using self-contained offline-verifiable bundles, before any binary swap; anti-downgrade
  protection; fail-closed behavior on verify failure; auto-rollback on repeated crash-loop after an update;
  surfaced to the user in CLI/GUI. Depends on M23. Done when: a signed test release installs and the daemon
  swaps to it, verified with no network call beyond the version-check/download itself; an unsigned or
  downgrade-attempt release is rejected and the current version keeps running; and a deliberately-crashing
  "bad" release triggers automatic rollback to the previous binary. Once Phase E exists, this milestone's
  guard additionally defers applying a downloaded update while the daemon is tracking an active voice session,
  applying it only once the call ends (Section 6).

#### Phase E — Voice

- **M25 — Mic-permission and global-hotkey spike (throwaway, time-boxed)**: a prototype determining whether a
  headless voice-worker process can reuse a foreground CLI/GUI's OS mic-permission grant (especially macOS
  TCC), and whether the headless daemon process can register an OS-wide global hotkey on each target OS
  (including the macOS Input Monitoring entitlement). This produces a written finding, not shippable code —
  its answer determines the exact shape of M32 and the daemon's hotkey-registration approach in M35. Do not
  proceed to M28–M32's detailed design before this completes.
- **M26 — Pion SFU core**: room/participant model, codec/track-kind-agnostic RTP forwarding (built
  generically from day one so M98–M99's video activation is additive, not a redesign). Done when: two test
  clients can exchange audio through the SFU.
- **M27 — Embedded TURN plus STUN**: `pion/turn` embedded in the backend (server-side, not the daemon),
  serving both TURN relay and plain STUN binding requests. Done when: a client behind simulated NAT can
  establish a voice connection via the embedded TURN server alone.
- **M28 — Voice-worker subprocess and IPC**: the daemon spawns the worker on-demand (via `os/exec`) when
  joining a voice channel, tears it down on leave; daemon↔worker control-plane IPC uses the child's inherited
  stdin/stdout pipes, framed with the same 4-byte-length-prefix-plus-JSON scheme used for the daemon↔CLI/GUI
  Unix socket (M20). Done when: joining voice spawns the worker, leaving voice cleanly tears it down, and the
  daemon detects a worker crash via the closed pipe.
- **M29 — Opus audio pipeline**: `hraban/opus` (cgo) capture/encode/decode/playback in the voice-worker.
  Depends on M25's finding for how mic access is actually obtained. Done when: two voice-worker instances can
  exchange intelligible audio through the SFU from M26.
- **M30 — Audio DSP**: RNNoise (noise suppression) plus `libspeexdsp` (echo cancellation plus AGC), both cgo,
  contained entirely to the voice-worker binary, wired in this exact order: Mic Capture → AEC → RNNoise → AGC
  → Opus Encode. Done when: a recorded test with background noise/echo shows measurable suppression versus the
  unprocessed pipeline from M29, and a two-party echo test confirms AEC correctly cancels far-end audio (i.e.
  it runs on the linear signal, before RNNoise's non-linear processing touches it).
- **M31 — Adaptive bitrate and congestion control**: `pion/interceptor`'s REMB/TWCC feedback wired into
  `hraban/opus`'s runtime bitrate control, audio-only, no simulcast. Depends on M26, M29, and M30. Done when:
  a simulated packet-loss/bandwidth-constrained test shows the encoder's bitrate measurably drop and recover
  in response, rather than staying fixed regardless of network conditions.
- **M32 — Mic-permission handoff, real implementation**: built per M25's actual finding — either the
  foreground-client-triggers-prompt design as originally intended, or whatever the spike determined actually
  works. Done when: first voice use on a clean OS install prompts for mic permission exactly once, and the
  worker successfully captures audio afterward.
- **M33 — Voice signaling, real**: `VOICE_STATE_UPDATE`/`VOICE_SERVER_UPDATE` go from inert stub to real,
  `MediaCoordinator` becomes `PionMediaCoordinator`, and the `supports_video: bool` capability field is added
  to the voice-join payload now (CLI always sends `false`) even though nothing consumes it until M97. Done
  when: joining a voice channel via the real gateway path (not a test harness) triggers real SFU allocation
  end to end.
- **M34 — CLI voice controls**: join/leave/mute/deafen via keybinding, a status line (no visual call UI —
  it's a terminal), an active-speaker indicator in the status line/participant list, and two separate
  keybinds for local-mute and report actions. Done when: a user can fully control a voice session from the
  CLI TUI, see who is currently speaking, and locally mute or report a participant independently of each
  other.
- **M35 — Voice input mode**: voice-activity-detection is the default; push-to-talk via
  `golang.design/x/hotkey`, registered by the daemon (per M25's finding), as a true OS-wide global hotkey,
  including the macOS Input Monitoring entitlement flow. Done when: push-to-talk works while the CLI/GUI is
  unfocused (e.g. alt-tabbed into another app), and registering the hotkey from both a CLI and a GUI attach
  session simultaneously does not double-register or double-trigger.
- **M36 — Voice auto-rejoin**: the "last active voice channel" breadcrumb (the one exception to in-memory
  daemon state), the daemon respawns the worker and rejoins on crash/restart. Done when: killing the daemon
  process mid-call results in automatic rejoin within a few seconds of restart.
- **M37 — Voice deployment opt-out**: the Instance Admin config toggle (also added to `app instance init`,
  extending M2/M10), the SFU/TURN never start when off, voice+text channels degrade to text-only, voice UI is
  hidden entirely (not grayed out) in CLI/GUI when disabled. Done when: an instance with voice disabled shows
  no voice-related UI anywhere and never starts the SFU/TURN processes.

#### Phase F — Presence, Deep Work, CLI polish

- **M38 — Presence persistence**: the `presence_status` table (including the Deep Work value), replacing
  the in-memory-only original design. Done when: presence survives a backend restart.
- **M39 — Deep Work**: server-side notification-suppression logic, the `@urgent` mention bypass, and the
  optional per-user opt-in email fallback for an `@urgent` mention when the account has no live gateway
  connection (fires regardless of whether Deep Work is active; requires SMTP configured per M5). Depends on
  M5 for the SMTP path. Done when: enabling Deep Work suppresses a normal mention's push, and an `@urgent`
  mention still comes through; disconnecting every client and triggering an `@urgent` mention with the
  fallback enabled delivers an email instead.
- **M40 — OS desktop notifications**: `gen2brain/beeep` wiring, triggered by `@urgent`-during-Deep-Work and
  regular mentions. Done when: a real OS toast/notification-center alert appears for both cases.
- **M41 — CLI pane engine**: the custom Bubble Tea/Lip Gloss/Bubbles split-pane implementation, a fully
  flexible pane-content model (any pane shows any channel/DM). Done when: a user can split the terminal into
  multiple panes, each independently pointed at a different channel/DM.
- **M42 — CLI keybindings**: the Emacs-style chorded scheme as the shipped default, applied to navigation and
  pane management; consumes the shared theme spec from M21; stored in and overridable via the config file's
  `[cli]` section. Done when: the documented default keybinding set is fully wired, and a user-supplied remap
  in the config file's `[cli]` section takes effect without requiring a rebuild.
- **M43 — CLI markdown renderer**: the small custom allow-list-only renderer (bold/italic/code/links/
  mentions), not `glamour`. Done when: it renders exactly the allow-listed subset and nothing more, verified
  against a test corpus of disallowed markdown.
- **M44 — CLI terminal-safe sanitization layer**: the blanket function stripping/escaping control characters
  and ANSI escape sequences, applied at the single point where any untrusted text meets terminal output
  (usernames, messages, link-preview titles, plugin manifest descriptions, webhook display names). Done when:
  a test string containing raw escape sequences renders harmlessly instead of manipulating the terminal.
- **M45 — CLI image rendering**: `BourgeoisBear/rasterm`-based capability detection (Kitty/iTerm2/Sixel),
  inline rendering with filename/link fallback, the "disable image loading" toggle hook point. Done when: the
  same attachment renders inline on a capable terminal and as a link on an incapable one.
- **M46 — CLI `--json` output and contracts**: every data-printing command gains `--json`, schemas versioned
  in `contracts/` as the third source-of-truth artifact alongside `openapi.yaml`/`gateway-events.schema.json`.
  Done when: a script can reliably parse `--json` output against its documented schema.
- **M47 — CLI logging**: file-based logging (never stderr, to avoid corrupting Bubble Tea's alternate-screen
  rendering), `app logs tail`, `lumberjack` rotation reused from M23. Done when: logs never appear on-screen
  during normal TUI use and are viewable via the tail command.
- **M48 — CLI TUI testing**: `teatest`-based tests for the pane engine and keybindings from M41/M42. Done
  when: key-press simulation tests cover pane split/focus/navigation.

#### Phase G — DMs, invites, attachments, chat power features

- **M49 — DMs/Group DMs/invites**: `DM`/`GROUP_DM` channel types, `channel_recipients`, guild invite codes
  (existing pattern). Done when: a user can DM another user and create/redeem a guild invite.
- **M50 — Attachments storage**: the pluggable storage interface (local disk default), server-side
  content-type sniffing, size/rate limits. The `minio-go` S3-compatible backend is wired in as the alternate
  implementation (used later by the flagship at M107, self-hosted instances never touch it). Done when: an
  attachment uploads, is retrievable, and its declared content-type is never trusted from the client.
- **M51 — Per-guild custom emoji**: the `guild_emojis` table, `PermManageEmojis`, static-image-only upload
  validation (resolution cap, format allow-list, decompression-bomb guard), and shortcode resolution added to
  the CLI markdown renderer (retrofitting M43) and to the GUI/web renderers whenever each is built. Depends on
  M50. Done when: a guild member with permission can upload a custom emoji and it renders correctly (not as a
  raw shortcode) in that guild's messages across every client that exists so far; and an oversized, malformed,
  or animated-format upload is rejected with a clear error, not silently accepted.
- **M52 — Incoming webhooks**: the `webhooks` table, `PermManageWebhooks`, the
  `POST /webhooks/{id}/{token}` endpoint with per-message name/avatar override support, automation-tagged
  messages via the existing `messages.type` reserved value routed through the same rendering/sanitization
  pipeline as any other message, hashed high-entropy tokens with independent per-webhook rate limiting. Done
  when: a message posted to a valid webhook URL appears in the target channel with its per-message override
  applied, visually tagged as automated, and rendered/sanitized identically to a normal message; an invalid or
  revoked token is rejected; regenerating a webhook's token invalidates the old one without deleting the
  webhook; and a burst of posts against one valid token is throttled independently of the owning user's own
  REST rate limit.
- **M53 — Whispers**: the private, message-visibility-restricted-to-selected-recipients feature; not
  guild-audit-logged; excluded from E2E scope (enforced later once E2E exists, at M91); the break-glass
  schema exists now (a whisper is queryable by internal tooling) even though the Instance-Admin-facing
  break-glass view isn't built until M66. Done when: a whisper is visible only to its selected recipients and
  invisible to other channel members who could otherwise read the channel.
- **M54 — Regex notification filters**: server-side evaluation via Go's stdlib `regexp` (RE2), a
  pattern-length cap as defense-in-depth. Done when: a saved filter correctly matches/suppresses
  notifications server-side, including for a client that's currently offline.
- **M55 — Bandwidth/network performance toggles**: client-side settings (e.g. disable image loading, wired
  to the M45 rendering path). Done when: toggling the setting suppresses inline image rendering without
  affecting anything else, including custom-emoji rendering, which stays unaffected.
- **M56 — Link previews**: generic OpenGraph fetching (`PuerkitoBio/goquery`), GitHub-specific previews via
  `google/go-github` plus an API token, the SSRF-protected `DialContext` (rejects private/loopback/link-local
  resolved IPs at connect time) plus a request timeout and an `io.LimitReader` response-size cap. Done when: a
  pasted GitHub URL gets a rich preview, a pasted arbitrary URL gets a generic OpenGraph preview, and a pasted
  URL pointing at an internal/private address is refused.

#### Phase H — Search

- **M57 — Postgres full-text search**: `tsvector` generated columns plus GIN indexes plus `pg_trgm` on
  `messages.content` and tag/channel-topic text, a search REST endpoint, synchronous on the insert path for v1
  at both self-hosted and flagship scale. The async-queue-decoupled indexing path noted in Section 8.18 is a
  documented future upgrade for the flagship deployment specifically, not v1 scope. Done when: a guild-scoped
  search query returns relevant messages ranked reasonably, with the index verified via `EXPLAIN ANALYZE`.

#### Phase I — Public matchmaking, friends, blocks, Instance Admin

- **M58 — Public matchmaking channel type**: the guild-less top-level channel type, fixed platform ruleset
  (no custom roles/ownership), a voice-and-text pair (the voice half depends on Phase E being done), an
  instance-level toggle defaulting ON (extends `app instance init` again). Done when: a public channel can be
  created, joined without invite, and automatically removed once empty per its lifecycle rules.
- **M59 — Public matchmaking anti-abuse**: the minimum account-age/verification threshold gate, layered on
  top of existing rate limiting — including that limiting's global `/64` IPv6-subnet grouping (Section 10),
  so a griefer cannot bypass the gate by rotating through one IPv6 block; the email-verification half of this
  gate is unavailable if SMTP was never configured (M5), leaving only the account-age threshold in that case.
  Done when: a freshly-created account is blocked from joining/creating a public channel until it clears the
  age/verification threshold; a simulated burst from one IPv6 `/64` block is throttled as a single source;
  voice-side abuse in these channels is documented as having no recorded evidence to review, by design.
- **M60 — Recently-met list**: server-side storage, a 7–30 day retention window, integrated into account
  export/deletion. Done when: users who shared a public channel appear on each other's recently-met list, and
  the entries expire on schedule.
- **M61 — Friends system**: `friend_requests`/`friendships` tables, a mutual request/accept flow, explicitly
  not DM-gating (organizational label only). Depends on M60 (the action "recently met" leads to). Done when:
  a friend request must be accepted before two accounts show as friends, and DMing works regardless of friend
  status either way.
- **M62 — Block/mute system**: the `blocks` table; enforcement in DM/whisper send paths (a silent,
  non-revealing failure, with no distinguishable signal that a rejection was specifically a block) and
  presence visibility; server-side gateway-dispatch filtering (a cached per-connection block-set, invalidated
  immediately on block/unblock) of a blocked account's messages/presence in shared guild channels, plus
  suppression of their `@mention` notifications there; auto-unfriend plus recently-met removal (and future
  recently-met suppression) on block; automation-token enforcement; export-symmetry (blocked-by-you included,
  blocked-you excluded). Depends on M49 (DMs), M60 (recently-met), M61 (friends), and M18 (the gateway
  protocol core, for the dispatch-filtering mechanism). Done when: a blocked account's DM/whisper attempt
  fails with no distinguishable signal that it was specifically a block; their guild-channel
  messages/presence never reach the blocker's gateway connection at all, verified by inspecting the actual
  DISPATCH stream, not just client rendering; a load test confirms the per-connection block-set check does not
  regress message fan-out latency; blocking removes any existing friendship; and an account export includes
  who the user blocked but not who blocked them.
- **M63 — Instance Admin tier, schema**: the boolean/flag-based tier (supports multiple admins per instance),
  sitting outside `roles.Resolve` entirely, with the last-admin-removal safety rail. Done when: granting or
  revoking the tier works, and removing the last remaining admin is blocked.
- **M64 — Instance Admin bans, enforcement, and audit log**: `instance_bans` (full account suspension, an
  optional `expires_at`), enforcement via the M11 revoke-all-sessions primitive (force-close plus revoke
  tokens; already-issued short-lived access tokens expire naturally per the stateless-JWT design), and
  `instance_audit_log` recording every Instance Admin action. Done when: issuing a ban immediately
  disconnects the account everywhere and blocks re-authentication, and the action is logged.
- **M65 — Instance Admin lockout recovery**: the server-side recovery CLI command
  (`app instance grant-admin <email>`, filesystem-access-gated). Done when: it successfully regrants the tier
  on a test instance with zero remaining admins.
- **M66 — Instance-level reports routing and whisper break-glass**: wires the M16 reports system's
  instance-scoped half — public-matchmaking, whisper, and plain-DM/Group-DM reports (none of which have a
  guild owner to escalate to) all route to an Instance Admin triage queue; whisper content becomes visible
  only attached to a specific filed report, itself audit-logged; report filing is rate-limited, and the
  triage view shows reporter history; the data-export asymmetry applies (filed-by-you included,
  filed-against-you excluded). Depends on M49 (DMs/Group DMs) and M53 (whispers). Done when: an Instance Admin
  can review a filed report on a whisper and that specific access is itself an audit-log entry, and a report
  filed against a plain DM or Group DM also reaches the Instance Admin triage queue.
- **M67 — Instance Admin proactive intervention**: report-less intervention capability
  (legal/compliance), gated by a mandatory logged justification field distinguishing it from
  report-triggered entries. Done when: a proactive action is blocked without a justification string and
  logged distinctly when one is given.
- **M68 — Public-channel/whisper retention windows**: the 48-hour post-empty retention on public channel
  history (and whispers exchanged within it) before permanent purge, for report-investigation purposes. Done
  when: a channel's history remains queryable by Instance Admins for 48 hours after it empties, then is gone.
- **M69 — Data export asymmetry verification**: an end-to-end test that a user's own export includes their
  filed reports and blocked accounts, and excludes reports filed against them and who has blocked them. Done
  when: both asymmetries are covered by an automated test, not just documented intent.

#### Phase J — Native GUI

- **M70 — GUI skeleton**: the Gio app scaffold, attaching to the daemon via the same local socket/protocol as
  the CLI (M20). Done when: the GUI receives the same DISPATCH events the CLI does, from the same daemon.
- **M71 — GUI message rendering**: the virtualized message list, the allow-list markdown renderer
  reimplemented for Gio's immediate-mode primitives (the same allow-list as M43, including emoji-shortcode
  resolution, on a different rendering target). Done when: a long channel scrolls smoothly and renders the
  same allow-listed markdown subset as the CLI.
- **M72 — GUI pane splitting**: native widget-based tiling, the same flexible pane-content model as M41. Done
  when: a user can split the GUI window into independently-addressable panes.
- **M73 — GUI theming**: the shared theme spec (M21) mapped to Gio's native rendering. Done when: a theme
  change in the config file is reflected identically in spirit across CLI and GUI.
- **M74 — GUI settings and voice device tab**: config read/write via the same `go-toml` v2 document-editing
  approach as the CLI, plus the voice input/output device-selection settings tab. Done when: a setting
  changed in the GUI is correctly reflected when the CLI next reads the config.
- **M75 — GUI voice UI**: participant list, mute/deafen controls, an active-speaker indicator (a highlight/
  ring around whoever is transmitting), and separate local-mute and report actions, wired to the same
  voice-worker control path as the CLI (M34). Done when: joining voice from the GUI shows the same
  participant state the CLI would, including which participant is currently speaking, and mute/report work
  as two independent actions.
- **M76 — GUI testing**: golden-image/screenshot tests for the highest-value surfaces (message list,
  pane-split layout, voice UI states) plus documented manual-QA coverage for the rest. Done when: a
  deliberate rendering regression in a covered surface fails the golden-image comparison.
- **M77 — Integrated whiteboard**: GUI-only, solo local drafting (no real-time multi-user sync). Explicitly
  the lowest-priority item in this entire phase — built last, after M70–M76, never in parallel with the
  load-bearing GUI work. Done when: a user can open a blank canvas and draw/annotate locally.

#### Phase K — Dev tools and extensibility

- **M78 — Code block enhancements**: a copy button plus folding, in both CLI and GUI. Done when: a code block
  in either client can be copied with one action and collapsed/expanded.
- **M79 — Integrated shell**: spawns the user's actual shell, the same trust boundary as a real terminal (no
  extra sandboxing). Done when: a shell session opens inside the client and behaves like a normal terminal.
- **M80 — WASM plugin host**: `wazero` wiring inside the daemon, the host-function capability API surface
  defined (message-read, slash-command-registration, etc., each individually capability-gated). Done when: a
  minimal test plugin can call one host function successfully and is blocked from an ungranted one.
- **M81 — Plugin capability manifest**: the TOML `manifest.toml` format, the first-load user-approval UX, and
  SHA-256 hash-pinning of the approved `.wasm` file (stored alongside the capability grant in the daemon-owned
  state file, Section 5) with re-verification on every subsequent load. Done when: loading a new plugin shows
  its requested capabilities and requires explicit approval before it runs; swapping the `.wasm` file on disk
  after approval halts execution and re-prompts instead of silently running the swapped binary under the
  stale grant.
- **M82 — Plugin resource limits**: CPU (instruction-count/timeout) plus memory quotas per instance, plus a
  separate wall-clock timeout per invocation. Benchmark `wazero`'s instruction-metering overhead against real
  plugin workloads (e.g. a chat-filter on every incoming message); if metering costs more than the work it
  measures, memory quotas plus the wall-clock timeout become the primary safety mechanism instead of strict
  instruction counting. Done when: a deliberately runaway test plugin is killed by the quota/timeout without
  affecting the daemon's other responsibilities, and the metering-overhead benchmark's result is documented
  along with which enforcement mechanism v1 actually ships with.
- **M83 — Plugin distribution and TinyGo docs**: local-file-only distribution (drop a `.wasm` in a plugins
  folder), TinyGo documented as the recommended (not required) authoring toolchain. Done when: a TinyGo-built
  plugin loads and runs correctly following the documented steps.

#### Phase L — Self-hosting polish, P2P, ops

- **M84 — Backup/restore documentation**: `pg_dump`/`pg_restore` plus attachment-directory copy instructions
  for self-hosted instances, explicitly noting E2E key material lives client-side and isn't covered. Done
  when: following the documented steps produces a working restore on a test instance.
- **M85 — Prometheus metrics endpoint**: `prometheus/client_golang`, connection counts, message throughput,
  and voice/SFU call-health metrics (packet loss/bitrate/jitter, active once Phase E is done) — auth-gated
  (an Instance Admin token or localhost-only) with aggregate-only labels (no per-guild/per-channel
  identifiers). Done when: `/metrics` requires auth, exposes the documented metric set, and rejects
  unauthenticated access.
- **M86 — P2P file transfer**: explicit opt-in per transfer, the initiating client (CLI/GUI) owns the WebRTC
  negotiation directly (the daemon is not involved, the same rule as video), enforced as a real three-way
  handshake (server-relayed Intent-to-Transfer → recipient Accept → only then `RTCPeerConnection`/SDP/ICE).
  Done when: a large-file transfer between two consenting clients completes without going through server
  storage; inspecting network traffic confirms no ICE candidate (and therefore no IP address) reaches the
  recipient before they've explicitly accepted.
- **M87 — Container image scanning**: `Trivy` added to `just security-scan` (depends on Phase P's Dockerfiles
  existing). Done when: CI fails on a test image with a known critical vulnerability.
- **M88 — Self-hosted docker-compose production path**: extends the existing local-dev `docker-compose.yml`
  into a documented production option alongside bare-metal/systemd. Done when: a fresh self-hoster can stand
  up a working instance via `docker compose up` following the docs alone.

#### Phase M — E2E encryption

- **M89 — Crypto base integration**: verify `go.mau.fi/libsignal`'s license is compatible with the project's
  restrictive custom license (a blocking prerequisite step within this same milestone, documented and passed
  before any further work here proceeds); then integrate the library and build the device-linking protocol on
  top of it. Done when: the license check is documented and passed; two test identities can complete a key
  exchange and exchange messages with forward secrecy demonstrated via the library (rotating a key doesn't
  expose prior messages); and device-linking (fully custom) links a second device without per-conversation
  re-verification.
- **M90 — E2E keystore**: the `modernc.org/sqlite` local encrypted store, exclusively daemon-owned (Section 4's
  credential-ownership rule; no attach client holds a copy), the master key in the OS keychain via
  `zalando/go-keyring`, surviving daemon restarts. All keystore writes route through one dedicated writer
  goroutine fed by a buffered channel, so the WS event loop never blocks on disk I/O and a burst of concurrent
  incoming encrypted messages never produces a `database is locked` error. Also builds the mandatory
  `modernc.org/sqlite` FTS5 local search index over the decrypted message store, encrypted at rest via the
  same keystore master key. Done when: ratchet state persists correctly across a daemon restart
  mid-conversation; a burst of simultaneous incoming encrypted messages across different channels produces no
  lock contention error; and a local FTS5 query returns matching results from a user's own E2E-encrypted
  conversation history.
- **M91 — E2E opt-in UX**: strictly `DM`-channel-type-only enforcement (never `GROUP_DM`, never any guild
  channel), whispers explicitly excluded (enforced here, referencing M53). Done when: attempting to enable
  E2E on anything but a 1:1 DM is rejected.
- **M92 — Device-linking flow**: primary-device-authorizes-new-device, CLI-side text/code-based
  safety-number verification (no camera/QR). No history-transfer mechanism exists for the newly linked
  device — it sees only messages sent after linking, by design (Section 2/8.17). Done when: linking a second
  device grants it access to ongoing encrypted conversations without per-conversation re-verification, and
  the newly linked device's message history is confirmed empty prior to the link event.
- **M93 — Device revocation and E2E trust linkage**: logging out/revoking a device (M11's primitive) also
  revokes its E2E device-link trust. Done when: revoking a device's session also marks it untrusted for E2E
  purposes.
- **M94 — Ratchet and device-linking fuzz testing**: `go test -fuzz` targeting the M89 library-integration
  code and the device-linking protocol's message handling, with malformed/out-of-order/replayed inputs. Done
  when: the fuzz target runs cleanly for a defined corpus/duration with no crashes or invariant violations
  found.
- **M95 — External cryptographic security review (hard gate, not optional polish)**: a real external audit of
  the `go.mau.fi/libsignal` integration (correct use of the library, key-material handling around it) and the
  fully-custom device-linking protocol. A build/instance-level enforced flag makes E2E genuinely unavailable to
  any account beyond the developer's own test accounts until this milestone is marked complete — the flag,
  not a documentation policy, is what flips availability. E2E encryption must not be described as
  production-ready, marketed, or trusted with real user conversations until this milestone is complete. Done
  when: the flag blocks E2E for a normal account, and the audit sign-off is what flips it.
- **M96 — E2E-aware account data export**: the daemon-performed local decrypt-and-export step for E2E DMs
  (per the key-boundary rule in 8.17 — the daemon, not the CLI/GUI, holds the keys), producing a standalone
  `local_e2e_export.zip` presented alongside, never merged into, the server-side account export. Done when:
  exporting an account that has E2E conversations produces the separate local zip containing their decrypted
  content, never routed through the server, and the server-side export completes independently without
  attempting to merge it.

#### Phase N — Video/screen-share

- **M97 — GUI/web video connection**: a second, separate WebRTC connection to the SFU for the video track,
  owned directly by the GUI/web client (never the daemon), activating the `supports_video` flag from M33 and
  the codec-agnostic track model from M26. Done when: a GUI client can open a video track alongside its
  existing audio session with no daemon changes required.
- **M98 — Screen/camera capture and selection UI**: GUI-side capture and source-selection interface. Done
  when: a user can choose and share a specific window/screen or camera source.
- **M99 — SFU video track forwarding activation**: verify the already-track-kind-agnostic SFU from M26
  forwards video tracks (including screen-share as a second video track) without any SFU-side redesign, and
  design/implement simulcast/SVC spatial-temporal-layer switching so a participant on a poor connection
  doesn't degrade the whole call — the track-agnostic model from M26 made this additive rather than a
  redesign, but did not implement it, and it is real remaining engineering work (Section 2). Done when:
  multiple participants can see each other's video/screen-share simultaneously through the SFU, and a
  simulated poor-connection participant causes the SFU to drop to a lower layer for them without degrading
  other participants' streams.

#### Phase O — Web SPA (later, third-priority client)

- **M100 — BFF cookie-exchange auth layer**: the httpOnly-cookie-issuing layer in front of the token API,
  designed earlier but built only now. Done when: the web SPA can log in and receive a session cookie without
  ever holding a raw Bearer token in JS.
- **M101 — Web SPA rebuild**: adapt the originally-planned React SPA to the current backend/contracts.
- **M102 — Web SPA pane-splitting**: CSS grid/flex-based resizable panes, `localStorage`-based layout
  persistence, independent of the CLI/GUI layouts.
- **M103 — Web SPA E2E export**: the browser-side decrypt-and-export equivalent of M96.

#### Phase P — Flagship instance Kubernetes deployment (parallel track, not sequential with the feature
phases above)

This phase is a deployment target, not a feature-development phase — it can start once core messaging and
voice are usable (roughly after M37), well before every feature phase above is complete, and continues to
absorb new features (public matchmaking, E2E, etc.) as they land. Do not read it as coming "after" M103.

- **M104 — Helm chart skeleton and API pods**: the base chart structure, the API/gateway `Deployment` behind
  an Ingress.
- **M105 — CloudNativePG plus backups**: the Postgres operator in-cluster, native continuous backup/
  WAL-archiving to in-cluster MinIO (M107).
- **M106 — Redis in-cluster and event-bus/rate-limit activation**: activates the previously-reserved Redis
  pub/sub fan-out (required the moment multiple API replicas run) and switches `ulule/limiter` to its
  Redis-backed store for this deployment specifically.
- **M107 — MinIO in-cluster**: the object storage backend for attachments (M50) and Postgres backups (M105).
- **M108 — TURN/SFU pods**: `hostNetwork: true`, in their own dedicated "privileged" Pod-Security-Standard
  namespace, separate from the "restricted" API/backend/Postgres/Redis/MinIO namespace.
- **M109 — `cert-manager` plus Ingress TLS**: disables the backend's built-in `certmagic` path for this
  deployment specifically (self-hosted instances keep it).
- **M110 — Graceful rollout**: a `preStop` hook sending the gateway's existing `Reconnect` op-code to local
  connections before pod termination, staggered across `terminationGracePeriodSeconds` rather than fired all
  at once, paired with randomized exponential backoff in the daemon's `Reconnect` handling. Done when: a
  simulated rollout of a replica holding thousands of connections does not produce a synchronized
  reconnect spike against the remaining replicas' auth/DB layer.
- **M111 — DB migration Job hook**: a Helm `pre-upgrade`/`pre-install` Job running `golang-migrate`, before new
  pods start.
- **M112 — Secrets**: plain Kubernetes Secrets applied via the Helm chart.
- **M113 — Autoscaling**: CPU/memory-based HPA via `metrics-server` (connection-count-based scaling
  documented as a future upgrade once M85's metrics are actually scraped by something in-cluster).
- **M114 — NetworkPolicies and namespace isolation**: baseline pod-to-pod traffic restriction.
- **M115 — CI-triggered `helm upgrade`**: a simple CI pipeline deployment trigger, not GitOps.

#### M116–M117 — Gap-closure milestones (numerically appended, logically earlier — same treatment as Phase P)

These two milestones introduce schema/feature scope no earlier milestone covers. Like Phase P, they sit at
the numeric end of the roadmap but are not meant to be read as coming chronologically "after" M115 — each is
annotated below with where it actually belongs.

- **M116 — Read-state sync**: the `channel_read_states` table (per-user, per-channel watermark), a
  REST/gateway path to update it, and a new gateway dispatch event so every attached client — including a
  second daemon on a different machine — reflects accurate unread state, distinct from the daemon's ephemeral
  in-memory scroll/pane state (Section 5). A channel is marked read automatically when a client's viewport
  reaches the latest message, debounced. Conceptually belongs in Phase D/G — depends on M12 (channels exist)
  and M18 (gateway dispatch core). Done when: marking a channel read on one client is reflected as read when a
  second client (or a second daemon on another machine) next syncs, without a separate mark-as-read action.
- **M117 — Data retention / audit-log pruning**: the configurable pruning seam from Section 10, wired into
  `app instance init`/`app config set`, default-disabled, scoped to `audit_log_entries` and
  `instance_audit_log` only — message history and reports are explicitly not covered and stay permanent by
  design. Conceptually belongs in Phase L (self-hosting ops polish) — depends on M14 (audit log) and M64
  (instance audit log) existing, no other hard dependency. Done when: enabling a retention window on a test
  instance prunes entries older than the window and leaves newer ones (and all message/report data,
  unconditionally) untouched; leaving it disabled (the default) prunes nothing.

**Dependency notes:** M37 (voice opt-out) must exist before M58 (public matchmaking, which needs the
voice+text pair to degrade gracefully). M53 (whispers) must exist before M66 (its Instance-Admin-facing
break-glass view) and before M91 (which excludes whispers from E2E scope). M60 (recently-met) must exist
before M61 (friends). M49 (DMs), M60 (recently-met), and M61 (friends) must exist before M62 (blocks). M11
(revoke-all-sessions) must exist before M64 (bans) and M93 (device revocation↔E2E trust). M50 (attachments)
must exist before M51 (custom emoji). M89's license-compatibility check must pass before any further work in
Phase M proceeds. Phase P (Kubernetes) depends on M106 requiring Phase D's Redis-fan-out design to already
exist as a seam, and M50/M105 requiring M107 (MinIO) to be stood up first within that phase. M116 depends on
M12 and M18, conceptually belonging in Phase D/G despite its number; M117 depends on M14 and M64, conceptually
belonging in Phase L despite its number — the same "numerically-late, logically-earlier" treatment already
established for Phase P above.

## 14. Verification

This is a documentation-only plan (no running code exists yet), so verification means an internal-consistency
pass over the rewritten `CLAUDE.md`, `docs/architecture.md`, and `docs/adr/`, not tests:

- Grep the updated docs and ADRs for "AGPL," "open source," "cookie," "CSRF," "JWT," and "frontend" to
  confirm every remaining mention is scoped correctly: licensing language matches the source-available model;
  cookie/CSRF language is scoped to the future web SPA's BFF layer only; "frontend" clearly reads as "the
  later, tertiary web client." Also grep for "voice" and "deferred" to confirm no text still claims audio
  voice is deferred or inert — only video/screen-share should read as deferred-but-seamed.
- Grep for "X3DH," "hand-rolled ratchet," and "client-side" (in the context of blocks/guild-channel
  enforcement, and in the context of E2E key-holding/decryption) to catch the edit sites most likely to carry
  stale language: the E2E crypto base is `go.mau.fi/libsignal`, not a from-scratch X3DH/ratchet build; block
  enforcement in shared guild channels is server-side gateway-dispatch filtering, never described as
  client-side-only; and E2E key material/decryption is **daemon**-held and daemon-performed, never described
  as "the CLI/GUI hold the keys" or "the client decrypts" — that phrasing was the actual contradiction this
  plan's gap-closure pass resolved (Section 8.17), so any surviving instance of it is stale.
- Grep for "block," "SMTP," "password reset," "emoji," "webhook," "bitrate," "libsignal," "cosign," and
  "auto-update" to confirm each concept appears consistently across `CLAUDE.md`, `docs/architecture.md`, and
  its relevant ADR — no concept should appear in only one of the three.
- Confirm every milestone number referenced in prose anywhere in `CLAUDE.md`, `docs/architecture.md`, and
  `docs/adr/` matches Section 13's numbering exactly (M0 through M117, per the phase breakdown above,
  including the two gap-closure milestones appended after M115).
- Confirm the license file's Ed25519-JWT signing scheme and the release-binary Sigstore/cosign signing scheme
  are described as two distinct mechanisms for two distinct reasons, never merged into one description.
- Confirm every new concept introduced in this plan (the daemon, the dual IPC channels, presence status,
  public channels, recently-met, message tags, whispers, notification filters, `api_tokens`, the plugin host,
  E2E seams, blocks, custom emoji, webhooks, the SMTP/email opt-out) appears consistently across `CLAUDE.md`,
  `docs/architecture.md`, and its relevant ADR — no orphaned concept in only one document.
- Grep for `channel_read_states`, `device_id` (refresh-token families), `RLIMIT_NOFILE`, "advisory lock,"
  "retention"/"pruning," `/64`, "active-speaker," plugin hash-pinning, `ulule/limiter`, and the same-machine
  config toggle — the concepts introduced by this pass's gap-closure edits — to confirm each appears
  consistently across `CLAUDE.md`, `docs/architecture.md`, and its relevant ADR once those are rewritten, the
  same standard applied to every other concept above.
