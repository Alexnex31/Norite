# Architecture — Master Plan

> This is the canonical, version-controlled architecture reference for **Norite**. It is a snapshot of the
> plan the project was bootstrapped from — keep it updated as real decisions supersede it (a stale doc that
> nobody trusts is worse than no doc; if you deviate from something here, update this file in the same PR).
> See also: `/CLAUDE.md` for the fast-loading summary + non-negotiable rules, and `docs/adr/` for short,
> focused records of the most contested individual decisions below. This document absorbs and supersedes the
> repo-root `v2-plan.md`, which remains in the repo as historical planning record but is no longer the live
> reference.

## Context

Norite is a voice-and-text chat platform. The primary way to use it is the free, global, publicly-hosted
flagship instance (§12) — self-hosting is a real, fully-built feature, not the platform's core identity: a
one-time-purchase-licensed offering (§11) aimed at enterprises and other private groups who want their own
instance. Source visible but under no public license (all rights reserved, §11). Three clients: a
scriptable CLI, a native GUI, and a lower-priority web SPA built later, all sharing one local background
daemon per OS user account. The full scope described here is realistically multi-year,
systems-engineering-team-sized work; the milestone roadmap (§13) is a long-term dependency-ordered critical
path, not a near-term promise. No scope described in this document is removable; the roadmap is ordered so
the most foundational pieces land first.

Locked-in decisions:
- **V1 scope is large and real, not text-first-then-later**: guilds/channels/roles/permissions, real-time
  messaging, DMs, presence, invites, **voice calling on every client including the CLI**, BYOK end-to-end
  encryption (DM-only), public matchmaking, friends, blocks, Deep Work, message tagging, whispers, regex
  notification filters, per-guild custom emoji, incoming webhooks, a client-side WASM plugin system, P2P file
  transfer, the Instance Admin/reports moderation system, and self-hosting infrastructure (SMTP, automatic
  HTTPS) all ship in v1. Video/screen-share is the one genuinely deferred-but-seamed piece.
- **Clients**: CLI first, native GUI second, a web SPA third and lowest-priority, built later. All three are
  thin UIs; the CLI and GUI attach to one shared local daemon per OS user account (§3) which does the real
  work. See [ADR 0009](adr/0009-cli-and-gui-client-architecture.md).
- **Backend architecture**: Go modular monolith (single deployable), organized so pieces could be peeled into
  services later, without building actual service separation now. See
  [ADR 0001](adr/0001-modular-monolith.md).
- **Auth**: token-based (Bearer access + refresh, OS-keychain storage, `device_id`-scoped families) for the
  CLI/GUI/daemon; email/password + OAuth (Google/GitHub) with account linking at the backend. See
  [ADR 0011](adr/0011-token-based-client-auth.md).
- **Security and performance are first-class concerns**, not an afterthought pass at the end — §14 and §15
  below are as load-bearing as the feature sections and should be implemented alongside each milestone, not
  deferred.
- **License**: no public license — default copyright, all rights reserved. Not AGPL, not open source, not a
  drafted custom public license either. Self-hosted customers are granted rights individually via a signed
  license file (§11), never a public license text. See
  [ADR 0007](adr/0007-licensing-and-project-posture.md).
- **Commercial model**: two independent deployments of the same codebase. **The free flagship instance
  (Kubernetes, §12) is the primary product**; self-hosted instances sold via one-time license (flat pricing
  regardless of buyer, expected to appeal most to enterprises and other private groups) are a real, fully-
  built secondary offering, not a lesser-effort one. No shared multi-tenancy, no "Platform Operator" tier.
  See [ADR 0007](adr/0007-licensing-and-project-posture.md) and
  [ADR 0019](adr/0019-platform-scope-and-commercial-model.md).
- **Account lifecycle (deletion/export)** is planned now, alongside auth, rather than retrofitted later.

---

## 1. Monorepo Layout

```
/
├── backend/                     # Go modular monolith
│   ├── cmd/server/main.go       # composition root
│   ├── internal/
│   │   ├── config/              # config.go: typed Config struct, env-bound, validated at startup
│   │   ├── platform/
│   │   │   ├── database/        # pgxpool setup, RunInTx helper, migration-on-startup + advisory lock
│   │   │   ├── snowflake/       # ID generator + Snowflake type
│   │   │   ├── logging/         # zerolog setup, context-scoped logger helpers
│   │   │   ├── httpx/           # response envelope, error->HTTP mapping, JSON helpers, secure headers
│   │   │   ├── events/          # Bus interface (Publish/Subscribe), in-process + Redis impls
│   │   │   ├── ratelimit/       # ulule/limiter middleware, in-memory + Redis stores, /64 IPv6 grouping
│   │   │   ├── storage/         # attachment storage interface (local disk / S3-compatible via minio-go)
│   │   │   ├── mail/            # wneessen/go-mail wrapper, async send queue
│   │   │   └── license/         # offline Ed25519-JWT license file validation
│   │   ├── auth/                # password.go, jwt.go, oauth.go, tokens.go (device_id families), handlers
│   │   ├── users/  guilds/  channels/  roles/  messages/  invites/
│   │   ├── presence/            # Deep Work status, persisted
│   │   ├── friends/  blocks/  matchmaking/  tags/  whispers/  notifications/
│   │   ├── emoji/  webhooks/
│   │   ├── instanceadmin/       # instance_bans, instance_audit_log, reports (instance-scoped half)
│   │   ├── reports/             # unified reports system (guild + instance routing)
│   │   ├── gateway/             # ws.go, opcodes.go, session.go, registry.go, dispatch.go, block-aware fanout
│   │   ├── voice/                # Pion SFU room/participant model, PionMediaCoordinator
│   │   ├── turn/                 # embedded pion/turn server
│   │   └── db/                   # sqlc-generated, one package, narrow interfaces consumed per-domain
│   ├── migrations/               # golang-migrate .sql up/down, go:embed'd into the binary
│   ├── sqlc.yaml
│   └── go.mod
├── cli/                          # The `app` CLI — Bubble Tea/Lip Gloss/Bubbles TUI
│   ├── cmd/                      # cobra-style command tree, --json flag plumbing
│   ├── tui/                      # pane engine, keybindings, markdown renderer, sanitization, image rendering
│   └── go.mod
├── gui/                          # The native GUI — Gio
│   ├── app/                      # Gio window/event loop
│   ├── widgets/                  # hand-built: message list, pane tiling, voice UI, settings, whiteboard
│   └── go.mod
├── daemon/                       # Shared background daemon
│   ├── gatewayclient/            # holds the real WS connection, in-memory scrollback/presence
│   ├── ipc/                      # Unix socket / named pipe server, bot-automation TCP listener
│   ├── config/                   # go-toml v2 document-editing, fsnotify hot-reload, flock, config split
│   ├── plugins/                  # wazero host, capability manifest + hash-pinning
│   ├── voiceworker/               # os/exec spawn/supervise, stdin/stdout IPC framing
│   ├── e2e/                       # keystore (modernc.org/sqlite), ratchet, single-writer goroutine, FTS5
│   └── go.mod
├── voiceworker/                  # Separate binary — the only place cgo is allowed
│   ├── audio/                    # hraban/opus, RNNoise, libspeexdsp — capture/encode/DSP chain
│   └── go.mod
├── frontend/                     # React + TS SPA — the later, tertiary web client (Phase O)
│   ├── src/{app,features,gateway,api,stores,components}/
│   └── e2e/                       # Playwright
├── contracts/
│   ├── openapi.yaml               # REST contract — single source of truth
│   ├── gateway-events.schema.json # WS dispatch payload contract
│   └── cli-json/                  # CLI --json output schemas, versioned
├── docker/docker-compose.yml      # postgres, redis, backend (air hot-reload) — local dev + self-hosted prod
├── deploy/helm/                   # flagship Kubernetes Helm chart (§12)
├── .env.example
├── .github/workflows/ci.yml
└── justfile                       # just dev / test / lint / build / db-migrate / security-scan
```

**Production deployment story, self-hosted**: single binary + Postgres (+ Redis only if/when the flagship
activates it — self-hosted single-process instances never do). Bare-metal/systemd and docker-compose are
both supported production paths. The backend auto-runs migrations on startup (§2, advisory-lock-guarded,
blocking) and optionally provisions its own TLS via `certmagic`. **Production deployment story, flagship**:
Kubernetes via the Helm chart in `deploy/helm/` — see §12.

---

## 2. Backend (Go modular monolith)

**Router**: `go-chi/chi/v5`. **DB access**: `sqlc` over `pgx/v5`/`pgxpool` — no ORM, 100% of queries
parameterized by construction (§14). **WebSocket**: `coder/websocket`. **Other concrete deps**:
`alexedwards/argon2id`, `golang-jwt/jwt/v5`, `golang.org/x/oauth2` (PKCE), `go-playground/validator/v10`,
`golang-migrate/migrate/v4`, `oapi-codegen` (generates Go request/response types + routing from
`openapi.yaml`, wired starting at the milestone that first defines guild/channel endpoints), `ulule/limiter/v3`,
`zerolog`, `testify` + `testcontainers-go`, `google/go-github`, `PuerkitoBio/goquery`, `wneessen/go-mail`,
`caddyserver/certmagic`, `pion/turn`, `pion/webrtc` + `pion/interceptor`, `minio-go`, `govulncheck` (CI tool).

Each domain package has the same shape: `service.go` (business logic + permission checks — **every**
mutating method takes an already-authenticated `actor` and calls `roles.Resolve` before touching data),
`http.go` (chi sub-router), `model.go`, `events.go` (dispatches after a DB transaction commits, never
before). Services depend on narrow repository interfaces over the single `internal/db` sqlc package.

