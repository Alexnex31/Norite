// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/installreceipt"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Variables so that a test, and the one build that carries a test root (testrelease.go), can run a day in a
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
	// applyTimeout bounds an installation somebody asked for and is waiting on.
	applyTimeout = 100 * time.Second
	// requestWait is how long a request waits for a check or a download already under way before it
	// says so. The schedule's own download may take a quarter of an hour, and a client waits two minutes:
	// waiting it out reported a healthy daemon as one that did not answer (M24 /code-review).
	requestWait = 5 * time.Second
	// nudgeFloor is the least time between two checks a refused handshake asks for.
	nudgeFloor = 15 * time.Minute
	// wake is the longest the schedule sleeps without looking at the clock.
	wake = time.Hour
	// retryFailed is how soon a listing that could not be read is asked for again. A day was the answer
	// for a laptop that was offline at the one moment it asked.
	retryFailed = time.Hour
)

// errUndecided is a check that cannot be made yet: signed in, and the instance not yet heard from.
var errUndecided = &ErrRefused{"this daemon is signed in to an instance it has not heard from in this run, and that " +
	"instance's version decides which release it may move to; ask again once it is connected"}

// errBusy is a request that found a check or a download already under way.
var errBusy = &ErrRefused{"a check or a download is already under way; ask again in a moment"}

// The three things the norite_testrelease build replaces (testrelease.go). In every other build they are
// nil and nothing assigns them: where releases come from, what makes one believed, and a hook run once the
// daemon has started.
var (
	sourceOverride func(version string) *Source
	verifyOverride func(checksums, bundle []byte, tag string) error
	startedHook    func(running string)
)

// ErrRefused is a request understood and not carried out, with nothing changed. Its text is for a person.
type ErrRefused struct{ Msg string }

func (e *ErrRefused) Error() string { return e.Msg }

// ErrUnreachable is a request that could not be carried out because the release host could not be read:
// offline, rate limited, too slow. It is not a refusal: a script must be able to tell "nothing newer"
// from "could not find out" (M24 /code-review).
type ErrUnreachable struct{ Msg string }

func (e *ErrUnreachable) Error() string { return e.Msg }

