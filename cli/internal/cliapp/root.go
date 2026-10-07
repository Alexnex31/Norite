// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

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
	"os"
	"strings"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/configcmd"
	"github.com/Alexnex31/Norite/cli/internal/daemonctl"
	"github.com/Alexnex31/Norite/cli/internal/instanceadmin"
	"github.com/Alexnex31/Norite/cli/internal/instanceinit"
	"github.com/Alexnex31/Norite/cli/internal/login"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/cli/internal/output"
	"github.com/Alexnex31/Norite/cli/internal/tui"
	"github.com/Alexnex31/Norite/cli/internal/verbs"
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

// channelFlagName opens the terminal client on one channel rather than home (M20a). ESC still leads home.
const channelFlagName = "channel"

// interactive reports whether the root command reads from and writes to a terminal: the terminal client
// needs both, and the root's own writer is what a test substitutes.
func interactive(cmd *cli.Command) bool {
	out, ok := cmd.Root().Writer.(*os.File)
	return ok && term.IsTerminal(int(out.Fd())) && term.IsTerminal(int(os.Stdin.Fd()))
}

// New builds the root command.
func New(out, errOut io.Writer) *cli.Command {
	root := &cli.Command{
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
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if arg := cmd.Args().First(); arg != "" {
				// Taking over the not-found path means the library's own "did you mean" never fires, so
				// the suggestion is built here instead of being silently lost.
				// A usage error, exit 2 without the prefix, like every other mistake in a command line (M20).
				if suggestion := cli.SuggestCommand(cmd.Commands, arg); suggestion != "" && suggestion != arg {
					return clierr.Usage("unknown command %q; did you mean %q?", arg, suggestion)
				}
				return clierr.Usage("unknown command %q; run `norite --help` to see the available commands", arg)
			}
			// Bare `norite` on a terminal opens the client (M20a); anywhere else it shows what it can do, so a
			// script that ran it is unchanged. Both ends must be a terminal: the client reads keys from one
			// and draws on the other.
			channel := cmd.String(channelFlagName)
			if channel != "" && !ops.IsID(channel) {
				return clierr.Usage("--channel %q is not a channel id: ids are the numbers `norite channel list` prints",
					output.Clean(channel))
			}
			if !interactive(cmd) {
				if channel != "" {
					return fmt.Errorf("%w: --channel opens the terminal client, which needs one; "+
						"`norite message list %s` reads the channel without", clierr.ErrNoTerminal, channel)
				}
				return cli.ShowAppHelp(cmd)
			}
			return tui.Run(ctx, tui.Options{Dial: tui.DaemonDialer(Version), Channel: channel})
		},

		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  JSONFlagName,
				Usage: "print machine-readable JSON instead of human-formatted output",
			},
			&cli.StringFlag{
				Name:  channelFlagName,
				Usage: "open the terminal client on this channel `ID` rather than home",
				// The root's alone. urfave/cli v3 hands a root flag to every subcommand by default, so
				// `norite guild list --channel 5` parsed, ignored the flag and exited 0 — a flag a verb
				// accepts and does nothing with (M20a's manual pass). Local, it is a usage error there.
				Local: true,
			},
		},

		Commands: append([]*cli.Command{
			login.RegisterCommand(),
			login.Command(),
			login.LogoutCommand(),
			daemonctl.GroupCommand(),
			instanceinit.GroupCommand(instanceadmin.Command(), instanceadmin.InviteCommand()),
			licensesCommand(),
			aboutCommand(verbs.Daemon(Version)),
			configcmd.Command(),
		}, verbs.Commands(verbs.Daemon(Version))...),
	}
	refuseUnknownSubcommands(root.Commands)
	reportUsageErrors(root)
	return root
}

// reportUsageErrors makes a flag the command line got wrong — unknown, missing its value, a value of the
// wrong type — a usage error, on every command in the tree.
//
// urfave/cli's own handling exits 1 with the crash prefix and prints the command's help to stdout first,
// which puts a page of text into a pipeline that asked for --json. Its OnUsageError hook is what stops both,
// and it is per command, so it is set on all of them by the walk (M20 /code-review).
func reportUsageErrors(cmd *cli.Command) {
	cmd.OnUsageError = func(_ context.Context, c *cli.Command, err error, _ bool) error {
		name := c.FullName()
		return clierr.Usage("%s; see `%s --help`", err, name)
	}
	for _, sub := range cmd.Commands {
		reportUsageErrors(sub)
	}
}

// refuseUnknownSubcommands gives every group without an action of its own the root's: a mistyped
// subcommand is a usage error, with the nearest name offered, and a bare group shows its help.
//
// urfave/cli's default for a group is its help command, which answers an unknown name with "No help topic"
// and exit 3 — and since M20, 3 means the daemon is unavailable, so `norite guild lsit` read to a script
// as "wait and retry" (M20 /code-review). Applied across the whole tree rather than per group, so a group
// added later cannot forget it.
func refuseUnknownSubcommands(cmds []*cli.Command) {
	for _, c := range cmds {
		if len(c.Commands) == 0 {
			continue
		}
		refuseUnknownSubcommands(c.Commands)
		if c.Action != nil {
			continue
		}
		c.Action = func(_ context.Context, cmd *cli.Command) error {
			arg := cmd.Args().First()
			if arg == "" {
				return cli.ShowSubcommandHelp(cmd)
			}
			name := strings.TrimPrefix(cmd.FullName(), "norite ")
			if suggestion := cli.SuggestCommand(cmd.Commands, arg); suggestion != "" && suggestion != arg {
				return clierr.Usage("unknown command %q in `norite %s`; did you mean %q?", arg, name, suggestion)
			}
			return clierr.Usage("unknown command %q in `norite %s`; run `norite %s --help` to see its commands",
				arg, name, name)
		}
	}
}
