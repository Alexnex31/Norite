// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"context"
	"errors"
	"math/rand/v2"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/installreceipt"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Variables so that a test, and the one build that carries a test root (testroot.go), can run a day in a
// second. Nothing in a release build writes to them.
var (
	// checkEvery is how often the listing is read. firstCheck and checkJitter keep a daemon that has just
	// started, or a thousand that started together, from asking at once.
	checkEvery  = 24 * time.Hour
	firstCheck  = 2 * time.Minute
	checkJitter = 10 * time.Minute
	// retryStaged is how often a release that is ready and deferred is tried again.
	retryStaged = 5 * time.Minute
	// confirmAfter is how long a new version must stay up before it is kept.
	confirmAfter = 10 * time.Minute
)

// ErrRefused is a request understood and not carried out, with nothing changed. Its text is for a person.
type ErrRefused struct{ Msg string }

func (e *ErrRefused) Error() string { return e.Msg }

// Options configures an Updater.
type Options struct {
	// Version is the version running.
	Version string
	// StateDir is the daemon's state directory.
	StateDir string
	// Executable is the running daemon's own path, resolved.
	Executable string
	// Instance returns the version the signed-in instance reported, or "".
	Instance func() string
	// Busy reports that this is no moment to restart: a voice session is active (architecture.md §6).
	// Nil is never. Nothing sets it before Phase E.
	Busy func() bool
	// Restart ends the daemon's run so that Finish can install. It must not block.
	Restart func()
	// Changed is called when what Status answers has changed.
	Changed func()
	Log     zerolog.Logger

	// The seams. Zero values are the real ones.
	Source *Source
	Verify func(checksums, bundle []byte, tag string) error
	Now    func() time.Time
	GOOS   string
}

// Updater checks for a release, stages it, installs it when the daemon's run has ended, and undoes one
// that did not stay up.
type Updater struct {
	opts      Options
	installer *Installer
	current   gatewayproto.Version
	// installs is whether this daemon may write programs, and reason why not.
	installs bool
	reason   string

	mu sync.Mutex
	// staged is a release ready to install, and applying that Finish should install it.
	staged   *Staged
	applying bool
	// working is held by whichever of the loop and a request is checking or staging.
	working sync.Mutex
}

// New builds the Updater for a daemon. It reads the receipt and touches nothing.
func New(o Options) *Updater {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Source == nil {
		src := DefaultSource(o.Version)
		o.Source = &src
	}
	if o.Changed == nil {
		o.Changed = func() {}
	}
	u := &Updater{opts: o, installer: &Installer{Dir: filepath.Join(o.StateDir, "update"), GOOS: o.GOOS}}
	u.installs, u.reason = u.standing()
	return u
}

// standing decides whether this daemon installs releases itself.
func (u *Updater) standing() (bool, string) {
	current, err := gatewayproto.ParseVersion(u.opts.Version)
	if err != nil {
		return false, "this is a development build, which is not a release and does not update"
	}
	u.current = current
	receipt, err := installreceipt.Read(u.opts.StateDir)
	if err != nil {
		return false, "this install was not made by Norite's installer, so whatever installed it updates it"
	}
	u.installer.BinDir = receipt.BinDir
	if u.opts.Executable != filepath.Join(receipt.BinDir, daemonName+exeSuffix(u.opts.GOOS)) {
		return false, "this daemon is not the one the installer put in " + termsafe.Text(receipt.BinDir)
	}
	if u.opts.GOOS == "darwin" {
		return false, "on macOS Norite checks for a release and does not install one yet; run the installer again"
	}
	return true, ""
}

// AtStart is called once, with the lock held, before anything else runs. It reports whether the daemon
// must restart at once: the version running was undone and the previous one is back on disk.
func (u *Updater) AtStart(crashed bool) bool {
	// Only a daemon that installs releases has one to undo, and only the installed daemon may write over
	// the installed pair.
	if !u.installs {
		return false
	}
	outcome, err := u.installer.AtStart(u.current.String(), crashed)
	if err != nil {
		u.opts.Log.Error().Str("error", termsafe.Text(err.Error())).
			Msg("an update that did not stay up could not be undone; this version keeps running")
		return false
	}
	switch outcome {
	case RolledBack:
		u.opts.Log.Error().Str("version", u.current.String()).
			Msg("this version did not stay up after it was installed; the previous one is restored and starts now")
		return true
	case Recovered:
		u.opts.Log.Warn().Msg("an update was interrupted half-way and has been undone")
	case Counted:
		u.opts.Log.Info().Str("version", u.current.String()).
			Msg("running a newly installed version; it is kept once it has stayed up")
	}
	return false
}

