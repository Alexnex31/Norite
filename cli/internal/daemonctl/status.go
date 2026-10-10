// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/output"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// socketAnswer is what the daemon's attach socket said when asked who is there.
type socketAnswer struct {
	// Running is a daemon at the socket, whether or not this command could attach to it.
	Running bool
	// Attached is a completed handshake: the fields below it are the daemon's own account of itself.
	Attached bool
	Version  string
	Standing string
	Account  *ipc.Account
	// Problem is why a daemon that is there could not be attached to, in words for a person.
	Problem string
}

// statusAskTimeout bounds the handshake. A daemon answers in milliseconds when it answers.
const statusAskTimeout = 5 * time.Second

// socketStatus attaches to this user's daemon and reports what its handshake says.
//
// The service manager knows whether it started a process. It does not know a daemon somebody started by
// hand, which until M23 `norite daemon status` reported as "not installed" while it served clients, and it
// does not know whether the daemon is signed in or which version it is.
//
// A variable so the commands' tests can say what the socket answered without one.
var socketStatus = func(ctx context.Context, version string) socketAnswer {
	ctx, cancel := context.WithTimeout(ctx, statusAskTimeout)
	defer cancel()
	// In two steps, because they fail for different reasons. A connection that could not be made is not a
	// daemon: the socket's path is too long for one to listen on, or the state directory cannot be had.
	// Only a connection that was made and then went wrong is a daemon this command could not talk to.
	conn, err := ipc.Dial(ctx)
	if err != nil {
		if errors.Is(err, ipc.ErrNotRunning) {
			return socketAnswer{}
		}
		return socketAnswer{Problem: "no daemon could be looked for: " + output.Clean(err.Error())}
	}
	client, err := ipc.Attach(ctx, conn, ipc.Options{Client: "norite", Version: version})
	if err != nil {
		var mismatch *ipc.VersionError
		var closed *ipc.CloseError
		switch {
		case errors.As(err, &mismatch):
			// The versions are the daemon's and this binary's own, and the daemon's is still foreign text.
			return socketAnswer{Running: true, Version: output.Clean(mismatch.Daemon), Problem: output.Clean(mismatch.Error())}
		case errors.As(err, &closed):
			return socketAnswer{Running: true, Problem: "it refused the connection: " + output.Clean(closed.Reason)}
		case errors.Is(err, context.DeadlineExceeded):
			return socketAnswer{Running: true, Problem: fmt.Sprintf("it accepted the connection and did not answer "+
				"within %s; it may be hung", statusAskTimeout)}
		}
		return socketAnswer{Running: true, Problem: "it could not be attached to: " + output.Clean(err.Error())}
	}
	defer func() { _ = client.Close() }()
	ready := client.Ready()
	return socketAnswer{Running: true, Attached: true, Version: client.Hello().Version,
		Standing: ready.Standing, Account: ready.Account}
}

// statusView is `norite daemon status` (contracts/cli-json/daemon.schema.json). Every field is always
// present.
type statusView struct {
	// Running is a daemon running for this account, however it was started.
	Running bool `json:"running"`
	// Answering is that daemon having answered this command on its socket.
	Answering bool `json:"answering"`
	// Installed is a definition registered with the service manager, which is what starts the daemon at
	// login.
	Installed bool `json:"installed"`
	// Service is the service manager's own word for the service's state, or empty when none is installed.
	Service string `json:"service"`
	// ServiceRunning is the service manager saying it is running the daemon. Running without it is a
	// daemon somebody started by hand, whether or not a service is installed beside it.
	ServiceRunning bool `json:"service_running"`
	// Version is the running daemon's, or empty when none answered.
	Version string `json:"version"`
	// Standing is signed_in, starting or signed_out, or empty when none answered.
	Standing string `json:"standing"`
	// Instance and Username are who it is signed in as, or empty.
	Instance string `json:"instance"`
	Username string `json:"username"`
	// Problem is what is wrong when something is, in words for a person, or empty.
	Problem string `json:"problem"`
}

