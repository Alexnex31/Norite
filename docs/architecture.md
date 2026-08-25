# Architecture — Master Plan

> This is the canonical, version-controlled architecture reference for **Norite**. It is a snapshot of the
> plan the project was bootstrapped from — keep it updated as real decisions supersede it (a stale doc that
> nobody trusts is worse than no doc; if you deviate from something here, update this file in the same PR).
> See also: `/CLAUDE.md` for the fast-loading summary + non-negotiable rules, and `docs/adr/` for short,
> focused records of the most contested individual decisions below, and `docs/roadmap.md` for the
> dependency-ordered milestone sequence (`M0`–`M125`) that this document deliberately does not restate.

## Context

Norite is a voice-and-text chat platform. The primary way to use it is the free, global, publicly-hosted
flagship instance (§12) — self-hosting is a real, fully-built feature, not the platform's core identity: a
one-time-purchase-licensed offering (§11) aimed at enterprises and other private groups who want their own
instance. Source visible but under no public license (all rights reserved, §11). Four clients: the
scriptable CLI (§4), the full-screen TUI (§4a), a native GUI (§5), and a lower-priority web SPA built later
(§9) — the first three sharing one local background daemon per OS user account, as the Clients bullet
below sets out. The full scope described here is realistically multi-year,
systems-engineering-team-sized work; the milestone roadmap (§13) is a long-term dependency-ordered critical
path, not a near-term promise. No scope described in this document is removable; the roadmap is ordered so
the most foundational pieces land first.

Locked-in decisions:
- **V1 scope is large and real, not text-first-then-later**: guilds/channels/roles/permissions, real-time
  messaging, DMs, presence, invites, **voice calling on every client including the terminal ones**, BYOK
  end-to-end encryption (DM-only), public matchmaking, friends, blocks, Deep Work, message tagging,
  whispers, regex notification filters, per-guild custom emoji, incoming webhooks, a client-side WASM plugin
  system, P2P file transfer, the Instance Admin/reports moderation system, and self-hosting infrastructure
  (SMTP, automatic HTTPS) all ship in v1. Video/screen-share is the one genuinely deferred-but-seamed piece.
- **Clients**: four, not three — the scriptable CLI (the command tree, §4) and the full-screen TUI (§4a)
  ship in one binary and share one command tree, the native GUI (§5) mirrors the TUI's information
  architecture, and the web SPA (§9) is third and lowest-priority. All are thin UIs; CLI, TUI and GUI
  attach to one shared local daemon per OS user account (§3) which does the real work. See [ADR
  0009](adr/0009-cli-and-gui-client-architecture.md).
- **Backend architecture**: Go modular monolith (single deployable), organized so pieces could be peeled into
  services later, without building actual service separation now. See
  [ADR 0001](adr/0001-modular-monolith.md).
- **Auth**: token-based (Bearer access + refresh, OS-keychain storage, `device_id`-scoped families) for the
  CLI/TUI/GUI/daemon; email/password + OAuth (Google/GitHub) with account linking at the backend. See
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
├── cli/                          # The `norite` binary — command tree (§4) and TUI (§4a)
│   ├── cmd/app/                  # main() only: process lifetime and exit codes, nothing else
│   ├── internal/cliapp/          # urfave/cli v3 command tree, global --json/--help flags, completions
│   ├── internal/<command>/       # one package per command group, e.g. instanceinit (`norite instance init`)
│   ├── internal/termsafe/        # the blanket terminal-escape sanitizer every untrusted string passes
│   ├── tui/                      # pane engine, keybindings, markdown renderer, image rendering
│   └── go.mod
├── gui/                          # The native GUI — Gio
│   ├── app/                      # Gio window/event loop
│   ├── widgets/                  # hand-built: message list, pane tiling, voice UI, settings, whiteboard
│   └── go.mod
├── daemon/                       # Shared background daemon
│   ├── cmd/daemond/              # main() only: process lifetime, signals, exit codes
│   ├── credentials/              # the stored session: keyring-or-file secret, record, device identity
│   ├── internal/daemonproc/      # single-instance flock, log rotation, startup sign-in, clean shutdown
│   ├── internal/paths/           # the per-user 0700 state directory, resolved per platform
│   ├── gatewayclient/            # holds the real WS connection, in-memory scrollback/presence
│   ├── ipc/                      # Unix socket / named pipe server, bot-automation TCP listener
│   ├── config/                   # go-toml v2 document-editing, fsnotify hot-reload, flock, config split
│   ├── plugins/                  # wazero host, capability manifest + hash-pinning
│   ├── voiceworker/               # os/exec spawn/supervise, stdin/stdout IPC framing
│   ├── e2e/                       # keystore (modernc.org/sqlite), ratchet, single-writer goroutine, FTS5
│   └── go.mod
├── voiceworker/                  # Separate binary — the only place cgo is allowed
│   ├── audio/                    # hraban/opus, RNNoise, WebRTC APM (AEC3) — capture/encode/DSP
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

**Where the choice was contested**, so it isn't relitigated later: `zerolog` over `zap` and stdlib
`log/slog` for allocation-free structured logging on the gateway hot path; `certmagic` over the lower-level
`autocert` for its more robust renewal/retry/storage handling; `wneessen/go-mail` over bare `net/smtp`,
which is too low-level, without pulling in a templating engine for one or two plain emails; `minio-go` over
the full `aws-sdk-go-v2` — it speaks S3 and is far lighter for a single bucket; and `PuerkitoBio/goquery`
(a jQuery-like API over `golang.org/x/net/html`) for OpenGraph `<meta property="og:...">` extraction.

Each domain package has the same shape: `service.go` (business logic + permission checks — **every**
mutating method takes an already-authenticated `actor` and calls `roles.Resolve` before touching data),
`http.go` (chi sub-router), `model.go`, `events.go` (dispatches after a DB transaction commits, never
before). Services depend on narrow repository interfaces over the single `internal/db` sqlc package.

**Middleware chain order** (outermost first): `SanitizeInboundRequestID` → `RequestID` → `EchoRequestID` →
`RealIP` → `Recoverer` → `SecureHeaders` → `StructuredLogger` → `RateLimit` (route-bucketed, `/64` IPv6
grouping, §14) → `AuthenticateBearer` (populates `actor` from the JWT access token; 401 if absent on
protected routes) → domain handler. No CSRF middleware exists on this surface at all — see "Auth design"
below.

Two of those exist for the same reason and make the same call. `SanitizeInboundRequestID` and `RealIP` both
decide whether a client-supplied forwarded header may be believed, and both answer "only when
`trust_proxy_headers` is on". A request ID is not merely cosmetic: it is echoed to the client, written into
every log line, and returned in every error body, so adopting the caller's own value on a directly-exposed
process lets unrelated requests be collapsed under one ID and lets arbitrary text ride into the logs.
Neither decision is re-made further down the chain.

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

-- Added at M6. In-flight authorization requests: the bridge between /authorize and /callback.
-- PKCE requires the code verifier to be known to this server and nobody else, so it cannot travel in the
-- `state` parameter (which passes through the browser and the provider) — hence a row. Single-use.
CREATE TABLE oauth_states (
  id bigint PRIMARY KEY, state_hash bytea NOT NULL,      -- sha256; raw value only in the redirect URL
  provider varchar(32) NOT NULL,
  code_verifier text NOT NULL,                            -- necessarily plaintext: sent to the provider
  flow_challenge bytea NOT NULL,                          -- sha256 of the *client's* verifier (ADR 0024)
  client_redirect_uri text NOT NULL DEFAULT '',           -- M8: loopback listener to return to, or ''
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL, consumed_at timestamptz NULL
);

-- Added at M6. The bridge between a completed callback and a client that wants tokens: the callback
-- cannot redirect with a token pair without putting credentials in a URL, so it hands over a one-time
-- code instead, traded at POST /auth/oauth/exchange. Also serves the CLI loopback flow (M8).
CREATE TABLE oauth_exchange_codes (
  id bigint PRIMARY KEY, code_hash bytea NOT NULL,
  user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  flow_challenge bytea NOT NULL,                          -- carried from oauth_states; checked at exchange
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL, consumed_at timestamptz NULL
);

-- Sessions/tokens: token-based auth for CLI/TUI/GUI, device_id-scoped refresh families (ADR 0011)
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

