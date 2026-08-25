# Milestone roadmap

The authoritative, dependency-ordered build sequence: `M0` through `M125`, phase-grouped. This file is the
single source of truth for milestone numbering, scope, and "done when" criteria — `docs/architecture.md`
§13 points here rather than restating it, so a milestone only ever needs editing in one place.

Read it alongside `docs/architecture.md` (what the system *is*) and `docs/adr/` (why the contested
decisions went the way they did). Completion status lives in `CLAUDE.md` and `README.md`, not here: this
file describes the plan, not the progress.

---

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
  `architecture.md` §11's "Database connection management" guidance), `golang-migrate` wiring (an empty
  initial migration) that auto-runs on startup guarded by a Postgres advisory lock and blocks `/healthz`
  until it completes (`architecture.md` §2), `zerolog` structured logging wired in, base `ulule/limiter`
  rate-limiting middleware (in-memory store, global `/64` IPv6 subnet grouping per `architecture.md` §11's
  "Rate limiting" paragraph), a `/healthz` endpoint. Done when: the backend binary starts, blocks on a
  pending migration until it completes, connects to Postgres, and `/healthz` returns 200; a burst of
  requests from one IPv6 `/64` block is throttled as a single source.
- **M2 — CLI skeleton and `norite instance init`, infrastructure config only**: the `norite` binary builds
  and runs on a `urfave/cli` v3 command tree, with `--json`/`--help` flag plumbing and shell completions
  wired once for every command that follows (`architecture.md` §4) — no functional client commands yet. On
  that foundation, the setup wizard: DB-connection, storage-backend (local disk vs. S3/MinIO), ACME on/off
  (plus the LAN-only opt-out), and registration-gating prompts — plain sequential stdin/stdout prompts, not
  a full-screen TUI — writing a valid config file. **This milestone also decides and documents the instance
  config file's format, path, and precedence relative to the backend's `NORITE_*` environment variables**,
  writing the outcome into `architecture.md` §4; nothing before M2 fixes that choice. Explicitly does not
  include first-admin-account creation yet (no `users` table exists until M4) — that step is added at M10.
  Both quick-start and `--full` modes are stubbed now, fleshed out as later milestones add more prompts (the
  SMTP prompt at M5, the voice opt-out at M37, the public-matchmaking toggle at M66). Done when:
  `norite --help` lists the command tree, and running the wizard produces an instance config the backend
  actually loads.
- **M3 — Daemon lifecycle stub**: a daemon stub that installs itself as an OS-level service
  (systemd/launchd/ Windows task) but does nothing beyond starting and stopping cleanly, plus the `norite`
  subcommands that drive that install/start/stop. Depends on M2 for the CLI command tree those subcommands
  mount into. Done when: `norite` installs and starts a daemon that appears as a running OS service.

  Built as `norite daemon install | uninstall | start | stop | restart | status` over a per-platform
  `Manager` (`cli/internal/daemonctl`), always installing a *user*-scoped service so no step needs
  elevation, plus the daemon's own startup/shutdown sequence: single-instance `flock`, `RLIMIT_NOFILE`
  raise, rotating log, clean signal-initiated stop that exits 0 so no service manager mistakes it for a
  crash. See `architecture.md` §3 for the settled details.

#### Phase B — Auth

- **M4 — Backend auth core**: `users` table, `argon2id` password hashing, JWT access-token issuance
  (`golang-jwt`, 15-minute TTL), refresh-token issuance/hashing/rotation scoped per `device_id` (rotating one
  device's token must never invalidate another device's refresh-token family), the `api_tokens` (scoped) table
  plus validation middleware. Done when: a REST call with a valid password can obtain an access-plus-refresh
  token pair, a scoped `api_tokens` row can authenticate a request restricted to its granted scope only, and
  rotating one device's refresh token leaves a second device's own token family valid.
- **M5 — Transactional email (SMTP) and password reset**: `norite instance init` gains the SMTP prompt
  (skipped by default in quick-start, prompted explicitly in `--full`), the `password_reset_tokens` table, and
  reset-request/confirm endpoints with anti-enumeration and rate-limiting, using `wneessen/go-mail` for
  sending. Email sending is asynchronous/backgrounded, never synchronous in the request path. Depends on M4
  (the `users` table). Done when: a password reset completes end-to-end via a real SMTP relay; a reset request
  for a non-existent email returns a response identical to one for a real email; and email sending never
  blocks the HTTP response even against a deliberately slow SMTP relay in a test.
- **M6 — OAuth backend flow**: `golang.org/x/oauth2` wiring for Google and GitHub, an account-linking table,
  callback handling. Done when: completing a Google or GitHub OAuth flow against the backend issues a valid
  token pair.
- **M7 — CLI `norite login`, password plus keychain**: a direct in-terminal password prompt, storing the
  resulting tokens via `zalando/go-keyring` — and, where the machine has no keyring at all, in a `0600` file
  in the daemon's `0700` per-user state directory instead, since a headless Linux server has no Secret
  Service and that is precisely the kind of machine this CLI exists for
  ([ADR 0025](adr/0025-credential-storage-without-a-keyring.md)). Done when: `norite login` with a password
  succeeds, and the daemon can use the stored token on next launch without re-prompting.
- **M8 — CLI OAuth loopback flow**: the system-browser-plus-loopback-listener login. The client binds a
  local port, names it at `/authorize`, and the **instance's** callback returns the one-time exchange code
  there instead of rendering it. Requires a backend half M6 did not build — an optional `client_redirect_uri`
  validated by host, carried on the `oauth_states` row and across the sign-up form inside the signed
  continuation token (ADR 0027). What is registered with Google and GitHub is unchanged and stays the
  instance's own callback; the fixed primary port plus fallback list is a client-side convention, which is
  the correction ADR 0027 records. Done when: `norite login --provider` opens a browser, completes Google or
  GitHub OAuth, and stores the resulting token via the same keychain path as M7 — including for an account
  that does not exist yet, where the sign-up form still returns to the listener; if the primary port is
  occupied the CLI falls back to the next one and only fails with a clear "free this port and retry" error
  naming every port tried once the list is exhausted; and a declined consent is reported promptly rather
  than waited out. The listener mints a `flow_verifier` and passes its challenge to `/authorize`, exactly as
  any other client must (M6, ADR 0024) — that binding is what stops a crafted callback URL delivering
  somebody else's exchange code into a listener that would otherwise redeem it without the user doing
  anything at all, and it is also what makes validating the return URI by host alone sufficient.
- **M9 — CLI headless device-code fallback**: done (tag `m9`). The `device_codes` table, the
  server-rendered verification page at `/device`, and CLI headless-context detection plus polling logic.
  Depends on M6 (OAuth) and M8 (loopback, to detect when to fall back from it).

  **`--no-browser` is not the trigger, and this is the correction M8 forces.** That flag means "do not
  launch anything, print the URL, keep listening", which is a working flow for SSH with a forwarded port
  and is what M8 ships; overloading it to mean "use a device code instead" would give one flag two
  incompatible behaviours. The device code is what a machine with **no browser reachable at all** falls
  back to — detected, not asked for — which is the case M8 currently fails fast on. This refines ADR 0011,
  which named `--no-browser` as the trigger before either flow existed. Give the deliberate choice its own
  flag if one is wanted — it is `--device-code`. Done when: a login on a host with no browser displays a
  code instead of binding a listener, and completing it on a separate device with a browser finishes the
  login on the original CLI session.

  **The verification page offers providers, not only a password**, and that is the scope call worth
  recording. `norite login` already does headless password sign-in, so a password-only page would add
  almost nothing — while an account that signs in only with Google or GitHub had no way onto a server at
  all, since M8's listener binds `127.0.0.1` and the phone completing the flow cannot reach it. Decisions
  in ADR 0028, including why approval is a separate explicit step, why the user code is the one
  credential-shaped value stored in plaintext, and why the poll is a POST where §2 sketched a GET.
