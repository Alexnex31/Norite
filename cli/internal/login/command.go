package login

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/daemon/credentials"
)

// Command builds `norite login`.
//
// Mounted at the top level rather than under a group: it is the first thing anyone runs, and
// `norite auth login` would be a group with one member for the sake of symmetry nobody is asking for.
func Command() *cli.Command {
	return &cli.Command{
		Name:  "login",
		Usage: "Sign in to a Norite instance and store the credential for the daemon",
		Description: "Signs in with an email address and password, and stores the resulting credential\n" +
			"where the background daemon will find it on its next start.\n\n" +
			"The password is read without echo and is never accepted as a flag — a flag value is\n" +
			"visible in the process list to every other user on the machine. For scripted use, set\n" +
			passwordEnvVar + " instead.\n\n" +
			"Signing in again on the same machine replaces the stored credential and keeps this\n" +
			"installation's device identity, so the account's other sessions are untouched.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "instance",
				Usage: "`URL` of the instance to sign in to (default: the one from the last login)",
			},
			&cli.StringFlag{
				Name:  "email",
				Usage: "`EMAIL` of the account (default: ask)",
			},
			&cli.StringFlag{
				Name:  "device-name",
				Usage: "`NAME` for this device in the account's session list (default: this machine's hostname)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			store, err := credentials.Open()
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}

			readLine, readSecret, interactive := terminalReaders(os.Stdin, cmd.Writer)
			runner := &Runner{
				Options: Options{
					Instance:   cmd.String("instance"),
					Email:      cmd.String("email"),
					DeviceName: cmd.String("device-name"),
				},
				Store:       store,
				Out:         cmd.Writer,
				ReadLine:    readLine,
				ReadSecret:  readSecret,
				Interactive: interactive,
				Hostname:    os.Hostname,
				NewDeviceID: credentials.NewDeviceID,
			}

			if err := runner.Run(ctx); err != nil {
				// Returned unwrapped, deliberately. `main` recognizes this one and exits 2 without the
				// "norite:" prefix, exactly as it does for the wizard's equivalent — the prefix makes a
				// usage problem read like an internal failure, and "you are not at a terminal" is the
				// clearest possible usage problem. Wrapping it in cli.Exit here would keep the code and
				// lose the identity main matches on.
				if errors.Is(err, ErrNoTerminal) {
					return err
				}
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

// LogoutCommand builds `norite logout`.
//
// Here rather than at a later milestone because a credential this command can store and nothing can remove
// is half a feature: someone who signs in to the wrong instance, or on a machine they are handing over,
// needs a way to take it back that is not "find the keyring entry yourself".
func LogoutCommand() *cli.Command {
	return &cli.Command{
		Name:  "logout",
		Usage: "Remove this machine's stored credential",
		Description: "Removes the credential `norite login` stored, so the daemon has nothing to start\n" +
			"with. This is a local action: it does not revoke the session on the instance, which is\n" +
			"what `norite daemon stop` and the account's session list are for.",
		Action: func(_ context.Context, cmd *cli.Command) error {
			store, err := credentials.Open()
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}

			record, err := store.LoadRecord()
			switch {
			case errors.Is(err, credentials.ErrNoCredential):
				// Not an error. Logging out when already logged out is a no-op someone should be able to
				// put in a script without guarding it.
				_, _ = fmt.Fprintln(cmd.Writer, "No stored credential; nothing to remove.")
				return nil
			case err != nil:
				return cli.Exit(err.Error(), 1)
			}

			if err := store.Clear(); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			_, _ = fmt.Fprintf(cmd.Writer, "Removed the credential for %s on %s.\n",
				record.Username, record.InstanceURL)
			return nil
		},
	}
}
