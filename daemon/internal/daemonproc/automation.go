// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/internal/automation"
	"github.com/Alexnex31/Norite/daemon/internal/session"
	internalstate "github.com/Alexnex31/Norite/daemon/internal/statefile"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/statefile"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// signedIn is the part of the session the automation port needs: which instance, and whether there is one.
// It is the session's status and never its credential, which is the whole of what this file hands the port.
type signedIn interface {
	Status() (session.Standing, session.Account)
}

// automationControl opens and closes the port for scripts (M22) as the state file says, and answers the
// three requests that change what it says.
//
// The port is closed until its user turns it on, and it is turned on for one instance: the one the daemon
// is signed in to at that moment, recorded with the switch. A script's token is a credential for the
// instance that minted it, and the port sends it nowhere else.
//
// The requests are the daemon's own, on the attach socket, so they are that socket's tier (rule 16): the
// account itself. None of them is reachable from the port they control.
type automationControl struct {
	stateDir string
	session  signedIn
	version  string
	log      zerolog.Logger
	// base is the daemon's lifetime: a port opened by a request must outlive that request.
	base context.Context

	mu      sync.Mutex
	srv     *automation.Server
	cancel  context.CancelFunc
	served  chan struct{}
	problem string
	// running is what the open port was opened as. While a port is open it is what status reports, since
	// the state file is written a moment after the port moves and read without this lock.
	running statefile.State
	// started is set once start has run, and stopped as the daemon stops, after which nothing opens the
	// port again.
	started, stopped bool
}

// updateState is the state file's writer, a variable so a test can fail the write after the port has moved.
var updateState = internalstate.Update

func newAutomation(base context.Context, stateDir string, s signedIn, version string, log zerolog.Logger) *automationControl {
	return &automationControl{stateDir: stateDir, session: s, version: version, log: log, base: base}
}

// InstanceURL is where the port's requests go: automation.Instance. A daemon still establishing its
// sign-in names its instance already, once it has read the store.
//
// Before it has, it names none, and that is not being signed out: a daemon started before its keyring
// unlocks stays there for as long as that takes, and told to log in again its user would replace a sign-in
// that is perfectly good (M20's standing, M22 /code-review).
func (a *automationControl) InstanceURL() (string, error) {
	standing, account := a.session.Status()
	switch {
	case standing == session.SignedOut:
		return "", automation.ErrSignedOut
	case account.InstanceURL == "":
		return "", automation.ErrSignInPending
	}
	return account.InstanceURL, nil
}

// start opens the port if the state file says it is on. Called once, as the daemon starts and holding its
// lock, which is what makes any file a killed daemon left stale: it is removed whether or not the port is
// to open.
func (a *automationControl) start() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.started = true
	// The attach socket is already served when this runs, so a request may have opened the port first. Its
	// file is this run's then, and removing it would leave a port no script can find.
	if a.srv != nil {
		return
	}
	automation.Clean(a.stateDir)
	st, err := statefile.ReadIn(a.stateDir)
	if err != nil {
		a.log.Warn().Str("error", termsafe.Text(err.Error())).Msg("cannot read the state file; the automation port stays closed")
		return
	}
	if !st.AutomationEnabled {
		return
	}
	if err := a.openLocked(st); err != nil {
		a.log.Warn().Str("error", termsafe.Text(err.Error())).Int("port", portOf(st)).
			Msg("the automation port is enabled and could not be opened; scripts cannot reach the daemon")
	}
}

// stop closes the port as the daemon stops, for good: a request still on its way in must not open it again
// behind the daemon's back and leave a file naming a port nothing holds (M22 /code-review).
func (a *automationControl) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopped = true
	a.closeLocked()
}

// errStopping is an attempt to open the port on a daemon that is stopping.
var errStopping = errors.New("the daemon is stopping")

func portOf(st statefile.State) int {
	if st.AutomationPort == 0 {
		return ipc.DefaultAutomationPort
	}
	return st.AutomationPort
}

