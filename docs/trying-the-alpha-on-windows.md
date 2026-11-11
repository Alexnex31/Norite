# Trying the alpha on Windows

This guide is for joining an instance that somebody else runs, from a Windows PC: with the invite link
you were sent, one installer line and one command take you to their guild. Running the instance itself is in [Trying the alpha](trying-the-alpha.md), which this guide leans on
for the words it uses — *instance*, *instance URL*, *guild*, and the two kinds of invite.

**Windows is the least-tried platform in this alpha.** Everything here builds for Windows, and the
Windows-specific parts — how the client reaches the daemon, and where the sign-in is stored — are covered by
tests that run on Linux. They had not yet run on a real Windows machine when this guide was written. If a
step fails, the output and the daemon's log (see [Where things are](#where-things-are)) are exactly what is
worth reporting as a [GitHub issue](https://github.com/Alexnex31/Norite/issues).

## Contents

- [What you need](#what-you-need)
- [The quick way: the installer](#the-quick-way-the-installer)
- [Getting the programs](#getting-the-programs)
- [Step 1 — check you can reach the instance](#step-1--check-you-can-reach-the-instance)
- [Step 2 — set up, with the invite link](#step-2--set-up-with-the-invite-link)
- [Step 3 — talk](#step-3--talk)
- [Doing setup's steps by hand](#doing-setups-steps-by-hand)
- [Where things are](#where-things-are)
- [Stopping and removing it](#stopping-and-removing-it)
- [When something goes wrong](#when-something-goes-wrong)

## What you need

- **Windows 10 or 11**, 64-bit: x64 (Intel and AMD) or ARM64.
- **[Windows Terminal](https://aka.ms/terminal)**. It is the default on Windows 11 and free from the
  Microsoft Store on Windows 10. The client draws a full-screen interface, and Windows Terminal is where
  that works properly; the old console window may, but has been tried less. Every command below is typed
  into a **PowerShell** tab in it.
- **From whoever invited you, an invite link**, such as
  `https://chat.example.com/join#PGTQRBXGTCDFFWBH`: it names the instance and the guild. Without one, the
  instance URL and a guild invite code do the same.
- **An instance invite code** too, only if the instance's registration is `invite` — ask its administrator.

## The quick way: the installer

[`scripts/install.ps1`](../scripts/install.ps1) does the rest of this guide in one go: it gets the two
programs, puts them in `%LOCALAPPDATA%\Programs\Norite` and on your `PATH`, starts the daemon, and runs
`norite setup`, which creates an account or signs in and, given the link, joins its guild. In PowerShell:

```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.ps1))) -Invite 'INVITE-LINK'
```

Without a link, the short form is enough:

```powershell
irm https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.ps1 | iex
```

From a checkout, with Go installed, it builds that checkout instead — the way to use it before a release
exists. Windows does not run a downloaded script file by default, so allow it for this one window first:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\scripts\install.ps1
```

What to know about it:

- **It was walked on Windows 11 in a virtual machine** for the release that updates itself: installing,
  the daemon at logon, an update and its undoing. Not on every Windows there is; if it stops, the steps
  below do the same by hand, and its output is worth an issue.
- **A download is checked before it is installed**: the archive must match the release's `checksums.txt`,
  and with [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) installed that file's
  signature is verified too. Without it the script says the signature was not checked.
- **The daemon starts at every logon**, as a Task Scheduler task with no window. `-NoAutoStart` starts it
  for this session only, and `-ShowConsole` gives it a window that shows its log as it is written, for
  testing (see [B](#b--start-the-daemon)).
- **Options need the longer form**, since `irm | iex` cannot carry any:

  ```powershell
  & ([scriptblock]::Create((irm https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.ps1))) -Instance https://chat.example.com
  ```

  `Get-Help .\scripts\install.ps1 -Detailed` lists them, `-Uninstall` among them.

## Getting the programs

You need two programs, kept together in one folder: `norite.exe`, the command line and the client, and
`norite-daemon.exe`, the background program it talks to. A release's archive holds a third,
`norite-voice.exe`, which the daemon starts for a voice call; keep it in the same folder. Voice calls
arrive with a later release, so a build from source can leave it out for now. There are three ways to get
them.

### From a release

Once a release is published, its [releases page](https://github.com/Alexnex31/Norite/releases) has a
client archive for Windows: `norite_VERSION_windows_amd64.zip`, or `…_arm64.zip` for an ARM PC. Download
it together with `checksums.txt` and `checksums.txt.sigstore.json`, into one folder.

**Check the download before running anything.** The release notes give a `cosign verify-blob` command,
which proves `checksums.txt` was signed by this repository's release workflow;
[cosign](https://docs.sigstore.dev/cosign/system_config/installation/) has a Windows build. The command is
written for a Unix shell: in PowerShell, put it on one line, or end each line with a backtick (`` ` ``)
instead of a backslash. It should print `Verified OK`. Then check the archive against the signed list:

```powershell
(Get-FileHash .\norite_0.1.0-alpha_windows_amd64.zip).Hash.ToLower()
Select-String "windows_amd64.zip" .\checksums.txt
```

The two long hexadecimal strings must be identical. If either check fails, do not run the files.

Then unpack it:

```powershell
Expand-Archive .\norite_0.1.0-alpha_windows_amd64.zip -DestinationPath $HOME\norite
cd $HOME\norite
```

### Building them on Windows, from a checkout

This needs [Git](https://git-scm.com/download/win) and **[Go](https://go.dev/dl/) 1.26 or newer** — the
`.msi` installer for your architecture. A Go from 1.21 on fetches 1.26 by itself the first time it needs to,
unless that has been switched off. In PowerShell, from the folder you cloned the repository into:

```powershell
git pull

$env:CGO_ENABLED = "0"
$env:GOWORK = "off"

mkdir bin -Force
cd cli
go build -trimpath -o ..\bin\norite.exe .\cmd\app
cd ..\daemon
go build -trimpath -o ..\bin\norite-daemon.exe .\cmd\daemond
cd ..\bin
.\norite.exe about
```

What the pieces do:

- `$env:CGO_ENABLED = "0"` builds self-contained programs and needs no C compiler.
- `$env:GOWORK = "off"` builds each module on its own, the way a release does. The client finds the
  daemon's and the backend's code through relative `replace` lines in `cli\go.mod`, so a plain checkout is
  all it needs.
- Both settings last only as long as that PowerShell window. Set them again in a new one.
- The first build downloads the dependencies and takes a minute or two; later ones are quick.
- `.\norite.exe about` proves the build works. It names the commit it was built from, marked modified if
  the checkout has uncommitted changes.

The repository's `just` recipes are not for Windows: they run through bash.

### Building them on another machine

Go builds for Windows from Linux or macOS too. From the repository's root there:

```sh
mkdir -p bin/windows
(cd cli    && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 GOWORK=off go build -trimpath -o ../bin/windows/norite.exe ./cmd/app)
(cd daemon && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 GOWORK=off go build -trimpath -o ../bin/windows/norite-daemon.exe ./cmd/daemond)
```

`GOARCH=arm64` instead for an ARM PC. Then copy both files into one folder on the PC — a USB stick, a
shared folder, or `scp` from PowerShell, which Windows 10 and 11 include.

**Files copied from another machine may be blocked** by Windows as having come from elsewhere. Unblock them
once, in their folder:

```powershell
Unblock-File .\norite.exe, .\norite-daemon.exe
```

### Optional: run them from anywhere

Every command below is written `.\norite.exe …`, run from the programs' folder. To type `norite` from any
folder instead, add the folder to your user `PATH` once, then open a new terminal tab:

```powershell
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
[Environment]::SetEnvironmentVariable("Path", "$userPath;$HOME\norite", "User")
```

Use the folder the programs are actually in, if it is not `$HOME\norite`.

## Step 1 — check you can reach the instance

```powershell
curl.exe INSTANCE-URL/api/v1/healthz
```

`INSTANCE-URL` is the invite link up to `/join`: for `https://chat.example.com/join#PGTQRBXGTCDFFWBH`, it
is `https://chat.example.com`. It should print `{"status":"ok"}`. Type `curl.exe`, not `curl`: in Windows PowerShell 5, `curl` is another
name for `Invoke-WebRequest`, which answers differently.

If nothing answers, this PC cannot reach the server, and nothing after this will work until it can. The
usual causes are on the server's side — its firewall, or the server not running — so tell whoever runs it.
On a home network, also check the PC is on the same network, not a guest Wi-Fi kept apart from it.

## Step 2 — set up, with the invite link

```powershell
.\norite.exe setup 'INVITE-LINK'
```

With no link, `.\norite.exe setup --instance INSTANCE-URL` does the same and joins nothing. The installer
runs this for you. It asks, in order:

1. **Whether the instance the link names is the one you meant.** Your account and password go there, so
   look at the address before pressing Enter.
2. **Whether you already have an account there.** If not, a username (2 to 32 characters: your public
   handle, shown as `@name`), an email address (which you sign in with and nobody else sees), and a
   password twice (at least 12 characters; nothing appears as you type it, deliberately). An instance whose
   registration is `invite` also asks for its instance invite code. The account is then signed in with the
   password you chose; with a mail relay, open the confirmation link the instance sends, then press Enter.
3. **Whether to install the daemon**, the background program that keeps you connected. It becomes a Task
   Scheduler task named "Norite Daemon", running as you at every logon with no window and no administrator
   rights. Enter says yes. [B](#b--start-the-daemon) below is how to run it in a tab instead.
4. **Whether to join the guild the link leads to**, once it has shown the guild, the channel and who
   invited you.

An instance at an `http://` address makes it warn that your password crosses the network unencrypted. On a
home network or inside a VPN that is expected. Every step already done is said and skipped, so running it
again is always safe. Your sign-in is stored in the **Credential Manager**.

## Step 3 — talk

```powershell
.\norite.exe
```

The client opens full-screen, on the home screen, which lists your guilds and their channels; a guild
`setup` joined is already there. With none yet, it looks like this:

```
Norite · signed in as @you

YOUR GUILDS
  NO GUILDS YET
  Redeem an invite below, or start one with `norite guild create --name NAME`.

REDEEM AN INVITE
  › paste an invite code or link
  RET looks the invite up; RET again joins

RET open · C-n/C-p move · C-x C-c quit · norite about
```

1. **To join another guild, paste its invite link or code** into the box with Ctrl+V, or type it — either
   case, with or without dashes — and press Enter. The client shows where it leads, and does nothing else
   yet:

   ```
     ✓ Tea Room · #general · invited by alice (@alice) · expires 2026-10-11 12:26 · RET join
   ```

2. **Press Enter again** to join. The guild appears in the list with its channels.
3. **Open a channel**: move to it with the arrow keys and press Enter. You see its last fifty messages,
   newest at the bottom, and new ones arrive as they are sent.
4. **Type a message and press Enter** to send it.

The keys, where `C-x` means holding Ctrl and pressing X:

| Key | What it does |
| --- | --- |
| Enter | On the home screen: look up the invite, then join; or open the selected channel. In a channel: send. |
| ↑ ↓ | Move through the guilds and channels on the home screen. |
| PgUp, PgDn | Scroll the channel. |
| Escape | Clear the invite box; leave a channel for the home screen. |
| Ctrl+V | Paste. |
| Ctrl+X, then Ctrl+C | Quit. |
| Ctrl+C alone | Only reminds you how to quit. It never quits by accident. |

The line at the bottom says when something changes underneath. *trying again in 4s · the daemon is not
running…* means the daemon stopped: `.\norite.exe daemon start` starts it again, and the client reconnects
by itself, keeping anything you had typed. The main guide's [Step 9](trying-the-alpha.md#step-9--open-the-client)
lists the others.

Everything also works as commands, which is handy for checking from a script:
`.\norite.exe guild list`, `.\norite.exe channel list GUILD_ID`, `.\norite.exe message list CHANNEL_ID`.

## Doing setup's steps by hand

`norite setup` runs three commands in turn, and each works alone too: to run the daemon in a tab rather
than as a task, from a script, or to change one thing later.

### A — create an account

```powershell
.\norite.exe register --instance INSTANCE-URL
```

Add `--invite-code CODE` if the instance needs an instance invite. It asks for:

- **a username**, 2 to 32 characters: your public handle, shown as `@name` beside your messages;
- **an email address**, which you sign in with and nobody else sees;
- **a password, twice**, at least 12 characters. Nothing appears as you type it; that is deliberate.

An instance at an `http://` address makes it warn that the password crosses the network unencrypted. On a
home network or inside a VPN that is expected; on an `https://` instance there is no warning.

```
Your account is ready. You can sign in now.

Sign in with:
  norite login --instance http://192.168.1.20:8080 --email you@example.com
```

If the instance sends mail, it asks you to open a confirmation link first instead.

### B — start the daemon

The daemon is the background program that holds your sign-in and the live connection to the server; the
client talks to it rather than to the server. **For trying it, run it in its own terminal tab**: open a
second PowerShell tab (Ctrl+Shift+T), go to the programs' folder, and start it:

```powershell
cd $HOME\norite      # or wherever the two programs are, such as the checkout's bin folder
.\norite-daemon.exe
```

It prints a few lines of log and keeps running. Leave that tab open for as long as you use Norite; Ctrl+C in
it stops the daemon. Everything else happens in the first tab.

**To have it start by itself whenever you log in**, `.\norite.exe daemon install` registers it as a Task
Scheduler task named "Norite Daemon", running as you and needing no administrator rights;
`.\norite.exe daemon start` starts it now, and `.\norite.exe daemon status` reports on it.
`.\norite.exe daemon stop` stops the daemon, however it was started.

Once a day the daemon checks for a new release and tells you: `.\norite.exe update status` says where
that stands, and `.\norite.exe update mode off` stops the check. **If you installed with `install.ps1`**,
the daemon verifies a new release's signature, installs it and starts it in its own place by itself;
`norite update mode notify` makes it wait for `norite update apply`. Programs you extracted or built
yourself, as in this step, are told about a release and are not replaced: both commands say so, and
running `install.ps1` is how such an install becomes one that updates.

The task starts the daemon at your logon, on battery as on mains, with no time limit, and with no window:
it runs in a console nobody sees, which is how Windows still tells it you are signing out.
`.\norite.exe logs tail --follow` shows what it is doing, and `.\norite.exe daemon install --show-console`
gives it a window that shows its log, for testing. After an update the new version runs on its own until
your next logon, and `daemon status` says so. If the task does not start, a tab is the reliable way, and
the output of `.\norite.exe daemon status` and `.\norite.exe logs tail` is worth an issue (see the last
row of the table at the end for what that output names).

### C — sign in

In the first tab:

```powershell
.\norite.exe login --instance INSTANCE-URL
```

It asks for your email and password:

```
Signed in as you on http://192.168.1.20:8080.
This device is "YOUR-PC"; its credential is stored in the OS keyring.
```

On Windows, "the OS keyring" is the **Credential Manager**. The daemon notices the sign-in by itself; you
do not need to restart it.

### D — join a guild

```powershell
.\norite.exe invite join 'INVITE-LINK'
```

It takes the link or the code alone. A link for another instance than the one this PC is signed in to is
refused, and nothing is sent to it.

## Where things are

| What | Where |
| --- | --- |
| The daemon's state, including its log, `daemon.log` | `%LOCALAPPDATA%\Norite\` — paste it into Explorer's address bar. `.\norite.exe logs tail` prints the log's newest lines |
| Your sign-in | Credential Manager → Windows Credentials → Generic Credentials, an entry named `norite:` followed by the instance URL |
| The scheduled task, if you installed one | Task Scheduler (`taskschd.msc`) → Task Scheduler Library → "Norite Daemon" |

## Stopping and removing it

```powershell
.\norite.exe logout             # removes the sign-in from Credential Manager
.\norite.exe daemon uninstall   # only if you ran daemon install
```

Then stop the daemon (Ctrl+C in its tab) and delete the programs' folder and `%LOCALAPPDATA%\Norite`.
Your account stays on the instance: deleting an account is not built yet (`M76a`).

## When something goes wrong

| What you see | What to do |
| --- | --- |
| `sh` is not recognized, after a line ending in `\| sh` | That was the macOS and Linux line, or `install.ps1` piped into `sh`, which Windows does not have. In PowerShell, run `irm https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.ps1 \| iex` ([the quick way](#the-quick-way-the-installer)). |
| `curl -fsSL …` fails with an `Invoke-WebRequest` error | In Windows PowerShell 5, `curl` is another name for `Invoke-WebRequest`, which takes none of those options. Use the `irm` line above. |
| `irm` is not recognized | That window is cmd, not PowerShell. Open PowerShell (a PowerShell tab in Windows Terminal) and run the line there. |
| `norite.exe : The term 'norite.exe' is not recognized` | Run it as `.\norite.exe` from its folder, or add the folder to `PATH` as above. |
| Windows says it protected your PC, or the file is blocked | `Unblock-File` on the programs, as above; or on the SmartScreen dialog, "More info", then "Run anyway". |
| Microsoft Defender quarantines a file | Unsigned Go programs are sometimes flagged by mistake. Build from source if you would rather not trust a binary, and report it as an issue. |
| `go: go.mod requires go >= 1.26.0` | Install Go 1.26 or newer from [go.dev/dl](https://go.dev/dl/). |
| `go: cannot find main module` | The build command ran in the wrong folder: `cli` for `norite.exe`, `daemon` for `norite-daemon.exe`. |
| `curl.exe` gets no answer | This PC cannot reach the server. See [step 1](#step-1--check-you-can-reach-the-instance). |
| `this instance takes new accounts by invite` | Ask for an instance invite code; `setup` asks for it, and `register` takes `--invite-code`. |
| `the instance did not take the sign-in` straight after `setup` created the account | Open the confirmation mail's link and press Enter. If the address already had an account, press Ctrl+C and run `.\norite.exe login` with its password. |
| `that is for …, and this machine is signed in to …, so nothing was sent` | The link names another instance than the one this PC uses, and nothing was sent. Check the link. |
| `password must be at least 12 characters` | A longer password; a few words strung together is easiest. |
| `the daemon is not running` or `unavailable` | `.\norite.exe daemon start`, or start `.\norite-daemon.exe` in its own tab and leave it open. |
| `the daemon is not signed in; run norite login` | `.\norite.exe setup` again, or [C](#c--sign-in). |
| `that email and password did not match an account` | Check both. With a mail relay on the instance, the address may not be confirmed yet; the answer is the same on purpose. |
| `not found` for an invite, in the client's box or from `setup` | Mistyped, expired, revoked or used up — these look identical on purpose. Ask for a new one, and check it is a *guild* invite, not an instance invite. |
| The screen looks garbled | Use Windows Terminal rather than the old console window, and make the window larger. |
| Anything else | The command's output and what `.\norite.exe logs tail` prints, in a [GitHub issue](https://github.com/Alexnex31/Norite/issues). Neither contains your password or tokens. The log does name your instance's address, your username and your Windows account's folder: an issue is public, so remove what you would rather not post. |