**Middleware chain order** (outermost first): `RequestID` → `RealIP` → `Recoverer` → `SecureHeaders` →
`StructuredLogger` → `RateLimit` (route-bucketed, `/64` IPv6 grouping, §14) → `AuthenticateBearer`
(populates `actor` from the JWT access token; 401 if absent on protected routes) → domain handler. No CSRF
middleware exists on this surface at all — see "Auth design" below.

### Data model (Postgres) — full DDL sketch

**ID strategy**: Discord-style Snowflake (`bigint`), `internal/platform/snowflake` — unchanged from the
original design: 41 bits ms-since-epoch, 10 bits `node_id`, 12 bits per-ms sequence, monotonic even across
clock adjustment, JSON-marshaled as a quoted string.

```sql
-- Core (unchanged in shape from the original plan; abbreviated to load-bearing columns)
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE users (
  id bigint PRIMARY KEY, username citext UNIQUE NOT NULL, email citext UNIQUE NOT NULL,
  password_hash text NULL, display_name text NOT NULL, avatar_hash text NULL,
  email_verified_at timestamptz NULL,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz NULL
);

CREATE TABLE oauth_identities (
  id bigint PRIMARY KEY, user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider varchar(32) NOT NULL, provider_user_id varchar(255) NOT NULL, email text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, provider_user_id), UNIQUE (user_id, provider)
);

-- Sessions/tokens: token-based auth for CLI/GUI, device_id-scoped refresh families (ADR 0011)
CREATE TABLE sessions (
  id bigint PRIMARY KEY, user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  device_id text NOT NULL,                     -- stable per daemon install; scopes the refresh-token family
  refresh_token_hash bytea NOT NULL,            -- sha256, raw value never stored
  device_name text, ip_address inet,
  created_at timestamptz NOT NULL DEFAULT now(), last_used_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL, revoked_at timestamptz NULL,
  replaced_by_id bigint NULL REFERENCES sessions(id)   -- rotation chain, scoped WITHIN one device_id only:
                                                        -- reuse-detected replay revokes only that device's
                                                        -- chain, never another device's (the M28-era bug
                                                        -- this schema specifically prevents)
);
CREATE INDEX ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX ON sessions (device_id);

CREATE TABLE api_tokens (                      -- scoped personal/bot tokens, replaces the old refresh-cookie model
  id bigint PRIMARY KEY, user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name text NOT NULL, token_hash bytea NOT NULL, scopes text[] NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(), last_used_at timestamptz NULL, revoked_at timestamptz NULL
);

CREATE TABLE device_code (                     -- CLI headless/SSH OAuth fallback
  code varchar(16) PRIMARY KEY, user_code varchar(9) UNIQUE NOT NULL,   -- short human-typeable code
  status smallint NOT NULL DEFAULT 0,          -- 0 pending, 1 completed, 2 expired
  session_id bigint NULL REFERENCES sessions(id),
  created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL
);

CREATE TABLE password_reset_tokens (
  id bigint PRIMARY KEY, user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash bytea NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL, used_at timestamptz NULL
);

CREATE TABLE guilds (
  id bigint PRIMARY KEY, name varchar(100) NOT NULL, owner_id bigint NOT NULL REFERENCES users(id),
  icon_hash text NULL, description text NULL, system_channel_id bigint NULL,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE guild_members (
  guild_id bigint NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
  user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  nickname text NULL, joined_at timestamptz NOT NULL DEFAULT now(),
  deaf boolean NOT NULL DEFAULT false, mute boolean NOT NULL DEFAULT false,   -- now ACTIVE, not reserved
  PRIMARY KEY (guild_id, user_id)
);
CREATE INDEX ON guild_members (user_id);

CREATE TABLE roles (
  id bigint PRIMARY KEY, guild_id bigint NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
  name varchar(100) NOT NULL, color integer NOT NULL DEFAULT 0,
  permissions bigint NOT NULL DEFAULT 0, position integer NOT NULL,
  hoist boolean NOT NULL DEFAULT false, mentionable boolean NOT NULL DEFAULT true,
  is_default boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON roles (guild_id, position);

CREATE TABLE guild_member_roles (
  guild_id bigint NOT NULL, user_id bigint NOT NULL, role_id bigint NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  PRIMARY KEY (guild_id, user_id, role_id),
  FOREIGN KEY (guild_id, user_id) REFERENCES guild_members(guild_id, user_id) ON DELETE CASCADE
);

CREATE TABLE channels (
  id bigint PRIMARY KEY, guild_id bigint NULL REFERENCES guilds(id) ON DELETE CASCADE,
  type smallint NOT NULL,   -- 0 GUILD_TEXT 1 DM 2 GUILD_VOICE(active) 3 GROUP_DM 4 GUILD_CATEGORY
                             -- 5 GUILD_ANNOUNCEMENT(reserved) 6 GUILD_STAGE_VOICE(reserved) 7 PUBLIC_MATCHMAKING
  parent_id bigint NULL REFERENCES channels(id) ON DELETE SET NULL,
  name varchar(100) NULL, topic text NULL, position integer NOT NULL DEFAULT 0, nsfw boolean NOT NULL DEFAULT false,
  last_message_id bigint NULL,
  bitrate integer NULL, user_limit integer NULL,   -- now ACTIVE for GUILD_VOICE
  topic_search tsvector GENERATED ALWAYS AS (to_tsvector('english', coalesce(topic,''))) STORED,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON channels (guild_id, position);
CREATE INDEX ON channels USING GIN (topic_search);
ALTER TABLE guilds ADD CONSTRAINT fk_system_channel FOREIGN KEY (system_channel_id) REFERENCES channels(id) ON DELETE SET NULL;

CREATE TABLE channel_recipients (
  channel_id bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (channel_id, user_id)
);
CREATE INDEX ON channel_recipients (user_id);

CREATE TABLE permission_overwrites (
  channel_id bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  target_type smallint NOT NULL, target_id bigint NOT NULL,
  allow bigint NOT NULL DEFAULT 0, deny bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (channel_id, target_type, target_id)
);

CREATE TABLE messages (
  id bigint PRIMARY KEY, channel_id bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  author_id bigint NULL REFERENCES users(id),
  content text NOT NULL,
  type smallint NOT NULL DEFAULT 0,   -- 0 DEFAULT, 1 SENT_VIA_AUTOMATION (webhooks + bot automation), reserved system values
  reply_to_id bigint NULL REFERENCES messages(id) ON DELETE SET NULL,
  is_e2e boolean NOT NULL DEFAULT false,   -- true only for DM-channel-type messages sent under E2E; content
                                            -- is ciphertext server-side when true, excluded from search below
  content_search tsvector GENERATED ALWAYS AS (
    CASE WHEN is_e2e THEN NULL ELSE to_tsvector('english', content) END
  ) STORED,
  edited_at timestamptz NULL, deleted_at timestamptz NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON messages (channel_id, id DESC);
CREATE INDEX ON messages USING GIN (content_search);
CREATE INDEX ON messages USING GIN (content gin_trgm_ops);   -- pg_trgm fuzzy matching

CREATE TABLE message_edit_history (
  id bigint PRIMARY KEY, message_id bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  content text NOT NULL, edited_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE attachments (
  id bigint PRIMARY KEY, message_id bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  filename text NOT NULL, storage_key text NOT NULL, content_type text NOT NULL, size_bytes bigint NOT NULL,
  width integer NULL, height integer NULL, created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE invites (
  code varchar(16) PRIMARY KEY, guild_id bigint NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
  channel_id bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE, inviter_id bigint NOT NULL REFERENCES users(id),
  max_uses integer NULL, uses integer NOT NULL DEFAULT 0, max_age_seconds integer NULL, expires_at timestamptz NULL,
  temporary boolean NOT NULL DEFAULT false, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON invites (guild_id);

CREATE TABLE instance_invites (                -- distinct from per-guild invites: gates account creation itself
  code varchar(16) PRIMARY KEY, created_by bigint NOT NULL REFERENCES users(id),
  max_uses integer NULL, uses integer NOT NULL DEFAULT 0, expires_at timestamptz NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE voice_states (                    -- now ACTIVE, not reserved
  guild_id bigint NOT NULL, channel_id bigint NOT NULL, user_id bigint NOT NULL, session_id text NOT NULL,
  self_mute boolean NOT NULL DEFAULT false, self_deaf boolean NOT NULL DEFAULT false,
  mute boolean NOT NULL DEFAULT false, deaf boolean NOT NULL DEFAULT false,
  supports_video boolean NOT NULL DEFAULT false,   -- client capability flag; CLI always false
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (guild_id, user_id)
);

CREATE TABLE audit_log_entries (
  id bigint PRIMARY KEY, guild_id bigint NULL REFERENCES guilds(id) ON DELETE CASCADE,
  actor_id bigint NOT NULL REFERENCES users(id), action varchar(64) NOT NULL, target_id bigint NULL,
  changes jsonb NULL, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON audit_log_entries (guild_id, created_at DESC);

-- Presence (persisted — Milestone M38; supersedes the original in-memory-only design)
CREATE TABLE presence_status (
  user_id bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  status smallint NOT NULL DEFAULT 0,   -- 0 online, 1 idle, 2 dnd, 3 invisible, 4 DEEP_WORK
  custom_status text NULL, updated_at timestamptz NOT NULL DEFAULT now()
);

-- Read state (durable, synced — distinct from the daemon's ephemeral in-memory scroll state, ADR 0010)
CREATE TABLE channel_read_states (
  user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  channel_id bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  last_read_message_id bigint NOT NULL, updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, channel_id)
);

-- Public matchmaking (ADR 0013)
CREATE TABLE public_channels (
  channel_id bigint PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,   -- type = PUBLIC_MATCHMAKING
  topic varchar(100) NOT NULL, voice_channel_id bigint NULL REFERENCES channels(id),
  created_by bigint NOT NULL REFERENCES users(id), emptied_at timestamptz NULL,   -- starts the 48h purge window
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE recently_met (
  user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  met_user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  met_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,   -- 7-30 day window
  PRIMARY KEY (user_id, met_user_id)
);

-- Friends (ADR 0013) — organizational label only, never gates DMing
CREATE TABLE friend_requests (
  id bigint PRIMARY KEY, requester_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  recipient_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  status smallint NOT NULL DEFAULT 0,   -- 0 pending, 1 accepted, 2 declined
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (requester_id, recipient_id)
);
CREATE TABLE friendships (
  user_a_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  user_b_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_a_id, user_b_id), CHECK (user_a_id < user_b_id)
);

-- Blocks (ADR 0013) — unilateral, server-enforced at gateway fan-out
CREATE TABLE blocks (
  blocker_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  blocked_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (blocker_id, blocked_id)
);
CREATE INDEX ON blocks (blocked_id);   -- "who has blocked me" — used to build the per-connection block-set

-- Message tags (ADR: guild-wide scope, not per-channel)
CREATE TABLE message_tags (
  id bigint PRIMARY KEY, guild_id bigint NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
  name varchar(50) NOT NULL, created_by bigint NOT NULL REFERENCES users(id),
  is_shared boolean NOT NULL DEFAULT false,   -- shared tags require PermManageMessages to create
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE message_tag_applications (
  tag_id bigint NOT NULL REFERENCES message_tags(id) ON DELETE CASCADE,
  message_id bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  applied_by bigint NOT NULL REFERENCES users(id), applied_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tag_id, message_id)
);

-- Whispers — message-visibility restriction, not a new authority tier (ADR 0008)
CREATE TABLE whispers (
  id bigint PRIMARY KEY, channel_id bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  author_id bigint NOT NULL REFERENCES users(id), content text NOT NULL,   -- never E2E — ADR 0014
  created_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz NULL
);
CREATE TABLE whisper_recipients (
  whisper_id bigint NOT NULL REFERENCES whispers(id) ON DELETE CASCADE,
  user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (whisper_id, user_id)
);

CREATE TABLE notification_filters (
  id bigint PRIMARY KEY, user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  pattern text NOT NULL CHECK (length(pattern) <= 200),   -- RE2 via Go stdlib regexp; length cap is defense-in-depth
  scope_guild_id bigint NULL REFERENCES guilds(id) ON DELETE CASCADE,   -- NULL = all guilds
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Instance Admin / platform moderation (ADR 0013)
CREATE TABLE instance_admins (
  user_id bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  granted_by bigint NULL REFERENCES users(id), granted_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE instance_bans (
  id bigint PRIMARY KEY, user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  issued_by bigint NOT NULL REFERENCES users(id), reason text NULL,
  expires_at timestamptz NULL,   -- NULL = permanent
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON instance_bans (user_id) WHERE expires_at IS NULL OR expires_at > now();

CREATE TABLE instance_audit_log (               -- separate from per-guild audit_log_entries
  id bigint PRIMARY KEY, actor_id bigint NOT NULL REFERENCES users(id),
  action varchar(64) NOT NULL, target_id bigint NULL, justification text NULL,   -- required for report-less action
  is_proactive boolean NOT NULL DEFAULT false, changes jsonb NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE reports (                          -- unified: guild, instance-level, DM/Group-DM, whisper
  id bigint PRIMARY KEY, reporter_id bigint NOT NULL REFERENCES users(id),
  target_type smallint NOT NULL,   -- 0 message, 1 whisper, 2 channel, 3 user
  target_id bigint NOT NULL, reason_category varchar(32) NOT NULL, detail text NULL,
  status smallint NOT NULL DEFAULT 0,   -- 0 open, 1 under_review, 2 resolved, 3 dismissed
  routed_to smallint NOT NULL,   -- 0 guild moderators, 1 instance admins
  created_at timestamptz NOT NULL DEFAULT now(), resolved_at timestamptz NULL
);
CREATE INDEX ON reports (reporter_id);
CREATE INDEX ON reports (status, routed_to);

-- Entitlements (ADR 0007) — inert seams, unused by any v1 code path
CREATE TABLE entitlements (                     -- per-instance (self-hosted license)
  id smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),   -- singleton
  licensed boolean NOT NULL DEFAULT false, license_key text NULL, entitlements_blob jsonb NOT NULL DEFAULT '{}'
);
CREATE TABLE user_entitlements (                -- per-user (flagship subscription perks)
  user_id bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  entitlements_blob jsonb NOT NULL DEFAULT '{}'
);

-- Custom emoji (ADR 0013)
CREATE TABLE guild_emojis (
  id bigint PRIMARY KEY, guild_id bigint NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
  name varchar(32) NOT NULL, storage_key text NOT NULL, uploader_id bigint NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (guild_id, name)
);

-- Webhooks (ADR 0013)
CREATE TABLE webhooks (
  id bigint PRIMARY KEY, channel_id bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  token_hash bytea NOT NULL, creator_id bigint NOT NULL REFERENCES users(id),
  default_name text NOT NULL, default_avatar_hash text NULL,
  created_at timestamptz NOT NULL DEFAULT now(), revoked_at timestamptz NULL
);
```