CREATE TABLE device_codes (                    -- CLI headless/SSH sign-in (M9, ADR 0028)
  id bigint PRIMARY KEY,
  device_code_hash bytea NOT NULL,             -- SHA-256; this half redeems for a token pair
  user_code varchar(9) NOT NULL,               -- the half a person types; plaintext, see below
  device_id text NOT NULL, device_name text NOT NULL,   -- captured at issuance: the session is scoped to
                                                        -- device_id, and the browser approving has no idea
                                                        -- what it is
  user_id bigint NULL REFERENCES users(id) ON DELETE CASCADE,   -- set on approval; NULL is "still waiting"
  denied_at timestamptz NULL,                  -- pressing Deny, distinct from an expiry so a client stops
  consumed_at timestamptz NULL,                -- single-use lives in ConsumeDeviceCode's WHERE
  last_polled_at timestamptz NULL,             -- the interval, advisory (RFC 8628 §3.5 slow_down)
  created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX ON device_codes (device_code_hash);
CREATE UNIQUE INDEX ON device_codes (user_code);       -- also what collision-on-generate retries against
CREATE INDEX ON device_codes (expires_at);             -- the sweep; non-partial, see 000005
```

Three things above differ from what this section specified before M9 built it, and each is a rule this
codebase does not bend. **The device code is hashed**, because it is redeemable for a token pair and every
credential here is stored only as its SHA-256. **There is no status column**, because "expired" is
`expires_at` and a second copy of a fact can disagree with the first. **There is no `session_id`**, because
approval cannot create the session: a session is scoped to one `device_id` and the approving browser does
not know the waiting client's, and the raw refresh token could not be kept here to hand over later anyway.
The row records *who* approved; the session is minted at poll time by the request that carries the device
identity, exactly as the OAuth exchange does.

**The user code is deliberately not hashed**, which is the one credential-shaped exception in this schema.
It is not a bearer credential — whoever holds it must still authenticate as themselves and press Approve,
and what that authorizes is somebody else's machine acting as *their* account, so a stolen user code buys
an attacker the ability to give their own account away. And it has to be readable back: the approval page
shows it so a person can compare it against the screen that produced it, which is where this flow's real
defense is (§14.21), and the OAuth callback reaching that page arrives holding a state row and an account,
never what somebody typed.

```sql

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
  code varchar(16) PRIMARY KEY,                -- plaintext, deliberately: see migration 000009
  created_by bigint NULL REFERENCES users(id), -- NULL when the instance operator issued it (M10)
  max_uses integer NULL, uses integer NOT NULL DEFAULT 0, expires_at timestamptz NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT instance_invites_uses_sane CHECK (
    uses >= 0 AND (max_uses IS NULL OR (max_uses > 0 AND uses <= max_uses)))
);

CREATE TABLE email_verification_tokens (       -- added at M10; the same shape as password_reset_tokens
  id bigint PRIMARY KEY, user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash bytea NOT NULL, sent_to text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,
  consumed_at timestamptz NULL
);

