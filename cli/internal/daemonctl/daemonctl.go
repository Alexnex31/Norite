// Package daemonctl installs and controls the Norite daemon as an OS-level service.
//
// The daemon runs as a real service of the user's own session — a systemd *user* unit, a launchd *agent*, a
// Windows scheduled task at logon — never as a system-wide service. That follows from one process per OS
// user account (ADR 0010): the daemon holds that account's tokens and, later, its E2E keystore, so it has
// to run as that account and be installable without root.
//
// # Why this lives in the CLI
//
// The CLI is the operator's interface to the daemon, and it is what the install instructions point at. The
// GUI will eventually want the same operations, but cli/ and daemon/ are separate Go modules that cannot
// import each other, and the alternative — a shared module dependency — is a real cost to pay ahead of a
// need that arrives at Milestone M78. When it does, the GUI shelling out to `norite daemon install` is
// likely to be the right answer anyway, since that is the path that gets exercised.
//
// # Why every backend shells out
//
// systemd, launchd, and Task Scheduler all have richer native interfaces than their command-line tools.
// None of them is reachable from pure Go without cgo or a large dependency, and cgo is reserved for the
// voice worker alone (CLAUDE.md tech stack). Shelling out to systemctl/launchctl/schtasks is what every
// comparable tool does, it is what an operator would type by hand, and — because the commands appear
// verbatim in error messages — it leaves them able to reproduce and debug any failure themselves.
package daemonctl

import (
	"context"
	"errors"
	"fmt"
	"runtime"
)

// ServiceName is the identifier the service is registered under.
//
// One name across all three platforms, adapted only where a platform demands its own shape (the launchd
// label is reverse-DNS, the Windows task is title-cased). Changing it later orphans every already-installed
// service, which is why it is a constant here rather than an option.
const ServiceName = "norite-daemon"

const (
	launchdLabel    = "com.norite.daemon"
	windowsTaskName = "Norite Daemon"
)

// ErrUnsupportedPlatform reports that no service backend exists for the running OS.
var ErrUnsupportedPlatform = errors.New("no service manager backend for this platform")

// ErrNotInstalled reports an operation that needs an installed service when none is installed.
var ErrNotInstalled = errors.New("the daemon service is not installed; run `norite daemon install` first")

// State is what a service manager reports about the daemon.
type State struct {
	// Installed is whether a service definition exists.
	Installed bool
	// Running is whether the daemon is up right now. Only meaningful when Installed.
	Running bool
	// Detail is the manager's own one-line description, shown verbatim so an operator sees what the
	// platform said rather than our paraphrase of it.
	Detail string
}

// Manager drives one platform's service manager.
//
// Every method is safe to call more than once with the same outcome: install over an existing install
// replaces the definition, start on a running daemon succeeds, stop on a stopped one succeeds. Service
// management is exactly the kind of thing people run twice when the first run's output scrolled past, and
// it is scripted in provisioning tooling that cannot easily check first.
type Manager interface {
	// Install writes the service definition and enables it for future logins.
	//
	// Whether it also starts the daemon is the platform's decision, not ours — see StartsOnInstall.
	Install(ctx context.Context, daemonBinary string) error
	// Uninstall stops the daemon if it is running and removes the service definition.
	Uninstall(ctx context.Context) error
	// Start starts the daemon now.
	Start(ctx context.Context) error
	// Stop stops the daemon now, leaving it installed.
	Stop(ctx context.Context) error
	// Status reports what the manager knows.
	Status(ctx context.Context) (State, error)

	// StartsOnInstall reports whether Install leaves the daemon running.
	//
	// It exists because launchd gives no choice: `launchctl bootstrap` loads the agent, and loading one
	// with RunAtLoad set runs it. systemd and Task Scheduler both register without starting, which is what
	// this CLI wants — install and start are separate verbs, and provisioning an image needs the first
	// without the second.
	//
	// Rather than have `install` print advice that is wrong on one of three platforms, or fake uniformity
	// by starting and then immediately stopping the daemon, the difference is reported and the command
	// says what actually happened.
	StartsOnInstall() bool

	// DefinitionPath is where the service definition lives, for display. Empty when the platform keeps it
	// somewhere that is not a file the user can look at (Windows).
	DefinitionPath() (string, error)
	// LogHint is the platform-native command for reading the service's own log capture, shown after a
	// successful install. Norite's own rotated log file is reported separately.
	LogHint() string
}

// New returns the Manager for the running platform.
func New(r Runner) (Manager, error) { return newFor(runtime.GOOS, r) }

// newFor returns the Manager for a named GOOS.
//
// The platform is a parameter so that all three backends are constructible, and their command lines
// assertable, from a test on any one platform. Service definitions are otherwise the classic thing that
// stays broken on the platform the author does not run.
func newFor(goos string, r Runner) (Manager, error) {
	if r == nil {
		r = ExecRunner{}
	}
	switch goos {
	case "linux":
		return &systemdUser{run: r}, nil
	case "darwin":
		return &launchdAgent{run: r}, nil
	case "windows":
		return &windowsTask{run: r}, nil
	default:
		// FreeBSD and friends: the daemon binary itself runs fine, there is simply no service definition
		// we know how to write. Say that rather than pretending, and leave the user able to run
		// norite-daemon under whatever supervisor they already have.
		return nil, fmt.Errorf("%w (%s); the daemon binary itself still runs: start %s directly", ErrUnsupportedPlatform, goos, ServiceName)
	}
}
