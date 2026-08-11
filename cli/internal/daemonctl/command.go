package daemonctl

import (
	"context"
	"fmt"
	"io"

	"github.com/urfave/cli/v3"
)

const daemonBinaryFlag = "daemon-binary"

// managerFor builds the Manager each action operates through.
//
// A variable so tests can substitute a fake and assert what the commands actually print and exit with.
// The alternative — threading a Manager through GroupCommand into the root command tree — would put a
// test-only parameter in the CLI's public assembly, which is a worse trade for one package.
var managerFor = func() (Manager, error) { return New(nil) }

// GroupCommand returns the `norite daemon` command group.
//
// Install and start are separate verbs rather than one "make it go" command. Provisioning a machine image
// wants install without start; a user recovering from a crash wants start without touching the definition;
// and a single fused command would have to guess which of those it was being asked for.
func GroupCommand() *cli.Command {
	return &cli.Command{
		Name:  "daemon",
		Usage: "manage the background daemon this machine's clients attach to",
		Description: "The daemon holds the connection to your instance, your presence, and (later) plugins\n" +
			"and end-to-end encryption keys. One runs per OS user account, as a service of your own\n" +
			"login session — installing it never needs administrator rights.\n\n" +
			"The CLI and GUI attach to it; they do not replace it.",
		Commands: []*cli.Command{
			installCommand(),
			uninstallCommand(),
			startCommand(),
			stopCommand(),
			restartCommand(),
			statusCommand(),
		},
	}
}

func installCommand() *cli.Command {
	return &cli.Command{
		Name:  "install",
		Usage: "register the daemon with this machine's service manager",
		Description: "Writes a systemd user unit, a launchd agent, or a logon task depending on the\n" +
			"platform, so the daemon starts automatically at login. It does not start it now —\n" +
			"run `norite daemon start` for that.\n\n" +
			"Safe to run again: an existing definition is replaced.",
		Flags: []cli.Flag{
			// Deliberately no Sources: cli.EnvVars(...) here. LocateDaemon already consults
			// NORITE_DAEMON_BINARY, one step below this flag, and having both read it would mean the
			// environment value arrives as if it had been typed — so a bad path in the environment produces
			// "the path given on the command line names /bad/path", pointing at a command line the user
			// never wrote. One mechanism owns the variable; this flag just documents it.
			&cli.StringFlag{
				Name: daemonBinaryFlag,
				Usage: "`PATH` of the norite-daemon executable to register (default: " +
					DaemonBinaryEnvVar + ", then next to this binary, then PATH)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			mgr, err := managerFor()
			if err != nil {
				return err
			}

			binary, err := LocateDaemon(cmd.String(daemonBinaryFlag))
			if err != nil {
				return err
			}

			if err := mgr.Install(ctx, binary); err != nil {
				return err
			}

			out := cmd.Root().Writer
			fprintf(out, "Installed %s.\n", ServiceName)
			fprintf(out, "  executable: %s\n", binary)
			if path, err := mgr.DefinitionPath(); err == nil && path != "" {
				fprintf(out, "  definition: %s\n", path)
			}
			// What follows differs by platform because the platforms differ: launchd starts the agent as
			// part of loading it, and no amount of wishing makes install-without-start available there.
			// Printing the systemd wording everywhere would tell a macOS user to start a daemon that is
			// already running.
			if mgr.StartsOnInstall() {
				fprintf(out, "\nIt is running now, and will start automatically at login.\n")
			} else {
				fprintf(out, "\nIt will start automatically at login. To start it now:\n")
				fprintf(out, "  norite daemon start\n")
			}
			return nil
		},
	}
}

func uninstallCommand() *cli.Command {
	return &cli.Command{
		Name:  "uninstall",
		Usage: "stop the daemon and remove it from the service manager",
		Description: "Removes the service definition. Your configuration and stored credentials are left\n" +
			"alone — this undoes `norite daemon install`, nothing more.",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			mgr, err := managerFor()
			if err != nil {
				return err
			}
			if err := mgr.Uninstall(ctx); err != nil {
				return err
			}
			fprintf(cmd.Root().Writer, "Removed %s from this machine's service manager.\n", ServiceName)
			return nil
		},
	}
}