// openLocked opens the port as st describes and serves it. The reason it could not is kept for status.
func (a *automationControl) openLocked(st statefile.State) error {
	if a.stopped {
		a.problem = errStopping.Error()
		return errStopping
	}
	if a.srv == nil {
		// With no port of this daemon's own, a file here is one a killed daemon left.
		automation.Clean(a.stateDir)
	}
	old, oldCancel, oldServed := a.srv, a.cancel, a.served
	srv, err := automation.Open(automation.Options{
		Port: portOf(st), StateDir: a.stateDir, Instance: a, EnabledFor: st.AutomationInstance,
		HTTP: session.NewHTTPClient(), Version: a.version, Log: a.log,
	})
	if err != nil {
		a.problem = termsafe.Text(err.Error())
		if errors.Is(err, automation.ErrPortTaken) {
			a.problem = "something else is listening on 127.0.0.1 port " + strconv.Itoa(portOf(st)) +
				"; stop it, or choose another port with `norite automation enable --port`"
		}
		return err
	}
	ctx, cancel := context.WithCancel(a.base)
	served := make(chan struct{})
	go func() {
		srv.Serve(ctx)
		close(served)
	}()
	a.srv, a.cancel, a.served, a.problem, a.running = srv, cancel, served, "", st
	a.log.Info().Str("address", srv.Address()).Msg("the automation port is open")
	// A port that was open on another number is closed only now, with the new one bound and its file
	// written over the old one's: until here nothing had been taken from the scripts using it.
	shut(old, oldCancel, oldServed)
	return nil
}

// shut closes one port, waiting for every script's connection to end. Its file goes first, while it is
// still this port's: the listener is gone the moment cancel runs, and the connections may take a while.
func shut(srv *automation.Server, cancel context.CancelFunc, served chan struct{}) {
	if srv == nil {
		return
	}
	cancel()
	srv.Close()
	<-served
}

// closeLocked closes the port if it is open, waiting for every script's connection to end.
func (a *automationControl) closeLocked() {
	if a.srv == nil {
		return
	}
	shut(a.srv, a.cancel, a.served)
	a.srv, a.cancel, a.served = nil, nil, nil
	a.log.Info().Msg("the automation port is closed")
}

// statusLocked is how the port stands, from the state file and from what is running.
//
// The two can differ for a moment: the file is written just after the port moves, and the attach socket
// answers before start has run. What is open is reported as it was opened, and an enabled port that is not
// open always says why, so no answer contradicts itself or leaves the reason blank (M22 /code-review).
func (a *automationControl) statusLocked(st statefile.State) ipc.AutomationStatus {
	if a.srv != nil {
		st = a.running
	}
	out := ipc.AutomationStatus{Enabled: st.AutomationEnabled, Port: portOf(st)}
	if !out.Enabled {
		return out
	}
	// From a file a person can edit, and about to be printed.
	out.Instance = termsafe.Text(st.AutomationInstance)
	out.Problem = a.problem
	if a.srv == nil {
		switch {
		case out.Problem != "":
		case a.stopped:
			out.Problem = errStopping.Error()
		case !a.started:
			out.Problem = "the daemon is still starting; ask again in a moment"
		default:
			out.Problem = "the port is recorded as on and this daemon has not opened it; run `norite automation enable`"
		}
		return out
	}
	out.Open, out.Address = true, a.srv.Address()
	// An open port that will refuse every request says so here, where a person looks first, and not only
	// to the script (M22 manual pass). The port asks the same two questions of each request.
	switch now, err := a.InstanceURL(); {
	case err != nil:
		out.Problem = err.Error()
	case !automation.SameInstance(now, st.AutomationInstance):
		out.Problem = "the daemon is signed in to " + termsafe.Text(now) + " now, and the port serves only the " +
			"instance it was turned on for; run `norite automation enable` to use it with this one"
	}
	return out
}

// status reads the state file and reports, under one lock.
func (a *automationControl) status() (ipc.AutomationStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := statefile.ReadIn(a.stateDir)
	if err != nil {
		return ipc.AutomationStatus{}, err
	}
	return a.statusLocked(st), nil
}

// handles reports whether a local request is one of the automation port's.
func (a *automationControl) handles(path string) bool {
	return path == ipc.PathAutomation || strings.HasPrefix(path, ipc.PathAutomation+"/")
}

// Do answers one request about the port. Each branch reports how the port stood while it held the lock, so
// the answer is one moment's and not two.
func (a *automationControl) Do(ctx context.Context, req ipc.Request) ipc.Response {
	const wrongMethod = "the automation port is read with GET and changed with POST"
	var out ipc.AutomationStatus
	var err error
	port, enabling := ipc.AutomationEnablePort(req.Path)
	switch {
	case req.Path == ipc.PathAutomation && req.Method != http.MethodGet:
		return ipc.Failure(ipc.RelayBadRequest, wrongMethod)
	case req.Path == ipc.PathAutomation:
		out, err = a.status()
	case req.Method != http.MethodPost:
		return ipc.Failure(ipc.RelayBadRequest, wrongMethod)
	case enabling:
		out, err = a.enable(ctx, port)
	case req.Path == ipc.PathAutomationDisable:
		out, err = a.disable(ctx)
	default:
		return ipc.Failure(ipc.RelayBadRequest, "the daemon answers no request of its own at that path")
	}

	var no *automationRefusal
	switch {
	case errors.As(err, &no):
		return ipc.Failure(ipc.RelayConflict, no.msg)
	case errors.Is(err, internalstate.ErrLocked):
		return ipc.Failure(ipc.RelayConflict, "another request is changing the daemon's state; try again")
	case err != nil:
		a.log.Error().Str("error", termsafe.Text(err.Error())).Msg("a request about the automation port failed")
		return ipc.Failure(ipc.RelayFailed, termsafe.Text(err.Error()))
	}
	body, err := json.Marshal(out)
	if err != nil {
		return ipc.Failure(ipc.RelayFailed, "the daemon could not encode its answer")
	}
	status := http.StatusOK
	return ipc.Response{Status: &status, Body: body}
}

