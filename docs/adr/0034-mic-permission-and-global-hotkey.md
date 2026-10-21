# ADR 0034: Who opens the microphone and who holds push-to-talk, per OS (M25's finding)

## Status
Accepted at M25 (2026-10). **Amends [ADR 0012](0012-voice-in-v1.md)** in one place: its consequence that
the mic-permission handoff and the daemon's global hotkey are "unverified per-OS behavior until a
dedicated throwaway spike" is answered here. The answers choose the shapes of M29 (capture), M32 (the
permission) and M35 (push-to-talk), and `architecture.md` §6 and §17 now point here.

Each answer says how it was found: **run** (a program was run and its output read) or **read** (from a
library's source, or a vendor's documentation or forum). The throwaway programs are not in the repository,
which is what the entry asked for.

## Context
The design until M25 rested on two sentences in `architecture.md` §6:

- "The foreground TUI/GUI triggers the OS permission prompt on first voice use, then hands capture to the
  worker once granted."
- "Push-to-talk (`golang.design/x/hotkey`) is registered once by the daemon."

Both were stated as unverified, and M25 exists to verify them before M28–M35 are designed further. Planning
read them against the code and the library and found the second could not work as written, whatever the OS
did:

- **The library needs cgo on Linux and macOS, and the daemon has none.** Built with `CGO_ENABLED=0`, as
  `.goreleaser.yaml` builds the daemon for all three OSes, a program using `golang.design/x/hotkey` v0.6.4
  compiles for Windows and **fails to compile** for Linux and macOS: the key constants live in its cgo
  files (run).
- **On Linux it is X11 only**, and the commonest Linux desktops are Wayland (read; the X11 half is measured
  below).
- **On macOS it uses a `CGEventTap`**, gated on Accessibility (read, from its source). The documents named
  Input Monitoring.

## What was found

### Linux

**Mic: yes, with no prompt, but only while the user has a seat session.**
- A process in a systemd user service, the daemon's shape, captured real signal with `pw-record` on the
  author's Hyprland machine (2.45 s, RMS 146): no prompt and no permission (run). Outside Flatpak and Snap,
  PipeWire has no per-application gate.
- In a Fedora 44 KDE VM, after the graphical session ended, logind dropped the user's ACL on `/dev/snd`
  and PipeWire listed no sources: "no target node available" (run). With `--linger` (M23) the daemon starts
  at boot, before anybody logs in, and **cannot capture until a session exists**. After login it can.
- So: the worker opens the device itself. There is nothing to hand over and nothing to prompt.