// Options configures an Updater.
type Options struct {
	// Version is the version running.
	Version string
	// StateDir is the daemon's state directory.
	StateDir string
	// Executable is the running daemon's own path, resolved.
	Executable string
	// Instance returns the version the signed-in instance reported in this run, or "" when it has
	// reported none, and whether the daemon holds a sign-in at all.
	Instance func() (version string, signedIn bool)
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
	// wanted is that somebody asked for what is staged to be installed, and it was put off: it is
	// installed when it can be, whatever the mode.
	wanted bool
	// tried is when the listing was last asked for in this run, read or not, and unverified the releases
	// whose signature did not stand up in this run. Neither is kept between runs: see check and failed.
	tried      time.Time
	unverified []string
	// withheld is the release the last check would have chosen had its signature not failed in this run.
	withheld string
	// working is held by whichever of the loop and a request is checking or staging: a channel of one, so
	// that a request can stop waiting for it.
	working chan struct{}
	// nudged asks the loop for a check soon, outside its schedule, and retry for another try at
	// installing what is staged.
	nudged chan struct{}
	retry  chan struct{}
	// life ends when the schedule's run does. It bounds a download a request started and stopped waiting
	// for.
	life context.Context
	end  context.CancelFunc
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
		if sourceOverride != nil {
			o.Source = sourceOverride(o.Version)
		}
	}
	if o.Verify == nil {
		o.Verify = verifyOverride
	}
	if o.Changed == nil {
		o.Changed = func() {}
	}
	u := &Updater{opts: o, installer: &Installer{Dir: filepath.Join(o.StateDir, "update"), GOOS: o.GOOS},
		working: make(chan struct{}, 1), nudged: make(chan struct{}, 1), retry: make(chan struct{}, 1)}
	u.life, u.end = context.WithCancel(context.Background())
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
	if !samePath(u.opts.Executable, filepath.Join(receipt.BinDir, daemonName+exeSuffix(u.opts.GOOS)), u.opts.GOOS) {
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
	if err != nil && outcome == RolledBack {
		// The previous pair is back on disk and only the record of it could not be written. This
		// version is the one undone either way, and must make way.
		u.opts.Log.Error().Str("error", said(err)).Str("version", u.current.String()).
			Msg("this version did not stay up and the previous one is restored, but that could not be recorded")
		return true
	}
	if err != nil {
		u.opts.Log.Error().Str("error", said(err)).
			Msg("an update that did not stay up could not be undone; this version keeps running")
		return false
	}
	if startedHook != nil && outcome != RolledBack {
		defer startedHook(u.current.String())
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
	// What a request left downloading ends with the run.
	defer u.end()
	confirm := time.NewTimer(confirmAfter)
	defer confirm.Stop()
	// The schedule is kept as a time of day and not as a timer's length. A timer counts the time this
	// machine is awake, so on a laptop that sleeps at night "once a day" was once every three days
	// (M24 /code-review). The timer only wakes the loop, at least every wake, to look at the clock.
	due := u.opts.Now().Add(u.untilFirstCheck()).Round(0)
	next := time.NewTimer(u.nap(due))
	defer next.Stop()
	arm := func(wait time.Duration) {
		due = u.opts.Now().Add(wait).Round(0)
		next.Reset(u.nap(due))
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-confirm.C:
			// Only the daemon that installs releases has one on trial. Any other program that held this
			// state directory for ten minutes, a development build or a copy run from elsewhere, used to
			// confirm a version it was not, and drop the pair kept to go back to (M24 /code-review).
			if !u.installs {
				continue
			}
			if err := u.installer.Confirm(); err != nil {
				u.opts.Log.Warn().Str("error", said(err)).Msg("could not record that this version stayed up")
			}
			u.opts.Changed()
		case <-u.nudged:
			// The instance has just refused this daemon for its version, which is the one moment a newer
			// release is needed and not merely wanted. Waiting out the schedule left a client locked out
			// for up to a day with the release that fixes it already published (M24 /code-review). Not
			// more often than nudgeFloor, so an instance that refuses in a loop asks the host once.
			// A last check dated after now is a clock that was set back, and holds nothing off.
			since := u.opts.Now().Sub(u.lastAsked())
			switch {
			case u.mode() == ipc.UpdateOff:
			case !u.canFollow():
				// Refused for being ahead of its instance: no release is both newer than this one and
				// one that instance takes, and reading the listing to find that out again, every hour
				// the gateway tries, is a megabyte and a record written for nothing (M24 /code-review).
			case since < 0 || since >= nudgeFloor:
				if waiting, again := u.turn(ctx); waiting || again > 0 {
					arm(max(again, retryStaged))
				}
			case u.opts.Now().Add(nudgeFloor - since).Before(due):
				// Asked for inside the floor: made when the floor is past, and not dropped. Dropped, the
				// next came only when the gateway tried again, an hour later (M24 /code-review).
				arm(nudgeFloor - since)
			}
		case <-u.retry:
			arm(retryStaged)
		case <-next.C:
			if u.opts.Now().Before(due) {
				next.Reset(u.nap(due))
				continue
			}
			wait := checkEvery + jitter()
			if u.mode() != ipc.UpdateOff || u.isWanted() {
				switch waiting, again := u.turn(ctx); {
				case waiting:
					// Ready and deferred: tried again soon, without asking the listing again.
					wait = retryStaged
				case again > 0:
					wait = again
				}
			}
			arm(wait)
		}
	}
}