// Run checks on a schedule and confirms a version on trial, until ctx ends.
func (u *Updater) Run(ctx context.Context) {
	confirm := time.NewTimer(confirmAfter)
	defer confirm.Stop()
	next := time.NewTimer(u.untilFirstCheck())
	defer next.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-confirm.C:
			if err := u.installer.Confirm(); err != nil {
				u.opts.Log.Warn().Str("error", termsafe.Text(err.Error())).Msg("could not record that this version stayed up")
			}
			u.opts.Changed()
		case <-next.C:
			wait := checkEvery + jitter()
			if u.mode() != ipc.UpdateOff {
				if u.scheduled(ctx) {
					// Ready and deferred: tried again soon, without asking the listing again.
					wait = retryStaged
				}
			}
			next.Reset(wait)
		}
	}
}

// untilFirstCheck is how long after starting the listing is first read: soon, unless it was read lately.
func (u *Updater) untilFirstCheck() time.Duration {
	wait := firstCheck + jitter()
	if last := u.installer.Load().LastCheck; !last.IsZero() {
		if since := u.opts.Now().Sub(last); since >= 0 && since < checkEvery {
			wait = max(wait, checkEvery-since)
		}
	}
	return wait
}

// scheduled is one turn of the loop. It reports whether a release is staged and waiting for a better
// moment.
func (u *Updater) scheduled(ctx context.Context) bool {
	u.working.Lock()
	defer u.working.Unlock()

	u.mu.Lock()
	staged := u.staged
	u.mu.Unlock()
	if staged == nil {
		release, found, err := u.check(ctx)
		if err != nil || !found || u.mode() != ipc.UpdateAuto || !u.installs {
			return false
		}
		if err := u.stage(ctx, release); err != nil {
			return false
		}
	}
	if u.opts.Busy != nil && u.opts.Busy() {
		return true
	}
	u.begin()
	return false
}

// check reads the listing and chooses. What it finds, or why it could not, goes into the record.
func (u *Updater) check(ctx context.Context) (Release, bool, error) {
	if _, err := gatewayproto.ParseVersion(u.opts.Version); err != nil {
		return Release{}, false, &ErrRefused{u.reason}
	}
	record := u.installer.Load()
	releases, err := u.opts.Source.Releases(ctx)
	now := u.opts.Now()
	if err != nil {
		u.opts.Log.Warn().Str("error", termsafe.Text(err.Error())).Msg("could not check for a new release")
		_ = u.installer.Update(func(r *Record) { r.LastCheck, r.LastError = now, termsafe.Text(err.Error()) })
		u.opts.Changed()
		return Release{}, false, err
	}
	instance := ""
	if u.opts.Instance != nil {
		instance = u.opts.Instance()
	}
	release, found := Choose(Policy{Current: u.current, Instance: instance,
		Prereleases: record.Channel == ipc.ChannelPrerelease, Bad: record.Bad}, releases)
	available := ""
	if found {
		available = release.Version.String()
		if available != record.Available {
			u.opts.Log.Info().Str("version", available).Msg("a newer release is available")
		}
	}
	_ = u.installer.Update(func(r *Record) { r.LastCheck, r.LastError, r.Available = now, "", available })
	u.opts.Changed()
	return release, found, nil
}

// stage downloads and verifies a release and keeps it ready.
func (u *Updater) stage(ctx context.Context, release Release) error {
	verify := u.opts.Verify
	if verify == nil {
		v, err := NewVerifier()
		if err != nil {
			return u.failed(release, err)
		}
		verify = v.Verify
	}
	stager := &Stager{Source: *u.opts.Source, Verify: verify, Dir: filepath.Join(u.installer.Dir, "staged"),
		GOOS: u.opts.GOOS, GOARCH: runtime.GOARCH}
	staged, err := stager.Stage(ctx, release)
	if err != nil {
		return u.failed(release, err)
	}
	u.opts.Log.Info().Str("version", release.Version.String()).Msg("a release is downloaded and verified")
	u.mu.Lock()
	u.staged = &staged
	u.mu.Unlock()
	return nil
}

// failed records why a release was not staged. The current version goes on running (ADR 0020).
func (u *Updater) failed(release Release, err error) error {
	level := u.opts.Log.Warn()
	if errors.Is(err, ErrNotVerified) {
		level = u.opts.Log.Error()
	}
	level.Str("version", release.Version.String()).Str("error", termsafe.Text(err.Error())).
		Msg("a release was not installed; this version keeps running")
	_ = u.installer.Update(func(r *Record) { r.LastError = termsafe.Text(err.Error()) })
	u.opts.Changed()
	return err
}

// begin asks the daemon to end its run, so that Finish installs what is staged.
func (u *Updater) begin() {
	u.mu.Lock()
	ready := u.staged != nil && !u.applying
	version := ""
	if ready {
		u.applying = true
		version = u.staged.Release.Version.String()
	}
	u.mu.Unlock()
	if ready {
		u.opts.Log.Info().Str("version", version).Msg("stopping to install a release")
		u.opts.Restart()
	}
}