// automationRefusal is a request understood and not carried out, with nothing changed.
type automationRefusal struct{ msg string }

func (r *automationRefusal) Error() string { return r.msg }

// enable opens the port on a port number for the instance the daemon is signed in to, and records both.
//
// The port is opened before anything is recorded, inside the state file's lock, so one that cannot be
// bound is refused with the reason and the file is left as it was. What happens to a port already open
// depends on the number:
//
//   - Another number: the new port is bound while the old one still serves, and the old is closed only
//     once the new is up. A refusal has then taken nothing from the scripts in use, and says so.
//   - The same number: it has to be closed to be bound again, which is also how the secret is replaced.
//     Should the bind then fail, what was open is opened again, with a new secret, and the refusal says
//     that too rather than that nothing changed.
//
// If the port moved and the record of it could not be written, the port is put back as the file says.
func (a *automationControl) enable(ctx context.Context, port int) (ipc.AutomationStatus, error) {
	instanceURL, err := a.InstanceURL()
	switch {
	case errors.Is(err, automation.ErrSignInPending):
		return ipc.AutomationStatus{}, &automationRefusal{"the port is turned on for the instance the daemon is " +
			"signed in to, and it has not finished reading its sign-in; try again in a moment"}
	case err != nil:
		return ipc.AutomationStatus{}, &automationRefusal{"the port is turned on for one instance, the one the " +
			"daemon is signed in to, and it is signed in to none; run `norite login` first"}
	}
	var out ipc.AutomationStatus
	var before statefile.State
	moved := false
	err = updateState(ctx, a.stateDir, func(st *statefile.State) error {
		a.mu.Lock()
		defer a.mu.Unlock()

		wasOpen, wasWrong := a.srv != nil, a.problem
		before = *st
		st.AutomationEnabled, st.AutomationPort, st.AutomationInstance = true, port, instanceURL

		beside := wasOpen && portOf(before) != port
		if !beside {
			a.closeLocked()
		}
		if err := a.openLocked(*st); err != nil {
			reason := a.problem
			// What was enabled and could not open still cannot, for the reason it had.
			a.problem = wasWrong
			switch {
			case !wasOpen || beside:
				return &automationRefusal{reason + ". Nothing was changed"}
			case a.openLocked(before) != nil:
				return &automationRefusal{reason + ". The port it was open on could not be opened again " +
					"either, and is closed: " + a.problem}
			}
			return &automationRefusal{reason + ". The port is open as it was, with a new secret: scripts " +
				"started before this must be started again"}
		}
		out, moved = a.statusLocked(*st), true
		return nil
	}, nil)
	if err != nil && moved {
		// The port is as asked and the file is as it was. The file is what the next start reads and what
		// status reports, so the port is made to agree with it.
		a.mu.Lock()
		a.closeLocked()
		a.problem = ""
		if before.AutomationEnabled {
			if rerr := a.openLocked(before); rerr != nil {
				a.log.Warn().Str("error", termsafe.Text(rerr.Error())).
					Msg("the automation port could not be reopened as it was")
			}
		}
		a.mu.Unlock()
	}
	return out, err
}

// disable closes the port and records that it stays closed. The port number is kept, so turning it on again
// without naming one finds the one that was chosen. Asked of a port already off, it changes nothing.
func (a *automationControl) disable(ctx context.Context) (ipc.AutomationStatus, error) {
	var recorded statefile.State
	var out ipc.AutomationStatus
	err := updateState(ctx, a.stateDir, func(st *statefile.State) error {
		st.AutomationEnabled, st.AutomationInstance = false, ""
		recorded = *st
		return nil
	}, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.closeLocked()
		a.problem = ""
		out = a.statusLocked(recorded)
	})
	return out, err
}