// exitCode is the command's exit: 0 running, 1 installed and not running, 2 neither. The three M3 gave it,
// with "running" now meaning that a daemon is, and not that the service manager started one.
func (v statusView) exitCode() int {
	switch {
	case v.Running:
		return 0
	case v.Installed:
		return 1
	default:
		return 2
	}
}

func viewOfStatus(state State, socket socketAnswer) statusView {
	v := statusView{
		Running:        socket.Running || (state.Installed && state.Running),
		Answering:      socket.Attached,
		Installed:      state.Installed,
		ServiceRunning: state.Installed && state.Running,
		Version:        socket.Version,
		Standing:       socket.Standing,
		Problem:        socket.Problem,
	}
	if state.Installed {
		v.Service = state.Detail
	}
	if socket.Account != nil {
		v.Instance, v.Username = socket.Account.InstanceURL, socket.Account.Username
	}
	// The service manager says it started one, and nothing is at the socket this command looked at.
	if !socket.Running && socket.Problem == "" && state.Installed && state.Running {
		v.Problem = "the service manager reports it running, and nothing answers on its socket: it is still " +
			"starting, or it was started with another state directory than this shell's (XDG_STATE_HOME)"
	}
	return v
}

// Text draws the status for a person. The service manager's word, the version, the instance and the name
// are all somebody else's text.
func (v statusView) Text(t *output.Text) {
	switch {
	case v.Running && v.ServiceRunning:
		t.Line("%s is running (%s).", ServiceName, output.Clean(v.Service))
	case v.Running && v.Installed:
		// Not "running (inactive)": the word is the service's, and the service is not what is running.
		t.Line("%s is running, started by hand. The installed service is not running (%s), and",
			ServiceName, output.Clean(v.Service))
		t.Line("`norite daemon start` cannot start it while this one runs: `norite daemon restart` replaces it.")
	case v.Running:
		t.Line("%s is running, started by hand: it is not installed as a service, so nothing starts it at login.", ServiceName)
		t.Line("Run `norite daemon install` to register it.")
	case v.Installed:
		t.Line("%s is installed but not running (%s).", ServiceName, output.Clean(v.Service))
		t.Line("Run `norite daemon start` to start it.")
	default:
		t.Line("%s is not installed.", ServiceName)
		t.Line("Run `norite daemon install` to register it with this machine's service manager.")
	}
	if v.Version != "" {
		t.Line("  version:  %s", output.Clean(v.Version))
	}
	switch v.Standing {
	case ipc.StandingSignedIn:
		t.Line("  account:  %s at %s", output.Clean(v.Username), output.Clean(v.Instance))
	case ipc.StandingStarting:
		t.Line("  account:  signing in")
	case ipc.StandingSignedOut:
		t.Line("  account:  not signed in; run `norite login`")
	}
	if v.Problem != "" {
		t.Line("  problem:  %s", output.Clean(v.Problem))
	}
	if v.Running || v.Installed {
		t.Line("Its log: norite logs tail")
	}
}

func statusCommand(version string) *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "report whether the daemon is running, installed, and signed in",
		Description: "Asks the service manager and the daemon itself. Exits 0 when a daemon is running,\n" +
			"however it was started; 1 when it is installed but not running; and 2 when it is neither —\n" +
			"so a script can branch on the exit code. --json prints the same as an object.",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			mgr, err := managerFor()
			if err != nil {
				return err
			}
			state, err := mgr.Status(ctx)
			if err != nil {
				return err
			}
			view := viewOfStatus(state, socketStatus(ctx, version))
			if err := output.Render(cmd.Root().Writer, cmd.Root().Bool("json"), view); err != nil {
				return err
			}
			if code := view.exitCode(); code != 0 {
				// The status is the output. The code says which, with no message of its own.
				return cli.Exit("", code)
			}
			return nil
		},
	}
}