// nap is how long the loop sleeps before it next looks at the clock: until due, and never longer than
// wake.
func (u *Updater) nap(due time.Time) time.Duration {
	return min(max(due.Sub(u.opts.Now()), 0), wake)
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

// lastAsked is when the listing was last asked for: read, as the record says, or tried in this run.
func (u *Updater) lastAsked() time.Time {
	last := u.installer.Load().LastCheck
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.tried.After(last) {
		return u.tried
	}
	return last
}

// canFollow reports whether any release could be both newer than this daemon and one its instance takes.
// An instance not heard from, or one whose version is not a release's, rules nothing out.
func (u *Updater) canFollow() bool {
	version, _ := u.instance()
	instance, err := gatewayproto.ParseVersion(version)
	return err != nil || reachable(u.current, instance)
}

// onTrial reports whether the version running was installed here and has not yet stayed up.
func (u *Updater) onTrial() bool {
	p := u.installer.Load().Pending
	return u.installs && p != nil && p.Phase == phaseApplied
}

// turn is one turn of the loop. waiting is a release staged and waiting for a better moment; again is how
// soon to take another turn when this one could not be finished, or zero for the usual day.
func (u *Updater) turn(ctx context.Context) (waiting bool, again time.Duration) {
	if !u.acquire(ctx, 0) {
		return false, 0
	}
	defer u.release()

	u.mu.Lock()
	// What is staged is installed by the schedule only while that is still what was chosen: the mode is
	// auto, or somebody asked. Staged under auto and then told to ask first, it used to be installed at
	// the next turn all the same (M24 /code-review).
	if u.staged != nil && !u.wanted && u.mode() != ipc.UpdateAuto {
		u.staged = nil
	}
	staged := u.staged
	u.mu.Unlock()
	if staged == nil {
		// Signed out, a daemon updates by itself like any other, a pre-release included: the running
		// version stands in for the instance it has none of (Choose). Holding a signed-out pre-release
		// back until somebody asked was built on a review's finding and taken out again on the author's
		// decision: an install nobody has signed in to yet is the one that most needs to be current.
		release, found, err := u.check(ctx)
		switch {
		case errors.Is(err, errUndecided):
			// Asked again soon, and without reading the listing: nothing was learned from the host.
			return false, retryStaged
		case err != nil && ctx.Err() == nil:
			return false, retryFailed
		}
		if err != nil || !found || u.mode() != ipc.UpdateAuto || !u.installs {
			return false, 0
		}
		// A version on trial is not installed over. Its predecessor is the pair kept to go back to, and
		// an installation replaces that pair with the version on trial before it has swapped anything:
		// one that then failed left a version nobody had confirmed with nothing to go back to
		// (M24 /code-review). It waits out the trial, which is ten minutes of staying up.
		if u.onTrial() {
			return false, retryStaged
		}
		if err := u.stage(ctx, release); err != nil {
			if errors.Is(err, errDownload) && ctx.Err() == nil {
				// A download that dropped, or a release listed before its files were all there: tried
				// again as a listing that could not be read is, and not a day later.
				return false, retryFailed
			}
			return false, 0
		}
	}
	if u.opts.Busy != nil && u.opts.Busy() {
		return true, 0
	}
	// A run that is already ending is not ended again to install: whatever is stopping it was asked for.
	if ctx.Err() != nil {
		return false, 0
	}
	u.begin()
	return false, 0
}

// instance is the signed-in instance's version as heard in this run, and whether the daemon is signed in.
func (u *Updater) instance() (version string, signedIn bool) {
	if u.opts.Instance != nil {
		version, signedIn = u.opts.Instance()
	}
	if !signedIn {
		// The version heard belongs to an instance this daemon has signed out of. It bounded the choice
		// still: a daemon refused by its instance and then signed out was moved to that instance's
		// version, where signed out only the running version's own patches are taken (M24 /code-review).
		version = ""
	}
	return version, signedIn
}

func (u *Updater) isWanted() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.wanted && u.staged != nil
}

