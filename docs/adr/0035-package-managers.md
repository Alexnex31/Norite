# ADR 0035: Package managers — which channels, who updates, and who holds the keys

## Status
Accepted (2026-10-20), on the `package-managers` branch, which is not a milestone. **Amends
[ADR 0020](0020-operations.md)** in one place: "any other install checks and says so" now says
*which* command updates it, when the install came from a package manager this record publishes to. Adds
two suffixed milestones, **M37a** (Snap) and **M78a** (Flatpak), and states the conditions under which a
distribution's own repository becomes a third.

## Context
Until now a release was archives on GitHub and two installer scripts. Somebody on Arch, Debian, Fedora,
macOS or Windows expects `pacman`, `apt`, `dnf`, `brew` or `winget`, and the testers the alpha is reaching
are on several of them.

"Publish to a package manager" names two different things, and the plan rests on keeping them apart:

- **Channels the project publishes to itself.** Its own APT, RPM and pacman repositories, a Homebrew tap,
  a Scoop bucket, the AUR, winget's community repository, the Snap Store, Flathub, a nixpkgs pull request.
  Each is a name registered by an account, first come, first served. No company, trademark or proof of
  ownership is asked anywhere, which fits ADR 0032 reserving the name by declining to grant it rather
  than by a claim.
- **A distribution's own repository**: Debian (and Ubuntu, which takes Debian's), Fedora, Arch `extra`,
  homebrew-core. Somebody else decides, on their schedule, and builds from source under their policy.

Three facts about Norite shape every decision below:

- **The daemon is a per-user service**, and M23 settled that nothing installs or starts one unasked.
- **The daemon updates itself** (M24), but only an install whose receipt names it. A package manager
  also updates what it installed, and two updaters on one file is the conflict to avoid.
- **While MAJOR is 0, the gateway handshake requires the same MINOR** (ADR 0033). A frozen distribution
  shipping a 0.1 client for two years ships one every 0.2 instance refuses, the flagship included.

On 2026-10-20 the name `norite` was free on every registry this record names: the AUR and Arch's own
repositories, Homebrew (formula and cask), Debian, Ubuntu, Fedora, the Snap Store (`norite` and
`norite-server`), nixpkgs and Scoop's main and extras buckets. npm's `norite` is an unrelated site
generator. winget identifiers are `Publisher.Name`, so nothing can collide.

## Decision

### 1. Two packages, everywhere
- **`norite`**: the client, `norite` and `norite-daemon`, and the voice-worker once Phase E builds it.
  They are one install for the reason they are one archive: `norite` does nothing without the daemon.
- **`norite-server`**: the server binary and its documentation, with the reference listing every
  `instance.toml` setting (`contracts/instance-config.toml`, which says it is not a starting file). No
  system user and no service: `norite instance init` and `install-server.sh` stay how an instance is set
  up, and M96 owns the production path.

Every channel below carries both. The AUR is the one place a name changes: its rules give the plain name
to a package built from source, so the release binaries are `norite-bin` and `norite-server-bin`, and
`norite` and `norite-server` build from the tag.

**Identity.** Vendor and publisher: **Norite**. Maintainer: **Alexnex31 <norite.chat@gmail.com>**. The
copyright file names Alexandre Duffez, as every source file's SPDX header does. winget's identifiers are
`Norite.Norite` and `Norite.NoriteServer`.

### 2. What ships now, and what waits
**Now**, from this branch, starting with the first release after it lands (§7):

| Channel | Package names | How a user gets it |
| --- | --- | --- |
| Our APT repository | `norite`, `norite-server` | add the repository once, then `apt install norite` |
| Our RPM repository | `norite`, `norite-server` | add the repository once, then `dnf install norite` (or `zypper`) |
| Our pacman repository | `norite`, `norite-server` | add the repository once, then `pacman -S norite` |
| The AUR | `norite-bin`, `norite-server-bin`; `norite`, `norite-server` from source | `yay -S norite-bin` or any AUR helper |
| Our Homebrew tap | casks `norite`, `norite-server` | `brew install alexnex31/tap/norite` |
| winget (`microsoft/winget-pkgs`) | `Norite.Norite`, `Norite.NoriteServer` | `winget install Norite.Norite` |
| Our Scoop bucket | `norite`, `norite-server` | `scoop bucket add norite …`, then `scoop install norite` |
| nixpkgs | `norite`, `norite-server` | a pull request, opened; its merge is the reviewers' |