**New permission bits**: `PermManageEmojis`, `PermManageWebhooks` (see "Permission system" below).

**Data retention** (`docs/adr/0020-operations.md`): a configuration-level (not schema-level) pruning seam
covers `audit_log_entries`/`instance_audit_log` only, default disabled — see §11.

E2E key material (ADR 0014) lives client-side in the daemon's own `modernc.org/sqlite` keystore, **never** in
Postgres — there is no server-side schema for it beyond `messages.is_e2e` marking a message as
server-opaque ciphertext.

**sqlc query files**: one `.sql` per domain under `backend/internal/db/queries/`, named queries with
explicit typed parameters — the guarantee against string-concatenated SQL (§14).

### Permission system

```go
// internal/roles/permissions.go
type Permission uint64

const (
    PermViewChannel Permission = 1 << iota
    PermSendMessages
    PermManageMessages
    PermManageChannels
    PermManageRoles
    PermKickMembers
    PermBanMembers
    PermCreateInvite
    PermManageGuild
    PermAdministrator
    PermConnectVoice    // ACTIVE
    PermSpeakVoice      // ACTIVE
    PermVideoVoice      // still reserved — video is the one deferred-but-seamed piece
    PermMuteMembers     // ACTIVE
    PermDeafenMembers   // ACTIVE
    PermMentionEveryone
    PermManageWebhooks  // ACTIVE
    PermManageEmojis    // ACTIVE
)
```

`roles.Resolve` is unchanged in shape from the original design (owner bypass → `PermAdministrator`
short-circuit → `@everyone` overwrite → role overwrites → member overwrite), cached per
`(guild_id, user_id, channel_id)`, invalidated on role/overwrite/membership change dispatch. See
[ADR 0008](adr/0008-guild-authority-hierarchy.md) for the full consolidated authority hierarchy, including
the parts that deliberately sit **outside** `roles.Resolve` entirely:

- **Instance Admin** (`instance_admins` table) — checked via a dedicated middleware/service check, never
  through `roles.Resolve`. Last-admin-removal safety rail enforced at the service layer.
- **Public matchmaking channels** — a fixed platform-wide ruleset bypasses the permission engine entirely;
  there is no per-channel role/overwrite resolution for `PUBLIC_MATCHMAKING`-type channels.
- **Blocks** — a gateway-dispatch-layer filter (below), never a `roles.Resolve` input.
- **Whispers, friends, E2E** — none of these are permission-bearing; see ADR 0008.

### Real-time gateway

WS endpoint `wss://<host>/gateway`, envelope `{ "op": <int>, "d": <object|null>, "s": <int|null>, "t": <string|null> }`.
The **daemon**, not each attach client, is the actual gateway client — see §3.

Op-codes: Server→Client `0` Dispatch, `7` Reconnect, `9` Invalid Session, `10` Hello, `11` Heartbeat ACK.
Client→Server `1` Heartbeat, `2` Identify, `3` Presence Update, `4` Voice State Update (**now real**, wired
to `PionMediaCoordinator`), `6` Resume, `8` Request Guild Members.

**Auth transport**: `Identify` carries the daemon's Bearer access token (obtained via the token-based auth
flow, §"Auth design" below) — never a cookie, since there is no browser. **`Hello` carries the backend's
current server time**, so the daemon can compute and apply a local clock offset (`ADR 0010`) rather than
trusting a potentially-skewed OS clock for JWT-expiry checks.

```json
// server -> client, immediately on connect
{"op":10,"d":{"heartbeat_interval":41250,"server_time":"2026-01-01T00:00:00.000Z"}}
// client -> server, first frame after Hello
{"op":2,"d":{"token":"<bearer access token>","properties":{"os":"linux","client":"daemon"},"intents":0}}
// server -> client — READY payload sends guild/channel metadata upfront; full per-guild member lists and
// other bulk state are deferred until a guild is actually opened (lazy per-guild loading), keeping this
// payload's size from scaling linearly with total guild count for accounts in many guilds
{"op":0,"s":1,"t":"READY","d":{"session_id":"...","user":{...},"guilds":[...],"dm_channels":[...],"presences":[...]}}
// client -> server, on reconnect
{"op":6,"d":{"session_id":"...","seq":57}}
```

The daemon **stream-decodes** (`json.Decoder`) this payload rather than buffering it fully before parsing.

**Fan-out** (`internal/platform/events.Bus`): unchanged interface shape from the original design — in-process
by default, swappable for Redis Pub/Sub via `EVENTS_BACKEND=redis`, activated only by the flagship (§12).
Domain services call `bus.Publish` only after the originating DB transaction commits.

**Block-aware fan-out** (ADR 0013): each connection's block-set (accounts that have blocked this user, or
that this user has blocked) is cached per-connection, not queried per message. At DISPATCH fan-out time for
guild-channel message/presence/`@mention` events, a blocked author's content is filtered out of the fan-out
target list entirely — never sent over the wire to the blocker's connection, and never merely hidden
client-side. A block/unblock action updates the affected connection's cached set immediately.