- **M10 — `norite instance init` finish, and registration hardening**: adds the first-admin-account-creation
  step (now that M4 exists) and wires up instance-level registration gating (the `instance_invites` table
  plus enforcement at registration). Depends on M2 and M4.

  **Also closes the two registration gaps M6 surfaced and could not fix from where it sat**, both of which
  come down to this instance having no way to verify an address itself — `users.email_verified_at` has
  existed since M4 and only the OAuth path ever sets it:

  - *Registration is an account-existence oracle.* `POST /auth/register` answers 409 "that email is already
    registered", so anyone can probe any address. That is the whole reason M6 had to merge its two
    unverified-address refusals into one message (ADR 0024) — an OAuth-side fix would have closed the
    smaller hole while this one stayed open. The answer is the same shape M5 already uses for reset: accept
    the registration identically either way, send mail that differs (a verification link, or a "someone
    tried to register with your address; you already have an account" notice), and let the account become
    usable only once the link is followed. Anti-enumeration then holds across register, reset *and* OAuth
    rather than two of the three.
  - *An address a provider will not vouch for is refused outright.* M6 has no alternative, since it cannot
    verify anything itself. GitHub in particular lets an account hold entirely unverified addresses, so
    those users cannot sign in at all today. With verification here, the refusal becomes a detour: create
    the account against our own verification instead of the provider's, and the linking rule (ADR 0024) is
    satisfied by evidence we gathered rather than evidence we were denied.

  Depends on M5 for the mail queue. Done when: a fresh instance can only be bootstrapped via the wizard,
  ending with one working admin account; normal registration requires a valid instance invite code if
  gating is on; **registering an address that already has an account is indistinguishable, in status, body
  and timing, from registering a new one**; an account created by password cannot sign in until its address
  is verified; and a provider identity whose address is unverified completes through this instance's own
  verification rather than being refused.

  **Done** (tag `m10`). Decisions in ADR 0029. First-administrator creation landed as a sibling command,
  `norite instance bootstrap`, rather than a step inside the wizard — when `init` finishes there is no
  running server to create an account on. It is authorized by an **operator token**: an unsubjected JWT
  signed with the instance's own key, which makes filesystem access to `instance.toml` the authority, so
  there is no window in which whoever reaches a fresh instance first becomes its administrator.
  `instance_admins` therefore lands here rather than at M71, which keeps grant/revoke and the last-admin
  rail.

  Two limitations are accepted and recorded rather than hidden: an instance with **no SMTP relay** creates
  accounts already verified, so the enumeration hole stays open there (it cannot verify an address by any
  route); and an **invite-only instance is password-registration-only**, there being nowhere to carry a
  code through a provider redirect.
- **M11 — Session revocation primitive**: the general-purpose "revoke all sessions/tokens for account X"
  mechanism (force-close live gateway connections, revoke refresh plus scoped tokens), exposed now as a
  self-service "log out all other devices" account-security feature. Ban-triggered use of this same
  primitive comes later at M72, once Instance Admin exists — the primitive itself is built now. Done when: a
  user can log out all their other sessions from one account action, and previously-issued refresh tokens
  immediately stop working.

#### Phase C — Guild/channel/permission core

- **M12 — Guilds/channels/roles schema plus CRUD**: the core guild/channel/role tables and REST endpoints.
  `oapi-codegen` against `openapi.yaml` is wired up starting here — every REST endpoint from this point on is
  generated, not just documented. **First job here is that the contract does not currently generate**, which
  is latent rather than new: `openapi.yaml` declares `openapi: 3.1.0` and expresses its three nullable
  fields as 3.1 type unions (`type: [string, "null"]`), which `oapi-codegen` v2 does not support — it warns
  that 3.1 is unimplemented and fails on the first such field. Nothing has noticed because no milestone
  generates from the document yet; `cmd/server/contract_test.go` checks routes against the router, not
  schemas. So M12 decides the version this project targets — downgrade to 3.0.x and use `nullable: true`,
  which is what the tool recommends, or stay on 3.1 and wait — before it can generate anything. Done when: a
  guild, its channels, and its roles can be created, read,
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
  Instance-Admin-facing half does not exist until M74. Report filing is rate-limited now (reuses existing
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
  Done when: the daemon alone (no CLI/TUI/GUI attached) stays connected and accumulates state correctly; a
  deliberately skewed system clock does not cause spurious auth failures.
- **M20 — Daemon↔client local IPC**: the Unix domain socket / named pipe, 4-byte-length-prefixed JSON
  framing, reusing the gateway's op-code/DISPATCH shape and one shared client-side event parser, and the
  semver MAJOR-must-match/MINOR-window version-compatibility handshake. The daemon's write path to each
  attach client is asynchronous and bounded (a per-connection outbound channel with fixed capacity, fed by its
  own writer goroutine); a client whose socket buffer fills gets dropped rather than blocking the daemon.
  Done when: a CLI-side test client attaches to the daemon's socket and receives the same DISPATCH events the
  daemon itself gets from the real gateway; a deliberately frozen test client gets dropped without stalling
  delivery to a second, healthy attached client.
- **M21 — Config file**: the shared TOML config (`pelletier/go-toml` v2, document-editing mode for
  comment-preserving programmatic writes), namespaced `[shared]` / `[tui]` / `[gui]` — there is no `[cli]`
  section, because the scriptable command tree has nothing to style and the section that once carried that
  name was always about chords and colours (ADR 0026) — atomic writes (temp file plus rename) plus
  `gofrs/flock`-based locking around each read-modify-write cycle from every writer — by then there are two
  implementations of that write to unify, `instanceinit.writeAtomically` (M2) and
  `credentials.writeFileAtomically` (M7), and **neither fsyncs the parent directory after the rename**, so
  the rename itself is not durable across a crash; one shared helper is one place to fix that, and it has to
  live where every writer can reach it (the `daemon` module, as `credentials` already does) — the
  `fsnotify`-based hot-reload in the daemon, and the shared theme spec (named roles mapped to ANSI/native
  color, defined here, consumed later by the TUI at M45 and the GUI at M81). Also: the config-file split
  (hand-editable `config.toml` vs. the daemon-owned state file for plugin grants/hashes and the voice
  breadcrumb), the same-machine CLI/TUI/GUI separate-config toggle (living in daemon state, copying shared
  state on enable and reconciling via last-write-wins on disable), and `norite config export` / `norite config import`
  subcommands (covering `config.toml` scope only, import merging key-by-key). Done when:
  hand-editing the config file in a text editor while the daemon runs takes effect without a restart;
  `norite config set` never destroys hand-written comments elsewhere in the file; two near-simultaneous
  writers no longer race thanks to the file lock; flipping the same-machine toggle on/off preserves existing
  customization; and `norite config export` on one machine followed by `norite config import` on another
  correctly carries over theme/keybindings/pane-layout without disturbing the target's own existing
  settings.
- **M22 — Local bot-automation port**: the separate localhost-only TCP listener with a per-session secret
  (`0600` file or environment variable), authenticated via scoped `api_tokens` (M4), messages sent through it
  tagged via the `messages.type` "sent via automation" reserved value. Done when: an external script with a
  valid scoped token can send a message via the local port, and it renders visually tagged as automated.
- **M23 — Daemon lifecycle polish**: OS-service auto-install across all three platforms, a startup
  `RLIMIT_NOFILE` raise (`syscall.Setrlimit`, to a safe ceiling such as 4096) before any
  IPC/network/subprocess
  handle is opened, log-file-not-stderr logging with `natefinch/lumberjack` rotation, `norite logs tail`. Done
  when: the daemon survives a full reboot and comes back up automatically; its log file rotates instead of
  growing unbounded; and a test forcing many simultaneous attach-client/voice-worker/log handles open does not
  hit the OS's default file-descriptor ceiling.
- **M24 — Client auto-update mechanism**: version-check endpoint polling; Sigstore/cosign signature
  verification, using self-contained offline-verifiable bundles, before any binary swap; anti-downgrade
  protection; fail-closed behavior on verify failure; auto-rollback on repeated crash-loop after an update;
  surfaced to the user in CLI/TUI/GUI. Depends on M23. Done when: a signed test release installs and the
  daemon swaps to it, verified with no network call beyond the version-check/download itself; an unsigned or
  downgrade-attempt release is rejected and the current version keeps running; and a deliberately-crashing
  "bad" release triggers automatic rollback to the previous binary. Once Phase E exists, this milestone's
  guard additionally defers applying a downloaded update while the daemon is tracking an active voice
  session, applying it only once the call ends (`architecture.md` §6).

#### Phase E — Voice

- **M25 — Mic-permission and global-hotkey spike (throwaway, time-boxed)**: a prototype determining whether a
  headless voice-worker process can reuse a foreground CLI/TUI/GUI's OS mic-permission grant (especially macOS
  TCC), and whether the headless daemon process can register an OS-wide global hotkey on each target OS
  (including the macOS Input Monitoring entitlement). This produces a written finding, not shippable code —
  its answer determines the exact shape of M32 and the daemon's hotkey-registration approach in M35. Do not
  proceed to M28–M32's detailed design before this completes.
- **M26 — Pion SFU core**: room/participant model, codec/track-kind-agnostic RTP forwarding (built
  generically from day one so M106–M107's video activation is additive, not a redesign). Done when: two test
  clients can exchange audio through the SFU.
- **M27 — Embedded TURN plus STUN**: `pion/turn` embedded in the backend (server-side, not the daemon),
  serving both TURN relay and plain STUN binding requests. Done when: a client behind simulated NAT can
  establish a voice connection via the embedded TURN server alone.
- **M28 — Voice-worker subprocess and IPC**: the daemon spawns the worker on-demand (via `os/exec`) when
  joining a voice channel, tears it down on leave; daemon↔worker control-plane IPC uses the child's
  inherited stdin/stdout pipes, framed with the same 4-byte-length-prefix-plus-JSON scheme used for the
  daemon↔CLI/TUI/GUI Unix socket (M20). Done when: joining voice spawns the worker, leaving voice cleanly
  tears it down, and the daemon detects a worker crash via the closed pipe.
- **M29 — Opus audio pipeline**: `hraban/opus` (cgo) capture/encode/decode/playback in the voice-worker.
  Depends on M25's finding for how mic access is actually obtained. Done when: two voice-worker instances can
  exchange intelligible audio through the SFU from M26.
- **M30 — Audio DSP**: RNNoise (noise suppression) plus WebRTC's Audio Processing Module (AEC3 echo
  cancellation plus AGC2 — replacing `libspeexdsp`, ADR 0023), both cgo,
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
  to the voice-join payload now (the terminal clients always send `false`) even though nothing consumes it
  until M105. Done
  when: joining a voice channel via the real gateway path (not a test harness) triggers real SFU allocation
  end to end.
- **M34 — Voice control surface (command tree and daemon)**: join/leave/mute/deafen as verbs on the command
  tree, the daemon holding call state, and active-speaker plus per-participant local-mute and report exposed
  over the IPC protocol — everything a client needs to drive and display a call, with no client drawing it
  yet. The chords, the status-line indicator and the call UI belong to the TUI and arrive with it: bindings
  at M44, `4b`'s strip and `4a`'s full view at M54. Splitting it this way is ADR 0026's rule applied
  backwards — this milestone sits in Phase E, seven milestones before a terminal application exists, so a
  done-when phrased as "control a voice session from the TUI" could not be checked when it is built. Done
  when: `norite voice join/leave/mute/deafen` fully control a session; a test client attached to the daemon
  receives active-speaker updates and can local-mute and report a participant independently of each other.
- **M35 — Voice input mode**: voice-activity-detection is the default; push-to-talk via
  `golang.design/x/hotkey`, registered by the daemon (per M25's finding), as a true OS-wide global hotkey,
  including the macOS Input Monitoring entitlement flow. Done when: push-to-talk works while the TUI/GUI is
  unfocused (e.g. alt-tabbed into another app), and registering the hotkey from a TUI and a GUI attach
  session simultaneously does not double-register or double-trigger.
- **M36 — Voice auto-rejoin**: the "last active voice channel" breadcrumb (the one exception to in-memory
  daemon state), the daemon respawns the worker and rejoins on crash/restart. Done when: killing the daemon
  process mid-call results in automatic rejoin within a few seconds of restart.
- **M37 — Voice deployment opt-out**: the Instance Admin config toggle (also added to
  `norite instance init`, extending M2/M10), the SFU/TURN never start when off, voice+text channels degrade to text-only,
  voice UI is hidden entirely (not grayed out) in CLI/TUI/GUI when disabled. Done when: an instance with
  voice disabled shows no voice-related UI anywhere and never starts the SFU/TURN processes.

#### Phase F — Presence, Deep Work, and the TUI foundation

- **M38 — Presence persistence**: the `presence_status` table (including the Deep Work value), replacing
  the in-memory-only original design. Done when: presence survives a backend restart.
- **M39 — Deep Work**: server-side notification-suppression logic, the `@urgent` mention bypass, the
  **allow-rule model** the client exposes at `5d` — mentions of you, a named DM sender, or a user-supplied
  pattern — the **held-message queue** with its review, the auto-reply others see, and the session
  countdown. Rules are evaluated server-side, because that is the only place a notification can be
  suppressed before it is sent; a user-supplied pattern therefore runs on the instance, so it is bounded
  (Go's `regexp` is RE2 — linear time, no catastrophic backtracking — plus a length cap and a compile-time
  check at save), the same treatment M62's notification filters get. Also the
  optional per-user opt-in email fallback for an `@urgent` mention when the account has no live gateway
  connection (fires regardless of whether Deep Work is active; requires SMTP configured per M5). Depends on
  M5 for the SMTP path. Done when: enabling Deep Work suppresses a normal mention's push, and an `@urgent`
  mention still comes through; disconnecting every client and triggering an `@urgent` mention with the
  fallback enabled delivers an email instead.
- **M40 — OS desktop notifications**: `gen2brain/beeep` wiring, triggered by `@urgent`-during-Deep-Work and
  regular mentions. Done when: a real OS toast/notification-center alert appears for both cases.
- **M41 — TUI shell and grid** (`1a` skeleton): the application frame — guild rail, channel list, message
  area, member list — on the Charm stack, one window and one pane, drawn from fixture data. Rounded
  borders, the focused/unfocused palette split, and the responsive drop order from
  `docs/design/tui/README.md` (member list first, then channel list, then side-by-side splitting is
  refused, then the rail collapses). Depends on M20 for the socket the real data will arrive on. Done when:
  the frame renders at 120×40 and at 80×24 with the documented columns dropped, and nothing in it assumes a
  feature that does not exist yet.
- **M42 — TUI status bar** (`7a`): the bottom two rows, present on every screen — row 1 route/counts/health,
  row 2 the contextual chord set. Never wraps: segments truncate in the documented order. Done when: every
  screen built after this feeds it without special-casing, and a narrow terminal truncates rather than
  reflowing.
- **M43 — TUI message rendering** (`1a`): author grouping, day dividers, system lines, the composer, and the
  small allow-list-only markdown renderer (bold/italic/code/links/mentions — not `glamour`, to keep the
  trusted-rendering surface as narrow as the security posture used everywhere else). Every rendered string
  passes `termsafe` (M7, rule 19), and this is where the two limits that function deliberately left open get
  revisited with a renderer that has font and width rules in front of it — invisible-but-inert characters
  and confusables, both out of scope at M7 because filtering by "invisible" corrupts written Arabic and the
  joiners Persian, Indic and composed emoji need. Guild channels only; DM and group-DM presentation arrive
  with the feature at M57.

  **This is also where the TUI stops drawing fixtures and attaches.** M41 deliberately renders a frame with
  no connection behind it, and nothing between the two milestones closes that: here the client opens the
  daemon socket (M20), completes the semver handshake, and renders from DISPATCH events and the daemon's
  in-memory scrollback (M19) instead of a fixed table — and the composer posts a real message through it.
  Stated because it is the one seam in this phase with no milestone of its own, and a phase that never
  names it is how a client stays on fixtures until somebody notices. Done when: a channel's history renders
  live from the daemon with grouping and dividers, a message typed in the composer reaches the instance and
  comes back through the gateway, a disallowed markdown corpus renders inert, and a message containing
  escape sequences cannot move the cursor.
- **M44 — TUI chord dispatcher and help** (`3d`, `1d`): the two-prefix Emacs model (`C-x` panes, `C-c` app,
  `M-x` command mode, `M-1`…`M-9` guilds), armed-prefix feedback in the status bar, unknown chords as a
  status-bar error rather than a modal, `[tui.keys]` in the config file with hot reload, the help overlay,
  and the rebinding UI with its overrides panel. Depends on M21 for the config file. Done when: the
  documented default set is wired, a remap in `[tui.keys]` takes effect without a rebuild, and `C-h` lists
  what is actually bound rather than a hardcoded table.
- **M45 — TUI theming and ricing**: the token roles from `docs/design/tui/TOKENS.md` mapped by default onto
  the terminal's own ANSI 0–15, so an existing palette is inherited rather than overridden; the drawn hex
  palette shipped as the named `norite-dark` theme; `~/.config/norite/themes/*.toml` selected from `[tui]`;
  and user overrides for border style, density, timestamp format, author colours, the glyph table and the
  column widths. A theme is a shared file, so it is untrusted input: colours parse or the theme fails as a
  whole, every string passes `termsafe`, and **a glyph override must be exactly one cell wide** — a
  two-cell glyph shears every row it lands in. Width is measured the way the terminal will measure it
  (`mattn/go-runewidth`'s East-Asian mode), because most of the shipped glyph set is East-Asian-Ambiguous
  and renders two cells wide on a CJK-locale terminal — so this milestone also picks the one-cell
  alternative each ambiguous glyph falls back to there, rather than leaving a sheared grid for the first
  user in that locale to discover. Done when: Norite is legible on a light terminal it was never designed
  for, the grid holds on a terminal that treats ambiguous width as wide, a downloaded theme cannot corrupt
  it, and a rejected theme names the line.
- **M46 — TUI pane engine and windows** (`2a`, `2b`, `2c`): splits, directional focus, zoom, close, the
  window tab bar, and the chrome-by-pane-count rule — one or two panes may each be a complete client with
  its own rail and columns, three or more draw chrome once and hold content only. Non-chat pane types
  `log` and `scratch` land here. Layout tree and scroll offsets live in the daemon (ADR 0010), so detach
  and reattach restores them. Done when: two panes show different guilds with full chrome each, adding a
  third switches the window to shared chrome, and reattaching after a detach restores the tree.
- **M47 — TUI pty pane**: the `shell` pane type — the user's real shell, the same trust boundary a terminal
  emulator has, with resize and escape handling. Pulled ahead of the old dev-tools placement because it is
  what interactive `norite` verbs run inside (M49). Done when: a shell runs in a pane, resizes with it, and
  its output cannot escape the pane's cell region.
- **M48 — `--json` output and CLI contracts**: every data-printing verb returns a **structured result**
  rather than printed text, rendered as JSON on the command line and into a pane by `M-x`; schemas
  versioned in `contracts/cli-json/` as the third source-of-truth artifact alongside `openapi.yaml` and
  `gateway-events.schema.json` (rule 15). This is load-bearing rather than cosmetic: a verb added without a
  structured result is a verb the TUI cannot run (ADR 0026). Done when: a script can parse `--json` against
  its documented schema, and the same call renders in a pane without a second code path.
- **M49 — `M-x` command mode** (`3e`): every verb in the command tree invocable from the TUI, output into a
  pane, flag completion, destructive verbs confirming in the status bar rather than a modal. Verbs that are
  interactive by construction — `norite instance init`, which refuses to run without a TTY — run in the pty
  pane from M47, so there is no second implementation of any prompt flow. Depends on M47, M48. Done when:
  `M-x` runs a read verb into a pane, runs `norite instance init` in a pty pane to completion, and refuses
  nothing that the command line accepts.
- **M50 — TUI fuzzy switcher** (`3a`, `3b`): the centred overlay and the minibuffer variant selected by
  `switcher = "minibuffer"`, with match highlighting, a preview column, and open-in-split. Lists channels
  and voice at this point; DMs join it with M57. Done when: both variants navigate to a channel, the
  minibuffer variant previews live without committing, and `C-x 3` opens the selection in a split.
- **M51 — TUI image rendering**: `BourgeoisBear/rasterm`-based capability detection (Kitty/iTerm2/Sixel),
  inline rendering with a filename/link fallback, and the "disable image loading" toggle hook point. Done
  when: the same attachment renders inline on a capable terminal and as a link on an incapable one.
- **M52 — TUI logging**: file-based logging (never stderr, which would corrupt the alternate screen), and
  a tail command that fills the `log` pane built at M46. Done when: an error during normal TUI use is recoverable from the log
  file afterwards, and nothing a client logs disturbs the rendered frame.
- **M53 — TUI testing**: `teatest`-based coverage of the shell, the chord dispatcher, the pane engine and
  the status bar — key-press simulation with rendered-output assertions, plus golden frames for the screens
  built so far. Done when: pane split/focus/navigation and prefix handling are covered, and a golden frame
  fails on an unintended layout change.

- **M54 — TUI voice view** (`4a`, `4b`): the chords and status-line indicator M34 deferred, then the
  in-call strip above the composer — one row, nothing hidden,
  which is the default while chatting during a call — and the full voice view it promotes to: speaker tiles
  with level meters, and the transport/processing/levels metric cards (codec, rtt, jitter, loss, route;
  denoise, aec, worker, device). Supersedes M34's "no visual call UI — it's a terminal", which was true
  only while there was no terminal application to draw it in. Depends on M34 for the controls and M41–M46
  for the frame. Done when: a call renders as a strip that never hides the conversation, `C-c V` promotes it
  to the full view, and the level meters redraw without a full repaint.
- **M55 — TUI lifecycle and state screens** (`5a`, `5c`, `5d`): first run (the device-code path of M9, with
  the browser login of M8 shown as progress rows when a browser is available), disconnected — cached header,
  the retry banner, queued messages marked `◷` in an in-memory outbox flushed in order on reconnect — and
  the Deep Work view with its rule strip, countdown and held-message review. `5b` (empty / first join) rides
  with invites at M57 and `5e` (whisper composer) with whispers at M61. Done when: pulling the network shows
  the cached state with a working queue rather than an error, and reconnecting flushes it in order.

#### Phase G — Profiles, DMs, invites, attachments, chat power features

- **M56 — User profiles** (`1e`, `1f`): the profile fields the client actually shows — pronouns, bio,
  links, timezone, a status line, an accent colour — plus the **per-field visibility scope** model, which is
  the part with teeth: every field carries who may read it, enforced server-side on read rather than hidden
  by the client, so `last seen: nobody` means the instance does not publish it rather than that the UI omits
  it. Both screens ride with it: your own profile with its live preview of how others see you, and the
  right-anchored card over the message area for someone else. Done when: a field scoped to nobody is absent
  from the API response for every other account, verified by a test that asks as a stranger, and the
  preview in `1e` matches what `1f` renders for that stranger.
- **M57 — DMs/Group DMs/invites**: `DM`/`GROUP_DM` channel types, `channel_recipients`, guild invite codes
  (existing pattern). Carries its screens: `1b` (DM — the peer column, verified-device header, `◈` composer)
  and `1c` (group DM, which shows instance-side encryption and **no** `◈`, since E2E is `DM`-only per rule
  13), plus `5b` (empty / first join, with the invite-redeem box) and the DM entries the M50 switcher gains.
  Done when: a user can DM another user and create/redeem a guild invite.
- **M58 — Attachments storage**: the pluggable storage interface (local disk default), server-side
  content-type sniffing, size/rate limits. The `minio-go` S3-compatible backend is wired in as the alternate
  implementation (used later by the flagship at M115, self-hosted instances never touch it). Carries the `▤`
  attachment row in the message list. Done when: an
  attachment uploads, is retrievable, and its declared content-type is never trusted from the client.
- **M59 — Per-guild custom emoji**: the `guild_emojis` table, `PermManageEmojis`, static-image-only upload
  validation (resolution cap, format allow-list, decompression-bomb guard), and shortcode resolution added to
  the TUI markdown renderer (retrofitting M43) and to the GUI/web renderers whenever each is built. Depends on
  M58. Adds shortcode resolution to the M43 renderer. Done when: a guild member with permission can upload a
  custom emoji and it renders correctly (not as a
  raw shortcode) in that guild's messages across every client that exists so far; and an oversized, malformed,
  or animated-format upload is rejected with a clear error, not silently accepted.
- **M60 — Incoming webhooks**: the `webhooks` table, `PermManageWebhooks`, the
  `POST /webhooks/{id}/{token}` endpoint with per-message name/avatar override support, automation-tagged
  messages via the existing `messages.type` reserved value routed through the same rendering/sanitization
  pipeline as any other message, hashed high-entropy tokens with independent per-webhook rate limiting. Adds
  the `AUTO` badge and the bot/webhook author colour to the M43 renderer (`1a`). Done
  when: a message posted to a valid webhook URL appears in the target channel with its per-message override
  applied, visually tagged as automated, and rendered/sanitized identically to a normal message; an invalid or
  revoked token is rejected; regenerating a webhook's token invalidates the old one without deleting the
  webhook; and a burst of posts against one valid token is throttled independently of the owning user's own
  REST rate limit.
- **M61 — Whispers**: the private, message-visibility-restricted-to-selected-recipients feature; not
  guild-audit-logged; excluded from E2E scope (enforced later once E2E exists, at M99); the break-glass
  schema exists now (a whisper is queryable by internal tooling) even though the Instance-Admin-facing
  break-glass view isn't built until M74. Carries `5e` (the whisper composer): recipient chips, the
  candidate list with a disabled row for someone not in the channel, and the received-whisper styling —
  stating that a whisper is stored by the instance like any other message and is not E2E, deliberately (ADR
  0013, ADR 0014). Done when: a whisper is visible only to its selected recipients and
  invisible to other channel members who could otherwise read the channel.
- **M62 — Regex notification filters**: server-side evaluation via Go's stdlib `regexp` (RE2), a
  pattern-length cap as defense-in-depth. Done when: a saved filter correctly matches/suppresses
  notifications server-side, including for a client that's currently offline.
- **M63 — Bandwidth/network performance toggles**: client-side settings (e.g. disable image loading, wired
  to the M51 rendering path). Done when: toggling the setting suppresses inline image rendering without
  affecting anything else, including custom-emoji rendering, which stays unaffected.
- **M64 — Link previews**: generic OpenGraph fetching (`PuerkitoBio/goquery`), GitHub-specific previews via
  `google/go-github` plus an API token, the SSRF-protected `DialContext` (rejects private/loopback/link-local
  resolved IPs at connect time) plus a request timeout and an `io.LimitReader` response-size cap. Preview titles and descriptions pass `termsafe` as they are fetched, not as they are drawn (rule 19) — they are third-party text from an arbitrary URL, which is the case that rule names first. Done when: a
  pasted GitHub URL gets a rich preview, a pasted arbitrary URL gets a generic OpenGraph preview, and a pasted
  URL pointing at an internal/private address is refused.

#### Phase H — Search

- **M65 — Postgres full-text search**: `tsvector` generated columns plus GIN indexes plus `pg_trgm` on
  `messages.content` and tag/channel-topic text, a search REST endpoint, synchronous on the insert path for v1
  at both self-hosted and flagship scale. The async-queue-decoupled indexing path noted in ADR 0018 is a
  documented future upgrade for the flagship deployment specifically, not v1 scope. Carries `3c`: the filter
  column, and results in two labelled groups — server hits from Postgres, and DM hits from the daemon's
  mandatory local FTS5 index over its decrypted E2E store (M98, ADR 0014), since the instance holds only
  ciphertext and cannot match against it (rule 13). The two groups are labelled because they come from
  different machines, with different guarantees. Done when: a guild-scoped
  search query returns relevant messages ranked reasonably, with the index verified via `EXPLAIN ANALYZE`.

#### Phase I — Public matchmaking, friends, blocks, Instance Admin

- **M66 — Public matchmaking channel type**: the guild-less top-level channel type, fixed platform ruleset
  (no custom roles/ownership), a voice-and-text pair (the voice half depends on Phase E being done), an
  instance-level toggle defaulting ON (extends `norite instance init` again). Lights up the `◎` discover
  entry in the TUI rail. Done when: a public channel can be
  created, joined without invite, and automatically removed once empty per its lifecycle rules.
- **M67 — Public matchmaking anti-abuse**: the minimum account-age/verification threshold gate, layered on
  top of existing rate limiting — including that limiting's global `/64` IPv6-subnet grouping
  (`architecture.md` §11),
  so a griefer cannot bypass the gate by rotating through one IPv6 block; the email-verification half of this
  gate is unavailable if SMTP was never configured (M5), leaving only the account-age threshold in that case.
  Done when: a freshly-created account is blocked from joining/creating a public channel until it clears the
  age/verification threshold; a simulated burst from one IPv6 `/64` block is throttled as a single source;
  voice-side abuse in these channels is documented as having no recorded evidence to review, by design.
- **M68 — Recently-met list**: server-side storage, a 7–30 day retention window, integrated into account
  export/deletion. Done when: users who shared a public channel appear on each other's recently-met list, and
  the entries expire on schedule.
- **M69 — Friends system**: `friend_requests`/`friendships` tables, a mutual request/accept flow, explicitly
  not DM-gating (organizational label only). Depends on M68 (the action "recently met" leads to). Done when:
  a friend request must be accepted before two accounts show as friends, and DMing works regardless of friend
  status either way.
- **M70 — Block/mute system**: the `blocks` table; enforcement in DM/whisper send paths (a silent,
  non-revealing failure, with no distinguishable signal that a rejection was specifically a block) and
  presence visibility; server-side gateway-dispatch filtering (a cached per-connection block-set, invalidated
  immediately on block/unblock) of a blocked account's messages/presence in shared guild channels, plus
  suppression of their `@mention` notifications there; auto-unfriend plus recently-met removal (and future
  recently-met suppression) on block; automation-token enforcement; export-symmetry (blocked-by-you included,
  blocked-you excluded). Depends on M57 (DMs), M68 (recently-met), M69 (friends), and M18 (the gateway
  protocol core, for the dispatch-filtering mechanism). Done when: a blocked account's DM/whisper attempt
  fails with no distinguishable signal that it was specifically a block; their guild-channel
  messages/presence never reach the blocker's gateway connection at all, verified by inspecting the actual
  DISPATCH stream, not just client rendering; a load test confirms the per-connection block-set check does not
  regress message fan-out latency; blocking removes any existing friendship; and an account export includes
  who the user blocked but not who blocked them.
- **M71 — Instance Admin tier, schema**: the boolean/flag-based tier (supports multiple admins per instance),
  sitting outside `roles.Resolve` entirely, with the last-admin-removal safety rail. Done when: granting or
  revoking the tier works, and removing the last remaining admin is blocked.
- **M72 — Instance Admin bans, enforcement, and audit log**: `instance_bans` (full account suspension, an
  optional `expires_at`), enforcement via the M11 revoke-all-sessions primitive (force-close plus revoke
  tokens; already-issued short-lived access tokens expire naturally per the stateless-JWT design), and
  `instance_audit_log` recording every Instance Admin action.

  **Must also cover instance-invite management**, which M10 built and logs structurally rather than
  durably. Rule 14 enumerates bans, report resolution, entitlement changes and tier grants, so minting an
  invite is not strictly among them — but "who opened the door onto this instance" is exactly the question
  an audit log exists to answer, and M10 deferred it here deliberately rather than by omission (ADR 0029).

  Done when: issuing a ban immediately disconnects the account everywhere and blocks re-authentication, and
  the action is logged.
- **M73 — Instance Admin lockout recovery**: the server-side recovery CLI command
  (`norite instance grant-admin <email>`, filesystem-access-gated). Done when: it successfully regrants the tier on a test
  instance with zero remaining admins.
- **M74 — Instance-level reports routing and whisper break-glass**: wires the M16 reports system's
  instance-scoped half — public-matchmaking, whisper, and plain-DM/Group-DM reports (none of which have a
  guild owner to escalate to) all route to an Instance Admin triage queue; whisper content becomes visible
  only attached to a specific filed report, itself audit-logged; report filing is rate-limited, and the
  triage view shows reporter history; the data-export asymmetry applies (filed-by-you included,
  filed-against-you excluded). Depends on M57 (DMs/Group DMs) and M61 (whispers). Carries `6c` (reports):
  the table, the expanding selected report with its defanged excerpt, the two action rows, and the standing
  note that only reporter-attached content is visible — the honest statement of ADR 0013's break-glass
  posture. Done when: an Instance Admin
  can review a filed report on a whisper and that specific access is itself an audit-log entry, and a report
  filed against a plain DM or Group DM also reaches the Instance Admin triage queue.
- **M75 — Instance Admin proactive intervention**: report-less intervention capability
  (legal/compliance), gated by a mandatory logged justification field distinguishing it from
  report-triggered entries. Done when: a proactive action is blocked without a justification string and
  logged distinctly when one is given.
- **M76 — Public-channel/whisper retention windows**: the 48-hour post-empty retention on public channel
  history (and whispers exchanged within it) before permanent purge, for report-investigation purposes. Done
  when: a channel's history remains queryable by Instance Admins for 48 hours after it empties, then is gone.
- **M77 — Data export asymmetry verification**: an end-to-end test that a user's own export includes their
  filed reports and blocked accounts, and excludes reports filed against them and who has blocked them. Done
  when: both asymmetries are covered by an automated test, not just documented intent.

#### Phase J — Native GUI

The GUI mirrors the TUI's information architecture (ADR 0026): the same layout, vocabulary and screens from
`docs/design/tui/`, presented natively — real scrollbars, pointer input, resizable splits, native dialogs
where the TUI uses a status-bar confirm. It is one application in two renderings, so these milestones cite
the same screen ids rather than redesigning them, and the open question each one answers is what changes
when a constraint the terminal imposed is lifted.

- **M78 — GUI skeleton**: the Gio app scaffold and the `1a` frame rendered natively, attaching to the daemon
  via the same local socket/protocol the terminal clients use (M20). Done when: the GUI receives the same
  DISPATCH events the TUI does, from the same daemon.
- **M79 — GUI message rendering**: the virtualized message list, the allow-list markdown renderer
  reimplemented for Gio's immediate-mode primitives (the same allow-list as M43, including emoji-shortcode
  resolution, on a different rendering target). Done when: a long channel scrolls smoothly and renders the
  same allow-listed markdown subset as the TUI.
- **M80 — GUI pane splitting**: native widget-based tiling, the same pane model and chrome-by-pane-count
  rule as the TUI (M46). Done
  when: a user can split the GUI window into independently-addressable panes.
- **M81 — GUI theming**: the same token roles and named themes as the TUI (M45, `docs/design/tui/TOKENS.md`)
  mapped to Gio's native rendering — a theme a user wrote for one client is legible in the other. Done when:
  a theme change in the config file is reflected identically in spirit across TUI and GUI.
- **M82 — GUI settings and voice device tab**: config read/write via the same `go-toml` v2 document-editing
  approach the other clients use, plus the voice input/output device-selection settings tab. Done when: a
  setting changed in the GUI is correctly reflected when the TUI next reads the config — `[gui]` overriding
  `[shared]` for its own keys and leaving `[tui]` untouched (M21).
- **M83 — GUI voice UI**: participant list, mute/deafen controls, an active-speaker indicator (a highlight/
  ring around whoever is transmitting), and separate local-mute and report actions, wired to the same
  voice-worker control path the TUI uses (M34). Done when: joining voice from the GUI shows the same
  participant state the TUI would, including which participant is currently speaking, and mute/report work
  as two independent actions.
- **M84 — GUI testing**: golden-image/screenshot tests for the highest-value surfaces (message list,
  pane-split layout, voice UI states) plus documented manual-QA coverage for the rest. Done when: a
  deliberate rendering regression in a covered surface fails the golden-image comparison.
- **M85 — Integrated whiteboard**: GUI-only, solo local drafting (no real-time multi-user sync). Explicitly
  the lowest-priority item in this entire phase — built last, after M78–M84, never in parallel with the
  load-bearing GUI work. Done when: a user can open a blank canvas and draw/annotate locally.

#### Phase K — Dev tools and extensibility

- **M86 — Code block enhancements**: a copy button plus folding, in both TUI and GUI. Done when: a code block
  in either client can be copied with one action and collapsed/expanded.
- **M87 — Shell piping and local port forwarding**: the remainder of `architecture.md` §4a's dev-tools
  bundle once the integrated shell is subtracted from it — piping a verb's structured result (M48) out to a
  shell pipeline and feeding one back in, and forwarding a local port over the daemon's existing connection.
  Both are asking the client to act on the user's behalf outside its own window, so both are governed by
  ADR 0017's local-automation trust tiers rather than being ordinary UI. **The integrated shell itself is
  not here**: it is the `shell` pane built at M47, pulled forward because interactive `norite` verbs run
  inside it (ADR 0026), and this milestone is what was left of the bundle after that move. Done when: a
  verb's output pipes into and out of a shell command without leaving the client, a forwarded local port
  carries a connection, and neither path grants an external script more than ADR 0017's lower tier allows.
- **M88 — WASM plugin host**: `wazero` wiring inside the daemon, the host-function capability API surface
  defined (message-read, slash-command-registration, etc., each individually capability-gated). A plugin
  registers `M-x` commands (ADR 0026) and never keybindings, so it cannot take a chord from the user and
  there is no conflict model to build. Done when: a
  minimal test plugin can call one host function successfully and is blocked from an ungranted one.
- **M89 — Plugin capability manifest**: the TOML `manifest.toml` format, the first-load user-approval UX,
  and SHA-256 hash-pinning of the approved `.wasm` file (stored alongside the capability grant in the
  daemon-owned state file, `architecture.md` §3) with re-verification on every subsequent load. Carries `6b`
  (the plugin manager): a card per state — enabled, needs approval with its requested capabilities, blocked
  on a hash mismatch — the capability tally, and the enforced quotas stated as a wall-clock timeout and
  memory cap **per invocation**, not a per-frame budget. Manifest names and descriptions pass `termsafe` at
  load: a manifest is a file the user was persuaded to install, and its description is rendered in the
  approval prompt before any decision is made. Done when: loading a new plugin shows its requested
  capabilities and requires explicit approval before it runs; swapping the `.wasm` file on disk after
  approval halts execution and re-prompts instead of silently running the swapped binary under the stale
  grant.
- **M90 — Plugin resource limits**: CPU (instruction-count/timeout) plus memory quotas per instance, plus a
  separate wall-clock timeout per invocation. Benchmark `wazero`'s instruction-metering overhead against real
  plugin workloads (e.g. a chat-filter on every incoming message); if metering costs more than the work it
  measures, memory quotas plus the wall-clock timeout become the primary safety mechanism instead of strict
  instruction counting. Done when: a deliberately runaway test plugin is killed by the quota/timeout without
  affecting the daemon's other responsibilities, and the metering-overhead benchmark's result is documented
  along with which enforcement mechanism v1 actually ships with.
- **M91 — Plugin distribution and TinyGo docs**: local-file-only distribution (drop a `.wasm` in a plugins
  folder), TinyGo documented as the recommended (not required) authoring toolchain. Done when: a TinyGo-built
  plugin loads and runs correctly following the documented steps.

#### Phase L — Self-hosting polish, P2P, ops

- **M92 — Backup/restore documentation**: `pg_dump`/`pg_restore` plus attachment-directory copy instructions
  for self-hosted instances, explicitly noting E2E key material lives client-side and isn't covered. Done
  when: following the documented steps produces a working restore on a test instance.
- **M93 — Prometheus metrics endpoint**: `prometheus/client_golang`, connection counts, message throughput,
  and voice/SFU call-health metrics (packet loss/bitrate/jitter, active once Phase E is done) — auth-gated
  (an Instance Admin token or localhost-only) with aggregate-only labels (no per-guild/per-channel
  identifiers), plus `net/http/pprof` mounted on the same internal-only path so profiling and metrics land
  together rather than as two separate retrofits. Both are deliberately here rather than in Phase A:
  `/metrics` is auth-gated on an Instance Admin token, which does not exist until M71, and there is nothing
  worth profiling before there are features generating load. Done when: `/metrics` requires auth, exposes
  the documented metric set, rejects unauthenticated access, and `pprof` is unreachable without the same
  gate.
- **M94 — P2P file transfer**: explicit opt-in per transfer, the initiating attach client (TUI/GUI) owns the
  WebRTC negotiation directly (the daemon is not involved, the same rule as video), enforced as a real
  three-way handshake (server-relayed Intent-to-Transfer → recipient Accept → only then
  `RTCPeerConnection`/SDP/ICE). Carries the `peers` pane type (`C-x c`): active and recent transfers,
  direction, progress, consent state and ICE/route status — the pane the TUI has been able to open since M46
  but had nothing to put in until now. Done when: a large-file transfer between two consenting clients
  completes without going through server storage; inspecting network traffic confirms no ICE candidate (and
  therefore no IP address) reaches the recipient before they've explicitly accepted.
- **M95 — Container image scanning**: `Trivy` added to `just security-scan` (depends on Phase P's Dockerfiles
  existing). Done when: CI fails on a test image with a known critical vulnerability.
- **M96 — Self-hosted docker-compose production path**: extends the existing local-dev `docker-compose.yml`
  into a documented production option alongside bare-metal/systemd. Done when: a fresh self-hoster can stand
  up a working instance via `docker compose up` following the docs alone.

#### Phase M — E2E encryption

- **M97 — Crypto base integration**: verify `go.mau.fi/libsignal`'s license is compatible with the project's
  restrictive custom license (a blocking prerequisite step within this same milestone, documented and passed
  before any further work here proceeds); then integrate the library and build the device-linking protocol
  on top of it. Done when: the license check is documented and passed; two test identities can complete a
  key exchange and exchange messages with forward secrecy demonstrated via the library (rotating a key
  doesn't expose prior messages); and device-linking (fully custom) links a second device without
  per-conversation re-verification.
- **M98 — E2E keystore**: the `modernc.org/sqlite` local encrypted store, exclusively daemon-owned
  (`architecture.md` §2's credential-ownership rule; no attach client holds a copy), the master key in the
  OS keychain via `zalando/go-keyring`, surviving daemon restarts. All keystore writes route through one
  dedicated writer goroutine fed by a buffered channel, so the WS event loop never blocks on disk I/O and a
  burst of concurrent incoming encrypted messages never produces a `database is locked` error. Also builds
  the mandatory `modernc.org/sqlite` FTS5 local search index over the decrypted message store, encrypted at
  rest via the same keystore master key. Carries `6a` (device verification): the two fingerprint cards, the
  affected-conversations panel, and the match/differ/later decision row. Done when: ratchet state persists
  correctly across a daemon restart mid-conversation; a burst of simultaneous incoming encrypted messages
  across different channels produces no lock contention error; and a local FTS5 query returns matching
  results from a user's own E2E-encrypted conversation history.
- **M99 — E2E opt-in UX**: strictly `DM`-channel-type-only enforcement (never `GROUP_DM`, never any guild
  channel), whispers explicitly excluded (enforced here, referencing M61). Done when: attempting to enable
  E2E on anything but a 1:1 DM is rejected.
- **M100 — Device-linking flow**: primary-device-authorizes-new-device, with text/code-based safety-number
  verification read out of band — never a camera or a QR code, which a terminal cannot show and an SSH
  session cannot photograph. It **reuses `6a`'s comparison surface** (built at M98 for verifying somebody
  else's new device) rather than drawing a second one: the same two fingerprint cards and the same
  match/differ/later decision row, with your own second device as the subject. No history-transfer
  mechanism exists for the newly linked device — it sees only messages sent after linking, by design
  (`architecture.md` §17/8.17). Done when: linking a second device grants it access to ongoing encrypted
  conversations without per-conversation re-verification, and the newly linked device's message history is
  confirmed empty prior to the link event.
- **M101 — Device revocation and E2E trust linkage**: logging out/revoking a device (M11's primitive) also
  revokes its E2E device-link trust. Done when: revoking a device's session also marks it untrusted for E2E
  purposes.
- **M102 — Ratchet and device-linking fuzz testing**: `go test -fuzz` targeting the M97 library-integration
  code and the device-linking protocol's message handling, with malformed/out-of-order/replayed inputs. Done
  when: the fuzz target runs cleanly for a defined corpus/duration with no crashes or invariant violations
  found.
- **M103 — External cryptographic security review (hard gate, not optional polish)**: a real external audit
  of the `go.mau.fi/libsignal` integration (correct use of the library, key-material handling around it) and
  the fully-custom device-linking protocol. A build/instance-level enforced flag makes E2E genuinely
  unavailable to any account beyond the developer's own test accounts until this milestone is marked
  complete — the flag, not a documentation policy, is what flips availability. E2E encryption must not be
  described as production-ready, marketed, or trusted with real user conversations until this milestone is
  complete. Done when: the flag blocks E2E for a normal account, and the audit sign-off is what flips it.
- **M104 — E2E-aware account data export**: the daemon-performed local decrypt-and-export step for E2E DMs
  (per the key-boundary rule in 8.17 — the daemon, not the CLI/TUI/GUI, holds the keys), producing a
  standalone `local_e2e_export.zip` presented alongside, never merged into, the server-side account export.
  Done when: exporting an account that has E2E conversations produces the separate local zip containing
  their decrypted content, never routed through the server, and the server-side export completes
  independently without attempting to merge it.

#### Phase N — Video/screen-share

- **M105 — GUI/web video connection**: a second, separate WebRTC connection to the SFU for the video track,
  owned directly by the GUI/web client (never the daemon), activating the `supports_video` flag from M33 and
  the codec-agnostic track model from M26. Done when: a GUI client can open a video track alongside its
  existing audio session with no daemon changes required.
- **M106 — Screen/camera capture and selection UI**: GUI-side capture and source-selection interface. Done
  when: a user can choose and share a specific window/screen or camera source.
- **M107 — SFU video track forwarding activation**: verify the already-track-kind-agnostic SFU from M26
  forwards video tracks (including screen-share as a second video track) without any SFU-side redesign, and
  design/implement simulcast/SVC spatial-temporal-layer switching so a participant on a poor connection
  doesn't degrade the whole call — the track-agnostic model from M26 made this additive rather than a
  redesign, but did not implement it, and it is real remaining engineering work (`architecture.md` §17).
  Done when: multiple participants can see each other's video/screen-share simultaneously through the SFU,
  and a simulated poor-connection participant causes the SFU to drop to a lower layer for them without
  degrading other participants' streams.

#### Phase O — Web SPA (later, third-priority client)

- **M108 — BFF cookie-exchange auth layer**: the httpOnly-cookie-issuing layer in front of the token API,
  designed earlier but built only now. Done when: the web SPA can log in and receive a session cookie without
  ever holding a raw Bearer token in JS.
- **M109 — Web SPA rebuild**: adapt the originally-planned React SPA to the current backend/contracts.
- **M110 — Web SPA pane-splitting**: CSS grid/flex-based resizable panes, `localStorage`-based layout
  persistence, independent of the CLI/TUI/GUI layouts.
- **M111 — Web SPA E2E export**: the browser-side decrypt-and-export equivalent of M104.

#### Phase P — Flagship instance Kubernetes deployment (parallel track, not sequential with the feature
phases above)

This phase is a deployment target, not a feature-development phase — it can start once core messaging and
voice are usable (roughly after M37), well before every feature phase above is complete, and continues to
absorb new features (public matchmaking, E2E, etc.) as they land. Do not read it as coming "after" M111.

- **M112 — Helm chart skeleton and API pods**: the base chart structure, the API/gateway `Deployment` behind
  an Ingress.
- **M113 — CloudNativePG plus backups**: the Postgres operator in-cluster, native continuous backup/
  WAL-archiving to in-cluster MinIO (M115).
- **M114 — Redis in-cluster and event-bus/rate-limit activation**: activates the previously-reserved Redis
  pub/sub fan-out (required the moment multiple API replicas run) and switches `ulule/limiter` to its
  Redis-backed store for this deployment specifically.
- **M115 — MinIO in-cluster**: the object storage backend for attachments (M58) and Postgres backups (M113).
- **M116 — TURN/SFU pods**: `hostNetwork: true`, in their own dedicated "privileged" Pod-Security-Standard
  namespace, separate from the "restricted" API/backend/Postgres/Redis/MinIO namespace.
- **M117 — `cert-manager` plus Ingress TLS**: disables the backend's built-in `certmagic` path for this
  deployment specifically (self-hosted instances keep it).
- **M118 — Graceful rollout**: a `preStop` hook sending the gateway's existing `Reconnect` op-code to local
  connections before pod termination, staggered across `terminationGracePeriodSeconds` rather than fired all
  at once, paired with randomized exponential backoff in the daemon's `Reconnect` handling. Done when: a
  simulated rollout of a replica holding thousands of connections does not produce a synchronized
  reconnect spike against the remaining replicas' auth/DB layer.
- **M119 — DB migration Job hook**: a Helm `pre-upgrade`/`pre-install` Job running `golang-migrate`, before
  new pods start.
- **M120 — Secrets**: plain Kubernetes Secrets applied via the Helm chart.
- **M121 — Autoscaling**: CPU/memory-based HPA via `metrics-server` (connection-count-based scaling
  documented as a future upgrade once M93's metrics are actually scraped by something in-cluster).
- **M122 — NetworkPolicies and namespace isolation**: baseline pod-to-pod traffic restriction, plus the
  backend-side half of the same concern — a trusted-proxy peer allowlist for `httpx.RealIP`. As built at
  M1, the backend honors `X-Forwarded-For` whenever `NORITE_TRUST_PROXY_HEADERS` is on, without checking
  that the immediate peer is actually the Ingress. That is sound for a self-hosted instance reachable only
  through its own reverse proxy, but not here: any pod in the cluster can dial the API Service directly,
  and one forged header per request would then mint an unlimited supply of rate-limit identities — exactly
  the hole the `/64` grouping rule exists to close. Add a configurable trusted-peer CIDR list checked
  against `r.RemoteAddr` before any forwarded header is consulted. Done when: a request arriving from
  outside the configured CIDRs has its `X-Forwarded-For` ignored and is rate-limited by its real source
  address, verified by a test that forges the header from an untrusted peer.
- **M123 — CI-triggered `helm upgrade`**: a simple CI pipeline deployment trigger, not GitOps.

#### M124–M125 — Gap-closure milestones (numerically appended, logically earlier — same treatment as Phase P)

These two milestones introduce schema/feature scope no earlier milestone covers. Like Phase P, they sit at
the numeric end of the roadmap but are not meant to be read as coming chronologically "after" M123 — each is
annotated below with where it actually belongs.

- **M124 — Read-state sync**: the `channel_read_states` table (per-user, per-channel watermark), a
  REST/gateway path to update it, and a new gateway dispatch event so every attached client — including a
  second daemon on a different machine — reflects accurate unread state, distinct from the daemon's
  ephemeral in-memory scroll/pane state (`architecture.md` §3). A channel is marked read automatically when
  a client's viewport reaches the latest message, debounced. Conceptually belongs in Phase D/G — depends on
  M12 (channels exist) and M18 (gateway dispatch core). Done when: marking a channel read on one client is
  reflected as read when a second client (or a second daemon on another machine) next syncs, without a
  separate mark-as-read action.
- **M125 — Data retention / audit-log pruning**: the configurable pruning seam from `architecture.md` §11,
  wired into `norite instance init`/`norite config set`, default-disabled, scoped to `audit_log_entries` and
  `instance_audit_log` only — message history and reports are explicitly not covered and stay permanent by
  design. Conceptually belongs in Phase L (self-hosting ops polish) — depends on M14 (audit log) and M72
  (instance audit log) existing, no other hard dependency. Done when: enabling a retention window on a test
  instance prunes entries older than the window and leaves newer ones (and all message/report data,
  unconditionally) untouched; leaving it disabled (the default) prunes nothing.

**How a screen is scheduled:** `docs/design/tui/` draws every screen in its finished state, so a milestone
that "carries" one builds only the parts whose features exist by then and leaves the rest **absent rather
than inert** — see "Screens are drawn finished; milestones are not" in `docs/design/tui/README.md`. This
bites hardest on E2E: M46, M55, M56, M57, M65 and M74 all carry screens with `◈`, a fingerprint, a keystore
row or a local-search group on them, and all of them precede Phase M. None of that chrome appears before
M99. A badge claiming a guarantee the build does not yet make is worse than no badge (rule 13).

**Dependency notes:** M37 (voice opt-out) must exist before M66 (public matchmaking, which needs the
voice+text pair to degrade gracefully). M61 (whispers) must exist before M74 (its Instance-Admin-facing
break-glass view) and before M99 (which excludes whispers from E2E scope). M68 (recently-met) must exist
before M69 (friends). M57 (DMs), M68 (recently-met), and M69 (friends) must exist before M70 (blocks). M11
(revoke-all-sessions) must exist before M72 (bans) and M101 (device revocation↔E2E trust). M58 (attachments)
must exist before M59 (custom emoji). M97's license-compatibility check must pass before any further work in
Phase M proceeds. Phase P (Kubernetes) depends on M114 requiring Phase D's Redis-fan-out design to already
exist as a seam, and M58/M113 requiring M115 (MinIO) to be stood up first within that phase. M124 depends on
M12 and M18, conceptually belonging in Phase D/G despite its number; M125 depends on M14 and M72, conceptually
belonging in Phase L despite its number — the same "numerically-late, logically-earlier" treatment already
established for Phase P above.
