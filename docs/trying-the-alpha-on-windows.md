# Trying the alpha on Windows

This guide is for joining an instance that somebody else runs, from a Windows PC: create an account and
talk. Running the instance itself is in [Trying the alpha](trying-the-alpha.md), which this guide leans on
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
- [Step 2 — create an account](#step-2--create-an-account)
- [Step 3 — start the daemon](#step-3--start-the-daemon)
- [Step 4 — sign in](#step-4--sign-in)
- [Step 5 — join a guild and talk](#step-5--join-a-guild-and-talk)
- [Where things are](#where-things-are)
- [Stopping and removing it](#stopping-and-removing-it)
- [When something goes wrong](#when-something-goes-wrong)

## What you need

- **Windows 10 or 11**, 64-bit: x64 (Intel and AMD) or ARM64.
- **[Windows Terminal](https://aka.ms/terminal)**. It is the default on Windows 11 and free from the
  Microsoft Store on Windows 10. The client draws a full-screen interface, and Windows Terminal is where
  that works properly; the old console window may, but has been tried less. Every command below is typed
  into a **PowerShell** tab in it.
- **From whoever runs the instance**:
  - **the instance URL**, such as `http://192.168.1.20:8080` or `https://chat.example.com`;
  - **an instance invite code**, only if the instance's registration is `invite` — ask;
  - **a guild invite code**, to join their guild once you have an account.

## The quick way: the installer

[`scripts/install.ps1`](../scripts/install.ps1) does the rest of this guide in one go: it gets the two
programs, puts them in `%LOCALAPPDATA%\Programs\Norite` and on your `PATH`, starts the daemon, and offers
to create an account or sign in. In PowerShell:

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

- **It has run the least of anything here.** Its downloading and checking were exercised under PowerShell
  on Linux, tampered archive included. Everything after the files are in place — the `PATH`, the daemon,
  the questions — had not run on a real Windows machine when this was written. If it stops, the steps
  below do the same by hand, and its output is worth an issue.
- **A download is checked before it is installed**: the archive must match the release's `checksums.txt`,
  and with [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) installed that file's
  signature is verified too. Without it the script says the signature was not checked.
- **The daemon is started hidden, for now.** It does not come back after Windows restarts unless you pass
  `-AutoStart`, which registers it at logon and is the least-tried path of all (see step 3).
- **Options need the longer form**, since `irm | iex` cannot carry any:

  ```powershell
  & ([scriptblock]::Create((irm https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.ps1))) -Instance https://chat.example.com
  ```

  `Get-Help .\scripts\install.ps1 -Detailed` lists them, `-Uninstall` among them.

## Getting the programs

You need two programs, kept together in one folder: `norite.exe`, the command line and the client, and
`norite-daemon.exe`, the background program it talks to. There are three ways to get them.

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

It should print `{"status":"ok"}`. Type `curl.exe`, not `curl`: in Windows PowerShell 5, `curl` is another
name for `Invoke-WebRequest`, which answers differently.

If nothing answers, this PC cannot reach the server, and nothing after this will work until it can. The
usual causes are on the server's side — its firewall, or the server not running — so tell whoever runs it.
On a home network, also check the PC is on the same network, not a guest Wi-Fi kept apart from it.

## Step 2 — create an account

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

## Step 3 — start the daemon

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
`.\norite.exe daemon start` starts it now, and `.\norite.exe daemon status` reports on it. This path has
been tried least of all. Because the daemon is a console program, Windows may show its console window at
each logon, and closing that window stops it — which is why a tab is the better choice for a first try.

## Step 4 — sign in

Back in the first tab:

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

## Step 5 — join a guild and talk

```powershell
.\norite.exe
```

The client opens full-screen, on the home screen:

```
Norite · signed in as @you

YOUR GUILDS
  NO GUILDS YET
  Redeem an invite below, or start one with `norite guild create --name NAME`.

REDEEM AN INVITE
  › paste an invite code
  RET looks the code up; RET again joins

RET open · C-n/C-p move · C-x C-c quit · norite about
```

1. **Paste the guild invite code** into the box with Ctrl+V, or type it — either case, with or without
   dashes — and press Enter. The client shows where it leads, and does nothing else yet:

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
| Enter | On the home screen: look up the code, then join; or open the selected channel. In a channel: send. |
| ↑ ↓ | Move through the guilds and channels on the home screen. |
| PgUp, PgDn | Scroll the channel. |
| Escape | Clear the invite box; leave a channel for the home screen. |
| Ctrl+V | Paste. |
| Ctrl+X, then Ctrl+C | Quit. |
| Ctrl+C alone | Only reminds you how to quit. It never quits by accident. |

The line at the bottom says when something changes underneath. *trying again in 4s · the daemon is not
running…* means the daemon tab was closed or stopped: start it again, and the client reconnects by itself,
keeping anything you had typed. The main guide's [Step 10](trying-the-alpha.md#step-10--open-the-client)
lists the others.

Everything also works as commands, which is handy for checking from a script:
`.\norite.exe guild list`, `.\norite.exe channel list GUILD_ID`, `.\norite.exe message list CHANNEL_ID`.

## Where things are

| What | Where |
| --- | --- |
| The daemon's state, including its log, `daemon.log` | `%LOCALAPPDATA%\Norite\` — paste it into Explorer's address bar |
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
| `norite.exe : The term 'norite.exe' is not recognized` | Run it as `.\norite.exe` from its folder, or add the folder to `PATH` as above. |
| Windows says it protected your PC, or the file is blocked | `Unblock-File` on both programs, as above; or on the SmartScreen dialog, "More info", then "Run anyway". |
| Microsoft Defender quarantines a file | Unsigned Go programs are sometimes flagged by mistake. Build from source if you would rather not trust a binary, and report it as an issue. |
| `go: go.mod requires go >= 1.26.0` | Install Go 1.26 or newer from [go.dev/dl](https://go.dev/dl/). |
| `go: cannot find main module` | The build command ran in the wrong folder: `cli` for `norite.exe`, `daemon` for `norite-daemon.exe`. |
| `curl.exe` gets no answer | This PC cannot reach the server. See [step 1](#step-1--check-you-can-reach-the-instance). |
| `this instance takes new accounts by invite` | Ask for an instance invite code, and pass `--invite-code`. |
| `password must be at least 12 characters` | A longer password; a few words strung together is easiest. |
| `the daemon is not running` or `unavailable` | Start `.\norite-daemon.exe` in its own tab, and leave it open. |
| `the daemon is not signed in; run norite login` | Step 4. |
| `that email and password did not match an account` | Check both. With a mail relay on the instance, the address may not be confirmed yet; the answer is the same on purpose. |
| The client's box says `not found` for a code | Mistyped, expired, revoked or used up — these look identical on purpose. Ask for a new one, and check it is a *guild* invite, not an instance invite. |
| The screen looks garbled | Use Windows Terminal rather than the old console window, and make the window larger. |
| Anything else | The command's output and `%LOCALAPPDATA%\Norite\daemon.log`, in a [GitHub issue](https://github.com/Alexnex31/Norite/issues). Neither contains your password or tokens. |