**Voice signaling**: `internal/voice.MediaCoordinator` is now `PionMediaCoordinator`, backed by the real SFU
(§6). The voice-join payload carries `supports_video: bool` (CLI always `false`).

### REST API

Base `/api/v1`. Auth endpoints issue Bearer tokens directly (JSON body), never cookies, for the CLI/GUI/daemon
surface (a future BFF layer in front of this same API handles cookie issuance for the web SPA, §9):

```
POST   /auth/register                     -- requires a valid instance invite code if registration is gated
POST   /auth/login                        -- email/password -> access + refresh token pair, device_id-scoped
POST   /auth/refresh                      -- rotates the refresh token within its device_id's family
POST   /auth/logout                       -- revokes current session
POST   /auth/logout/all                   -- revoke-all-sessions primitive (§ Account lifecycle)
POST   /auth/password/forgot              -- always 202 regardless of whether email exists
POST   /auth/password/reset
POST   /auth/email/verify
GET    /auth/oauth/{provider}/authorize
GET    /auth/oauth/{provider}/callback
POST   /auth/oauth/exchange
POST   /auth/device/code                  -- CLI headless device-code flow: issue a code
GET    /auth/device/code/{code}           -- poll for completion
GET    /auth/device                       -- minimal server-rendered completion page (enter code, log in)
POST   /auth/tokens                       -- mint a scoped api_token
DELETE /auth/tokens/{id}

GET    /users/@me
PATCH  /users/@me
GET    /users/@me/guilds
GET    /users/@me/sessions
DELETE /users/@me/sessions/{id}
POST   /users/@me/channels
DELETE /users/@me                          -- account deletion, invokes revoke-all-sessions
GET    /users/@me/export                   -- server-side export; see E2E export note below

POST   /guilds
GET    /guilds/{guild_id}
PATCH  /guilds/{guild_id}
DELETE /guilds/{guild_id}
GET    /guilds/{guild_id}/channels
POST   /guilds/{guild_id}/channels
GET    /guilds/{guild_id}/members?after={id}&limit=100
PATCH  /guilds/{guild_id}/members/{user_id}
DELETE /guilds/{guild_id}/members/{user_id}
GET    /guilds/{guild_id}/roles
POST   /guilds/{guild_id}/roles
PATCH  /guilds/{guild_id}/roles/{role_id}
GET    /guilds/{guild_id}/invites
GET    /guilds/{guild_id}/audit-log
GET    /guilds/{guild_id}/emojis
POST   /guilds/{guild_id}/emojis
DELETE /guilds/{guild_id}/emojis/{id}
GET    /guilds/{guild_id}/tags
POST   /guilds/{guild_id}/tags
POST   /guilds/{guild_id}/tags/{tag_id}/messages/{message_id}

PATCH  /channels/{channel_id}
DELETE /channels/{channel_id}
PUT    /channels/{channel_id}/permissions/{overwrite_id}
GET    /channels/{channel_id}/messages?before={id}&after={id}&limit=50
POST   /channels/{channel_id}/messages
PATCH  /channels/{channel_id}/messages/{message_id}
DELETE /channels/{channel_id}/messages/{message_id}
POST   /channels/{channel_id}/typing
POST   /channels/{channel_id}/invites
POST   /channels/{channel_id}/attachments
POST   /channels/{channel_id}/read           -- update channel_read_states watermark
POST   /channels/{channel_id}/whispers
POST   /channels/{channel_id}/webhooks
DELETE /webhooks/{id}
POST   /webhooks/{id}/regenerate-token
POST   /webhooks/{id}/{token}                -- unauthenticated-except-by-token, per-webhook rate limited

GET    /invites/{code}
POST   /invites/{code}
DELETE /invites/{code}

-- Public matchmaking, friends, blocks, reports
GET    /matchmaking/channels
POST   /matchmaking/channels
GET    /users/@me/recently-met
POST   /users/@me/friend-requests
POST   /friend-requests/{id}/accept
POST   /friend-requests/{id}/decline
GET    /users/@me/friends
POST   /users/@me/blocks/{user_id}
DELETE /users/@me/blocks/{user_id}
POST   /reports

-- Instance Admin
POST   /instance/bans
DELETE /instance/bans/{user_id}
GET    /instance/reports
POST   /instance/reports/{id}/resolve
GET    /instance/audit-log
POST   /instance/admins/{user_id}
DELETE /instance/admins/{user_id}

-- Search
GET    /guilds/{guild_id}/search?q=...

-- Observability (§14/§15)
GET    /healthz
GET    /metrics                              -- Instance Admin token or localhost-only
```

E2E-encrypted DM content is never exposed via `/channels/{channel_id}/messages` in plaintext — the endpoint
returns ciphertext for `is_e2e = true` messages, decrypted only by the daemon (ADR 0014). Cursor-only
pagination (snowflake IDs) everywhere. Rate limiting via `ulule/limiter` (§14/§15), `/64` IPv6 grouping.
Attachments served from a separate origin/subdomain with no ambient credentials attached (§14).

### Auth design

`argon2id` password hashing, JWT access tokens (15 min TTL). Refresh tokens are opaque random 256-bit
values, SHA-256-hashed at rest, **scoped per `device_id`** — rotation on one device's daemon never
invalidates another device's session (a user may run daemons on more than one machine under the same
account); reuse-detection revokes only the affected device's chain. Scoped `api_tokens` support named scopes
(e.g. `messages:send`-only) for bots/automation, minted from either CLI or GUI once logged in.

OAuth (Google/GitHub) via `x/oauth2` with PKCE, same account-linking rules as the original design
(auto-link only on a provider-verified email). Two CLI login paths: a system-browser-plus-localhost-callback
loopback flow (fixed registered port + documented fallback list, since GitHub OAuth Apps require an exact
pre-registered callback URL), and a headless/SSH device-code fallback (`device_code` table, minimal
server-rendered completion page, independent of the web SPA).

**Credential ownership**: the daemon is the sole holder of its account's tokens (ADR 0011) — one keychain
entry, one process; CLI/GUI never independently store a token copy.

Password reset: unchanged behavior from the original design (always-202 anti-enumeration, single-use hashed
token, revokes all sessions on successful reset), now sent asynchronously via the SMTP relay (§11), never
blocking the HTTP response.

### Account lifecycle: deletion & data export

`DELETE /users/@me` and self-service "log out all other devices" both invoke the **general-purpose
revoke-all-sessions primitive**: force-close live gateway connections, revoke refresh + scoped tokens
(DB-backed, instantly revocable — an already-issued 15-minute access token simply can't be renewed and
expires naturally), and **revoke every linked device's E2E device-link trust** (ADR 0014) — the same
primitive an Instance Admin ban invokes (ADR 0013). Account deletion otherwise follows the original design:
soft-delete with placeholder username/email, hard-delete `oauth_identities`/`sessions`, leave authored
content in place rendered as "Deleted User."

`GET /users/@me/export` covers everything the server can see. **For E2E-encrypted DMs, the daemon — not the
server, not the CLI/GUI independently — performs its own local decrypt-and-export step**, producing a
standalone `local_e2e_export.zip` presented alongside, never merged into, the server-side export (ADR 0014).
**Export asymmetries**, applied consistently everywhere they occur: a user's own export includes reports
they filed and accounts they've blocked (their own actions), but excludes reports filed against them and who
has blocked them (protects reporters/blockers from retaliation) — verified by a dedicated end-to-end test.

### Cross-cutting

Table-driven unit tests for pure logic (permission `Resolve` near-100% coverage), `testcontainers-go`
integration tests (Postgres, and Redis for the flagship-only paths, §12), gateway handshake/heartbeat/
resume tests over real WS connections. `zerolog` structured logging, request/session-scoped loggers via
context, **never log secrets, tokens, or password hashes**. `golang-migrate` migrations, auto-run on backend
startup guarded by a Postgres advisory lock, **blocking** — `/healthz` stays unavailable until complete —
mirrored by a Helm `pre-upgrade` Job hook for the flagship (§12), same tooling, different trigger.

---

## 3. Client daemon architecture

The CLI and GUI are thin "attach" UIs over one shared local background daemon, one process per OS user
account. See [ADR 0010](adr/0010-client-daemon.md) for the full reasoning.

**Ownership**: the daemon holds the persistent WebSocket gateway connection, presence/Deep Work state,
in-memory scrollback state, the WASM plugin host (§8), and the local bot-automation listener. Voice is
deliberately **not** in the daemon (§6). The daemon is also the sole holder of auth tokens (§2) and, once E2E
exists, the E2E keystore (§7) — nothing credential-bearing lives in an attach client.

**Lifecycle**: auto-installed as a real OS-level service (systemd user unit / launchd agent / Windows
startup task), running from login. On startup, before opening any handle, the daemon raises `RLIMIT_NOFILE`
(`syscall.Setrlimit`, e.g. to 4096) — it simultaneously holds the gateway WS, N attach-client sockets, the
bot-automation TCP listener, voice-worker pipes, and SQLite/log files, and default OS limits (256 on macOS)
are easy to exceed under normal multi-client, active-voice use.