**Global hotkey: yes, through the GlobalShortcuts portal on Wayland, and an X11 grab on X11. Both in pure
Go, and only once a session exists.**
- **Wayland**, `org.freedesktop.portal.GlobalShortcuts` over D-Bus (`godbus/dbus`, no cgo), run from a
  systemd user service with another application in front:
  - On Hyprland 0.56 (physical keys) and KDE Plasma 6.6 (the VM's keyboard), a tap and a hold each
    arrived as exactly one press and one release: 1.78 s for a physical hold, 1.500 s for a 1.5 s
    synthetic one. That is push-to-talk's shape, with no autorepeat to filter (run).
  - **A host program must have an app id**: without one the portal answers "An app id is required"; with
    `Registry.Register` it answers "App info not found" until a `.desktop` file for that id is installed
    (run on both). The installers have to write one.
  - **The user chooses the key, in a way each desktop decides.** KDE shows a "Global Shortcuts Requested"
    dialog naming the application, honors the suggested key, stores the choice, and binds again silently
    after a restart. Hyprland ignores the suggestion; the user writes a `global` bind in their config. A
    daemon restarted by M24's updater re-registers without a duplicate on either (run).
  - **Before login the portal does not exist**: registration fails ("Could not activate remote peer"). A
    client retrying every 3 s bound about 6 s after login, from a process started before it, whose
    environment never had `WAYLAND_DISPLAY` (run). The first press after that login arrived collapsed,
    with press and release at the same instant; every later one was exact (run, KDE, once).
- **X11**, a passive grab on the root window, pure Go (`jezek/xgb`, no cgo), under Xvfb:
  - With another window focused, a tap and a hold each gave one press and one release, after filtering X
    autorepeat (a release and a press sharing one timestamp) (run).
  - A second client grabbing the same combination is refused, because grabs are exclusive (run). The
    library's own X11 loop counts every key that reaches the root window as the hotkey, and reports a
    held key as press and release every 40 ms (run).
- **X11 through XWayland is not a path.** Under KWin, with a Wayland window in front, 5 presses sent gave
  4 pairs whose times did not match the sends, and one 1.5 s hold was lost outright (run).

### Windows
Run in a Windows 11 Pro 26H2 VM (build 26300), from a task shaped exactly like Norite's logon task:
`conhost.exe --headless`, an interactive token, least privilege, and the program under `%LOCALAPPDATA%`.
Keys came through the VM's keyboard with Notepad in front. The mic input was a 440 Hz tone from the host.

**Mic: yes, with no prompt. One device-wide switch decides, and Windows attributes each use to the exe
that opens the device.**
- The probe opened the default input through `waveIn`, pure Go, and captured the tone (RMS 5186). No
  prompt, no notification (run).
- Windows recorded the use against the capturing program's **full path**, and Settings lists it by file
  name under "desktop apps that have previously accessed your microphone", with no switch of its own
  (run).
- With "Let desktop apps access your microphone" turned off in Settings, the open itself is refused
  (`MMSYSERR_ERROR`), again with no prompt and no notification. Turned back on, capture resumed (run).
  Writing that setting's registry value directly changed nothing; only a change made through Settings is
  enforced, which matters to anyone testing this later (run).
- **A per-app prompt is coming and is not here yet.** Insider Experimental builds since 26340 (August 2026)
  show a dialog the first time a desktop program uses the microphone, with per-app switches, and no
  release date is announced (read: Microsoft's announcement as reported by BleepingComputer and Windows
  Latest). If it ships as tested, the dialog will name the program that opens the device. That is the
  worker, so a grant to a client would not cover it, which rules out the handover design here too.

**Global hotkey: yes, from the logon task, and only from an interactive session.**
- `RegisterHotKey` with a message loop on a locked thread (the hotkey library's Windows half, which is
  plain `syscall` and builds without cgo). With Notepad in front, a tap and holds of 1.5 s and 3 s came
  back as 0.090, 1.501 and 3.002 s, release detected by polling `GetAsyncKeyState`. Notepad received
  nothing (run).
- A second registration of the same combination is refused, "Hot key is already registered" (run). As on
  X11, the key is exclusive and swallowed everywhere while it is held.
- From the SSH session, a service's window station, registration fails: "This operation requires an
  interactive window station" (run). Norite's daemon is never a service (M3), and this is one more reason
  it must not become one.

### macOS
Run on the author's Mac, macOS 27.0 on Apple silicon, as two short scripted passes. The probes were pure
Go (`ebitengine/purego`, no cgo), cross-built here and ad-hoc signed by Go's linker, as Norite's own darwin
binaries are. They ran as LaunchAgents in the user's session, which is how `norite daemon install` runs
the daemon on macOS, with another application in front for the hotkey.

**Mic: yes, after one prompt, and the prompt and the grant belong to the daemon, not the worker.**
- From Terminal, the prompt read "**Terminal** wants access to the microphone", and the grant went to
  Terminal (run). This is the original design's failure exactly: a TUI asking would put the permission on
  the terminal emulator, where the worker never benefits from it.
- **A LaunchAgent can prompt**, as a bare binary with no `Info.plist` and no usage string. The prompt names
  its file (run).
- **A LaunchAgent's child is charged to its parent.** With a daemon named `norite-daemon` starting a
  separate `norite-worker` that captured:
  - the prompt read "**norite-daemon**";
  - TCC logged the worker as requesting and the daemon as responsible, and stored the grant against the
    daemon's path;
  - asked alone afterwards, the daemon was authorized and the worker was not (run).

  `norite-daemon` is the release's real binary name, so it is the name users will see.
- **A grant is tied to the build, not the path.** Replacing the worker with a different build at the same
  path asked nothing. Replacing the daemon did: TCC "failed to match existing code requirement" and
  prompted again (run). An ad-hoc signature's identity is its code hash, so **every new daemon build is
  asked again**. A Developer ID signature gives a stable identity that survives updates (read), and
  Norite's releases carry cosign signatures only. Since M24, macOS checks for updates and does not install
  them, so for now this is one prompt per manual upgrade rather than per automatic one.
- **Samples arrived while the prompt was still open**, and the worker's own status read "not determined"
  before and after (run). Whether those samples were real input was not established. Data arriving is
  not proof of permission, in either direction.
- Inside a minimal `.app` bundle with a usage description, the prompt names the bundle instead (run).
  That is the shape to reach for if the file name is not good enough.

**Global hotkey: yes, through Carbon's `RegisterEventHotKey`, with no permission at all.**
- From a LaunchAgent, with Accessibility and Input Monitoring both ungranted, Control+Shift+P registered.
  With another application in front, a tap and a 2 s hold came back as one press and one release each:
  0.078 s and 2.005 s (run). macOS asked for nothing.
- The library M35 named uses a `CGEventTap` instead. A listen-only tap from the same LaunchAgent was
  refused without Input Monitoring (run), so that route needs a permission and Carbon does not.
- Carbon's event dispatch needs an application run loop on the process's main thread. The probe ran
  `NSApplication` in "prohibited" activation mode (no Dock icon, no menu bar) through `purego` (run).
- **Option-only combinations**: macOS 15 is reported to refuse a hotkey whose only modifiers are Option or
  Shift+Option (read: Apple Developer Forums, thread 763878). Option+P registered without error on macOS 27
  in an unsandboxed agent, but was not pressed (run). A default containing Control avoids the question.
- Each LaunchAgent added raised macOS's "can run in the background" notification, naming the program's
  file (run). Norite's daemon raises it at install today; it says `norite-daemon`.

## Decision

### 1. The worker opens the microphone itself, and no client is involved
The voice-worker, the process that captures (rule 11), opens the device. No attach client opens the
microphone, to trigger a prompt or for any other reason, so the design §6 carried until M25 ("the
foreground client triggers the prompt, then hands capture to the worker") is withdrawn.

Who the operating system asks about follows from that, differently on each OS:
- **Linux**: nobody. There is no per-program gate outside Flatpak and Snap.
- **Windows**: the worker's exe, by path, in the switch it shows today and in the prompt it is testing.
- **macOS**: **the daemon.** The worker is the daemon's child, and TCC charges a child to its parent, a
  LaunchAgent being its own responsible process. The prompt reads "norite-daemon", and the grant survives
  a new worker build and not a new daemon build. The worker must not disclaim that responsibility: the
  daemon's identity is the stable one of the two, and it is the program the user installed.

What the clients do instead is **explain**. The worker reports the outcome of opening the device, and the
daemon carries it as call state over the attach socket (M34). The outcomes:
- capturing;
- waiting on the operating system's question;
- refused, with where the user changes it:
  - Windows: "Let desktop apps access your microphone";
  - macOS: System Settings → Privacy & Security → Microphone → `norite-daemon`;
- no input device;
- on Linux, no session: no device is available until somebody logs in at the machine.

On macOS the worker asks for the outcome explicitly, through AVFoundation's access request and its
answer. It does not infer the outcome from samples arriving or from a status read once: both misled the
probe (run). A refusal is never retried in a loop. The user acts, and the next join tries again.

### 2. Push-to-talk is registered by the daemon, in pure Go, per OS, and not through `golang.design/x/hotkey`
The daemon stays cgo-free; the library cannot build into it on two of three OSes. Each OS gets its own
mechanism, every one of which was run without cgo:

| OS | Mechanism | Who chooses the key |
| --- | --- | --- |
| Windows | `RegisterHotKey` on a locked thread with its own message loop; release by polling `GetAsyncKeyState` | Norite's config |
| Linux, Wayland, or any desktop offering it | the `GlobalShortcuts` portal over D-Bus (`godbus/dbus`); `Activated` and `Deactivated` are press and release | the desktop and the user; Norite only suggests |
| Linux, X11 with no portal | a passive grab on the root window (`jezek/xgb`), filtering autorepeat by timestamp | Norite's config |
| macOS | Carbon's `RegisterEventHotKey`, through `ebitengine/purego`, with an `NSApplication` run loop on the daemon's main thread in "prohibited" activation mode; press and release are Carbon events | Norite's config, with Control or Command in it |

The portal is preferred wherever it answers, because it is the only one of these the desktop knows about.
X11 through XWayland is never used: it loses and reorders presses.

The daemon rather than the worker, because the daemon already holds call state (M34) and the worker is
kept to media (rule 11). The press travels to the worker over the existing pipe (M28). Every mechanism
above is pure Go, so cgo is not a reason to move it into the worker.

**No mechanism here needs a permission.** macOS's Carbon route was run with Accessibility and Input
Monitoring both ungranted, which is why the event-tap route the documents assumed, and its Input
Monitoring prompt, is not used.

**On macOS this changes how the daemon starts.** Carbon dispatches only through an application run loop
on the main thread, so the daemon's `main` gives its main thread to that loop and runs everything else as
it does today, on other goroutines.

### 3. The key is held only during a call
On Windows and X11 the key is exclusive and swallowed system-wide while registered (run on both). Held
all day, Norite would take that combination from every other program whether or not a call exists. The
daemon registers at join and releases at leave. On the portal the binding persists in the desktop; the
session is opened at join and closed at leave, and after the first time, binding again is silent (KDE,
run).

That also settles M35's "does not double-register": there is one daemon per account, and it registers once
per call, whichever clients are attached. A second registration would be refused by the OS anyway (run on
Windows and X11).

### 4. Registration and capture wait for a session, and failing is reported
Neither the portal nor the audio devices exist before somebody logs in (run on Linux). A daemon started at
boot under `--linger` therefore does nothing about either at start. Join fails with the "no session"
outcome above, and the hotkey's absence is reported in the call state, never fatal to the call.
Voice-activity detection, the default input mode, needs no hotkey at all.

### 5. Linux needs an app id, so the installers write one
A program outside a sandbox cannot use the portal without an app id, and cannot register one without an
installed `.desktop` file for it (run). `install.sh` writes `~/.local/share/applications/<app id>.desktop`
with `NoDisplay=true` and removes it on uninstall; the daemon calls `Registry.Register` with that id. The
id is chosen at M35.

## Consequences
- **M29** chooses the capture library. "`hraban/opus` capture" named a codec, which captures nothing.
  Whatever it chooses must open the device in the worker's own process, and its cgo means the worker's
  release builds cannot be cross-compiled from Linux for macOS the way every other binary is. That is
  M28's or M29's release problem, recorded there.
- **M32** builds §1: the outcomes, their wording on each client, and a done-when per OS.
  - Linux: capture works, and the before-login case says so.
  - Windows: capture works; with the switch off, the client names the switch.
  - macOS: the prompt names `norite-daemon` and comes once per daemon build. A refusal names the setting.
  - **Whether releases get an Apple Developer ID is the author's decision, and M32 carries it.** Without
    one, every new daemon build is asked again (run), so "prompts exactly once" holds per build and not
    per install. A Developer ID costs a paid Apple account and changes the release pipeline. Since M24
    does not install updates on macOS, that cost buys a prompt per manual upgrade, not per automatic one.
- **M35** builds §2–§5 and drops `golang.design/x/hotkey`. Its done-when gains the portal path on at
  least one Wayland desktop and the X11 path, and on macOS the Carbon path from the installed LaunchAgent,
  with no permission granted.
- **The terminal cannot do push-to-talk by itself.** KEYMAP's `C-space (hold)` assumes a release event,
  and a terminal reports none without the kitty keyboard protocol's event types. `C-space` is also NUL in
  most terminals, and the input-source switch on macOS. In-terminal push-to-talk is M35's and M44's to
  settle: kitty protocol release events where the terminal has them, a toggle where it does not. The
  global hotkey is the push-to-talk path.
- **Three new daemon dependencies at M35**, all permissive and all fine under rule 22's client allow-list:
  `godbus/dbus` (BSD-2-Clause), `jezek/xgb` (BSD-3-Clause) and `ebitengine/purego` (Apache-2.0).
- **A Windows service could never hold the key**, a fourth reason after M3's for the daemon staying a
  logon task.
- **macOS already names the daemon.** Installing its LaunchAgent raises a "can run in the background"
  notification naming `norite-daemon` (run). That has been true since M3 and nothing said so. If the
  file name is not good enough in either place, a minimal `.app` bundle around the daemon changes both
  (run, for the prompt).

## Alternatives considered
- **Keep `golang.design/x/hotkey` and turn cgo on for the daemon.** The release builds every OS on one
  Linux runner, and cgo for darwin does not cross-compile there without Apple's SDK. It would still be
  X11-only on Linux, and its X11 loop reports any key reaching the root window as the hotkey (run).
  Rejected.
- **Register push-to-talk in the worker**, which already has cgo and lives exactly as long as a call. It
  is attractive for the same "only during a call" reason as §3. But every mechanism is pure Go, so cgo
  buys nothing, and the worker is kept to media (rule 11). The daemon holding call state is where a press
  belongs.
- **Register at daemon start and hold the key all day.** Rejected by §3: on Windows and X11 that takes the
  combination from every other program.
- **X11 through XWayland on Wayland desktops**, since it works with no app id and no dialog. It lost and
  reordered presses under KWin (run). Rejected.
- **The foreground client triggers the prompt, the original design.** On Linux there is nothing to
  trigger; on Windows, the program that opens the device is the one asked about; on macOS, a TUI's
  prompt names the terminal emulator, and the grant stays there (run on all three). Withdrawn.
- **The worker disclaims its parent and owns its own grant on macOS.** That needs a private spawn
  attribute, and it moves the identity onto the binary that matters least to the user. The daemon is the
  program they installed and the one the background-items notice already names. Rejected.
- **A `CGEventTap` on macOS**, as `golang.design/x/hotkey` uses. It needs Input Monitoring, refused here
  without it (run), for nothing Carbon does not already give with no permission. Rejected.
