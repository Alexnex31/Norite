// Package cliapp assembles the `norite` command tree.
//
// Every command the CLI will grow mounts here. Keeping the tree in its own package, rather than in
// cmd/app, means it can be built and exercised in tests without running a process — which is how the
// help output and flag plumbing are tested (root_test.go).
package cliapp

import (
	"context"
	"fmt"
	"io"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/daemonctl"
	"github.com/Alexnex31/Norite/cli/internal/instanceadmin"
	"github.com/Alexnex31/Norite/cli/internal/instanceinit"
	"github.com/Alexnex31/Norite/cli/internal/login"
)

// Version is the build's version string, overridden at link time by goreleaser.
var Version = "dev"

// JSONFlagName is the global flag every data-printing command honors.
//
// It is defined once, at the root, rather than repeated per command: `--json` is a promise about the
// whole CLI, and its output shape is a versioned source-of-truth contract in contracts/cli-json/, on the
// same footing as openapi.yaml (CLAUDE.md rule 15). Commands that print data read it from the root
// command; `norite instance init` is a conversation rather than a data-printing command, so it has no JSON
// form and none is faked for it.
const JSONFlagName = "json"

// New builds the root command.
func New(out, errOut io.Writer) *cli.Command {
	return &cli.Command{
		Name:    "norite",
		Usage:   "Norite — voice and text chat",
		Version: Version,
		Description: "The Norite command-line client — the scriptable command tree.\n\n" +
			"Most commands talk to a local daemon that holds the connection to your instance. Instance\n" +
			"administration commands, `norite instance init` among them, work on this machine's own\n" +
			"configuration and need no daemon and no network.",
		Writer:    out,
		ErrWriter: errOut,

		// Shell completion is wired on from the start rather than retrofitted: a CLI meant to be
		// scriptable is judged partly on whether its commands complete, and the cost here is one field.
		EnableShellCompletion: true,

		// Offer the nearest command on a typo. Cheap, and this tree will grow large enough to need it.
		Suggest: true,

		// Take over urfave/cli's exit handling. Its default, HandleExitCoder, prints the error and calls
		// os.Exit from *inside* Run — so a command returning cli.Exit(…) would terminate the process
		// without Run ever returning, silently bypassing cmd/app/main.go, which is supposed to be the one
		// place process lifetime and exit codes are decided. It would also make any such command
		// untestable, since the test binary would exit along with it.
		//
		// A no-op handler leaves the ExitCoder to travel back as an ordinary error; main unwraps it and
		// exits with the code it carries. Commands keep using cli.Exit — only who acts on it changes.
		ExitErrHandler: func(context.Context, *cli.Command, error) {},

		// A mistyped command must fail, not print help and succeed. The default behavior returns no error,
		// so `norite instnace init && echo ok` would print "ok" without having configured anything — exactly
		// the kind of silent success a scriptable CLI must never produce.
		Action: func(_ context.Context, cmd *cli.Command) error {
			if arg := cmd.Args().First(); arg != "" {
				// Taking over the not-found path means the library's own "did you mean" never fires, so
				// the suggestion is built here instead of being silently lost.
				if suggestion := cli.SuggestCommand(cmd.Commands, arg); suggestion != "" && suggestion != arg {
					return fmt.Errorf("unknown command %q; did you mean %q?", arg, suggestion)
				}
				return fmt.Errorf("unknown command %q; run `norite --help` to see the available commands", arg)
			}
			// Bare `norite` with no arguments: show what it can do rather than erroring.
			return cli.ShowAppHelp(cmd)
		},

		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  JSONFlagName,
				Usage: "print machine-readable JSON instead of human-formatted output",
			},
		},

		Commands: []*cli.Command{
			login.Command(),
			login.LogoutCommand(),
			daemonctl.GroupCommand(),
			instanceinit.GroupCommand(instanceadmin.Command(), instanceadmin.InviteCommand()),
		},
	}
}