**Dual IPC, different trust tiers**:
- **Daemon↔attach-client**: a Unix domain socket / Windows named pipe, OS-file-permission-protected (no
  secret needed — only the owning OS user can open it). Reuses the gateway's exact op-code/DISPATCH protocol
  over 4-byte-length-prefixed JSON framing, so CLI and GUI share one client-side event parser. The shared
  HELLO/IDENTIFY handshake carries a semver field (MAJOR must match exactly; a defined MINOR-version-back
  window is tolerated). **The daemon's write path to each attach client is asynchronous and bounded** — a
  per-connection outbound channel with fixed capacity, fed by its own writer goroutine (see "Concurrency
  model" below); a client whose buffer fills gets **dropped**, never allowed to block the daemon's core loop,
  since that would also stall E2E ratchet advancement and voice signaling for everyone else attached. The
  dropped client resyncs on reattach.
- **Local bot-automation port**: a separate, localhost-only TCP listener with its own per-session secret
  (`0600` file or env var), authenticated via scoped `api_tokens` — deliberately lower-trust than the attach
  socket, since external scripts must not receive first-party trust.

**State persistence**: scrollback/pane/presence state is in-memory only, lost on daemon restart (tmux
semantics) — the gateway's RESUME mechanism rebuilds it. The one deliberate exception is a "last active voice
channel" breadcrumb, persisted so voice can auto-rejoin after a crash (§6). **Read state is a separate,
durable concern**: `channel_read_states` (§2) is Postgres-backed and synced via its own gateway dispatch
event — a channel is marked read automatically when the client's viewport reaches the latest message,
debounced, so opening any client on any machine shows accurate unread state.

**Config file** (`~/.config/norite/config.toml`, TOML, `pelletier/go-toml` v2 document-editing mode —
preserves hand-written comments/formatting): covers theme, keybindings, notification filters, pane-layout
preferences — anything a user should freely hand-edit. Every writer (CLI, GUI, daemon) uses atomic writes
(temp file + rename) **plus `gofrs/flock`-based locking** around each read-modify-write cycle. The daemon
hot-reloads on external changes via `fsnotify`. A **second, daemon-owned state file** holds anything
daemon-written-only: plugin capability grants + pinned `.wasm` hashes (§8), the voice-channel breadcrumb, and
the same-machine config-toggle setting below — never hand-edited, never included in export.

**Same-machine CLI/GUI config toggle**: default off (CLI and GUI share one `config.toml`, as above). An
app-settings toggle (living in the daemon state file) lets them diverge into separate files on one machine;
flipping on copies the current shared file to both as a starting point, flipping off reconciles via
last-write-wins onto one shared file.

**Config export/import**: `app config export` / `app config import` — a portable file covering the
`config.toml` scope only (never the daemon state file, which is machine-local by nature), for carrying
preferences between separate daemons/machines. Import merges key-by-key, preserving the target's existing
customization.

**Concurrency model**: the daemon leans on a deliberate family of Go concurrency patterns, not incidental
goroutine use — a bounded per-connection writer goroutine for each attach client; a single dedicated writer
goroutine + buffered channel serializing all E2E keystore SQLite writes (§7) so the WS event loop never
blocks on disk I/O; the daemon's main loop as a `select`-based multiplexer over the gateway WS, the IPC
listener, the voice-worker pipe, and `fsnotify` events; `context.Context`-based cancellation for each
plugin's per-invocation wall-clock timeout (§8). One shape throughout: isolate anything that can block or
run unboundedly behind a goroutine and a bounded channel.

**Protocol version compatibility**: the shared HELLO/IDENTIFY handshake (real gateway and local socket alike)
carries a semver field; MAJOR must match exactly, a defined MINOR-version-back window is tolerated.

---

## 4. CLI

A separate, performance-focused, fully scriptable client (Unix-style: one action, exit, pipeable
stdin/stdout), attaching to the shared daemon (§3). See [ADR 0009](adr/0009-cli-and-gui-client-architecture.md).

**Pane engine**: a custom TUI pane/split engine on the Charm stack (Bubble Tea + Lip Gloss + Bubbles), not a
real installed tmux, for identical cross-platform behavior. Any pane is a fully flexible viewport pointed at
any channel/DM independently. Tested with `teatest` (key-press/message simulation, rendered-output
assertions).

**Keybindings**: Emacs-style chorded (Ctrl/Meta combinations), not vim-modal, shipped default — stored in and
overridable via the config file's `[cli]` section.

**Markdown rendering**: a small custom renderer implementing only the allow-listed subset (bold, italic,
code, links, mentions, custom-emoji shortcodes) — not Charm's `glamour`, to keep the trusted-rendering
surface as narrow as the security posture used for message content everywhere else.

**Terminal-escape sanitization**: a blanket function strips/escapes ASCII control characters and ESC
sequences from all untrusted text (usernames, message content, link-preview titles, plugin manifest
descriptions, webhook display names) at the single point it meets terminal output — specific to the CLI,
since a malicious string with raw ANSI sequences could otherwise manipulate the terminal.

**Image rendering**: `BourgeoisBear/rasterm`-based capability detection (Kitty/iTerm2/Sixel), inline when
supported, filename/link fallback otherwise — the hook point for the "disable image loading" bandwidth
toggle (which does not suppress custom-emoji rendering).

**Structured output**: every data-printing command supports `--json`, schemas versioned in
`contracts/cli-json/` as a third source-of-truth contract alongside `openapi.yaml`/`gateway-events.schema.json`
— schema changes ship in the same commit as the code change causing them.

**Logging**: file-based, never stderr (Bubble Tea owns the alternate screen buffer), `app logs tail`,
`natefinch/lumberjack` rotation — reused by daemon, CLI, and GUI alike.

**Voice controls**: join/leave/mute/deafen via keybinding, a status line (no visual call UI — it's a
terminal), an active-speaker indicator in the status line/participant list, and two separate actions
(keybind each) for local-mute and report (§6).

**Integrated shell and dev tools**: the integrated shell spawns the user's actual shell, the same trust
boundary as a real terminal, no extra sandboxing. Code block copy/fold, local bot automation, CLI piping/
local port forwarding, and link previews (GitHub-aware + generic) are all real v1 CLI-native scope.

---

## 5. Native GUI

Built with **Gio** (`gioui.org`) — immediate-mode, GPU-rendered, tight memory control, chosen over Fyne for
that control despite the lack of a built-in widget library. See
[ADR 0009](adr/0009-cli-and-gui-client-architecture.md).

**Message rendering**: a virtualized message list, the same allow-listed markdown renderer as the CLI
reimplemented for Gio's immediate-mode primitives, including emoji-shortcode resolution.

**Pane splitting**: native widget-based tiling, the same flexible pane-content model as the CLI (§3/§4) —
independently implemented, never synced with the CLI's pane state by default.

**Theming**: the shared theme spec (named roles: background/accent/danger/muted/etc.), mapped to Gio's
native rendering, defined once in config and shared with the CLI's ANSI mapping.

**Settings**: config read/write via the same `go-toml` v2 document-editing approach as the CLI, plus a voice
input/output device-selection tab.

**Voice UI**: participant list, mute/deafen controls, an active-speaker indicator (a highlight/ring around
whoever is transmitting), and separate local-mute and report actions — wired to the same voice-worker
control path the CLI uses.

**Accessibility** is an explicit, documented non-goal for v1 (ADR 0009) — Gio provides no OS-level
accessibility-API integration, and building it on an immediate-mode toolkit with no component tree is a real,
ongoing cost, not a footnote.

**Testing**: golden-image/screenshot comparison for the highest-value, most regression-prone surfaces
(message list, pane-split layout, voice UI states), rendered headlessly and compared pixel-for-pixel against
a reference image, with manual QA covering everything else.

**Integrated whiteboard**: GUI-only, solo local quick-drafting (no real-time multi-user sync) — explicitly
the lowest-priority item in the whole GUI milestone phase, built last.

---

## 6. Voice architecture

Voice is real, working audio calling on every client in v1, including the CLI. Video/screen-share is
deferred but architected now so it's additive later. See [ADR 0012](adr/0012-voice-in-v1.md).

**Media server**: a self-hosted, custom-built SFU on Pion (Go), not LiveKit, not a plain P2P mesh.

```go
// internal/voice — room/participant model, codec/track-kind-agnostic RTP forwarding
type MediaCoordinator interface {
    AllocateSession(ctx context.Context, guildID, channelID, userID snowflake.ID, supportsVideo bool) (VoiceServerInfo, error)
}
```

The track/participant model is kept codec/track-kind-agnostic (not hardcoded to one audio track per
participant) so adding video is additive — necessary but **not sufficient**: simulcast/SVC layer-switching
for poor-connection participants is real, separate engineering work, explicitly scoped into the video phase
rather than assumed free (§2/ADR 0012).

**Voice-worker subprocess**: spawned on-demand by the daemon via `os/exec` when joining a voice channel, torn
down on leaving (never a persistent idle process). Owns the entire audio session: capture/encode/send,
receive/decode/play, noise suppression, echo cancellation, AGC. Daemon↔worker IPC uses the child's inherited
stdin/stdout pipes (free, no socket/port allocation, pipe-close is itself a crash signal), the same
4-byte-length-prefix JSON framing as the daemon↔CLI/GUI socket. The worker holds its own direct WebRTC
connection to the SFU — RTP audio never flows through the daemon, only control signaling, which is what makes
fault isolation real: a media-pipeline bug can only crash the worker.

**DSP pipeline, strict non-negotiable order**: **Mic Capture → AEC → RNNoise → AGC → Opus Encode**. RNNoise
before AEC would non-linearly distort the mic signal and break AEC's linear echo-correlation assumption,
producing a feedback loop instead of cancelling it. Opus, RNNoise, and `libspeexdsp`'s AEC/AGC are all cgo
bindings, contained entirely to the `voiceworker/` binary — the only place cgo is allowed anywhere in the
stack.

**Adaptive bitrate**: `pion/interceptor`'s REMB/TWCC feedback drives `hraban/opus`'s runtime bitrate control,
audio-only, no simulcast — the Prometheus voice/SFU metrics (§15) feed this loop directly, not just a
dashboard.

**TURN**: an embedded Go TURN server (`pion/turn`, also answering plain STUN), bundled into the backend —
self-hosters don't run a separate `coturn`.

**Voice deployment opt-out**: TURN/SFU need a reachable public IP and forwarded UDP range — a real burden
many home self-hosters can't satisfy. An Instance Admin can disable voice entirely; the SFU/TURN never start,
voice+text channel pairs degrade to text-only, and voice UI is hidden entirely in CLI/GUI (never grayed out).

**No call recording, ever** — a permanent non-goal. Public-matchmaking voice abuse therefore has no recorded
evidence; both CLI and GUI mitigate this with a real-time **active-speaker indicator** (a highlight/ring
around whoever is transmitting) plus two **separate** actions (each its own keybind/click) — local-mute
(silences a participant for this user alone) and report.

**Mic permission and global hotkey**: the foreground CLI/GUI triggers the OS permission prompt on first voice
use, then hands capture to the worker once granted — unverified per-OS behavior until a dedicated spike
milestone determines the real answer (macOS TCC, Input Monitoring entitlement for global hotkeys).
Voice-activity-detection is the default input mode; push-to-talk (`golang.design/x/hotkey`) is registered
once by the daemon (not either attach client), avoiding double-registration if both CLI and GUI are attached.

**Auto-rejoin**: on daemon crash/restart mid-call, the daemon respawns the worker and rejoins using the
persisted "last active voice channel" breadcrumb (§3) — the one exception to otherwise-ephemeral daemon
state. The client auto-update mechanism (§11) defers applying an update while a voice session is active, so
it never forces a mid-call daemon restart.

**Video/screen-share (deferred, seamed now)**: owned directly by the GUI/web client — a second, separate
WebRTC connection to the SFU, never through the daemon. The voice-join payload carries `supports_video: bool`
from day one (CLI always `false`).

---

## 7. BYOK end-to-end encryption

Opt-in, restricted to the `DM` channel type only. See [ADR 0014](adr/0014-e2e-encryption.md) for full
reasoning including the pairwise-scaling limitation and the compounding-risk framing.

**Cryptographic base**: `go.mau.fi/libsignal`, a mature pure-Go Signal-protocol port — license compatibility
with the project's restrictive license (ADR 0007) is a **blocking prerequisite**, checked before any
integration code is written.

**Key boundary — the daemon holds the keys**: the daemon owns the E2E keystore/ratchet state end to end and
performs all decryption itself; CLI/GUI receive plaintext over the already-trusted local IPC socket (§3),
same as every other event. They never independently hold key material.

**Keystore**: `modernc.org/sqlite` (pure Go), master key in the OS keychain, surviving daemon restarts. All
writes route through one dedicated writer goroutine + buffered channel (§3's "Concurrency model") so a burst
of concurrent incoming encrypted messages never produces a `database is locked` error or blocks the WS event
loop. A **mandatory** local FTS5 search index sits on this same keystore, over the decrypted message store,
encrypted at rest via the keystore master key — E2E DMs lose server-side search (below), so this is not
optional.

**Device linking**: a fully custom flow (no off-the-shelf equivalent — a second real piece of custom crypto
protocol) where the primary device authorizes a new device. CLI-side verification is text/code-based safety
numbers (no camera/QR). **No history-transfer mechanism exists** — a newly linked device sees only messages
sent after linking, matching the permanent-loss framing below.

**External cryptographic security review — hard release gate**: a build/instance-level flag keeps E2E
unavailable to any account beyond the developer's own test accounts until a real external audit of the
library integration and the device-linking protocol passes. Property-based/fuzz testing (`go test -fuzz`) is
a cheap complementary layer, never a substitute.

**No key-loss backup, permanent, by design**: losing a device means permanent loss of that device's history —
keys never leave the device, no server-side or passphrase-backup escrow.

**Feature tradeoffs**: E2E-encrypted DMs are excluded, full stop, from any server-side capability that reads
message content — search, moderation visibility, audit-diffing, edit-history, link-preview generation,
account export (`CLAUDE.md` rule 13). Whispers are explicitly excluded from E2E scope entirely, so the
Instance Admin break-glass path (ADR 0013) always has plaintext to fall back on.

**Account export**: the daemon performs its own local decrypt-and-export step, producing a standalone
`local_e2e_export.zip` presented alongside — never merged into — the server-side export (§2).

**Device revocation**: revoking a device's session (§2's revoke-all-sessions primitive) also revokes its E2E
device-link trust in the same action.

---

## 8. Client-side plugins

Sandboxed via WASM using `wazero` (pure Go, no cgo), running inside the daemon (one host, available to both
CLI and GUI). See [ADR 0015](adr/0015-plugin-sandboxing.md).

**Headless by design**: the host-function API surface is slash-commands, text-parsing, and data/message
reads only — no UI-injection capability, no IPC bridge for painting native CLI/GUI elements. A plugin affects
what the user sees only through the data/text an already-capability-gated host function returns.

**Distribution**: local files only in v1 (drop a `.wasm` in a plugins folder) — no registry/marketplace.
TinyGo is the recommended (not required) authoring toolchain.

**Capability manifest + hash-pinning**: each plugin ships `manifest.toml` declaring needed capabilities; the
daemon requires explicit first-load approval (browser-extension-style). Approval also pins a SHA-256 hash of
the `.wasm` file, stored alongside the grant in the daemon's state file (§3); every later load re-verifies
against that hash, halting and re-prompting on mismatch — so a file swapped on disk after approval can never
silently run under the stale grant.

**Resource limits**: enforced CPU (instruction-count/timeout) and memory quotas per instance, plus a separate
per-invocation wall-clock timeout (catches a plugin that hangs on a slow network call without ever tripping
the CPU quota). `wazero`'s instruction-metering imposes a real baseline host-CPU cost; a dedicated milestone
benchmarks this against real plugin workloads, falling back to memory quotas + the wall-clock timeout as the
primary mechanism if metering costs more than what it measures.

---

## 9. The (later) web client

The originally-planned React SPA design is kept, demoted to the third, lowest-priority client, built only
after the CLI and GUI exist (Phase O). See [ADR 0009](adr/0009-cli-and-gui-client-architecture.md).

**Stack**: React 18 + TypeScript (strict) + Vite, React Router v7, Tailwind + shadcn/ui, TanStack Query,
Zustand (gateway-fed real-time stores), Zod (validates every gateway payload + form input), react-hook-form,
`@tanstack/react-virtual`, pnpm, Vitest + Playwright.

**Auth**: its own BFF-style httpOnly-cookie exchange layer in front of the same token API the CLI/GUI/daemon
use (§2) — the web client never holds a raw Bearer token in JS. See
[ADR 0002](adr/0002-cookie-based-auth.md) (still-correct historical rationale) and
[ADR 0011](adr/0011-token-based-client-auth.md) (current design). `openapi.yaml`/`gateway-events.schema.json`
are sanity-checked against real browser constraints (CORS, chattiness, BFF-compatibility) on an ongoing
basis, starting the moment each contract becomes load-bearing — not deferred until Phase O.

**State layers**: the same three-layer split as the original design (TanStack Query server cache, Zustand
gateway-fed real-time stores with one dispatcher routing DISPATCH frames, local/component UI state).

**Pane splitting**: CSS grid/flex-based resizable panes, `localStorage`-based layout persistence — the web
client's own independent implementation of the same pane-content model the CLI/GUI use, never synced with
either by default (the manual `app config export`/`import` path, §3, is how a user manually carries
preferences across clients).

**E2E export**: the browser-side decrypt-and-export equivalent of §7's daemon-performed step.

**Testing**: Vitest + React Testing Library + MSW + WS mocks, `tsc --noEmit` strict, ESLint + Prettier,
Playwright E2E against the real docker-compose stack.

---

## 10. Contracts & Dev Workflow

- `contracts/openapi.yaml` — REST source of truth. `oapi-codegen` generates Go server-side request/response
  types and routing scaffolding directly from it, starting at the milestone that first wires up guild/channel
  CRUD — a spec change not reflected in code fails to compile rather than silently drifting.
- `contracts/gateway-events.schema.json` — WS event source of truth, hand-maintained, mirrored by
  frontend/daemon-side Zod schemas.
- `contracts/cli-json/` — CLI `--json` output schemas, the third versioned contract artifact, same
  same-commit-with-the-code-change rule as the other two.
- `docker/docker-compose.yml`: Postgres, Redis (present from the start so the Redis-backed swap can be
  exercised later without a compose change), backend (hot-reload via `air`).
- `justfile`: `just dev`, `just test`, `just lint`, `just build`, `just db-migrate`, `just security-scan`
  (`govulncheck` + `pnpm audit` + `Trivy` once Dockerfiles exist).
- CI: Go and frontend jobs gated by path filters; a dedicated `security` job runs `govulncheck`, `pnpm
  audit`, and (once Dockerfiles exist) `Trivy` image scanning on every PR.

---

## 11. Business posture, platform scope, and operations

**Licensing and commercial model**: no public license — default copyright, all rights reserved. Two
independent deployments, each granted rights individually via a signed license file: **the free flagship
instance is the primary product**; self-hosted instances (sold via a flat one-time license, most likely to
appeal to enterprises and other private groups) are a real, fully-built secondary offering. No shared
multi-tenancy, no public license text to draft or maintain. See
[ADR 0007](adr/0007-licensing-and-project-posture.md).

**Federation and mobile**: both explicit non-goals for v1 — each instance is an island, no dedicated mobile
client planned. See [ADR 0019](adr/0019-platform-scope-and-commercial-model.md).

**Rate limiting**: `ulule/limiter` is the base REST/gateway rate-limiting library from the foundational
backend milestone onward — in-memory store self-hosted, Redis-backed for the flagship (§12). **All**
IP-based limiting groups IPv6 traffic by `/64` subnet, not exact address, globally — not scoped to any one
feature.

**Database connection management**: `pgx` pool sizing per backend replica is kept intentionally small and
documented; PgBouncer is recommended once a self-hoster's instance/device count outgrows a small direct pool.

**Data retention**: a configurable pruning seam for `audit_log_entries`/`instance_audit_log` only, default
**disabled** — message history and reports stay permanent by design, never covered by this seam.

**Auto-update, code signing, ACME, SMTP, telemetry**: see
[ADR 0020](adr/0020-operations.md) for the full design — Sigstore/cosign release signing (distinct from the
license file's own Ed25519-JWT scheme), anti-downgrade/fail-closed/rollback-on-crash-loop hardening,
backend/server updates staying manual, `certmagic`-based automatic HTTPS (deployment-time opt-out),
async/opt-out SMTP, and opt-in-crash-reports-only telemetry to a self-operated endpoint.

---

## 12. Flagship instance Kubernetes deployment architecture

See [ADR 0021](adr/0021-flagship-kubernetes-deployment.md) for full reasoning. Summary:

- **API/Gateway pods**: standard `Deployment`, multiple replicas behind Ingress — possible because Redis
  pub/sub fan-out (§2) lets gateway events cross replicas.
- **TURN/SFU pods**: separate, `hostNetwork: true`, own "privileged" Pod-Security-Standard namespace,
  isolated from the "restricted" namespace everything else runs in.
- **Stateful dependencies**: self-managed in-cluster operators — CloudNativePG (Postgres, with native
  continuous WAL-archiving backup to in-cluster MinIO), a Redis Helm chart, MinIO — not managed cloud
  services.
- **TLS**: `cert-manager` + Ingress, not the backend's built-in `certmagic` (every replica racing to manage
  one cert would fail).
- **Rate limiting**: `ulule/limiter`'s Redis-backed store specifically here, so replica count can't multiply
  an intended limit.
- **Migrations**: a Helm `pre-upgrade` Job hook — same `golang-migrate` tooling as self-hosted (§2), Job
  trigger instead of auto-run-on-startup.
- **Graceful rollout**: the gateway's `Reconnect` op-code via a `preStop` hook, **staggered** across
  `terminationGracePeriodSeconds` with client-side randomized exponential backoff — avoids a thundering herd
  against the remaining replicas' auth/DB layer.
- **Deployment**: Helm charts, CI-triggered `helm upgrade`, not GitOps (not yet justified by scale).
- Doubles as the reference "how to run this at real scale" pattern for advanced self-hosters.

---

## 13. Unified Milestone Roadmap

Dependency-ordered, phase-grouped, `M0` through `M117`. Read as a long-term critical path, not a near-term
promise — the accumulated scope (custom SFU, custom crypto, native GUI, plugin sandbox, licensing
infrastructure) is realistically multi-year work. Phase P (flagship Kubernetes) is explicitly parallel, not
sequential — it can start once core messaging and voice are usable (after M37) and continues absorbing new
features as they land. `M116`/`M117` sit numerically at the end but are logically earlier, annotated below.

**Phase A — Foundation**: `M0` monorepo scaffolding → `M1` backend skeleton (chi, sqlc/pgx, blocking
advisory-lock auto-migration, base `ulule/limiter` w/ `/64` grouping, `/healthz`) → `M2` `app instance init`
infra config → `M3` CLI skeleton + daemon lifecycle stub.

**Phase B — Auth**: `M4` backend auth core (`device_id`-scoped refresh families) → `M5` SMTP + password reset
→ `M6` OAuth backend → `M7` CLI password login → `M8` CLI OAuth loopback → `M9` CLI headless device-code →
`M10` `app instance init` finish → `M11` session revocation primitive.

**Phase C — Guild/channel/permission core**: `M12` guilds/channels/roles schema + CRUD (`oapi-codegen`
wired) → `M13` permission engine → `M14` guild audit log → `M15` core messaging CRUD → `M16` guild-level
reports → `M17` message tagging.

**Phase D — Real-time gateway and daemon**: `M18` gateway protocol core (server-time-in-HELLO, lazy/streamed
READY) → `M19` daemon as gateway client (clock offset, stream-decode) → `M20` daemon↔CLI/GUI IPC (bounded
async writes) → `M21` config file (split, flock, toggle, export/import) → `M22` local bot-automation port →
`M23` daemon lifecycle polish (`RLIMIT_NOFILE`) → `M24` client auto-update (voice-session-aware once Phase E
exists).

**Phase E — Voice**: `M25` mic-permission/global-hotkey spike → `M26` Pion SFU core → `M27` embedded
TURN/STUN → `M28` voice-worker subprocess + IPC → `M29` Opus pipeline → `M30` audio DSP (strict AEC→RNNoise→
AGC order) → `M31` adaptive bitrate → `M32` mic-permission handoff → `M33` voice signaling (real) → `M34` CLI
voice controls (active-speaker, mute/report) → `M35` voice input mode/PTT → `M36` voice auto-rejoin → `M37`
voice deployment opt-out.

**Phase F — Presence, Deep Work, CLI polish**: `M38` presence persistence → `M39` Deep Work (+ offline
`@urgent` email fallback) → `M40` OS desktop notifications → `M41` CLI pane engine → `M42` CLI keybindings →
`M43` CLI markdown renderer → `M44` CLI terminal-safe sanitization → `M45` CLI image rendering → `M46` CLI
`--json` output → `M47` CLI logging → `M48` CLI TUI testing.

**Phase G — DMs, invites, attachments, chat power features**: `M49` DMs/Group DMs/invites → `M50`
attachments storage → `M51` custom emoji → `M52` incoming webhooks → `M53` whispers → `M54` regex
notification filters → `M55` bandwidth toggles → `M56` link previews.

**Phase H — Search**: `M57` Postgres full-text search (sync v1, async-decoupling documented as flagship-only
future upgrade).

**Phase I — Public matchmaking, friends, blocks, Instance Admin**: `M58` public matchmaking channel type →
`M59` anti-abuse (global `/64` grouping) → `M60` recently-met → `M61` friends → `M62` blocks (server-side
gateway-dispatch filtering) → `M63` Instance Admin schema → `M64` bans/enforcement/audit → `M65` lockout
recovery → `M66` instance-level reports routing + whisper break-glass → `M67` proactive intervention → `M68`
public-channel/whisper retention → `M69` data export asymmetry verification.

**Phase J — Native GUI**: `M70` skeleton → `M71` message rendering → `M72` pane splitting → `M73` theming →
`M74` settings + voice device tab → `M75` voice UI (active-speaker, mute/report) → `M76` GUI testing → `M77`
integrated whiteboard.

**Phase K — Dev tools and extensibility**: `M78` code block enhancements → `M79` integrated shell → `M80`
WASM plugin host → `M81` capability manifest + hash-pinning → `M82` plugin resource limits + metering
benchmark → `M83` plugin distribution/TinyGo docs.

**Phase L — Self-hosting polish, P2P, ops**: `M84` backup/restore docs → `M85` Prometheus metrics → `M86` P2P
file transfer (three-way handshake) → `M87` container image scanning → `M88` docker-compose production path.

**Phase M — E2E encryption**: `M89` crypto base integration (license gate) → `M90` E2E keystore (mandatory
FTS5, single-writer goroutine) → `M91` E2E opt-in UX (DM-only) → `M92` device-linking flow (no history sync)
→ `M93` device revocation/E2E trust linkage → `M94` fuzz testing → `M95` external cryptographic security
review (hard gate) → `M96` E2E-aware account export (standalone zip).

**Phase N — Video/screen-share**: `M97` GUI/web video connection → `M98` capture/selection UI → `M99` SFU
video forwarding activation (including simulcast/SVC as real remaining work).

**Phase O — Web SPA**: `M100` BFF cookie-exchange auth → `M101` web SPA rebuild → `M102` pane-splitting →
`M103` E2E export.

**Phase P — Flagship Kubernetes (parallel track)**: `M104` Helm skeleton + API pods → `M105` CloudNativePG +
backups → `M106` Redis + event-bus/rate-limit activation → `M107` MinIO → `M108` TURN/SFU pods → `M109`
`cert-manager` + Ingress TLS → `M110` graceful rollout (staggered preStop + backoff) → `M111` DB migration Job
hook → `M112` Secrets → `M113` autoscaling → `M114` NetworkPolicies → `M115` CI-triggered `helm upgrade`.

**Gap-closure milestones** (numerically appended, logically earlier): `M116` read-state sync (conceptually
Phase D/G, depends on `M12`/`M18`) → `M117` data retention/audit-log pruning (conceptually Phase L, depends on
`M14`/`M64`).

**Key dependency notes**: `M37` before `M58`; `M53` before `M66` and `M91`; `M60` before `M61`; `M49`/`M60`/
`M61` before `M62`; `M11` before `M64`/`M93`; `M50` before `M51`; `M89`'s license check before any further
Phase M work; Phase P depends on `M106` requiring Phase D's Redis-fan-out seam, and `M50`/`M105` requiring
`M107`.

---

## 14. Security (deep dive)

**Threat model summary**: a multi-tenant system exposed to the public internet, handling user-generated
content, third-party OAuth, credential/key material (auth tokens, E2E keys), and — new in this design — a
local-machine daemon holding real secrets and two distinct local IPC trust tiers. Main threat classes: authz
bypass, injection, session/token/key theft, SSRF, terminal-escape injection (CLI-specific), abuse/DoS, and
E2E key-boundary violations.

1. **Injection**: unchanged from the original design — 100% parameterized SQL via `sqlc`, validated request
   bodies, unknown-field rejection.

2. **AuthZ, not just AuthN**: unchanged core rule — every mutating handler resolves `roles.Resolve` (or an
   explicit hierarchy/Instance-Admin check) before the write, using freshly-loaded data for the specific
   guild/channel in the request path; every guild-scoped mutation writes an audit log entry in the same
   transaction; every Instance Admin action writes to `instance_audit_log` in the same transaction.

3. **Token-scope model**: Bearer access tokens (15 min), `device_id`-scoped refresh-token families, scoped
   `api_tokens`. **The daemon is the sole holder of its account's credential material** — access/refresh
   tokens and, once E2E ships, the E2E keystore — never an attach client independently.

4. **Two-tier local IPC trust model**: the daemon↔CLI/GUI Unix socket (OS-permission-protected, first-party)
   vs. the local bot-automation TCP port (secret-protected, external scripts) — never treated as
   interchangeable; any new local IPC surface must state its trust tier explicitly.

5. **XSS / terminal-escape injection**: web/GUI message content renders through the allow-listed markdown
   subset, never raw HTML. The CLI has an additional, CLI-specific risk: untrusted text (usernames, messages,
   link-preview titles, plugin manifest descriptions, webhook display names) must pass through the
   terminal-safe sanitization function before reaching terminal output, or a malicious ANSI escape sequence
   could manipulate the user's terminal.

6. **CSRF/CSWSH**: not applicable to the CLI/GUI/daemon token-authenticated surface (no ambient browser
   credential). Returns, scoped to the future web SPA's BFF cookie layer only, when that client is built.

7. **WASM plugin sandbox boundary**: `wazero`, no raw filesystem/network/syscall access unless explicitly
   granted; capability manifest + first-load approval + SHA-256 hash-pinning (re-verified on every load, so a
   file swapped on disk after approval can't silently run under a stale grant); enforced CPU/memory quotas
   plus a per-invocation wall-clock timeout — no plugin path is exempt.

8. **E2E threat model**: the daemon holds all key material; E2E is `DM`-channel-type-only, text-only, never
   `GROUP_DM`/guild channels/whispers/voice. The `go.mau.fi/libsignal` integration is license-gated (ADR
   0007) before any code is written. A hard release-gate flag keeps E2E disabled beyond the developer's own
   test accounts until the external audit (covering the library integration and the fully-custom
   device-linking protocol) passes. Device revocation cuts E2E device-link trust in the same action as
   session revocation.

9. **P2P opt-in/IP-exposure disclosure**: file transfer is explicit opt-in per transfer, enforced as a real
   three-way handshake (Intent → Accept → only then SDP/ICE) so an IP address is never exposed before
   consent.

10. **Ban/deletion session-revocation mechanism**: force-closes live connections and revokes refresh/scoped
    tokens immediately (DB-backed); an already-issued short-lived access token simply can't be renewed and
    expires naturally within its short window — this keeps the stateless-JWT performance design intact
    rather than adding a revocation check to every request.

11. **`/metrics` endpoint**: requires an Instance Admin token or is localhost-only bound; metric labels stay
    aggregate-only (no per-guild/per-channel identifiers) — avoids both information disclosure and unbounded
    label-cardinality growth.

12. **SSRF protection** (link previews, and any future user-supplied-URL fetch): a custom `DialContext`
    rejects private/loopback/link-local resolved IPs, checked at actual connect time (not string-matching the
    URL, since DNS rebinding can shift the resolved IP between check and connection), with a strict request
    timeout and a response-size cap.

13. **Block-enforcement checkpoints**: server-side at gateway DISPATCH fan-out time (a cached per-connection
    block-set, invalidated immediately on block/unblock) — never a client-side-only filter, both to avoid
    shipping blocked content over the wire and to prevent a modified client from ignoring the filter.

14. **Password-reset and reports anti-abuse**: always-identical response regardless of email existence
    (anti-enumeration), rate-limited; report filing rate-limited per user with reporter-history triage.

15. **OAuth loopback design**: a fixed registered local callback port with a documented fallback-port list,
    since GitHub OAuth Apps require an exact pre-registered callback URL.

16. **Auto-update hardening**: Sigstore/cosign signature verification via self-contained offline-verifiable
    bundles, anti-downgrade protection, fail-closed on verify failure, auto-rollback on repeated crash-loop —
    deliberately distinct from the license file's own Ed25519-JWT scheme (ADR 0020).

17. **File uploads**: unchanged from the original design — server-side content-type sniffing, randomized
    storage keys, separate origin with no ambient credentials, plus a stricter validation path for custom
    emoji specifically (resolution cap, format allow-list, decompression-bomb guard — emoji render
    automatically and repeatedly for every viewer, unlike a regular attachment opened deliberately).

18. **Rate limiting & abuse prevention**: `ulule/limiter`, `/64` IPv6-subnet grouping globally (not scoped to
    one feature), stricter limits on `/auth/*`, per-webhook rate limiting independent of the creating user's
    own limit.

19. **Dependency & supply-chain hygiene**: `govulncheck`/`pnpm audit`/`Trivy` in CI on every PR.

20. **Testing security properties**: adversarial `Resolve` unit tests, cross-guild access-control integration
    tests, and (new) an automated test asserting the export asymmetries (blocks, reports) hold.

## 15. Performance & Optimization (deep dive)

1. **Indexing planned alongside schema**: every index in §2's DDL serves a specific known query shape,
   including the new GIN indexes for search (`content_search`, `topic_search`, `pg_trgm`) and the
   `channel_read_states`/`blocks(blocked_id)` indexes added for this design. Any new query scanning without
   index backing is a bug, not a follow-up — verify with `EXPLAIN ANALYZE`.

2. **Avoiding N+1 on gateway READY**: unchanged core discipline (batched queries, not per-guild loops), now
   combined with **lazy per-guild loading** — full member lists and other bulk per-guild state are deferred
   until a guild is actually opened, keeping the payload from scaling linearly with total guild count. The
   daemon stream-decodes (`json.Decoder`) rather than buffering fully before parsing.

3. **Connection pooling**: `pgxpool` sized relative to available CPU cores, kept intentionally small per
   backend replica (§11); PgBouncer recommended once a self-hoster's instance/device count outgrows it.

4. **Gateway backpressure**: bounded per-connection outbound buffer with disconnect-on-full (unchanged
   principle), now also applied identically to the daemon's own write path to each attach client (§3) — the
   same discipline at both hops of the pipeline.

5. **Regex notification filters**: server-side evaluation via Go's stdlib `regexp` (RE2) — linear-time by
   construction, no catastrophic-backtracking vulnerability class — with a pattern-length cap as cheap
   defense-in-depth; no third-party PCRE-style regex library is used, since that would reintroduce the exact
   risk class being avoided.

6. **Search indexing**: synchronous `tsvector` generated columns for v1 at both deployment scales; an
   async-queue-decoupled indexing path is a documented, not-built, upgrade path specifically for the flagship
   if/when insert-path latency under real load justifies it.

7. **Horizontal scaling seams**: `EVENTS_BACKEND=inproc|redis` swap for the event bus, Redis-backed
   rate-limiter store — both activated only by the flagship (§12); self-hosted single-process instances never
   touch either. Don't build the Redis paths' operational surface before there's real load to justify it.

8. **Voice/SFU performance**: the adaptive-bitrate control loop (REMB/TWCC → Opus runtime bitrate) closes a
   real optimization gap in the custom SFU — framed as load-bearing v1 functionality, not a nice-to-have,
   fed directly by the Prometheus voice metrics (packet loss, bitrate, jitter), which are themselves a real
   v1 operability requirement given how hard voice-call-quality problems are to debug from logs alone.

9. **WASM plugin overhead**: `wazero`'s instruction-metering has a real baseline host-CPU cost; benchmarked
   against real plugin workloads, with memory quotas + wall-clock timeout as the fallback primary mechanism
   if metering costs more than it measures (§8).

10. **E2E keystore I/O**: SQLite is single-writer — all keystore writes route through one dedicated writer
    goroutine + buffered channel, so a burst of concurrent incoming encrypted messages never blocks the WS
    event loop on disk I/O (§7).

11. **Frontend rendering performance** (web SPA, §9): unchanged from the original design —
    `@tanstack/react-virtual` for message/member lists, route-level code splitting, shallow-equality Zustand
    selectors, tuned TanStack Query `staleTime`.

12. **Asset/attachment performance**: unchanged — server-side thumbnail generation, long-lived cache headers
    from the separate attachment origin.

13. **Measure before optimizing further**: `net/http/pprof` (internal-only), the Prometheus `/metrics`
    endpoint (auth-gated, aggregate-only labels) from the foundational milestone — real production numbers
    drive future optimization work, not guesses.

---

## 16. Verification

This is largely still a documentation-and-early-implementation-phase plan, so verification means both an
internal-consistency pass over this document/`CLAUDE.md`/`docs/adr/`, and, once code exists, real tests:

- **Consistency**: grep this doc set for "AGPL," "cookie," "CSRF," "frontend" (outside §9's now-scoped
  usage), and "voice"+"deferred" — confirm none read as stale (licensing/auth/voice language should all match
  the current design, not the pre-v2 one). Confirm the daemon-holds-E2E-keys language is consistent
  everywhere (never "the CLI/GUI hold the keys"). Confirm every milestone number referenced in prose matches
  §13 exactly (`M0`–`M117`).
- **Backend**: `go test ./...` clean, `govulncheck ./...` clean; manually exercise the token-based auth
  round-trip (register → login → Bearer-authenticated request) and a raw WS connection through
  Hello→Identify→READY with a real access token; confirm a request scoped to another guild/channel is
  rejected (403, not a silent empty result); confirm the block-aware fan-out filter actually removes a
  blocked author's events from the DISPATCH stream (inspect the stream, not just client rendering).
- **Daemon/CLI/GUI**: confirm a CLI-side test client attached to the daemon's socket receives the same
  DISPATCH events the daemon gets from the real gateway; confirm a deliberately frozen attach client gets
  dropped without stalling a second, healthy attached client; confirm `app config export`/`import` round-trips
  correctly.
- **Voice**: confirm the DSP chain order (AEC before RNNoise) via a two-party echo test; confirm the
  voice-worker crash is detected via the closed pipe without affecting messaging.
- **E2E**: confirm two test identities complete a key exchange with forward secrecy demonstrated; confirm the
  release-gate flag actually blocks E2E for a normal account until flipped.
- **Security spot-checks each milestone**: an XSS/ANSI-escape payload renders as inert text on every client;
  the audit log gets an entry for every guild-scoped mutation exercised in tests; the account-export
  asymmetries (blocks, reports) hold under an automated test.