winget's moderators approve each release's pull request, usually within days. That is review of a
manifest, not a decision about the project, and it is why winget is here and Snap is not.

**Later, each with a home:**
- **Snap, M37a.** Strict confinement is a port: it moves `$HOME`, so M3's and M21's directories move; it
  cuts the keyring off behind a plug connected by hand; a strict snap cannot write a systemd user unit;
  and it puts an `audio-record` gate in front of the microphone, which ADR 0034 records as the one Linux
  case with a gate. Classic confinement runs the client as it is, and needs the Store's manual approval.
  M37a requests classic for the client once Phase E's worker exists, so the request is judged on what
  will ship. **Both names are registered now**, so nobody else takes them.
- **Flatpak, M78a.** Flathub takes graphical applications; a command tree and a daemon are not one. The
  GUI is M78, and Flatpak's sandbox asks the questions Snap's does.
- **A distribution's own repository: no milestone until its condition holds**, and then a suffixed one at
  that point.
  - **Arch `extra`**: an Arch Package Maintainer adopts a package, usually one popular on the AUR. The AUR
    source packages are what would be adopted. Rolling, so the MINOR rule does not bite. The likeliest
    first.
  - **homebrew-core**: built from source, a stable (non-pre-release) version, and GitHub notability the
    project does not have yet; a formula submitted by its own author is held to a higher bar. Rolling.
  - **Debian, Ubuntu, Fedora**: a sponsor in each project, built from source, and Debian's Go policy
    packages each dependency separately (Fedora now permits vendoring; confirm before relying on it).
    **Not before the handshake tolerates an older MINOR**, which ADR 0033 gives MAJOR ≥ 1. Until then a
    stable release of these distributions freezes a client that stops connecting.
  - **Scoop's main bucket, nixpkgs merged**: whenever their reviewers accept; nothing to schedule.

### 3. Installing sets up no service, and removing undoes what it can
**A package installs files and starts nothing.** M23's rule, and three reasons it is also the only design
that works:
- a package's scripts run as root on Linux, and root cannot reach a user's systemd manager;
- the alternative, `systemctl --global enable`, starts a daemon for every account on the machine;
- Arch, Homebrew, winget and Scoop do not start services from a package, and Debian does it for
  plumbing every user needs (PipeWire), which a chat client is not.

The first `norite login` on a terminal already offers to install the service (M23). That is the one path
on every channel.

**Removal does not run `norite daemon uninstall` on any channel**, and says to run it first. A removal
step could only do it where the manager runs it as the user *and* tells a removal from an upgrade, since an
upgrade that removed the service would leave every upgraded machine without its daemon at the next login.
No channel offers both through what publishes it:
- **Homebrew** runs a cask's uninstall steps on `brew upgrade` as well, with nothing telling the two apart.
  The cask's caveats carry the instruction.
- **Scoop**'s own uninstaller is told which it is running for, but GoReleaser's Scoop manifest has no field
  for one. The install guide carries the instruction.
- **apt, dnf and pacman** run removal as root, which cannot reach a user's systemd manager. Removal (never
  an upgrade) prints the two commands that remove an account's unit, since `norite` itself is gone by the
  time anybody reads them.
- **winget**'s portable packages have no hook. The install guide carries the instruction.