// check reads the listing and chooses. What it finds, or why it could not, goes into the record.
func (u *Updater) check(ctx context.Context) (Release, bool, error) {
	if _, err := gatewayproto.ParseVersion(u.opts.Version); err != nil {
		return Release{}, false, &ErrRefused{u.reason}
	}
	record := u.installer.Load()
	if record.unreadable {
		// Nothing found could be kept, and an installation would stop the daemon only to fail.
		return Release{}, false, &ErrRefused{errUnreadable.Error() + ": " + termsafe.Text(u.installer.recordPath())}
	}
	instance, signedIn := u.instance()
	if signedIn && instance == "" {
		// Not a check that found nothing. Recorded as one, it said "no newer release" of a question it
		// had not been able to ask, and was not asked again for a day (M24 /code-review).
		return Release{}, false, errUndecided
	}
	releases, err := u.opts.Source.Releases(ctx)
	now := u.opts.Now()
	u.mu.Lock()
	u.tried = now
	unverified := append([]string(nil), u.unverified...)
	u.mu.Unlock()
	if err != nil && ctx.Err() != nil {
		// Whoever asked went away, or the daemon is stopping. That is not a check that failed: recorded
		// as one, it put the next check off by a day and left "context canceled" as the last error after
		// an ordinary stop (M24 /code-review).
		return Release{}, false, err
	}
	if err != nil {
		u.opts.Log.Warn().Str("error", said(err)).Msg("could not check for a new release")
		// The time of the last check stays the last one that read the listing. Moved here, a start after
		// a failure waited out a day from it, and the status said "no newer release, as of" a moment
		// nothing had been learned at (M24 /code-review).
		_ = u.installer.Update(func(r *Record) { r.LastError = said(err) })
		u.opts.Changed()
		return Release{}, false, err
	}
	policy := Policy{Current: u.current, Instance: instance, SignedIn: signedIn,
		Prereleases: u.channelOf(record) == ipc.ChannelPrerelease, Bad: record.Bad}
	newest, _ := Choose(policy, releases)
	policy.Bad = append(append([]string(nil), record.Bad...), unverified...)
	release, found := Choose(policy, releases)
	// Why a newer release is not the one offered stays said for as long as it is so.
	withheld := ""
	if !found || newest.Version != release.Version {
		withheld = newest.Version.String()
	}
	if len(unverified) == 0 {
		withheld = ""
	}
	u.mu.Lock()
	u.withheld = withheld
	u.mu.Unlock()
	available := ""
	if found {
		available = release.Version.String()
		if available != record.Available {
			u.opts.Log.Info().Str("version", available).Msg("a newer release is available")
		}
	}
	_ = u.installer.Update(func(r *Record) {
		r.LastCheck, r.Available = now, available
		if withheld == "" {
			r.LastError = ""
		}
	})
	u.opts.Changed()
	return release, found, nil
}