func startCommand() *cli.Command {
	return &cli.Command{
		Name:   "start",
		Usage:  "start the daemon now",
		Action: simpleAction("Started", func(m Manager) func(context.Context) error { return m.Start }),
	}
}

func stopCommand() *cli.Command {
	return &cli.Command{
		Name:  "stop",
		Usage: "stop the daemon now",
		Description: "The daemon stays installed and will start again at your next login. To prevent that,\n" +
			"use `norite daemon uninstall`.",
		Action: simpleAction("Stopped", func(m Manager) func(context.Context) error { return m.Stop }),
	}
}

func restartCommand() *cli.Command {
	return &cli.Command{
		Name:  "restart",
		Usage: "stop the daemon and start it again",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			mgr, err := managerFor()
			if err != nil {
				return err
			}
			// Stop failures are surfaced rather than swallowed: if the daemon could not be stopped, starting
			// it again either does nothing or produces a second one, and both are worse than saying so.
			if err := mgr.Stop(ctx); err != nil {
				return err
			}
			if err := mgr.Start(ctx); err != nil {
				return err
			}
			fprintf(cmd.Root().Writer, "Restarted %s.\n", ServiceName)
			return nil
		},
	}
}

func statusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "report whether the daemon is installed and running",
		Description: "Exits 0 when the daemon is running, 1 when it is installed but stopped, and 2 when it\n" +
			"is not installed — so a script can branch on the exit code without parsing this output.",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			mgr, err := managerFor()
			if err != nil {
				return err
			}
			state, err := mgr.Status(ctx)
			if err != nil {
				return err
			}

			out := cmd.Root().Writer
			switch {
			case !state.Installed:
				fprintf(out, "%s is not installed.\n", ServiceName)
				fprintf(out, "Run `norite daemon install` to register it with this machine's service manager.\n")
				// A distinct exit code per state, so `norite daemon status` is usable as a condition. Exit
				// codes are the machine-readable surface here; --json arrives with the CLI's JSON output
				// machinery at M46 (docs/architecture.md §4), and inventing a one-off shape for it now would
				// mean shipping a contract that the real one has to break.
				return cli.Exit("", 2)
			case !state.Running:
				fprintf(out, "%s is installed but not running (%s).\n", ServiceName, state.Detail)
				fprintf(out, "Run `norite daemon start` to start it.\n")
				return cli.Exit("", 1)
			default:
				fprintf(out, "%s is running (%s).\n", ServiceName, state.Detail)
				if hint := mgr.LogHint(); hint != "" {
					fprintf(out, "Logs: %s\n", hint)
				}
				return nil
			}
		},
	}
}

// simpleAction builds the action for a command that calls one Manager method and prints one line.
func simpleAction(pastTense string, pick func(Manager) func(context.Context) error) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		mgr, err := managerFor()
		if err != nil {
			return err
		}
		// Errors pass through unwrapped: ErrNotInstalled already carries the "run install first" advice, and
		// a failure from the service manager already quotes the exact command that failed. A second layer of
		// explanation on top of either would only bury the useful part.
		if err := pick(mgr)(ctx); err != nil {
			return err
		}
		fprintf(cmd.Root().Writer, "%s %s.\n", pastTense, ServiceName)
		return nil
	}
}

// fprintf writes to w, discarding the write error.
//
// The same justification as the wizard's prompter (cli/internal/instanceinit/prompt.go): a failed write to
// a closed stdout cannot be reported anywhere, and swallowing it in one named helper is honest, whereas
// ignoring it at twenty call sites is what errcheck exists to catch.
func fprintf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