A service left behind does not start. The systemd unit carries `ConditionFileIsExecutable=` naming the
daemon, so a missing program is skipped; without it, `Restart=on-failure` at five-second intervals never
reaches systemd's default start limit and retries for ever. launchd has no condition, and retries quietly
every thirty seconds (the plist's `ThrottleInterval`) until the agent is removed. A unit installed before
this change has no condition until `norite daemon install` writes it again. A binary deleted by hand is
not handled beyond that.

### 4. The package manager updates what it installed
**A package install never writes a binary.** The daemon still checks for a release and says one is out,
and names the command that installs it:

| Installed by | The daemon says |
| --- | --- |
| our APT repository | `sudo apt update && sudo apt upgrade` |
| our RPM repository | `sudo dnf upgrade norite` (`zypper update norite` on openSUSE) |
| our pacman repository | `sudo pacman -Syu` |
| the AUR | your AUR helper, e.g. `yay -Syu` |
| Homebrew | `brew upgrade norite` |
| Scoop | `scoop update norite` |
| winget | `winget upgrade Norite.Norite` |
| Nix | the Nix configuration that installed it |

**How the daemon knows.** One binary goes into every Linux format, so it cannot know from itself. Each
package that can install a file installs a marker naming its manager: `/usr/lib/norite/packaged-by` from
the `.deb`, the `.rpm`, the pacman package and both AUR packages, and a file beside the binaries from
Scoop's post-install step. Homebrew, winget and Nix cannot run such a step for a portable binary; the
daemon recognises them by the directory each installs into (`/Caskroom/`, `\Microsoft\WinGet\Packages\`,
`/nix/store/`). A receipt naming the running daemon still wins: an installer-script install is the
updater's whatever else is on the machine.

**After an upgrade the daemon starts the new version itself.** A package manager replaces the file and
leaves the process alone, so the daemon would run the old version until the next login, saying all the
while that a release is out which is already installed. A packaged daemon looks at the path it was started
by once a minute; when another program is there that runs and reports a different version, it ends its run
as an update does and `exec`s that program, keeping its process id
(`daemon/internal/updater/replaced.go`). A reinstall of the same version is not restarted for, a file not
yet runnable is looked at again, and a voice session puts it off. Only a packaged daemon watches: the
updater restarts its own installs, and a development build is replaced by somebody who did not ask for a
restart. Walked in a Debian container from a `.deb` of 0.1.1 to one of 0.1.2: the same process was 0.1.2
56 seconds later. **Not on Windows**, where a running program's file cannot be replaced in place; whether
winget and Scoop stop, refuse or wait for a running daemon is unverified, and their guide says to run
`norite daemon stop` before upgrading until it is.

**A service must name a path the manager keeps.** `norite daemon install` resolved the daemon through
links, so under Homebrew it recorded the version's own directory, which `brew upgrade` deletes. It now
uses the daemon beside the link when there is one, and resolves only when there is not.

### 5. Who holds the keys
**The APT, RPM and pacman repositories are signed with one GPG key, and that key never enters the Norite
repository.** Two pipelines:

1. **`release.yml`** (GoReleaser) builds the `.deb`, `.rpm` and `.pkg.tar.zst` packages as release
   assets. `checksums.txt` lists them, so the existing cosign signature covers them. It also pushes the
   tap, the bucket, the AUR packages and the winget pull request.
2. **`Alexnex31/packages`**, a separate repository served by GitHub Pages at
   `https://alexnex31.github.io/packages`, has its own workflow. It reads Norite's releases, **verifies
   the cosign bundle against `release.yml@refs/tags/<tag>` and every package against `checksums.txt`**,
   then signs: `InRelease` and `Release.gpg` for apt, each package and `repomd.xml` for rpm, a detached
   signature per package and the database for pacman. It deploys the site from the workflow, so no
   package is ever committed and the repository's history does not grow with them. It keeps the two
   newest versions.

It runs on a schedule, polling Norite's releases, and by hand. **Never on a pull request**: the
repository is public, and a pull-request run from a fork must never reach the signing key. Polling means
Norite holds no token for it; the cost is up to one interval between a release and its repositories.

**The key.** Generated on the author's machine. RSA 4096 rather than Ed25519, because some rpm still in
service verifies RSA and not EdDSA. The primary key stays offline, with a backup; a signing subkey with
an expiry is the packages repository's only secret, extended before it lapses and replaced through the
primary. Users import the public key once. If the subkey leaks, a new one is issued and the old revoked;
if the primary leaks, every user re-imports.

**What the other channels need**, each a separate credential with one reach:

| Channel | Credential | Where it lives |
| --- | --- | --- |
| Homebrew tap (`Alexnex31/homebrew-tap`) | a fine-grained token, contents write on that repository only | `release.yml` secret |
| Scoop bucket (`Alexnex31/scoop-bucket`) | the same shape, that repository only | `release.yml` secret |
| winget | a token able to push to the author's fork of `winget-pkgs` and open a pull request | `release.yml` secret |
| AUR | an SSH key registered to the author's AUR account | `release.yml` secret |
| Snap Store (M37a) | Store credentials | not yet |

**A channel whose secret is not set is skipped**, not failed: each publisher's `skip_upload` is true
unless its secret is in the environment, so a missing one cannot fail a release after GitHub's half is
already published, and each channel goes live when its secret is added.

These sit in the workflow beside cosign's OIDC identity. The four tokens can rewrite what the tap,
bucket, fork and AUR packages say, never a release's signed files; a GitHub environment requiring the
author's approval per release narrows that further, at one click per release.

**Every repository is public.** The tap and the bucket must be, or every user configures a token; GitHub
Pages needs a public repository on a free plan; a fork of a public repository is public; the AUR and
nixpkgs are. None holds a secret: secrets are Actions secrets.

### 6. macOS
The cask installs the two binaries Homebrew has checked against the SHA-256 the cask names, then
**removes the quarantine attribute from those two binaries**. Without it, macOS refuses an unsigned,
quarantined program even from a terminal. `install.sh` downloads with curl, which sets no quarantine, so
this is the installer's state reached through another tool, not a weaker one.

Homebrew 5 deprecated `--no-quarantine` and disabled the official cask repository's casks that fail
Gatekeeper from September 2026. Its notes name only `Homebrew/homebrew-cask`; whether a tap's cask may
still remove quarantine is checked on a real Mac before the tap is announced. If it may not, the
fallback is a formula in the tap, which Homebrew downloads without quarantine. A Developer ID (M32) ends
the question, and every new ad-hoc daemon build re-prompting for the microphone (ADR 0034) with it.

### 7. Versions and addresses
- **The first packaged release is v0.1.1.** GoReleaser packages what it builds, and v0.1.0 cannot be
  rebuilt into the files its signature covers. Cutting it stays the author's step.
- **Pre-releases are not packaged.** Alphas and release candidates stay on GitHub and the installer
  scripts.
- **The repositories live at `https://alexnex31.github.io/packages`, with no domain of the project's
  own.** That address goes into every user's apt, dnf and pacman configuration, so moving it later
  breaks updates for every configured machine until its owner edits one line. Accepted, since 0.x
  already allows breaking changes; a domain, if one is ever bought, is the time to move, and the move
  is announced in a release before the old address stops.
- **GitHub Pages' limits** are 1 GB per site and a soft 100 GB of transfer a month. Two versions of every
  Linux package are about 270 MB (a client package is 13.5 MB and a server one 8.7 MB, in three formats
  and two architectures). Reopen when either limit comes within reach.

## Consequences
- `.goreleaser.yaml` grows nFPM packages, a cask pair, a Scoop pair, a winget pair and four AUR packages,
  with a source archive for the AUR's source builds. CLAUDE.md's rule that the installers restate what a
  release publishes now covers the package definitions too: an archive name, a binary or a path changed
  in one changes the others in the same commit.
- `release.yml` gains four secrets, and `just release-dry-run` builds every package without publishing and
  checks each client package carries both programs and its marker.
- A release has 27 files where it had 14, so its entry in GitHub's listing roughly doubles, to about
  60 KB. The updater reads at most 32 MiB of listing (M24), which a hundred such releases stay well under.
- `daemonctl` records the link a package manager keeps, and the systemd unit skips a program that is gone;
  the updater reads the marker and the path.
- `norite-server` is installable without being set up, which is what `install-server.sh` did not offer.
- macOS is the channel with an open question, and it is answered on a Mac before the tap is announced.
- Under Nix, `norite daemon install` records a store path, which an upgrade does not replace: the service
  runs the old version until it is installed again or garbage-collected away. Nix users who manage
  services declaratively are better served by their own service definition; the nixpkgs draft says so.
- Every package channel is something the author maintains. A channel nobody uses can be dropped; one
  somebody configured is a promise.

## Alternatives considered
- **GoReleaser's config as Gemini sketched it**: `brews` (deprecated since v2.10 for `homebrew_casks`),
  `nfpms.builds` and `snapcrafts.builds` (renamed `ids` at v2.8), a strict snap published straight to the
  Store, and the GPG key, had there been one, in `release.yml`. It would have produced packages, and two
  of them (Snap, the key) would have been wrong in ways no build fails on.
- **Hosted repositories (Cloudsmith, Gemfury, packagecloud)**: less to run, and a company in the supply
  chain holding the signing key. Declined while GitHub, already the root of trust for `release.yml`, can
  serve them.
- **OBS, Copr, Launchpad PPAs**: they build from source and sign with their own keys, and each covers part
  of the Linux list. They are the shape a distribution's own repository wants, and are revisited there.
- **Packaging inside `release.yml` with the GPG key beside cosign's identity**: one pipeline instead of
  two, and a long-lived key in the one workflow that publishes. The second pipeline is the cost of keeping
  them apart.
- **Setting the daemon up at install**: rejected in §3.
- **Letting the updater replace a packaged binary**: two updaters on one file, and a package manager that
  no longer knows what it installed.