// stage downloads and verifies a release and keeps it ready.
func (u *Updater) stage(ctx context.Context, release Release) error {
	if err := u.installer.Ready(); err != nil {
		return u.failed(release, fmt.Errorf("it cannot be installed here: %w", err))
	}
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
	if err != nil && ctx.Err() != nil {
		return err
	}
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
	level.Str("version", release.Version.String()).Str("error", said(err)).
		Msg("a release was not installed; this version keeps running")
	_ = u.installer.Update(func(r *Record) {
		r.LastError = said(err)
		if errors.Is(err, ErrNotVerified) {
			// The listing named it and no signature stands behind it: it is not announced as out.
			r.Available = ""
		}
	})
	if errors.Is(err, ErrNotVerified) {
		// Nor chosen again in this run. The next check used to announce it again and clear the error,
		// and with the mode on auto its files were fetched and refused every day (M24 /code-review).
		// Not kept in the record: a signature may fail because this install's root is too old, and an
		// install made afresh must be offered the release.
		u.mu.Lock()
		if len(u.unverified) < maxBad {
			u.unverified = append(u.unverified, release.Version.String())
		}
		u.mu.Unlock()
	}
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
//
// stopped reports whether somebody has asked the daemon to stop: a signal, or a stop request. Stopping and
// updating end the run the same way, so a stop that arrived once the update had begun was taken for part
// of it, and the daemon started again after being told to stop: under systemd, to be killed ninety
// seconds later, which counted against the new version (M24 /code-review). A daemon told to stop stops.
// It is asked again after the installation, which takes a moment: the new version is then on disk and
// starts at the next start.
func (u *Updater) Finish(stopped func() bool) bool {
	u.mu.Lock()
	staged, applying := u.staged, u.applying
	u.mu.Unlock()
	if !applying || staged == nil {
		u.cleanExit()
		return false
	}
	if stopped() {
		u.opts.Log.Info().Msg("asked to stop before the release was installed; it is installed another time")
		u.cleanExit()
		return false
	}
	if err := u.installer.Install(*staged, u.current.String()); err != nil {
		u.opts.Log.Error().Str("error", said(err)).
			Msg("the release could not be installed; the version that was running starts again")
		_ = u.installer.Update(func(r *Record) { r.LastError = said(err) })
		return !stopped()
	}
	if stopped() {
		u.opts.Log.Info().Str("to", staged.Release.Version.String()).
			Msg("a release is installed, and the daemon was asked to stop; it starts as that version next time")
		return false
	}
	u.opts.Log.Info().Str("from", u.current.String()).Str("to", staged.Release.Version.String()).
		Msg("a release is installed; starting it")
	return true
}

// cleanExit takes back this start's count against a version on trial. Only the daemon that installs
// releases is that version: see Run.
func (u *Updater) cleanExit() {
	if u.installs {
		u.installer.CleanExit()
	}
}

func (u *Updater) mode() string {
	return u.modeOf(u.installer.Load())
}

// modeOf is the mode in force. Unset, it is auto where this daemon installs and notify where it does not:
// an install nobody's installer made has never been told it would be changed.
//
// On Windows it is notify until somebody chooses: installing there has not been watched working, and what
// nobody has watched does not happen unasked (M23's rule for install.ps1's task). `norite update apply`
// and `norite update mode auto` both work there.
//
// A record that cannot be read is off: nothing says what was chosen, and "off" may be it.
func (u *Updater) modeOf(r Record) string {
	if r.unreadable {
		return ipc.UpdateOff
	}
	switch r.Mode {
	case ipc.UpdateAuto, ipc.UpdateNotify, ipc.UpdateOff:
		return r.Mode
	}
	if u.installs && u.opts.GOOS != "windows" {
		return ipc.UpdateAuto
	}
	return ipc.UpdateNotify
}

// channelOf is the channel in force. Unset, an install that is itself a pre-release is offered
// pre-releases: the installer installs the newest release of any kind, and a release candidate that was
// never offered the next one stayed locked out of an instance that had moved to it, with the release that
// fixed it published (M24 /code-review).
func (u *Updater) channelOf(r Record) string {
	switch r.Channel {
	case ipc.ChannelStable, ipc.ChannelPrerelease:
		return r.Channel
	}
	if u.current.Pre != "" {
		return ipc.ChannelPrerelease
	}
	return ipc.ChannelStable
}

// Status reports where updating stands.
func (u *Updater) Status() ipc.UpdateStatus {
	r := u.installer.Load()
	s := ipc.UpdateStatus{Version: u.opts.Version, Mode: u.modeOf(r), Channel: u.channelOf(r),
		Installs: u.installs, Reason: u.reason, Available: u.stillNewer(r.Available), RolledBack: r.RolledBack,
		LastError: r.LastError}
	if r.unreadable {
		s.LastError = errUnreadable.Error() + ": " + termsafe.Text(u.installer.recordPath())
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
	if !u.acquire(ctx, requestWait) {
		return u.Status(), errBusy
	}
	defer u.release()
	if _, _, err := u.check(ctx); err != nil {
		return u.Status(), unreachable(err)
	}
	return u.Status(), nil
}

// unreachable is a failed check as a request answers it: a refusal stays one.
func unreachable(err error) error {
	var refused *ErrRefused
	if errors.As(err, &refused) {
		return err
	}
	return &ErrUnreachable{"the release listing could not be read: " + said(err)}
}

// errDeferred is an installation asked for and put off until the daemon is free.
var errDeferred = errors.New("deferred")

// Apply checks, stages and begins installing, because somebody asked. The daemon restarts after the
// answer has left.
//
// The download belongs to the daemon and not to the request. A client waits two minutes for an answer,
// and a download given only that long, and thrown away when it ran out, could never finish on a slow
// link: with the mode on notify, which is the default on Windows, that is the only way a release is
// installed (M24 /code-review). The request waits applyTimeout and is then answered that the release is
// on its way, and the download goes on, with the quarter of an hour the schedule's own has.
func (u *Updater) Apply(ctx context.Context) (ipc.UpdateStatus, error) {
	if !u.installs {
		return u.Status(), &ErrRefused{u.reason}
	}
	// Counted from here. Counted from after the check, the wait for whatever was under way and a slow
	// listing came on top of it, and the answer left after the client had stopped waiting for it
	// (M24 /code-review).
	wait := time.NewTimer(applyTimeout)
	defer wait.Stop()
	if u.onTrial() {
		return u.Status(), &ErrRefused{"the version running was just installed and is kept once it has stayed up for " +
			"ten minutes; ask again then"}
	}
	if !u.acquire(ctx, requestWait) {
		return u.Status(), errBusy
	}
	u.mu.Lock()
	staged := u.staged
	u.mu.Unlock()
	var release Release
	if staged == nil {
		var (
			found bool
			err   error
		)
		release, found, err = u.check(ctx)
		if err != nil || !found {
			u.release()
			if err != nil {
				return u.Status(), unreachable(err)
			}
			u.mu.Lock()
			withheld := u.withheld
			u.mu.Unlock()
			if withheld != "" {
				return u.Status(), &ErrRefused{"release " + withheld + " was refused because its signature did not " +
					"verify, and is not tried again until the daemon is restarted (`norite daemon restart`)"}
			}
			return u.Status(), &ErrRefused{"there is no newer release this install can move to"}
		}
	}
	done := make(chan error, 1)
	go func() {
		defer u.release()
		if staged == nil {
			if err := u.stage(u.life, release); err != nil {
				done <- err
				return
			}
		}
		// Asked for: installed whatever the mode, and not dropped by a turn of the schedule that finds
		// it staged with the mode on notify. That turn, arriving between this answer and the restart,
		// used to leave "Installing" said and nothing installed (M24 /code-review).
		u.mu.Lock()
		u.wanted = true
		u.mu.Unlock()
		if u.opts.Busy != nil && u.opts.Busy() {
			// Tried again soon, and not at the next daily check.
			select {
			case u.retry <- struct{}{}:
			default:
			}
			done <- errDeferred
			return
		}
		done <- nil
		// After the answer is on its way: the daemon's own stop request does the same (M23).
		time.AfterFunc(200*time.Millisecond, u.begin)
	}()
	select {
	case err := <-done:
		switch {
		case err == nil:
			return u.Status(), nil
		case errors.Is(err, errDeferred):
			return u.Status(), &ErrRefused{"the release is ready and will be installed when the voice call ends"}
		case errors.Is(err, errDownload):
			return u.Status(), &ErrUnreachable{"the release could not be downloaded, and this version keeps running: " + said(err)}
		default:
			return u.Status(), fmt.Errorf("the release was not installed, and this version keeps running: %s", said(err))
		}
	case <-wait.C:
		// Still downloading. It is installed when it has been verified, and `update status` says so.
		return u.Status(), nil
	case <-ctx.Done():
		return u.Status(), ctx.Err()
	}
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

// maxSaid bounds what is kept or repeated of an error. Its text is partly a release host's: the address a
// redirect named, a body a parser quoted. A megabyte of it, kept as the last error, made a record too
// large to be read back, which forgot the mode, the channel and every version rolled back from
// (M24 /security-sweep).
const maxSaid = 300

// said is an error's text, cleaned, and cut to a length a person reads.
func said(err error) string {
	return cut(termsafe.Text(err.Error()), maxSaid)
}

// cut shortens s to at most limit bytes, on a character's boundary, and marks that it did.
func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

func jitter() time.Duration {
	if checkJitter <= 0 {
		return 0
	}
	return rand.N(checkJitter)
}

// acquire takes the working turn, waiting at most wait for it, or for as long as ctx lasts when wait is
// zero. It reports whether it was taken.
func (u *Updater) acquire(ctx context.Context, wait time.Duration) bool {
	if wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	select {
	case u.working <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (u *Updater) release() { <-u.working }

// Nudge asks for a check soon: the instance has refused this daemon for its version. It never blocks, and
// asking twice before the loop has looked is asking once.
func (u *Updater) Nudge() {
	select {
	case u.nudged <- struct{}{}:
	default:
	}
}

// stillNewer is a recorded "available" version, or "" once it is no longer above what runs. The record is
// written by a check and outlives the daemon that made it: after somebody runs the installer by hand, the
// version it named is the one running, and it was still being announced as out until the next check, up
// to a day later (M24 /code-review).
func (u *Updater) stillNewer(available string) string {
	version, err := gatewayproto.ParseVersion(available)
	if err != nil || version.Compare(u.current) <= 0 {
		return ""
	}
	return available
}

// samePath reports whether two paths name the same file. Both are resolved through any links first: the
// daemon's own path arrives resolved and the receipt's as the installer typed it, and a home or a bin
// directory reached through a link made the two differ, which silently turned updating off for an install
// that had just been told it updates itself (M24 /code-review). Windows compares names without regard to
// case.
func samePath(a, b, goos string) bool {
	resolve := func(p string) string {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return resolved
		}
		return filepath.Clean(p)
	}
	a, b = resolve(a), resolve(b)
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