CREATE TABLE voice_states (                    -- now ACTIVE, not reserved
  guild_id bigint NOT NULL, channel_id bigint NOT NULL, user_id bigint NOT NULL, session_id text NOT NULL,
  self_mute boolean NOT NULL DEFAULT false, self_deaf boolean NOT NULL DEFAULT false,
  mute boolean NOT NULL DEFAULT false, deaf boolean NOT NULL DEFAULT false,
  supports_video boolean NOT NULL DEFAULT false,   -- client capability flag; terminal clients always false
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
(§6). The voice-join payload carries `supports_video: bool` (the terminal clients always `false`).

### REST API

Base `/api/v1`. Auth endpoints issue Bearer tokens directly (JSON body), never cookies, for the CLI/TUI/GUI/daemon
surface (a future BFF layer in front of this same API handles cookie issuance for the web SPA, §9):

```
POST   /auth/register                     -- 202 always; invite_code required while gating is on (M10)
POST   /auth/verify/request                -- re-send a verification link; always 202 (M10)
GET    /verify                             -- the confirmation page (root, HTML)
POST   /verify                             -- confirm; a form POST, never a GET side effect (rule 4)
GET    /oauth/continue                     -- resume a sign-up a provider would not vouch for (root, HTML)
POST   /instance/bootstrap                 -- create the first administrator; operator token only (M10)
POST   /instance/invites                   -- mint an invite; operator or Instance Admin
GET    /instance/invites                   -- list them, codes in full
POST   /instance/invites/revoke            -- revoke one; the code is in the body, never a path (rule 8)
POST   /auth/login                        -- email/password -> access + refresh token pair, device_id-scoped
POST   /auth/refresh                      -- rotates the refresh token within its device_id's family
POST   /auth/logout                       -- revokes current session
POST   /auth/logout/all                   -- revoke-all-sessions primitive (§ Account lifecycle)
POST   /auth/password/forgot              -- always 202 regardless of whether email exists
POST   /auth/password/reset
POST   /auth/email/verify
GET    /auth/oauth/{provider}/authorize
GET    /auth/oauth/{provider}/callback
POST   /auth/oauth/exchange              -- trade the callback's one-time code for a token pair
POST   /auth/oauth/complete              -- finish a sign-up by choosing a username (M6)
POST   /oauth/signup                     -- the same, as the form the rendered page submits (root, HTML)
POST   /auth/device/code                  -- CLI headless device-code flow: issue a code (M9)
POST   /auth/device/token                 -- poll for completion; POST because it spends the code and
                                          --   starts a session, which rule 4 forbids a GET from doing,
                                          --   and because a path is logged and this one is a credential
GET    /device                            -- the verification page (root, HTML): enter the code
POST   /device                            -- ...look it up, and offer the ways to sign in
POST   /device/signin                     -- ...the password branch; a provider is a link to /authorize
POST   /device/approve                    -- ...approve or deny, a separate step on purpose (§14.21)
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
for bots/automation, minted from any attach client once logged in.

**Settled at Milestone M4** (ADR 0022):

- **Access tokens are HS256**, signed with a single 32-byte secret (`[auth].jwt_secret`, generated by
  `norite instance init`, overridable by `NORITE_JWT_SECRET`). The algorithm is pinned at verification, so a
  token claiming `alg: none` is rejected before its signature is considered. Symmetric is correct here
  because **nothing outside the backend ever verifies an access token** — the gateway is in-process, and the
  SFU authenticates connections through WebRTC's own handshake rather than a token (ADR 0023). An external
  verifier appearing is the trigger to revisit, not to distribute the key.
- **The access token is never presented to the media server or to the voice-worker.** A component holding
  live access tokens could replay them against the API as those users, which no signing scheme prevents.
- **Scopes only ever restrict**, never grant: they bound a delegated credential below what its owner can
  already do, and permission resolution still runs on top (rule 1). A user's own access token passes every
  scope check. Unknown scopes are rejected at mint time rather than dropped.
- **Token management is not delegable.** Minting, listing and revoking `api_tokens` all require a user actor
  and carry no scope — a credential that can enumerate or create credentials undermines the model it is part
  of.
- Refresh rotation distinguishes *replay* from *deliberate revocation* by `replaced_by_id`: only rotation
  sets it, so a logged-out or superseded token is simply invalid, while a token that was rotated away from
  and presented again revokes that device's family.
- **Registration requires an invite code while `registration_mode = invite`** (M10). A request without one
  is refused `invite_required`; one whose code is unknown, exhausted or expired is refused
  `invite_invalid`, deliberately one answer for all three so nobody can learn which codes exist. An open
  instance ignores a stray code rather than rejecting it, so a client works against either mode without
  knowing which it faces. Redemption is a single `UPDATE` with every guard in its `WHERE`, sharing the
  account insert's transaction — so a one-use code admits exactly one account under concurrency, and a
  registration that fails does not burn somebody else's invite. **OAuth sign-up still refuses outright on a
  gated instance**: there is nowhere to carry a code through a provider redirect, so an invite-only
  instance is password-registration-only until that gains a design.
- Concurrent `argon2id` operations are bounded (a gate sized from `GOMAXPROCS`): each holds 64 MiB for its
  duration, and a distributed login flood would otherwise exhaust memory well before any per-IP limit
  noticed.

**OAuth (Google/GitHub)** via `x/oauth2` with PKCE, built at M6 — decisions recorded in
[ADR 0024](adr/0024-oauth-account-linking-and-signup.md).

An identity links to an existing account **only when the provider reports the address verified**, and is
otherwise refused with instructions to sign in by password and link from settings. Matching on the address
alone would let anyone who can put someone else's address on a provider account take over the matching
Norite account. For GitHub that means a second call to `/user/emails`, which is the only place the
verification flag exists — `/user` omits the address entirely when it is private. Once linked, sign-in
consults the provider's immutable user ID and never the address again.

**No account exists until a username is chosen**, and nothing is written to `users` before that: the
callback returns a short-lived signed continuation token, and the account, its `oauth_identities` row and
its first session are created together in one transaction. There is deliberately no pending-account state
for later milestones to have to respect. **The callback never returns tokens** — it hands over
a single-use exchange code, because a redirect carrying a token pair would put credentials in a URL, in
browser history, and in every proxy log on the way. It renders that code on a page, or, when the client
named a loopback listener, `302`s to it carrying the code (M8,
[ADR 0027](adr/0027-loopback-redirect-for-the-oauth-callback.md)) — a two-minute single-use value, useless
without the flow verifier that never left the client, on a hop that never leaves the machine.

Two CLI login paths: a system-browser-plus-loopback-listener flow, and a headless/SSH device-code fallback
(`device_codes` table, server-rendered verification page at `/device`, independent of the web SPA). The
second is **detected, not asked for** — a provider sign-in on a machine where no browser can be reached
falls back to it and says so, while a password login is left alone; `--device-code` asks for it directly,
and `--no-browser` keeps its own meaning of "print the link, I will open it myself" (M9, ADR 0028). It is
the only way an account with no password signs in on a server, which is why it exists at all. **What is
registered with Google and GitHub is this instance's own callback**,
`{public_base_url}/api/v1/auth/oauth/{provider}/callback`, and nothing else — the provider never sees the
loopback URI, which is a second hop it has no opinion about. The instance validates that URI by host
(loopback IP literal, explicit port, `http`, nothing else) and accepts any port. The CLI's fixed primary
port `http://127.0.0.1:51763/callback` plus its fallback list is therefore a **client-side convention**,
kept because it makes the port predictable enough to document and to allow through a local firewall, not a
protocol requirement.

**Credential ownership**: the daemon is the sole holder of its account's tokens (ADR 0011) — one keychain
entry, one process; CLI/TUI/GUI never independently store a token copy. `norite login` (M7) is the single
exception and a temporary one: it writes that entry because it is the only process that ever sees the
password, and stops doing so at M20, when the local IPC socket exists and credentials cross it instead.

**Where the credential actually lives** (M7, [ADR 0025](adr/0025-credential-storage-without-a-keyring.md)):
the OS keyring where the machine has one, and a `0600` file in the daemon's `0700` per-user state directory
where it does not — a headless Linux server has no Secret Service, and keyring-only would make the CLI
unusable on exactly the machines it exists for. The fallback is plaintext, deliberately: a decryption key
stored beside its ciphertext is obfuscation, not protection. `norite login` says which of the two it used,
so the degradation is never discovered later. Only the refresh token is stored; an access token expires
long before any restart it would be persisted to survive. Beside it sits a non-secret record — instance and
account — kept in a plain file so that showing which account is signed in never has to open the keyring,
and, in a third file, the per-installation `device_id` that scopes the refresh family. That one is separate
precisely so a logout cannot take it: a local logout revokes nothing server-side, so a fresh ID would add a
session-list entry while the family the old one named stayed live for its full TTL.

**Registration** (M4, hardened at M10): `POST /auth/register` answers **202 with a fixed body** whether or
not the address already has an account, and never returns the account itself. What differs is the mail: a
verification link for a new account, a "somebody tried to register with your address" notice for one that
already exists. The notice is what makes the silence honest rather than merely quiet — somebody has to
learn the two cases differ, and the only party entitled to know is whoever controls the address. It carries
no link and asks for nothing, because a "was this you?" button would be a phishing template written by us.

Timing is part of the guarantee and is not automatic: `HashPassword` runs *before* the address check, so
both branches pay argon2id's 64 MiB and tens of milliseconds, which swamps the single insert they differ
by. Moving it below that check makes the taken branch ~1 ms against ~31 ms, and a test fails on the ratio.
The race the pre-check cannot close — two simultaneous registrations for one address, one losing at the
unique constraint — is answered with the same silence, because reporting that conflict would leave the
oracle reachable on purpose by firing two requests at once.

A **taken username** is still reported (409). A username is an `@handle`, public by construction and
discoverable by any client that can look one up, so refusing it discloses nothing; an address is not
public. That asymmetry is the whole design.

**Email verification** (M10, `email_verification_tokens`): the same shape as `password_reset_tokens` —
SHA-256 hashed, single-use in the query's `WHERE`, `sent_to` recorded so a later address change cannot
redirect a confirmation in flight — with a **24-hour** TTL against reset's one hour, because a
verification link is followed on a person's own schedule rather than being the whole proof of identity for
changing a password. The page confirms by **form POST, never on GET**: a GET that verified would be
triggered by anything that follows links in mail — scanning gateways, chat-client previewers, antivirus —
confirming an address the person never acted on and spending the link before they clicked it.

An unverified account **cannot log in**, and is refused with the *same* answer a wrong password gets.
Reporting it distinctly is the obvious design and it reopens the oracle registration just closed, in two
requests: register an address with a password of your choosing, then log in with it — if the address was
free an account now exists with that password and the login says "unverified", and if it was taken nothing
was created and the same login says "wrong password". So the difference goes where every other difference
here goes: a correct password on an unverified account queues a fresh link and an explanation to the
address, and the caller is told nothing either way. The mail follows only a *correct* password, so guessing
at addresses queues nothing. Migration `000010` backfills every pre-existing account as verified, because
locking out every existing user is an outage delivered as a hardening.

**An instance with no SMTP relay creates accounts already verified**, and the enumeration hole stays open
there. This is an accepted limitation, not an oversight: such an instance cannot verify an address by any
route, so requiring verification would mean nobody could register at all, and there is no mail to carry the
difference between the two branches either. It is stated in three places — the wizard warns when SMTP is
declined, the server logs it once at startup, and §14 records it — because the failure mode to guard
against is it becoming quiet, not it existing.

**Password reset** (built at M5): always-202 anti-enumeration, single-use SHA-256-hashed token with a
one-hour TTL, sent asynchronously via the SMTP relay (§11) and never blocking the HTTP response — the
detachment is what makes the 202 honest, since sending inline would leak through timing whatever the body
said. Requesting again spends any earlier token, and a token is refused if the account's email changed
after it was issued.

A successful reset **revokes every session and every API token on the account**. Sessions alone would not be
enough: a reset is how someone recovers a compromised account, and a token an intruder minted while they had
access outlives a password change unless it is revoked with it. The cost — a user who merely forgot their
password re-mints their bots — is stated in the email and on the confirmation page rather than left to be
discovered.

The emailed link lands on a **server-rendered page** the backend serves at `/reset`, outside the versioned
API prefix. It exists because its recipient is by definition locked out, may have no CLI installed, and may
be on an instance that never deploys the web SPA. It posts a plain form, so its Content-Security-Policy —
overridden per-route from the JSON API's `default-src 'none'` — can forbid scripts outright.

### Account lifecycle: deletion & data export

`DELETE /users/@me` and self-service "log out all other devices" both invoke the **general-purpose
revoke-all-sessions primitive**: force-close live gateway connections, revoke refresh + scoped tokens
(DB-backed, instantly revocable — an already-issued 15-minute access token simply can't be renewed and
expires naturally), and **revoke every linked device's E2E device-link trust** (ADR 0014) — the same
primitive an Instance Admin ban invokes (ADR 0013). Account deletion otherwise follows the original design:
soft-delete with placeholder username/email, hard-delete `oauth_identities`/`sessions`, leave authored
content in place rendered as "Deleted User."

`GET /users/@me/export` covers everything the server can see. **For E2E-encrypted DMs, the daemon — not the
server, not the CLI/TUI/GUI independently — performs its own local decrypt-and-export step**, producing a
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

The CLI, TUI and GUI are thin "attach" UIs over one shared local background daemon, one process per OS user
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

**Service installation** (settled at Milestone M3): `norite daemon install | uninstall | start | stop |
restart | status`, implemented in `cli/internal/daemonctl` behind one `Manager` interface with a backend per
platform. Always a **user-scoped** service — a systemd *user* unit (`~/.config/systemd/user/`), a launchd
*agent* (`~/Library/LaunchAgents/`), or a logon task at the user's own integrity level (`schtasks /SC
ONLOGON /RL LIMITED`) — never a system-wide one, so installing needs no elevation and the daemon runs as the
account whose tokens and keystore it holds. Each backend shells out to that platform's own tool
(`systemctl --user`, `launchctl`, `schtasks`) through an injectable `Runner`, which is what lets all three
command lines be asserted from one CI machine; the failed command appears verbatim in any error, so an
operator can reproduce it. **Install and start are separate verbs** — provisioning an image wants one
without the other. `install` is repeatable and replaces an existing definition; `uninstall` succeeds when
nothing is installed.

**Single instance, and how a stop is distinguished from a crash**: the daemon holds a `gofrs/flock` advisory
lock on `<state-dir>/daemon.lock` for its whole life, taken before it opens anything else. flock is owned by
the open file description, so the kernel releases it however the process dies — nothing goes stale, unlike a
PID file. A second daemon exits **3**, which the systemd unit is told never to retry
(`RestartPreventExitStatus=3`), since the condition it reports is by definition already satisfied; launchd
has no per-exit-code equivalent and is throttled instead (see the platform differences below). A
signal-initiated stop exits **0**, not 128+signum: every service manager reads a non-zero exit as a crash
and answers with a restart, which would make an ordinary stop loop. `norite daemon status` reports through
its exit code — 0 running, 1 installed-but-stopped, 2 not installed — so a script can branch without parsing
prose, and until the `--json` machinery lands (M48) that code is the machine-readable surface.

**Daemon-owned state directory**: `$XDG_STATE_HOME/norite` (`~/.local/state/norite`), `~/Library/Application
Support/Norite`, or `%LOCALAPPDATA%\Norite`, created `0700` — it will later hold plugin capability grants and
pinned `.wasm` hashes (§8), so the mode is established now rather than migrated. It holds the lock and, by
default, the daemon's own rotating log (`natefinch/lumberjack`, per §4a's file-based logging rule); the
daemon also copies every line to stderr, which is what journald captures, so `systemctl --user status` and
the log file both show something useful. **The lock always stays in the state directory** even when the log
is redirected — it is the per-user rendezvous point, and a lock that moved with the logs would let two
daemons take different ones.

Because the systemd user manager does not read the shell profile, `install` **captures `XDG_STATE_HOME` into
the unit's `Environment=`** when it is set. Without that, a user who exports it in their shell gets one state
directory from a hand-started daemon and another from the service, so the two take *different* locks and both
start — breaking the single-instance invariant with no error anywhere.

**Where the three platforms genuinely differ**, rather than being papered over:
- **launchd starts the agent as part of installing it** (`bootstrap` loads it; `RunAtLoad` runs it). There is
  no register-without-starting, so `Manager.StartsOnInstall()` reports the difference and `norite daemon
  install` prints what actually happened instead of systemd's "to start it now" advice everywhere.
- **launchd cannot exempt an exit code** the way `RestartPreventExitStatus=3` does, so an exit-3 daemon is
  respawned while another instance holds the lock. The loop is self-correcting; `ThrottleInterval` keeps it
  cheap.
- **launchd's `StandardErrorPath` is never rotated**, so on macOS the daemon is launched with `-log-file`
  (rotating log into `~/Library/Logs`) and `-stderr-log=false`; that leaves the launchd-captured file holding
  only panics and pre-logging failures rather than an unbounded copy of the rotated log.

**Dual IPC, different trust tiers**:
- **Daemon↔attach-client**: a Unix domain socket / Windows named pipe, OS-file-permission-protected (no
  secret needed — only the owning OS user can open it). Reuses the gateway's exact op-code/DISPATCH protocol
  over 4-byte-length-prefixed JSON framing, so every attach client shares one client-side event parser. The
  shared HELLO/IDENTIFY handshake carries a semver field (MAJOR must match exactly; a defined
  MINOR-version-back window is tolerated). **The daemon's write path to each attach client is asynchronous
  and bounded** — a per-connection outbound channel with fixed capacity, fed by its own writer goroutine
  (see "Concurrency model" below); a client whose buffer fills gets **dropped**, never allowed to block the
  daemon's core loop, since that would also stall E2E ratchet advancement and voice signaling for everyone
  else attached. The dropped client resyncs on reattach.
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
preferences — anything a user should freely hand-edit. Keys are namespaced `[shared]`, `[tui]`, `[gui]`:
cross-cutting settings live in `[shared]` and a client section overrides them. There is deliberately **no
`[cli]` section** — the scriptable command tree has nothing to style, and the section that used to carry
that name was really about chords and colours, which belong to the TUI (§4a, ADR 0026). Chords are
`[tui.keys]`. Namespacing this way and `norite config get`/`norite config set` expose the
same file as a scriptable interface rather than a second source of truth. Every writer (CLI, TUI, GUI,
daemon)
uses atomic writes
(temp file + rename) **plus `gofrs/flock`-based locking** around each read-modify-write cycle. The daemon
hot-reloads on external changes via `fsnotify`. A **second, daemon-owned state file** holds anything
daemon-written-only: plugin capability grants + pinned `.wasm` hashes (§8), the voice-channel breadcrumb, and
the same-machine config-toggle setting below — never hand-edited, never included in export.

**Themes are files, and files are untrusted.** A theme lives at `~/.config/norite/themes/<name>.toml` and
is selected by name from `[tui]`; a few ship built in. The default maps the token roles onto the terminal's
own ANSI 0–15, so a user's existing palette is inherited rather than overridden, and `docs/design/tui/`'s
hex palette is a named theme they opt into. Sharing themes is the point of the format, which makes a theme
file text from a stranger on its way to a terminal: every string in one passes `termsafe` (rule 19), every
colour must parse or the theme fails to load as a whole rather than half-applying, and **a glyph override
must be exactly one cell wide** — a two-cell glyph shears every row it lands in, which is why `TOKENS.md`
bans emoji in the first place. A theme sets appearance only; it cannot bind keys, run commands, or reach
the network.

**Same-machine config toggle**: default off (all local clients share one `config.toml`, as above). An
app-settings toggle (living in the daemon state file) lets them diverge into separate files on one machine;
flipping on copies the current shared file to both as a starting point, flipping off reconciles via
last-write-wins onto one shared file.

**Config export/import**: `norite config export` / `norite config import` — a portable file covering the
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

## 4. CLI — the scriptable command tree

A separate, performance-focused, fully scriptable client (Unix-style: one action, exit, pipeable
stdin/stdout), attaching to the shared daemon (§3). See [ADR 0009](adr/0009-cli-and-gui-client-architecture.md).

**This section is the command tree only.** The full-screen terminal client — panes, chords, screens — is
§4a, and is a client in its own right ([ADR 0026](adr/0026-tui-as-a-first-class-client.md)). They ship in
one binary and share one command tree: every verb here is invocable from the TUI's `M-x`, which is what
stops the two surfaces drifting apart. Where this document said "CLI" and meant the terminal UI, it now
says TUI; that conflation is why the roadmap once held six milestones of TUI capabilities and no milestone
that drew a screen.

**Command routing**: `urfave/cli` v3 — the argument parser and command tree (`norite instance init`,
`norite config get`, …), distinct from the Charm stack, which is only the interactive TUI layer. It carries
`--help` and shell completions for every command, and declares the global `--json` flag; the machinery that
*renders* JSON, and the per-command schemas in `contracts/cli-json/`, arrive with the first data-printing
command (Milestone M48) — until then the flag is a declared seam, not a working output mode. The tree lives
in `internal/cliapp`, not under `cmd/`, so it can be constructed and exercised in tests without spawning a
process; `cmd/app/main.go` owns process lifetime and exit codes and nothing else. A mistyped command exits
non-zero rather than printing help and succeeding, which `urfave/cli` does by default. *Where the choice was contested*: over
`spf13/cobra`, the heavier ecosystem default, for a lighter dependency and less per-command boilerplate
across what will become dozens of commands; nested subcommand groups, the one thing this CLI genuinely needs
from a router, work equally well in both.

**Structured output**: every data-printing command supports `--json`, schemas versioned in
`contracts/cli-json/` as a third source-of-truth contract alongside `openapi.yaml`/`gateway-events.schema.json`
— schema changes ship in the same commit as the code change causing them.

**Instance setup wizard** (`norite instance init`): the self-hosted operator's first-run flow, living in the
`norite` CLI rather than the server binary so that creating the first admin account is a normal API call
from the client that already knows how to make one. That step landed at M10 as a **sibling command**,
`norite instance bootstrap`, rather than inside the wizard: when `init` finishes, the backend has not been
started and the schema has not been migrated, so there is nothing to create an account on, and holding an
administrator's password in memory across an unbounded wait is worse than a second command. The wizard
prints the ordered next steps instead (ADR 0029). Prompts are **plain
sequential stdin/stdout question-and-answer, not a full-screen TUI**: the wizard is a rare one-time flow
that has to work over SSH, inside `docker exec`, and in CI, and it must degrade to an error rather than a
hang when stdin is not a TTY. Two modes — a quick-start path that prompts only for what has no safe default,
and `--full`, which prompts for everything — plus a fully non-interactive flag form for scripted
provisioning. Quick-start asks only what has no safe default — how to reach Postgres, and whether
registration is open, which is a policy decision that shouldn't be made silently for a private instance —
then prints what it defaulted for everything else; `--full` asks about all of it. The database connection
is asked for in parts and assembled with `net/url` rather than typed as a whole DSN, because a password
containing `@`, `/`, or `:` has to be percent-encoded to survive in a URL and making an operator get that
right by hand is a trap.

**Instance config file** (settled at Milestone M2): **TOML**, at `/etc/norite/instance.toml`
(`%ProgramData%\Norite\instance.toml` on Windows). TOML for consistency with the CLI/TUI/GUI client config
above, and because the generated file is meant to be hand-edited afterwards — it ships as a commented
document explaining every setting it writes, which a struct marshal cannot produce. This is a *different
file* from `~/.config/norite/config.toml`: that one is per-user client preferences, this one configures a
server, which is typically run by a system account or inside a container.

Discovery order: the server's `-config` flag, then `NORITE_CONFIG_FILE`, then the conventional location. A
file named explicitly by the flag or the variable must exist — starting on defaults that look nothing like
what was asked for is worse than refusing; the conventional location is allowed to be absent.

**Precedence, highest first: environment variable, then this file, then the built-in default.** The
direction is load-bearing rather than arbitrary: the flagship injects `DATABASE_URL` from a Kubernetes
Secret (§12), and a config file baked into a container image must never be able to shadow it. It also
means every environment-variable-only deployment — docker-compose, Kubernetes — behaves exactly as it did
before the file existed. **The file is entirely optional**; no file at all is a fully supported setup.
Every key within it is optional too, and unknown keys are *rejected* at startup rather than ignored, so a
typo fails loudly instead of silently leaving a setting on its default.

The file holds the database password, and an S3 secret key when object storage is used, so the wizard
creates it `0600` and fails rather than producing one it cannot restrict to the owner. Because a validation
failure could name either vocabulary, an error on a file-configured instance names both the environment
variable and the file key (`NORITE_REGISTRATION_MODE ([registration].mode in /etc/norite/instance.toml)`).

`contracts/instance-config.toml` is the source-of-truth reference listing every key, on the same footing as
`openapi.yaml` and `gateway-events.schema.json`. It exists because the backend and the CLI are separate Go
modules that cannot import each other's types: the backend proves it loads that document, the CLI proves
the wizard writes only keys appearing in it, and between them the two sides cannot drift apart.


## 4a. TUI — the in-terminal client

The full-screen terminal client: a Discord-shaped layout (guild rail → channel list → message area →
member list) with tmux-like pane splitting, driven entirely by Emacs-style chorded keybindings. It is where
a person actually spends time, and it is the in-terminal form of the same application the native GUI (§5)
presents natively — not a different product sharing a backend.
[ADR 0026](adr/0026-tui-as-a-first-class-client.md) records why it is its own client rather than a mode of
§4.

**The screens are specified, not described here.** `docs/design/tui/` is normative: `SCREENS.md` (25
screens, stable ids `1a`…`7a`), `KEYMAP.md` (every chord), `TOKENS.md` (palette, glyphs, component
recipes), `README.md` (the grid, the responsive rules, and the corrections applied to the original
handoff). Milestones cite screen ids rather than restating them. `mockups.dc.html` is a visual reference
rendered as HTML — a simulation of a terminal, not a web app to ship, and not authoritative where it
disagrees with the markdown.

**Stack**: Bubble Tea (`tea.Model`) for the event loop, Lip Gloss for styling and layout, Bubbles for
reusable widgets, over the daemon's local socket. A custom pane/split engine rather than shelling out to a
real tmux, for identical cross-platform behavior including Windows. Tested with `teatest` (key-press
simulation, rendered-output assertions).

**Panes and windows**: a pane is any viewport, not just a conversation — `chat`, `log`, `shell` (a pty),
`peers` (file-transfer sessions, ADR 0016), `scratch`. Panes live in named windows shown in a tab bar. The
layout tree and per-pane scroll offsets belong to the **daemon** (ADR 0010), so detaching and reattaching
restores them; they are in-memory, so a daemon restart does not.

**Chrome is a function of pane count**, and this is the rule the engine is built around: one or two panes
may each be a *complete client* — own rail, own channel list, own member list, a different guild each —
while three or more draw chrome once for the window and hold content only. At three panes there is no
width left for per-pane columns, and pretending otherwise produces columns nobody can read. `C-x 1`
restores full chrome.

**`M-x` — one command tree, two front ends**: every verb in §4's tree is invocable from the TUI and renders
into a pane, with `--json` output syntax-coloured (`3e`). Plugin-registered commands appear alongside the
built-ins. Verbs that are interactive by construction — `norite instance init`, a sequential stdin
conversation that refuses to run without a TTY — run in a **pty pane**, so they get a real terminal and
there is no second implementation of any prompt flow.

**Keybindings**: Emacs-style chorded, two prefixes — `C-x` for panes and windows, `C-c` for app actions,
`M-x` for command mode, `M-1`…`M-9` to jump to a guild. An armed prefix is shown in the status bar; an
unknown chord is a status-bar error, never a modal. Bindings live in `[tui.keys]` (§3) and are
hot-reloaded. Plugins never bind chords ([ADR 0015](adr/0015-plugin-sandboxing.md) as extended by ADR
0026); binding a chord to a plugin's command is the user's own override.

**Security state is on screen and never overclaims**: `◈` verified E2E, `▲` unverified device, `○`
offline. `◈` appears on 1:1 DMs and nowhere else — E2E is `DM`-only (rule 13, ADR 0014), so a group DM
says it is encrypted by the instance and a whisper says it is delivered to a private recipient set and
stored like any other message. Search (`3c`) shows server hits and DM hits as separate labelled groups: the
instance holds DM ciphertext and cannot match against it, so that group is served by the daemon's own
**mandatory local FTS5 index** over its decrypted E2E message store, encrypted at rest under the keystore
master key (§7, ADR 0014). Everything else — guild channels, group DMs — is in-memory scrollback re-fetched
from the instance (ADR 0010); the local store exists for exactly the messages the server cannot search.

**Appearance is the user's** (§3, `[tui]`): the default theme maps the token roles onto the terminal's own
ANSI 0–15, so Norite inherits a palette somebody already tuned; `TOKENS.md`'s hex palette is a named theme
(`norite-dark`) they opt into. Palette, border style, density, timestamp format, author colours, the glyph
table and the column widths are all overridable; the information architecture is not.

**Below 120×40** the drop order is specified in `docs/design/tui/README.md` — member list, then channel
list, then side-by-side splitting is refused, then the rail collapses — because 80 columns is the
commonest terminal width and leaving that to the layout code is how it gets decided badly.

**Markdown rendering**: a small custom renderer implementing only the allow-listed subset (bold, italic,
code, links, mentions, custom-emoji shortcodes) — not Charm's `glamour`, to keep the trusted-rendering
surface as narrow as the security posture used for message content everywhere else.

**Terminal-escape sanitization** (`cli/internal/termsafe`, built at M7). A blanket function over all
untrusted text — usernames, message content, link-preview titles, plugin manifest descriptions, webhook
display names, the output of any tool the CLI shells out to. Specific to the terminal clients, because a
terminal acts on what it is printed and no other client does — it covers both front ends in that binary,
§4's command output as much as this section's screens.

Its guarantee: *what a terminal displays, and the order it displays it in, is the printable characters that
were in the string.* Two classes break that and are removed — Unicode category `Cc` (C0, DEL, and C1, the
last on its own because a lone `0x9b` is CSI with no ESC in front of it), and the bidirectional embeddings,
overrides and isolates (`U+202A`–`U+202E`, `U+2066`–`U+2069`), which reorder what is printed. The three bidi
*marks* (`U+061C`, `U+200E`, `U+200F`) are kept: they only set the direction of neighbouring neutrals, and
they occur in ordinary Arabic and Hebrew.

Deliberately **not** covered: characters that are merely invisible (zero-width spaces, word joiners, tag
characters, soft hyphens) and confusable letters. They can deceive a reader but not about which visible
characters are present or where — and removing them by category is actively harmful, since the same `Cf`
category holds `U+0600` and `U+06DD` (written Arabic) and `U+200C`/`U+200D` (Persian and Indic joining,
composed emoji). Legibility is a rendering policy for the TUI's renderer, which has the font and width
rules; this filter is not the place. Escape sequences are likewise not *parsed*: removing the ESC leaves an
inert `[2K`, which is worse-looking and strictly safer than a parser that must be right about DCS, OSC, and
malformed input.

Each removed run becomes one `U+FFFD` rather than vanishing, so that two different strings cannot render as
one — an impostor's name is never displayed as the name it imitates — and so a reader can see something was
taken out. Invalid UTF-8 is replaced before anything examines runes, since a decoded `U+FFFD` looks
printable while the underlying byte is not.

Two forms: `Text` for a value printed inside a line, which removes newlines and tabs as well, because a
one-line value that can contain a newline can forge a whole line of output; and `Block` for text meant to
span lines, which keeps them.

It is applied where foreign text *enters* the program rather than at each place it later leaves — the API
client sanitizes a response as it decodes it, `daemonctl`'s `Runner` sanitizes what a subprocess printed —
so a value is safe wherever it subsequently goes, including a file it is stored in. Text read back out of a
file a person can edit is foreign again, and is sanitized at the print. `cmd/app` sanitizes every error on
its way to stderr as a backstop, so a command that forgets cannot put an escape sequence on a terminal.
Values that also *bound* something (an instance URL, which becomes a filename and a request target) are
**rejected** rather than sanitized: for those, silently altering the value is the worse failure, and asking
for it again costs nothing.

**Image rendering**: `BourgeoisBear/rasterm`-based capability detection (Kitty/iTerm2/Sixel), inline when
supported, filename/link fallback otherwise — the hook point for the "disable image loading" bandwidth
toggle (which does not suppress custom-emoji rendering).

**Logging**: file-based, never stderr (Bubble Tea owns the alternate screen buffer), `norite logs tail`,
`natefinch/lumberjack` rotation — reused by the daemon, both front ends of this binary, and the GUI alike.

**Voice controls**: join/leave/mute/deafen on a chord, an active-speaker indicator, and two separate
actions (keybind each) for local-mute and report (§6). A call is *drawn* here, not merely announced: `4b`
is a one-row in-call strip above the composer that hides nothing, and `C-c V` promotes it to `4a` — speaker
tiles with level meters and the transport/processing/levels cards. "No visual call UI — it's a terminal"
was this section's earlier claim and was true only while there was no terminal application to draw one in;
the roadmap records M54 superseding it.

**Dev tools**: the `shell` pane above *is* the integrated shell — the user's own shell, the same trust
boundary a terminal emulator has, no extra sandboxing ([ADR 0017](adr/0017-local-automation-security.md)).
It is not a second feature scheduled separately. Code block copy/fold, local bot automation, shell piping
and local port forwarding, and link previews (GitHub-aware + generic) are the rest of the terminal-native
v1 scope.

---

## 5. Native GUI

Built with **Gio** (`gioui.org`) — immediate-mode, GPU-rendered, tight memory control, chosen over Fyne for
that control despite the lack of a built-in widget library. See
[ADR 0009](adr/0009-cli-and-gui-client-architecture.md).

**Message rendering**: a virtualized message list, the same allow-listed markdown renderer as the TUI
reimplemented for Gio's immediate-mode primitives, including emoji-shortcode resolution.

**Pane splitting**: native widget-based tiling, the same flexible pane-content model as the TUI (§3/§4a) —
independently implemented, never synced with the TUI's pane state by default.

**Information architecture**: the GUI mirrors the TUI (§4a) — same layout, same vocabulary, same screens
(`docs/design/tui/SCREENS.md`), presented natively: real scrollbars, pointer input, resizable splits,
native dialogs where the TUI uses a status-bar confirm. One application in two renderings, so a person
moving between them is not learning a second product. It does not inherit the terminal's constraints, only
its structure.

**Theming**: the shared theme spec (named roles: background/accent/danger/muted/etc.), mapped to Gio's
native rendering, defined once in config and shared with the TUI's ANSI mapping.

**Settings**: config read/write via the same `go-toml` v2 document-editing approach as the TUI, plus a voice
input/output device-selection tab.

**Voice UI**: participant list, mute/deafen controls, an active-speaker indicator (a highlight/ring around
whoever is transmitting), and separate local-mute and report actions — wired to the same voice-worker
control path the TUI uses.

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

Voice is real, working audio calling on every client in v1, including the terminal ones. Video/screen-share is
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
4-byte-length-prefix JSON framing as the daemon↔CLI/TUI/GUI socket. The worker holds its own direct WebRTC
connection to the SFU — RTP audio never flows through the daemon, only control signaling, which is what makes
fault isolation real: a media-pipeline bug can only crash the worker.

**Connection authentication** (settled by ADR 0023): **no bearer token participates in a voice connection.**
The voice-worker generates an ephemeral DTLS keypair and ICE credentials per call; its fingerprint travels up
through the daemon and over the already-authenticated gateway (op 4); the backend resolves
`PermConnectVoice` against fresh data and provisions the chosen SFU over internal mTLS; the DTLS handshake
then mutually verifies both fingerprints. `VoiceServerInfo` carries connection parameters, never a
credential. A compromised SFU pod therefore holds nothing replayable, and the client verifies the SFU's
identity too — which a bearer token would not give. Revocation mid-call (ban, kick, lost permission) is a
**push** from the backend over the same control channel, not an expiry.

**Stream model**: streams are always per-sender and **never mixed** — the client mixes locally in the
voice-worker. The client **drives subscription** over the connection's data channel: pin a participant, drop
one (local mute becomes an unsubscribe that also saves bandwidth), or cap how many streams it accepts, which
is what makes §4's bandwidth toggle meaningful for voice. Speaker-based selective forwarding is the default
policy when no preference is expressed, not a fixed rule. Stream-to-participant mapping is signalled on the
data channel over pre-allocated transceiver slots, so joins and leaves cost a message rather than an SDP
renegotiation. This is what makes per-user volume, per-user jitter buffers and spatial audio possible.

**Active speaker**: the RFC 6464 audio-level header extension, readable by the SFU without touching the
payload, drives both the indicator and selective forwarding. Levels are self-reported, so the SFU
cross-checks against observed packet flow — Opus DTX means a silent participant sends almost nothing.

**DSP pipeline, strict non-negotiable order**: **Mic Capture → AEC → RNNoise → AGC → Opus Encode**. RNNoise
before AEC would non-linearly distort the mic signal and break AEC's linear echo-correlation assumption,
producing a feedback loop instead of cancelling it. Opus, RNNoise, and WebRTC's Audio Processing Module
(AEC3 for echo cancellation, AGC2 — replacing `libspeexdsp`, ADR 0023) are all cgo bindings, contained
entirely to the `voiceworker/` binary — the only place cgo is allowed anywhere in the stack. AEC3 rather
than speexdsp because the echo canceller is the single most audible determinant of call quality, and
double-talk handling is where the older component falls behind what users expect.

**Call quality**: Opus at **64 kbps** by default, per-channel configurable, with in-band FEC and DTX enabled
— parity with the commercial platforms users compare against. The remaining determinant is geography:
mouth-to-ear latency is dominated by the network leg, so a user far from the SFU sees a less fluid
conversation that no codec or DSP work compensates for. `MediaCoordinator.AllocateSession` chooses which SFU
serves a session, so **proximity routing is a seam already in place**; v1 runs a single region deliberately.

**Adaptive bitrate**: `pion/interceptor`'s REMB/TWCC feedback drives `hraban/opus`'s runtime bitrate control,
audio-only, no simulcast — the Prometheus voice/SFU metrics (§15) feed this loop directly, not just a
dashboard.

**TURN**: an embedded Go TURN server (`pion/turn`, also answering plain STUN), bundled into the backend —
self-hosters don't run a separate `coturn`.

**Voice deployment opt-out**: TURN/SFU need a reachable public IP and forwarded UDP range — a real burden
many home self-hosters can't satisfy. An Instance Admin can disable voice entirely; the SFU/TURN never start,
voice+text channel pairs degrade to text-only, and voice UI is hidden entirely in TUI/GUI (never grayed out).

**No call recording, ever** — a permanent non-goal. Public-matchmaking voice abuse therefore has no recorded
evidence; both TUI and GUI mitigate this with a real-time **active-speaker indicator** (a highlight/ring
around whoever is transmitting) plus two **separate** actions (each its own keybind/click) — local-mute
(silences a participant for this user alone) and report.

**Mic permission and global hotkey**: the foreground TUI/GUI triggers the OS permission prompt on first voice
use, then hands capture to the worker once granted — unverified per-OS behavior until a dedicated spike
milestone determines the real answer (macOS TCC, Input Monitoring entitlement for global hotkeys).
Voice-activity-detection is the default input mode; push-to-talk (`golang.design/x/hotkey`) is registered
once by the daemon (not an attach client), avoiding double-registration if several clients are attached.

**Auto-rejoin**: on daemon crash/restart mid-call, the daemon respawns the worker and rejoins using the
persisted "last active voice channel" breadcrumb (§3) — the one exception to otherwise-ephemeral daemon
state. The client auto-update mechanism (§11) defers applying an update while a voice session is active, so
it never forces a mid-call daemon restart.

**Video/screen-share (deferred, seamed now)**: owned directly by the GUI/web client — a second, separate
WebRTC connection to the SFU, never through the daemon. The voice-join payload carries `supports_video: bool`
from day one (terminal clients always `false`).

---

## 7. BYOK end-to-end encryption

Opt-in, restricted to the `DM` channel type only. See [ADR 0014](adr/0014-e2e-encryption.md) for full
reasoning including the pairwise-scaling limitation and the compounding-risk framing.

**Cryptographic base**: `go.mau.fi/libsignal`, a mature pure-Go Signal-protocol port — license compatibility
with the project's restrictive license (ADR 0007) is a **blocking prerequisite**, checked before any
integration code is written.

**Key boundary — the daemon holds the keys**: the daemon owns the E2E keystore/ratchet state end to end and
performs all decryption itself; CLI/TUI/GUI receive plaintext over the already-trusted local IPC socket (§3),
same as every other event. They never independently hold key material.

**Keystore**: `modernc.org/sqlite` (pure Go), master key in the OS keychain, surviving daemon restarts. All
writes route through one dedicated writer goroutine + buffered channel (§3's "Concurrency model") so a burst
of concurrent incoming encrypted messages never produces a `database is locked` error or blocks the WS event
loop. A **mandatory** local FTS5 search index sits on this same keystore, over the decrypted message store,
encrypted at rest via the keystore master key — E2E DMs lose server-side search (below), so this is not
optional.

**Device linking**: a fully custom flow (no off-the-shelf equivalent — a second real piece of custom crypto
protocol) where the primary device authorizes a new device. Verification is text/code-based safety numbers
read out of band, never a camera or a QR code — screen `6a` in the terminal, mirrored by the GUI.
**No history-transfer mechanism exists** — a newly linked device sees only messages sent after linking,
matching the permanent-loss framing below.

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

Sandboxed via WASM using `wazero` (pure Go, no cgo), running inside the daemon (one host, available to every
attach client). See [ADR 0015](adr/0015-plugin-sandboxing.md), extended by
[ADR 0026](adr/0026-tui-as-a-first-class-client.md): a plugin registers **`M-x` commands, never
keybindings** — binding a chord to a plugin's command is the user's own override, so no plugin can take a
binding from them or phish for whatever is typed after a prefix it claimed.

**Headless by design**: the host-function API surface is slash-commands, text-parsing, and data/message
reads only — no UI-injection capability, no IPC bridge for painting native TUI/GUI elements. A plugin affects
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
after the terminal clients and the GUI exist (Phase O). See
[ADR 0009](adr/0009-cli-and-gui-client-architecture.md).

**Stack**: React 18 + TypeScript (strict) + Vite, React Router v7, Tailwind + shadcn/ui, TanStack Query,
Zustand (gateway-fed real-time stores), Zod (validates every gateway payload + form input), react-hook-form,
`@tanstack/react-virtual`, pnpm, Vitest + Playwright.

**Auth**: its own BFF-style httpOnly-cookie exchange layer in front of the same token API the
CLI/TUI/GUI/daemon use (§2) — the web client never holds a raw Bearer token in JS. See
[ADR 0002](adr/0002-cookie-based-auth.md) (still-correct historical rationale) and
[ADR 0011](adr/0011-token-based-client-auth.md) (current design). `openapi.yaml`/`gateway-events.schema.json`
are sanity-checked against real browser constraints (CORS, chattiness, BFF-compatibility) on an ongoing
basis, starting the moment each contract becomes load-bearing — not deferred until Phase O.

**State layers**: the same three-layer split as the original design (TanStack Query server cache, Zustand
gateway-fed real-time stores with one dispatcher routing DISPATCH frames, local/component UI state).

**Pane splitting**: CSS grid/flex-based resizable panes, `localStorage`-based layout persistence — the web
client's own independent implementation of the same pane-content model the TUI/GUI use, never synced with
either by default (the manual `norite config export`/`import` path, §3, is how a user manually carries
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
independent deployments, each granted rights individually via a signed license file — an offline,
Ed25519-signed JWT-like structure whose claims are `license_id`, `issued_to`, and an `entitlements` blob,
with no expiry claim, verified locally with a compiled-in public key so an instance never phones home. For
v1 the entitlements blob is a simple binary unlock (`{licensed: bool, license_key: string}`-shape); richer
per-feature entitlements are the reason it is a blob rather than a boolean. **The free flagship
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

## 13. Milestone roadmap

**The roadmap lives in [`roadmap.md`](roadmap.md)** — `M0` through `M125`, phase-grouped, each with its
scope, dependencies, and a checkable "done when" condition.

It is deliberately not restated here. It used to exist in two places, which meant every milestone change
needed two edits and the two copies could disagree; `roadmap.md` is now the single source of truth for
milestone numbering and scope.

Two properties of it are worth knowing without opening the file: Phase P (the flagship Kubernetes
deployment, `M112`–`M123`) is an explicitly **parallel** track that can start once core messaging and voice
are usable, rather than following the feature phases; and `M124`/`M125` sit at the numeric end but belong
logically much earlier, each annotated with where.

Completion status is tracked in `CLAUDE.md` and `README.md`, not in the roadmap.

---

## 14. Security (deep dive)

**Threat model summary**: a multi-tenant system exposed to the public internet, handling user-generated
content, third-party OAuth, credential/key material (auth tokens, E2E keys), and — new in this design — a
local-machine daemon holding real secrets and two distinct local IPC trust tiers. Main threat classes: authz
bypass, injection, session/token/key theft, SSRF, terminal-escape injection (terminal-only), abuse/DoS, and
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

4. **Two-tier local IPC trust model**: the daemon↔CLI/TUI/GUI Unix socket (OS-permission-protected, first-party)
   vs. the local bot-automation TCP port (secret-protected, external scripts) — never treated as
   interchangeable; any new local IPC surface must state its trust tier explicitly.

5. **XSS / terminal-escape injection**: web/GUI message content renders through the allow-listed markdown
   subset, never raw HTML. The terminal clients have an additional risk: untrusted text (usernames, messages,
   link-preview titles, plugin manifest descriptions, webhook display names) must pass through the
   terminal-safe sanitization function before reaching terminal output, or a malicious ANSI escape sequence
   could manipulate the user's terminal.

6. **CSRF/CSWSH**: not applicable to the CLI/TUI/GUI/daemon token-authenticated surface (no ambient browser
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
    timeout and a response-size cap. The link-local rule is what closes the cloud
    instance-metadata endpoint (`169.254.169.254`), the highest-value SSRF target on any hosted deployment.

13. **Block-enforcement checkpoints**: server-side at gateway DISPATCH fan-out time (a cached per-connection
    block-set, invalidated immediately on block/unblock) — never a client-side-only filter, both to avoid
    shipping blocked content over the wire and to prevent a modified client from ignoring the filter.

14. **Password-reset and reports anti-abuse**: always-identical response regardless of email existence
    (anti-enumeration), rate-limited; report filing rate-limited per user with reporter-history triage.

15. **OAuth loopback design**: the callback returns the one-time exchange code to a listener the client
    named, validated by host — `http` on a loopback IP literal with an explicit port, no userinfo, query or
    fragment, any port. `localhost` is refused because a name resolves through `/etc/hosts` and DNS and an
    IP literal resolves through nobody (RFC 8252 §8.3). The fixed primary port and fallback list are the
    CLI's own convention; what is registered with the providers is the instance's callback. See ADR 0027,
    which also records why an exchange code may travel in a loopback URL where a token pair may not.

16. **Auto-update hardening**: Sigstore/cosign signature verification via self-contained offline-verifiable
    bundles, anti-downgrade protection, fail-closed on verify failure, auto-rollback on repeated crash-loop —
    deliberately distinct from the license file's own Ed25519-JWT scheme (ADR 0020).

17. **File uploads**: unchanged from the original design — server-side content-type sniffing, randomized
    storage keys, separate origin with no ambient credentials, plus a stricter validation path for custom
    emoji specifically (resolution cap, format allow-list, decompression-bomb guard — emoji render
    automatically and repeatedly for every viewer, unlike a regular attachment opened deliberately).

18. **Rate limiting & abuse prevention**: `ulule/limiter`, `/64` IPv6-subnet grouping globally (not scoped to
    one feature), stricter limits on `/auth/*`, per-webhook rate limiting independent of the creating user's
    own limit. The client address the limiter groups on is decided once, in the router: `X-Forwarded-For`
    is honored only when explicitly configured, and is read from the right-hand end (the entry a trusted
    proxy appended) rather than the leftmost, client-supplied one. **Known gap, closed at `M122`**: the
    backend does not yet verify that the immediate peer *is* a trusted proxy, which matters only on
    Kubernetes, where a pod can reach the API Service without passing the Ingress.

19. **Dependency & supply-chain hygiene**: `govulncheck`/`pnpm audit`/`Trivy` in CI on every PR.

20. **Testing security properties**: adversarial `Resolve` unit tests, cross-guild access-control integration
    tests, and (new) an automated test asserting the export asymmetries (blocks, reports) hold.

21. **Device-code phishing** (M9, ADR 0028): the one threat in this system that no amount of correct
    cryptography prevents. An attacker starts a device authorization on their own machine, sends the user
    code to a victim — "enter this to verify your account" — and the victim, who signs in legitimately and
    presses Approve, has authorized the attacker's machine to act as them. It is not hypothetical; there
    have been real campaigns against other implementations of this grant.

    What is done about it, all of it on the verification page rather than in the protocol: **approval is a
    separate, explicit step** after authenticating, never implied by a successful sign-in; the page **names
    the device** asking and **shows the code back** so it can be compared against the screen that produced
    it; the wording says plainly that a code somebody sent you is a request to sign them in as you; a
    decision that is neither approve nor deny **denies**, because failing open here means authorizing a
    machine nobody chose to. There is deliberately **no `verification_uri_complete`** — a URL carrying the
    code turns the whole attack into one click (RFC 8628 §3.3.1, §5.4), and the `?code=` parameter the page
    does accept only prefills a field that still has to be submitted.

    One qualification on "no code-carrying URL", found by review: the page's provider buttons are links
    carrying a continuation that is not bound to the browser it was issued to, so an attacker holding one
    can carry a victim past the code-entry step. It does not carry them past the *approval* step, which is
    where the defense is — so the property that holds is "no link reaches an authorized device without a
    human decision on a page that names it", not "no link skips a step". Binding that continuation needs a
    browser session, which this surface does not have until Phase O.

    The residual risk belongs to the flow, not to this implementation, and is stated rather than implied:
    somebody who is talked all the way through those screens is signed in to an attacker's device. `Deny`
    exists so that realizing it a moment later is actionable — it revokes an approval the waiting client
    has not yet collected, so the sign-in never completes — and it says plainly when it was too late.

22. **The instance operator tier** (M10, ADR 0029): a third authority beside an access token and an API
    token, and the only credential in this system minted by a *client* rather than by the server. It is an
    HS256 JWT signed with the instance's own `[auth].jwt_secret`, carrying `typ: "operator"` and no
    subject, and it authorizes `/api/v1/instance/*` — bootstrap and invite management.

    What it proves is possession of the instance's configuration file, which concedes nothing that was not
    already conceded: anyone who can read that file can already forge an access token for any account here.
    What it buys is that the authority is *stated* — a bootstrap request proves filesystem access rather
    than being trusted for arriving early, so there is no window in which whoever reaches a freshly-migrated
    instance first becomes its administrator.

    Trust tier, per the rule that every local surface names one: **filesystem-permission-protected**, the
    same tier as the config file, and deliberately not either token tier above it. It is **not revocable** —
    there is no row to delete — so it lives two minutes and is minted per request. The `typ` claim is
    load-bearing rather than decorative: a *device entry token* carries the same issuer and key, a live
    expiry, and no subject, so without the check, entering a valid device code on the verification page
    would hand that browser instance-operator authority.

    `/instance` mounts outside the group that runs the ordinary Bearer middleware, so whether an operator
    token can authenticate an ordinary request is answered by the router rather than by each verifier
    remembering a check.

23. **Registration on an instance with no mail relay** (M10): an accepted limitation, recorded here so it
    stays deliberate. Such an instance cannot verify an address by any route, so registration creates
    accounts already verified and the account-existence oracle M10 closed elsewhere **stays open there** —
    there is no mail to carry the difference between "created" and "already exists". Refusing to register
    instead would trade a working instance for nothing.

    The same absence keeps M6's outright refusal for a provider address that cannot be vouched for, since
    the detour that replaces it is delivered by mail.

    Stated in three places on purpose — the wizard warns when SMTP is declined, the server logs it once at
    startup, and a test pins it — because the failure mode to guard against is this becoming quiet rather
    than it existing.

---

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

13. **Measure before optimizing further**: `net/http/pprof` (internal-only) and the Prometheus `/metrics`
    endpoint (auth-gated, aggregate-only labels) both arrive at `M93`, not earlier — real production
    numbers drive future optimization work, not guesses. They are deliberately not in the foundational
    milestone: `/metrics` is auth-gated on an Instance Admin token, which does not exist until `M71`, and
    there is nothing worth profiling before there are features generating load.

---

## 16. Verification

The project is in early implementation, so verification means both an internal-consistency pass over this
document / `docs/roadmap.md` / `CLAUDE.md` / `docs/adr/` / `docs/design/tui/`, and real tests:

- **One authority per topic**: this document describes what the system *is*; `docs/roadmap.md` owns
  milestone numbering and scope; `docs/adr/` owns the contested rationale; `docs/design/tui/` owns what the
  terminal client looks like, screen by screen. If the same thing is described in two of them, that is
  drift — collapse it to one and leave a pointer, rather than keeping both in sync by hand. (This is not
  hypothetical: a duplicated roadmap lived in two files until M1.)
- **Consistency**: grep this doc set for "AGPL," "cookie," "CSRF," "frontend" (outside §9's now-scoped
  usage), and "voice"+"deferred" — confirm none read as stale (licensing/auth/voice language should all
  match the current design, not the pre-v2 one). Confirm the daemon-holds-E2E-keys language is consistent
  everywhere (never "the CLI/TUI/GUI hold the keys"). Confirm every milestone number referenced in prose
  matches `docs/roadmap.md` exactly (`M0`–`M125`), and that no milestone is described in two places.
- **The terminal client's two vocabularies**: grep for "CLI" and confirm each use means the *command tree*
  (§4) and not the full-screen application (§4a) — that conflation is what ADR 0026 exists to undo, and it
  reappears every time a paragraph written before it is edited. Confirm every screen id in
  `docs/design/tui/SCREENS.md` is claimed by exactly one milestone and that no milestone cites an id that
  does not exist, and that every chord a screen names appears in `KEYMAP.md` with a scope.
- **Backend**: `go test ./...` clean, `govulncheck ./...` clean; manually exercise the token-based auth
  round-trip (register → login → Bearer-authenticated request) and a raw WS connection through
  Hello→Identify→READY with a real access token; confirm a request scoped to another guild/channel is
  rejected (403, not a silent empty result); confirm the block-aware fan-out filter actually removes a
  blocked author's events from the DISPATCH stream (inspect the stream, not just client rendering).
- **Daemon/clients**: confirm a client-side test client attached to the daemon's socket receives the same
  DISPATCH events the daemon gets from the real gateway; confirm a deliberately frozen attach client gets
  dropped without stalling a second, healthy attached client; confirm `norite config export`/`import`
  round-trips correctly.
- **Voice**: confirm the DSP chain order (AEC before RNNoise) via a two-party echo test; confirm the
  voice-worker crash is detected via the closed pipe without affecting messaging.
- **E2E**: confirm two test identities complete a key exchange with forward secrecy demonstrated; confirm the
  release-gate flag actually blocks E2E for a normal account until flipped.
- **Security spot-checks each milestone**: an XSS/ANSI-escape payload renders as inert text on every client;
  the audit log gets an entry for every guild-scoped mutation exercised in tests; the account-export
  asymmetries (blocks, reports) hold under an automated test.

---

## 17. Known tensions and accepted limitations

These are the places where this design knowingly trades one good property against another, or depends on
something not yet proven. They are recorded so a future reader can tell a deliberate trade-off from an
oversight — and so that revisiting one is a decision, not a discovery.

These are permanent, deliberate properties of the design. They must never be treated as gaps or oversights
during implementation, and must be documented plainly wherever the relevant subsystem is described in
`docs/architecture.md` and the relevant ADR:

- **Voice-worker isolation.** Voice/audio media (capture, encoding, the SFU connection, DSP) runs in an
  isolated voice-worker subprocess, spawned on demand by the daemon and torn down when a voice session ends —
  never inside the daemon process itself. A crash in the voice-worker must never take down messaging,
  presence, or plugins.
- **Mic-permission handoff is unverified until Milestone M25 completes.** The design intent is that a
  foreground TUI/GUI client triggers the OS permission prompt on first voice use, then hands audio capture off
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
  stack (daemon, CLI/TUI, GUI, backend) does not extend to the voice-worker: Opus (`hraban/opus`), RNNoise, and
  WebRTC's APM are all cgo bindings, because no mature pure-Go equivalents exist for production-grade audio
  codec/DSP work. This exception is contained entirely to the isolated, opt-out-gated voice-worker binary; the
  daemon, CLI/TUI, and GUI stay pure Go and cross-compile cleanly regardless.
- **E2E encryption carries compounding, not merely additive, cryptographic risk.** The feature depends on
  two independent custom protocol surfaces: the device-linking protocol (fully custom, no off-the-shelf
  equivalent) and the correct integration of the `go.mau.fi/libsignal` library into this project's own
  key-management and multi-device model. Either surface can silently break forward secrecy or device-trust
  guarantees with no visible symptom. Both require the dedicated external cryptographic security review at
  Milestone M103 before E2E is enabled for any account beyond the developer's own test accounts — enforced
  by a build/instance-level flag, not a documentation policy. This is a hard release gate, not optional
  polish. No history-transfer mechanism exists for a newly linked device, either: a device linked via
  Milestone M100 sees only messages sent after linking, matching the no-backup,
  permanent-loss-on-device-loss philosophy already accepted below. This is a deliberate limitation, not an
  oversight — it adds zero new custom-crypto surface to a protocol already carrying the two risks above.
- **The SFU's codec-agnostic track model is necessary but not sufficient for video.** §6 deliberately keeps
  Pion's internal track/participant model track-kind-agnostic now, specifically so a video track type is
  additive later rather than a redesign — but that agnosticism only covers whether the SFU *can* forward a
  track, not whether it forwards it *well* under real-world bandwidth constraints. Simulcast/SVC (dropping
  spatial/temporal layers for a participant on a poor connection) is real, separate engineering work,
  deliberately scoped into Phase N (Milestones M105–M107) rather than assumed to fall out for free from the
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
- **A browser alone cannot complete an OAuth sign-in, by design.** The flow is bound to the *client* that
  started it rather than to the browser that walks through it: `/authorize` requires a `flow_challenge` and
  `/auth/oauth/exchange` requires the matching `flow_verifier` (§2). Someone who opens an authorize URL by
  hand therefore reaches a page carrying a code that nothing they hold can spend. That is the intended
  behaviour and it is what closes login CSRF — an attacker who consents with their own provider account and
  hands the resulting callback to somebody else produces a code the victim's client cannot redeem, because
  the verifier never left the attacker's machine. The accepted cost is that "just visit `/authorize`" is not
  a usable sign-in path and never will be: every client mints a verifier first, including the CLI at M8 and
  the web SPA at Phase O. See ADR 0024.
