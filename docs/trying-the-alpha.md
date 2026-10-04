# Trying the alpha

This guide takes you from nothing to two or more people talking in Norite, each on their own computer. One
machine runs the **server**, which everybody connects to; every person, the server's owner included if
they want to talk, runs the **client** on their own machine.

Set aside about half an hour for the server and five minutes for each person after that. You will type
commands in a terminal throughout; nothing here needs a graphical interface.

**What you get is small on purpose.** Text conversations in guild channels, drawn live in a terminal
client — no voice, no direct messages, no formatting, one channel on screen at a time. The full list is at
the end, in [What does not work yet](#what-does-not-work-yet), so nobody spends an evening finding it.

**This is not production self-hosting.** That documentation is milestone `M96`'s, and it will replace this
guide. Nothing here comes with a support commitment, and the next MINOR version (`v0.2.0`) may change
things in ways that need you to start over.

## Contents

- [The pieces, and the words this guide uses](#the-pieces-and-the-words-this-guide-uses)
- [The quick way: the installers](#the-quick-way-the-installers)
- [Getting the programs](#getting-the-programs)
- [Decide how people will reach the server](#decide-how-people-will-reach-the-server)
- [Part 1 — the server](#part-1--the-server)
- [Part 2 — each person's machine](#part-2--each-persons-machine)
- [Part 3 — a guild and an invite](#part-3--a-guild-and-an-invite)
- [Part 4 — talking](#part-4--talking)
- [Everyday things](#everyday-things)
- [When something goes wrong](#when-something-goes-wrong)
- [What does not work yet](#what-does-not-work-yet)

## The pieces, and the words this guide uses

There are three programs. Each archive in a release holds some of them.

| Program | Runs on | What it does |
| --- | --- | --- |
| `norite-server` | the server machine only | The instance itself. It keeps every account, guild and message in a Postgres database, and answers everybody's clients over HTTP. In the **server archive**. |
| `norite` | every person's machine | The command line (`norite guild create`, `norite login`, …) and, when run with no arguments, the **terminal client** you talk in. In the **client archive**. |
| `norite-daemon` | every person's machine | A small background program, one per OS account. It holds your sign-in and a live connection to the server, so the client can come and go without signing in again. `norite` talks to it, never to the server directly, once you are signed in. In the **client archive**, next to `norite`. |

And the words:

- **Instance** — one Norite server and everything on it. Accounts belong to an instance; an account on one
  instance means nothing to another.
- **The instance URL** — the address everybody types to reach the server, such as
  `http://192.168.1.20:8080` or `https://chat.example.com`. Choosing it is the next section.
- **Administrator** — the account that runs the instance. Today that mostly means it can make instance
  invites (below). It is created once, from the server machine.
- **Guild** — a group with its own channels and members; what Discord calls a server. Anybody with an
  account can create one and owns it.
- **Channel** — a place inside a guild where messages are posted. A new guild has none.
- **Two kinds of invite**, which are easy to confuse:

  | | An instance invite | A guild invite |
  | --- | --- | --- |
  | Lets somebody | create an account on the instance | who already has an account join one guild |
  | Needed when | the instance's registration is `invite` | always, to join a guild somebody else owns |
  | Made by | the administrator, on the server machine | the guild's owner, from any machine |
  | Made with | `norite instance invite create` | `norite invite create CHANNEL_ID` |
  | Used with | `norite register --invite-code CODE` | the box on the client's home screen |

## The quick way: the installers

Three scripts in [`scripts/`](../scripts) do most of this guide for you. The rest of the guide is what they
do, step by step, and is where to look when one of them stops.

**On each person's machine**, macOS or Linux, this installs the client and the daemon, starts the daemon,
and offers to create an account or sign in. It replaces [Part 2](#part-2--each-persons-machine):

```sh
curl -fsSL https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.sh | sh
```

On Windows, it is `install.ps1`; see [Trying the alpha on Windows](trying-the-alpha-on-windows.md).

**On the server machine**, Linux with systemd, this sets up the instance. It replaces
[Part 1](#part-1--the-server), apart from making the address reach the machine, which is the section
[Decide how people will reach the server](#decide-how-people-will-reach-the-server) and is still yours:

```sh
curl -fsSL https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install-server.sh |
    sh -s -- --url http://192.168.1.20:8080 --docker-postgres
```

Add `--behind-proxy` when a reverse proxy or a tunnel on the same machine handles HTTPS, with the `https://`
address as `--url`. `--docker-postgres` starts Postgres 16 in Docker with a generated password; without it,
the script asks how to reach a Postgres you already have.

What to know about them:

- **Nothing needs root.** The programs go in `~/.local/bin`, the instance in `~/norite-instance`, and the
  server runs as a systemd *user* service. The server script asks systemd to keep that service running
  after you log out ("lingering"); where that is refused, it says so and gives the one `sudo` command.
- **From a checkout they build that checkout**, with Go installed: `./scripts/install.sh`, and
  `./scripts/install-server.sh` with the same options. That is the way to use them before a release exists.
- **A download is checked before it is installed.** Each archive must match the release's `checksums.txt`,
  and with [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) installed, that file's
  signature is verified too. Without cosign the script says plainly that the signature was not checked;
  `--require-signature` makes that a failure instead.
- **Running one again is an upgrade.** The programs are replaced and the daemon or the server restarted.
  The server script leaves an existing `instance.toml`, and so the database and every account, alone.
- **`--help` lists the switches**, including `--no-daemon`, `--no-account`, `--no-service` and, for the
  client, `--uninstall`.
- **If you would rather read a script before running it**, download it, read it, then run it with `sh`.

## Getting the programs

**Joining from Windows?** [Trying the alpha on Windows](trying-the-alpha-on-windows.md) covers a PC's side
from start to finish: getting or building the programs, creating an account, signing in, joining and talking.

**From a release.** Download from the [releases page](https://github.com/Alexnex31/Norite/releases):

- the **server archive** for the server machine's operating system and architecture;
- the **client archive** for every machine that will talk, the server's included if its owner talks.

Before running anything, check the files. The release notes give two commands to run in the directory
holding the archives, `checksums.txt` and `checksums.txt.sigstore.json`: `cosign verify-blob` proves the
checksums were signed by this repository's release workflow, and `sha256sum` proves your archives match
them. `Verified OK` and an `OK` for each archive mean the files are genuine; anything else means do not
run them.

Unpack the client archive and **keep `norite` and `norite-daemon` in the same directory**, ideally one on
your `PATH` such as `~/.local/bin` or `/usr/local/bin`. Setting up the daemon later records where
`norite-daemon` lives, so moving it afterwards means running that step again.

The binaries are not code-signed for macOS or Windows yet, so either may warn the first time you run them.
On macOS, allow it under System Settings → Privacy & Security; on Windows, choose "More info" → "Run
anyway". Linux is where this release has been tried most; macOS and Windows builds exist and have been
exercised far less.

**From a checkout instead**, with Go and [`just`](https://github.com/casey/just) installed, `just build-local`
puts all four programs in `./bin/` (the fourth, `norite-gui`, is a placeholder).

## Decide how people will reach the server

Before installing anything, decide what the instance URL will be. Everything after this uses it, and the
server's configuration is written around it.

The server answers plain HTTP on port 8080, on every network interface. **It does not do HTTPS by itself.**
The configuration has an `[acme]` section and `norite instance init` has an `--acme` flag, both meant for
automatic certificates; nothing acts on them yet, so leave them off — turning them on still gives you plain
HTTP. There are two workable shapes.

### Shape A — everybody on one network

A home or office network, or a VPN such as [Tailscale](https://tailscale.com/) or WireGuard that puts
everybody's machines on one private network wherever they are.

- **The instance URL** is `http://` followed by the server machine's address on that network and `:8080` —
  for example `http://192.168.1.20:8080`, or `http://my-server:8080` if your network resolves names.
  On Linux, `ip -4 addr` shows the machine's addresses; on Tailscale, `tailscale ip -4`.
- **The server's firewall** must let that network reach TCP port 8080. With `ufw`, for example:
  `sudo ufw allow 8080/tcp`.
- **The trade-off**: passwords cross the network unencrypted, and the client says so every time it is
  about to send one. Inside a VPN that is fine — the tunnel encrypts everything. On a shared network
  where you do not trust everybody, use shape B instead.

### Shape B — over the internet, with HTTPS

You need a domain name whose DNS points at the server machine, and a reverse proxy on that machine that
handles HTTPS and passes requests on to Norite. [Caddy](https://caddyserver.com/) is the easiest, because
it obtains and renews a Let's Encrypt certificate by itself. Its whole configuration (`/etc/caddy/Caddyfile`
on most Linux packages) is:

```
chat.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

- **The instance URL** is `https://chat.example.com`.
- **The firewall** must allow TCP ports 80 and 443 from the internet (80 is how Let's Encrypt checks you own
  the name), and should *not* allow 8080.
- **Three settings** go into Norite's configuration in step 2, each for a reason:
  - `--listen-addr 127.0.0.1:8080`, so Norite only answers the proxy on the same machine and the unencrypted
    port is not reachable from outside at all;
  - `--public-base-url https://chat.example.com`, the address Norite uses when it needs to name itself;
  - `trust_proxy_headers = true` in the file it writes. Behind a proxy every request arrives from the
    proxy's own address, so without this the server's rate limits would count everybody as one person;
    with it, the server reads each person's real address from the header Caddy adds.
- **Another proxy than Caddy** works as long as it terminates TLS, forwards `X-Forwarded-For` and
  `X-Forwarded-Proto`, and passes WebSocket upgrades through: the live connection is a WebSocket at
  `/gateway`, and without the `Upgrade` header forwarded, sign-in works but no message ever arrives live.

## Part 1 — the server

Everything in this part happens on the server machine, in one directory you keep for the instance —
`~/norite-instance` in the examples:

```sh
mkdir -p ~/norite-instance && cd ~/norite-instance
```

### Step 1 — a Postgres database

Norite stores everything in PostgreSQL. It has been developed and tested against Postgres 16. It needs an
empty database and a user that owns it; it creates its own tables on first start.

- **With Docker, from a checkout of this repository**:

  ```sh
  docker compose -f docker/docker-compose.yml up -d postgres
  ```

  That starts Postgres 16 on `127.0.0.1:5432` with a user, password and database all named `norite` —
  fine on a machine you control, since it only listens locally.
- **With your system's Postgres** (`apt install postgresql` or similar):

  ```sh
  sudo -u postgres createuser --pwprompt norite    # asks for a password for the new user
  sudo -u postgres createdb --owner norite norite
  ```

### Step 2 — configure the instance

```sh
norite instance init -o ./instance.toml --public-base-url INSTANCE-URL
```

Add `--listen-addr 127.0.0.1:8080` too if you chose shape B. It asks a handful of questions, each with a
default in brackets that Enter accepts:

```
Database
--------
  Norite needs an existing, empty Postgres database. It creates its own schema on first start.
Postgres host [localhost]:
Postgres port [5432]:
Database name [norite]:
Database user [norite]:
Database password:
SSL mode (disable/allow/prefer/require/verify-ca/verify-full) [disable]:
Registration
------------
  "open" lets anyone create an account on this instance.
  "invite" requires an invite code, which is what a private instance wants.
Registration policy (open/invite) [open]:
```

- **The database answers** are the ones from step 1. `disable` for SSL mode is right when Postgres is on
  the same machine.
- **Registration** decides who can create an account. `open` means anybody who can reach the instance URL;
  `invite` means only somebody holding an instance invite. **Choose `invite` for shape B** — otherwise
  anybody on the internet who finds the address can sign up.

It then writes `instance.toml` and prints a summary. Three things to know about that file:

- **It holds the instance's signing key**, the secret every sign-in token is signed with, and the database
  password. It is written readable by your user only (`0600`). Keep it that way and keep a copy somewhere
  safe: losing it signs everybody out, and anybody who has it can sign in as anybody.
- **It documents itself.** Every setting it writes has a comment explaining it; open it in an editor. For
  shape B, this is where you add `trust_proxy_headers = true`, under the `[http]` heading.
- **There is no mail relay** unless you configure `[smtp]`, and the summary says what that means: new
  accounts can sign in immediately without confirming their address, nobody can reset a forgotten
  password, and registering reveals whether an address already has an account. All three are fine for
  trying it among people you know.

### Step 3 — start the server

```sh
norite-server -config ./instance.toml
```

It logs one JSON line per event to the terminal. On the first start it creates its tables, and you are
waiting for this line:

```
{"level":"info", … "message":"migrations complete — instance is ready"}
```

**Leave it running.** Closing the terminal stops the server; [Keeping the server
running](#keeping-the-server-running) below shows how to run it as a service instead.

**Now check, from one of the other machines**, that the server can be reached at the instance URL:

```sh
curl INSTANCE-URL/api/v1/healthz
```

It should print `{"status":"ok"}`. If it prints `{"status":"starting"}`, it is still creating its tables;
try again in a few seconds. If it does not answer at all, a firewall, the proxy or the address is in the
way, and nothing after this will work until it does — see [When something goes
wrong](#when-something-goes-wrong).

### Step 4 — create the administrator

In a second terminal on the server machine, in the same directory:

```sh
norite instance bootstrap --config ./instance.toml
```

It asks for a username, an email address and a password twice, and creates the administrator account.
This works exactly once per instance: it proves you run the instance by signing its request with the key
in `instance.toml`, so nobody who merely reaches the server first can claim it.

```
Created alice, the administrator of http://192.168.1.20:8080.

Sign in with:
  norite login --instance http://192.168.1.20:8080
```

The administrator is an ordinary account in every other way. To talk from it, do part 2 on whichever
machine its owner uses, skipping step 5 — the account already exists.

## Part 2 — each person's machine

Every person does this on their own machine, with only the client archive: no server, no Postgres. On
Windows, [Trying the alpha on Windows](trying-the-alpha-on-windows.md) covers this part and joining a guild.

### Step 5 — create an account

```sh
norite register --instance INSTANCE-URL
```

If the instance's registration is `invite`, add `--invite-code CODE`, with a code the administrator makes
on the server machine:

```sh
norite instance invite create --config ./instance.toml
```

That prints a code such as `QQQNBQQMGYTMQRMX` that never expires and can be used any number of times,
unless you add `--max-uses 1` or `--expires-in 48h` (hours and minutes, such as `48h` or `30m`; this command
does not take days). Codes can be typed in either case, with or without dashes.

`register` asks for:

- **a username** — 2 to 32 characters. It is your public handle, shown as `@name` next to your messages,
  and cannot be changed yet;
- **an email address** — used for sign-in. Nobody else sees it;
- **a password, twice** — at least 12 characters. Typed without echo, and never accepted as a flag,
  because a flag shows up in the process list for every user on the machine.

`--display-name` sets the name people see beside your handle; it defaults to the username. Without a mail
relay the instance answers "Your account is ready. You can sign in now."; with one, it sends a link that
has to be opened first.

### Step 6 — set up the daemon

```sh
norite daemon install
norite daemon start
```

`install` registers `norite-daemon` with your operating system's service manager — a systemd *user*
service on Linux, a launchd agent on macOS, a logon task on Windows — so it starts by itself whenever you
log in. It never needs administrator rights and never runs as a system service: it runs as you, because it
holds your sign-in. On macOS it is already running after `install`, and it says so.

`norite daemon status` tells you where it stands: it exits 0 when running, 1 when installed but stopped and
2 when not installed. Its log is `daemon.log` in its state directory: `~/.local/state/norite/` on Linux,
`~/Library/Application Support/Norite/` on macOS, `%LOCALAPPDATA%\Norite\` on Windows.

One daemon serves one OS account. Two people sharing a computer use two OS accounts.

### Step 7 — sign in

```sh
norite login --instance INSTANCE-URL
```

It asks for your email and password, and stores the sign-in for the daemon:

```
Signed in as bob on http://192.168.1.20:8080.
This device is "bobs-laptop"; its credential is stored in the OS keyring.
```

The credential goes in the operating system's keyring when there is one. On a machine without one — a
headless Linux box, or over SSH — it goes in a file only your user can read, and the message says so. The
daemon notices the sign-in by itself within a moment; you do not need to restart it.

`--email` saves typing the address. For scripts, the password can come from the `NORITE_PASSWORD`
environment variable instead of the prompt.

## Part 3 — a guild and an invite

Whoever creates the guild does this, from their own machine once signed in. Every command here goes
through the daemon, so it works the same on any machine.

### Step 8 — create a guild and a channel

```sh
norite guild create --name "Tea Room"
```

```
365082143254118400  Tea Room
  owner:       365082065139400704
  description: -
  recording:   off
  created:     2026-10-04 12:26
```

The long number first on the line is the guild's id. Everything in Norite is named by an id like this, and
the commands take them as arguments. A new guild has no channels, so make one:

```sh
norite channel create 365082143254118400 --name general
```

```
365082149730123776  text     general
```

The first number is the channel's id. `norite guild list` and `norite channel list GUILD_ID` show these
again whenever you need them, and every command takes `--json` if you would rather script it.

### Step 9 — invite somebody

```sh
norite invite create 365082149730123776
```

```
PGTQRBXGTCDFFWBH  into channel 365082149730123776  0 uses  expires 2026-10-11 12:26  by alice (@alice) 365082065139400704
```

The first word is the code. Send it to the person however you like; it is not a password, but anybody who
has it can join, so do not post it publicly. By default it lasts a week and can be used any number of
times:

- `--expires-in 1d` or `12h` (up to `30d`), or `--expires-in never`, changes how long it lasts;
- `--max-uses 1` makes it single-use;
- `norite invite list GUILD_ID` shows the guild's live invites, and `norite invite revoke CODE` ends one.

Only the guild's owner can create invites to begin with. Letting others do it is a permission the owner
grants through a role; see `norite role --help`.

## Part 4 — talking

### Step 10 — open the client

Run `norite` with no arguments. It opens full-screen in the terminal, on the home screen:

```
Norite · signed in as @bob

YOUR GUILDS
  NO GUILDS YET
  Redeem an invite below, or start one with `norite guild create --name NAME`.

REDEEM AN INVITE
  › paste an invite code
  RET looks the code up; RET again joins

RET open · C-n/C-p move · C-x C-c quit · norite about
```

**To join with a guild invite**, paste or type the code into the box — it always takes typing — and press
Enter. The client looks it up and shows where it leads before doing anything:

```
  ✓ Tea Room · #general · invited by alice (@alice) · expires 2026-10-11 12:26 · RET join
```

Press Enter again to join, or Escape to clear the box. The guild then appears in the list with its
channels.

**To open a channel**, move to it with the arrow keys (or `C-n` and `C-p`) and press Enter. The channel
shows its last fifty messages, newest at the bottom, and new ones appear as they are sent:

```
# general · Tea Room

alice  12:27
  hello, bob
bob  12:27
  hello, alice

──────────────────────────────────────────────────────────────────────────────────
› write a message
RET send · ESC home · PgUp/PgDn scroll · C-x C-c quit
```

Type and press Enter to send. A message can be up to 4,000 characters.

| Key | What it does |
| --- | --- |
| Enter | Home: look up the code in the box, then join; or open the selected channel. Channel: send. |
| ↑ ↓, or `C-n` `C-p` | Move through the guild and channel list on the home screen. |
| PgUp, PgDn | Scroll the channel. A message arriving while you are scrolled up does not move you. |
| Escape | Clear the invite box on the home screen; leave a channel for the home screen. |
| `C-x` then `C-c` | Quit. (`C-x` means hold Control and press x.) |
| `C-c` alone | Only reminds you of the above: it is reserved for later commands, so it never quits by accident. |

`norite --channel CHANNEL_ID` opens a channel directly, skipping the home screen.

**The line at the bottom** says when something has changed under the client, and what it is doing about it:

- *trying again in 4s · the daemon is not running…* — the daemon stopped, or was never started. Start it
  (`norite daemon start`); the client reconnects by itself, and a message you had typed is kept.
- *signed out; run `norite login`, and this will carry on* — exactly that. The screen clears, because the
  conversation belongs to the account that signed out.
- *this channel was deleted*, or *this guild was deleted, or you are no longer in it* — press Escape to go
  home.
- *not sent: forbidden* — the channel's permissions do not let you post there. Your text stays in the box.

**Everything is also available as commands**, which is handy for scripts:

```sh
norite message list CHANNEL_ID                    # the latest messages
norite message send CHANNEL_ID --content "text"   # post one
```

`norite message edit` and `norite message delete` change or remove your own. `norite --help`, and `--help`
after any command, list the rest.

## Everyday things

### Signing out, and switching accounts

`norite logout` removes this machine's sign-in. The daemon notices within a moment, closes its connection
and forgets what it held; an open client clears its screen. `norite login` with another account switches
to it the same way.

### Keeping the server running

Started from a terminal, the server stops when the terminal closes. For a quick try, running it inside
`tmux` or `screen` is enough. To have it start at boot and restart if it fails, on a Linux machine with
systemd, create `/etc/systemd/system/norite-server.service`:

```ini
[Unit]
Description=Norite server
Wants=network-online.target
After=network-online.target

[Service]
User=YOUR-USER
WorkingDirectory=/home/YOUR-USER/norite-instance
ExecStart=/usr/local/bin/norite-server -config /home/YOUR-USER/norite-instance/instance.toml
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Replace `YOUR-USER` and the paths with yours, then:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now norite-server
journalctl -u norite-server -f    # follow its log
```

If Postgres runs on the same machine as a system service, add `postgresql.service` to both `Wants=` and
`After=`.

### Backing up

Two things make up an instance: the database and `instance.toml`. This backs up the first:

```sh
sudo -u postgres pg_dump norite > norite.sql
```

Copy the second somewhere safe, remembering what it holds. Either without the other is not a working
instance.

### Upgrading to a newer alpha

Stop the server, replace `norite-server`, start it again: it upgrades its own tables on start. On each
person's machine, replace `norite` and `norite-daemon` in place and run `norite daemon restart`. Read the
release notes first — before `1.0.0`, a MINOR version may need you to start over, and the notes say when.
There is no downgrade.

### Removing it

On a person's machine: `norite logout`, `norite daemon uninstall`, then delete the binaries and the state
directory from step 6. On the server: stop it, drop the database (`sudo -u postgres dropdb norite`), and
delete the instance directory.

## When something goes wrong

| What you see | What it means | What to do |
| --- | --- | --- |
| `curl …/healthz` from another machine hangs or is refused | The request never reaches Norite. | Check the server is running and listening (`ss -ltn` shows `:8080`); the firewall (shape A: port 8080; shape B: 80 and 443); that you used the right address. In shape B, `--listen-addr 127.0.0.1:8080` makes Norite unreachable *except* through the proxy, by design. |
| `{"status":"starting"}` | The server is still creating or upgrading its tables. | Wait a few seconds. |
| The server exits right after starting | Its last log line says why — usually the database is unreachable or the password is wrong. | Fix the `[database]` section of `instance.toml`. |
| `the daemon is not running` or `unavailable` | No daemon is answering on this machine. | `norite daemon status`, then `norite daemon start`. Its log is `daemon.log` in the state directory. |
| `the daemon is not signed in; run norite login` | The daemon is running with no sign-in. | `norite login --instance INSTANCE-URL`. |
| `that email and password did not match an account` | Wrong email or password — or, with a mail relay, an address not confirmed yet. The answer is deliberately the same for all of them. | Check both; look for the confirmation mail. |
| `this instance takes new accounts by invite` | Registration is `invite`. | Ask the administrator for an instance invite and pass `--invite-code`. |
| `password must be at least 12 characters` | Exactly that. | A longer password; a few words strung together is easiest. |
| The home screen says `not found` for a code | The code is mistyped, expired, revoked or used up. These look identical on purpose, so codes cannot be guessed. | Ask for a new one — and check it is a *guild* invite, not an instance invite. |
| Signing in works, but messages only appear after reopening the channel | Behind a proxy, the live WebSocket connection is not getting through. | Make the proxy forward WebSocket upgrades for `/gateway` (Caddy does without configuration). |
| `the attach socket path … is N bytes` | You pointed `XDG_STATE_HOME` at a deep directory, and Unix sockets have a path limit. | Use a shorter directory, or unset it. |
| A warning that the instance is plain HTTP | Your password is about to cross the network unencrypted. | Expected in shape A; in shape B, check you typed `https://`. |

Questions and bug reports are welcome as [GitHub issues](https://github.com/Alexnex31/Norite/issues). A
security problem goes through [`SECURITY.md`](../SECURITY.md) instead, never a public issue.

## What does not work yet

Listed so nobody spends an evening finding it:

- voice and video;
- direct messages and friends;
- presence and typing indicators;
- more than one channel on screen;
- scrolling back past the last fifty messages (new ones keep arriving);
- formatting, attachments and link previews;
- editing, deleting or reacting from the client — `norite message edit` and `delete` work;
- notifications;
- the GUI and the web client;
- password reset, which needs `[smtp]` in `instance.toml`;
- HTTPS without a reverse proxy, as described above.