// Finish is called once the daemon's run has returned, with its lock still held. It installs a release
// the run ended for and reports whether the process must now be replaced by the program on disk.
//
// It reports true whenever the run ended to install, including when the installation failed: the run has
// ended either way, and a daemon that stopped to update and then merely exited would stay stopped.
func (u *Updater) Finish() bool {
	u.mu.Lock()
	staged, applying := u.staged, u.applying
	u.mu.Unlock()
	if !applying || staged == nil {
		u.installer.CleanExit()
		return false
	}
	if err := u.installer.Install(*staged, u.current.String()); err != nil {
		u.opts.Log.Error().Str("error", termsafe.Text(err.Error())).
			Msg("the release could not be installed; the version that was running starts again")
		_ = u.installer.Update(func(r *Record) { r.LastError = termsafe.Text(err.Error()) })
		return true
	}
	u.opts.Log.Info().Str("from", u.current.String()).Str("to", staged.Release.Version.String()).
		Msg("a release is installed; starting it")
	return true
}

func (u *Updater) mode() string {
	return u.modeOf(u.installer.Load())
}

// modeOf is the mode in force. Unset, it is auto where this daemon installs and notify where it does not:
// an install nobody's installer made has never been told it would be changed.
func (u *Updater) modeOf(r Record) string {
	switch r.Mode {
	case ipc.UpdateAuto, ipc.UpdateNotify, ipc.UpdateOff:
		return r.Mode
	}
	if u.installs {
		return ipc.UpdateAuto
	}
	return ipc.UpdateNotify
}

// Status reports where updating stands.
func (u *Updater) Status() ipc.UpdateStatus {
	r := u.installer.Load()
	s := ipc.UpdateStatus{Version: u.opts.Version, Mode: u.modeOf(r), Channel: ipc.ChannelStable,
		Installs: u.installs, Reason: u.reason, Available: r.Available, RolledBack: r.RolledBack, LastError: r.LastError}
	if r.Channel == ipc.ChannelPrerelease {
		s.Channel = ipc.ChannelPrerelease
	}
	if r.Pending != nil && r.Pending.Phase == phaseApplied {
		s.Trial = r.Pending.To
	}
	if !r.LastCheck.IsZero() {
		s.LastCheck = r.LastCheck.UTC().Format(time.RFC3339)
	}
	return s
}

// Check reads the listing now, whatever the mode: somebody asked.
func (u *Updater) Check(ctx context.Context) (ipc.UpdateStatus, error) {
	u.working.Lock()
	defer u.working.Unlock()
	if _, _, err := u.check(ctx); err != nil {
		var refused *ErrRefused
		if errors.As(err, &refused) {
			return u.Status(), err
		}
		return u.Status(), &ErrRefused{"the release listing could not be read: " + termsafe.Text(err.Error())}
	}
	return u.Status(), nil
}

// Apply checks, stages and begins installing, because somebody asked. The daemon restarts after the
// answer has left.
func (u *Updater) Apply(ctx context.Context) (ipc.UpdateStatus, error) {
	u.working.Lock()
	defer u.working.Unlock()
	if !u.installs {
		return u.Status(), &ErrRefused{u.reason}
	}
	u.mu.Lock()
	staged := u.staged
	u.mu.Unlock()
	if staged == nil {
		release, found, err := u.check(ctx)
		if err != nil {
			return u.Status(), &ErrRefused{"the release listing could not be read: " + termsafe.Text(err.Error())}
		}
		if !found {
			return u.Status(), &ErrRefused{"there is no newer release this install can move to"}
		}
		if err := u.stage(ctx, release); err != nil {
			return u.Status(), &ErrRefused{"the release was not installed, and this version keeps running: " +
				termsafe.Text(err.Error())}
		}
	}
	if u.opts.Busy != nil && u.opts.Busy() {
		return u.Status(), &ErrRefused{"the release is ready and will be installed when the voice call ends"}
	}
	status := u.Status()
	// After the answer is on its way: the daemon's own stop request does the same (M23).
	time.AfterFunc(200*time.Millisecond, u.begin)
	return status, nil
}

// SetMode and SetChannel record a choice. An unknown word is refused.
func (u *Updater) SetMode(mode string) (ipc.UpdateStatus, error) {
	switch mode {
	case ipc.UpdateAuto, ipc.UpdateNotify, ipc.UpdateOff:
	default:
		return u.Status(), &ErrRefused{"the mode is auto, notify or off"}
	}
	if mode == ipc.UpdateAuto && !u.installs {
		return u.Status(), &ErrRefused{u.reason}
	}
	err := u.installer.Update(func(r *Record) { r.Mode = mode })
	u.opts.Changed()
	return u.Status(), err
}

func (u *Updater) SetChannel(channel string) (ipc.UpdateStatus, error) {
	switch channel {
	case ipc.ChannelStable, ipc.ChannelPrerelease:
	default:
		return u.Status(), &ErrRefused{"the channel is stable or prerelease"}
	}
	err := u.installer.Update(func(r *Record) { r.Channel, r.Available = channel, "" })
	u.opts.Changed()
	return u.Status(), err
}

func jitter() time.Duration {
	if checkJitter <= 0 {
		return 0
	}
	return rand.N(checkJitter)
}
