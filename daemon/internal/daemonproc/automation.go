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
}

func newAutomation(base context.Context, stateDir string, s signedIn, version string, log zerolog.Logger) *automationControl {
	return &automationControl{stateDir: stateDir, session: s, version: version, log: log, base: base}
}

// InstanceURL is where the port's requests go: automation.Instance. A daemon still establishing its
// sign-in names its instance already, and a signed-out one names none.
func (a *automationControl) InstanceURL() (string, bool) {
	standing, account := a.session.Status()
	if standing == session.SignedOut || account.InstanceURL == "" {
		return "", false
	}
	return account.InstanceURL, true
}

// start opens the port if the state file says it is on. Called once, as the daemon starts and holding its
// lock, which is what makes any file a killed daemon left stale: it is removed whether or not the port is
// to open.
func (a *automationControl) start() {
	automation.Clean(a.stateDir)
	st, err := statefile.ReadIn(a.stateDir)
	if err != nil {
		a.log.Warn().Str("error", termsafe.Text(err.Error())).Msg("cannot read the state file; the automation port stays closed")
		return
	}
	if !st.AutomationEnabled {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.openLocked(st); err != nil {
		a.log.Warn().Str("error", termsafe.Text(err.Error())).Int("port", portOf(st)).
			Msg("the automation port is enabled and could not be opened; scripts cannot reach the daemon")
	}
}

// stop closes the port as the daemon stops.
func (a *automationControl) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeLocked()
}

func portOf(st statefile.State) int {
	if st.AutomationPort == 0 {
		return ipc.DefaultAutomationPort
	}
	return st.AutomationPort
}

// openLocked opens the port as st describes and serves it. The reason it could not is kept for status.
func (a *automationControl) openLocked(st statefile.State) error {
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
	a.srv, a.cancel, a.served, a.problem = srv, cancel, served, ""
	a.log.Info().Str("address", srv.Address()).Msg("the automation port is open")
	return nil
}

// closeLocked closes the port if it is open, waiting for every script's connection to end.
func (a *automationControl) closeLocked() {
	if a.srv == nil {
		return
	}
	a.cancel()
	<-a.served
	a.srv.Close()
	a.srv, a.cancel, a.served = nil, nil, nil
	a.log.Info().Msg("the automation port is closed")
}

// statusLocked is how the port stands, from the state file and from what is running.
func (a *automationControl) statusLocked(st statefile.State) ipc.AutomationStatus {
	out := ipc.AutomationStatus{Enabled: st.AutomationEnabled, Port: portOf(st)}
	if st.AutomationEnabled {
		// From a file a person can edit, and about to be printed.
		out.Instance = termsafe.Text(st.AutomationInstance)
		out.Problem = a.problem
	}
	if a.srv != nil {
		out.Open, out.Address = true, a.srv.Address()
	}
	return out
}

func automationFailure(code, msg string) ipc.Response {
	return ipc.Response{Error: &ipc.RelayError{Code: code, Message: msg}}
}

// handles reports whether a local request is one of the automation port's.
func (a *automationControl) handles(path string) bool {
	return path == ipc.PathAutomation || strings.HasPrefix(path, ipc.PathAutomation+"/")
}

// Do answers one request about the port.
func (a *automationControl) Do(ctx context.Context, req ipc.Request) ipc.Response {
	var st statefile.State
	var err error
	port, enabling := ipc.AutomationEnablePort(req.Path)
	switch {
	case req.Path == ipc.PathAutomation:
		if req.Method != http.MethodGet {
			return automationFailure(ipc.RelayBadRequest, "the automation port is read with GET and changed with POST")
		}
		if st, err = statefile.ReadIn(a.stateDir); err == nil {
			a.mu.Lock()
			defer a.mu.Unlock()
		}
	case req.Method != http.MethodPost:
		return automationFailure(ipc.RelayBadRequest, "the automation port is read with GET and changed with POST")
	case enabling:
		st, err = a.enable(ctx, port)
	case req.Path == ipc.PathAutomationDisable:
		st, err = a.disable(ctx)
	default:
		return automationFailure(ipc.RelayBadRequest, "the daemon answers no request of its own at that path")
	}

	var no *automationRefusal
	switch {
	case errors.As(err, &no):
		return automationFailure(ipc.RelayConflict, no.msg)
	case errors.Is(err, internalstate.ErrLocked):
		return automationFailure(ipc.RelayConflict, "another request is changing the daemon's state; try again")
	case err != nil:
		a.log.Error().Str("error", termsafe.Text(err.Error())).Msg("a request about the automation port failed")
		return automationFailure(ipc.RelayFailed, termsafe.Text(err.Error()))
	}
	if req.Path != ipc.PathAutomation {
		a.mu.Lock()
		defer a.mu.Unlock()
	}
	body, err := json.Marshal(a.statusLocked(st))
	if err != nil {
		return automationFailure(ipc.RelayFailed, "the daemon could not encode its answer")
	}
	status := http.StatusOK
	return ipc.Response{Status: &status, Body: body}
}

// automationRefusal is a request understood and not carried out, with nothing changed.
type automationRefusal struct{ msg string }

func (r *automationRefusal) Error() string { return r.msg }

// enable opens the port on a port number for the instance the daemon is signed in to, and records both.
//
// The port is opened before anything is recorded, inside the state file's lock: a port that cannot be
// bound is refused with the reason and leaves the file, and whatever was open before, as they were. It
// is always opened afresh, so enabling again is also how the secret is replaced.
func (a *automationControl) enable(ctx context.Context, port int) (statefile.State, error) {
	instanceURL, ok := a.InstanceURL()
	if !ok {
		return statefile.State{}, &automationRefusal{"the port is turned on for one instance, the one the " +
			"daemon is signed in to, and it is signed in to none; run `norite login` first"}
	}
	var out statefile.State
	err := internalstate.Update(ctx, a.stateDir, func(st *statefile.State) error {
		a.mu.Lock()
		defer a.mu.Unlock()

		before, wasOpen := *st, a.srv != nil
		st.AutomationEnabled, st.AutomationPort, st.AutomationInstance = true, port, instanceURL
		a.closeLocked()
		if err := a.openLocked(*st); err != nil {
			reason := a.problem
			// What was open is opened again, with a new secret: the old one went with the old listener.
			a.problem = ""
			if wasOpen {
				if rerr := a.openLocked(before); rerr != nil {
					a.log.Warn().Str("error", termsafe.Text(rerr.Error())).
						Msg("the automation port could not be reopened as it was")
				}
			}
			return &automationRefusal{reason + ". The port was not turned on"}
		}
		out = *st
		return nil
	}, nil)
	return out, err
}

// disable closes the port and records that it stays closed. The port number is kept, so turning it on again
// without naming one finds the one that was chosen. Asked of a port already off, it changes nothing.
func (a *automationControl) disable(ctx context.Context) (statefile.State, error) {
	var out statefile.State
	err := internalstate.Update(ctx, a.stateDir, func(st *statefile.State) error {
		st.AutomationEnabled, st.AutomationInstance = false, ""
		out = *st
		return nil
	}, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.closeLocked()
		a.problem = ""
	})
	return out, err
}
