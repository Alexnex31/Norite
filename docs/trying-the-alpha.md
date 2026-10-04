# Trying the alpha

This runs an instance on one machine and has people join it from their own. It is not self-hosting
documentation — that is `M96` — and nothing here is hardened for strangers: no support commitment, and the
next MINOR version may break it.

You need the server archive on the machine that runs the instance, and the client archive on every
machine that joins it, from the [release](https://github.com/Alexnex31/Norite/releases) — verify them with
the `cosign` command in the release notes — or a checkout and `just build-local`, which puts the same
programs in `./bin/`. The binaries are not code-signed for macOS or Windows, so both will warn on first run.

## How the others reach it

The server speaks plain HTTP on port 8080 of every interface. It does not terminate TLS itself: the
`[acme]` setting is accepted but nothing acts on it yet, so leave it off. Pick one of two shapes; what
follows calls the result **the instance URL**, and it is what everybody types.

- **On one network** — a LAN, or a VPN such as Tailscale or WireGuard joining everybody. The instance URL
  is `http://SERVER-ADDRESS:8080`, and the server's firewall has to let that network reach port 8080.
  Passwords cross the network unencrypted, which the client says each time one is about to; inside a VPN
  the tunnel encrypts them.
- **Over the internet** — a domain name pointing at the server, and a reverse proxy in front of it that
  terminates TLS. [Caddy](https://caddyserver.com/) does it in three lines and fetches its own certificate:

  ```
  chat.example.com {
      reverse_proxy 127.0.0.1:8080
  }
  ```

  The instance URL is `https://chat.example.com`, ports 80 and 443 must be open, and step 2 below sets
  three things: `--listen-addr 127.0.0.1:8080`, so the plain port is not reachable from outside;
  `--public-base-url https://chat.example.com`; and `trust_proxy_headers = true` under `[http]` in the
  file it writes, so rate limits count each person rather than the proxy. The real-time connection is a
  WebSocket at `/gateway`, which Caddy passes through untouched; another proxy has to forward its
  `Upgrade` header.

## Steps

1. **Postgres 16**, on the server machine. Any instance you control, or from a checkout: `docker compose
   -f docker/docker-compose.yml up -d postgres` (user, password and database all `norite`, on
   `127.0.0.1:5432`).
2. **Configure the instance.** `norite instance init -o ./instance.toml --public-base-url INSTANCE-URL`
   asks how to reach Postgres and whether registration is open, and writes a `0600` file holding the
   instance's signing key. Add the proxy settings above if you chose that shape.
3. **Start the server.** `norite-server -config ./instance.toml` migrates its schema on first start. Then,
   from one of the *other* machines, `curl INSTANCE-URL/api/v1/healthz` should answer `{"status":"ok"}`.
   If it does not, the firewall or the proxy is in the way, and nothing below will work until it does.
4. **Create the administrator**, on the server machine: `norite instance bootstrap --config
   ./instance.toml` — once, ever; the file's signing key is what proves you run the instance.
5. **Install the daemon**, on each machine that joins, the server's included if you will talk from it:
   `norite daemon install && norite daemon start` — a user-level service, one per OS account, that holds
   the connection to the instance.
6. **Sign in.** `norite login --instance INSTANCE-URL`.
7. **Make a guild and a channel.** `norite guild create --name "Tea Room"`, then `norite channel create
   GUILD_ID --name general`; a new guild has no channels.
8. **Invite somebody.** `norite invite create CHANNEL_ID` prints a code that lasts a week. Hand it over.
9. **The second person**, on their own machine with only the client archive — no server, no Postgres:
   `norite register --instance INSTANCE-URL`, then steps 5 and 6 as themselves. If step 2 made
   registration invite-only, `register` also needs `--invite-code`, which the administrator makes on the
   server machine with `norite instance invite create --config ./instance.toml`.
10. **Talk.** Both run `norite`. The second person pastes the code into the home screen's box and presses
    RET twice — once to see where it leads, once to join — and RET opens `#general`. `C-x C-c` quits.

## What does not work yet

Listed so nobody spends an evening finding it: voice; DMs and friends; presence and typing indicators; more
than one channel on screen; scrolling back past the last fifty messages (new ones keep arriving); formatting,
attachments and link previews; editing, deleting or reacting from the client (`norite message edit` and
`delete` work); notifications; the GUI and the web client; and password reset, which needs `[smtp]` in
`instance.toml`. `norite --help` lists what the command line can do.
