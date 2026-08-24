package instanceadmin

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// Command builds `norite instance bootstrap`.
func Command() *cli.Command {
	return &cli.Command{
		Name:  "bootstrap",
		Usage: "Create this instance's first administrator",
		Description: "Creates the account this instance is administered from, on an instance that has\n" +
			"none yet.\n\n" +
			"Run it after the backend is up: the order for a new instance is `norite instance init`,\n" +
			"apply the migrations, start the server, then this.\n\n" +
			"Authority comes from this machine's copy of the instance configuration — the signing key\n" +
			"in it is what proves you administer this instance, so the command must run somewhere\n" +
			"that file is readable. It works exactly once; afterwards the instance refuses, and\n" +
			"further accounts are created by registration or by an invite.\n\n" +
			"The password is read from the terminal, or from " + passwordEnvVar + " for an unattended\n" +
			"run. There is deliberately no flag for it: a flag value is visible in the process list to\n" +
			"every other user on the machine.",
		Flags: append(configFlags(),
			&cli.StringFlag{Name: "username", Usage: "the administrator's `USERNAME`"},
			&cli.StringFlag{Name: "email", Usage: "the administrator's `EMAIL` address"},
			&cli.StringFlag{
				Name:  "display-name",
				Usage: "the administrator's display `NAME`, defaulting to the username",
			},
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			runner := runnerFrom(cmd)
			runner.Options.Username = cmd.String("username")
			runner.Options.Email = cmd.String("email")
			runner.Options.DisplayName = cmd.String("display-name")
			return runner.Run(ctx)
		},
	}
}

// terminalReaders builds the real terminal's readers.
//
// The same shape `norite login` uses: a secret is read with term.ReadPassword so it is never echoed, and
// whether a terminal is present is decided once here rather than guessed at each prompt.
func terminalReaders(in *os.File, out io.Writer) (readLine func(string) (string, error),
	readSecret func(string) (string, error), interactive bool,
) {
	fd := int(in.Fd())
	interactive = term.IsTerminal(fd)

	readLine = lineReader(in, out)

	readSecret = func(prompt string) (string, error) {
		_, _ = fmt.Fprint(out, prompt)
		b, err := term.ReadPassword(fd)
		// The newline the terminal did not echo, so the next line does not begin on the prompt.
		_, _ = fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}

	return readLine, readSecret, interactive
}

// configFlags are the two every instance-administration command needs: where the configuration is, and
// where the instance answers.
//
// Shared rather than repeated so the wording stays identical across the group — a flag documented three
// slightly different ways reads as three different flags.
func configFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:  "config",
			Usage: "read the instance configuration from `PATH` instead of the usual location",
		},
		&cli.StringFlag{
			Name: "instance",
			Usage: "instance `URL` to reach, if it differs from the configured public_base_url " +
				"(a server behind a proxy often answers on localhost)",
		},
	}
}

// runnerFrom builds a Runner wired to the real terminal and the flags this invocation carried.
//
// jsonFlagName is read off the root command rather than declared here: --json is a promise about the whole
// CLI, made once at the root (see cliapp.JSONFlagName). It is spelled out rather than imported because
// cliapp mounts this package, so importing it back would be a cycle — cliapp's root_test asserts the flag
// exists under that name, which is what keeps the two in step.
func runnerFrom(cmd *cli.Command) *Runner {
	readLine, readSecret, interactive := terminalReaders(os.Stdin, cmd.Writer)

	return &Runner{
		Options: Options{
			ConfigPath: cmd.String("config"),
			Instance:   cmd.String("instance"),
		},
		Out:         cmd.Writer,
		ReadLine:    readLine,
		ReadSecret:  readSecret,
		Interactive: interactive,
		JSON:        cmd.Root().Bool(jsonFlagName),
	}
}

// jsonFlagName must match cliapp.JSONFlagName. See runnerFrom.
const jsonFlagName = "json"
