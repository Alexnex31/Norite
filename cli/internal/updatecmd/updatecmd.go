// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package updatecmd is `norite update`: what the daemon knows about a newer release, and telling it what
// to do about one (M24, ADR 0020).
//
// The daemon does the work, since it is the program that is replaced and the one that must stop cleanly
// first. These commands ask it over the attach socket and print what it answers. Every string in an answer
// is the daemon's, and parts of one began as a release host's or an instance's words, so each is cleaned
// before it is printed as text (rule 19).
package updatecmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/output"
	"github.com/Alexnex31/Norite/daemon/installreceipt"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// Connector attaches to the daemon. verbs.Daemon is the real one.
type Connector func(ctx context.Context) (daemonclient.Caller, func(), error)

// The seams the tests use: where a receipt is written and removed, and the zone a time is shown in.
var (
	recordReceipt = installreceipt.Record
	forgetReceipt = installreceipt.ForgetIfNames
	zone          = time.Local
)

// Command returns the `norite update` group.
func Command(connect Connector) *cli.Command {
	return &cli.Command{
		Name:  "update",
		Usage: "See whether a newer Norite is out, and install it",
		Description: "The daemon checks this project's releases once a day. A release is installed only\n" +
			"after its signature has been verified, on this machine, as the one this project's release\n" +
			"workflow made; anything else is refused and the version you have keeps running. A version\n" +
			"that does not stay up after it is installed is undone by itself.\n\n" +
			"An install made by Norite's installer updates itself unless you say otherwise; on Windows\n" +
			"it tells you and waits for `norite update apply`, unless you choose `mode auto`. Any other\n" +
			"install is told when a release is out, and is updated by whatever installed it.\n\n" +
			"The check asks GitHub for the list of releases and sends nothing but this program's version.\n" +
			"`norite update mode off` stops it.",
		Commands: []*cli.Command{
			{
				Name:  "status",
				Usage: "Say which version runs, and what is known about a newer one",
				Action: run(connect, nil, func(ctx context.Context, _ *cli.Command, c daemonclient.Caller) (ipc.UpdateStatus, error) {
					var st ipc.UpdateStatus
					return st, daemonclient.LocalRead(ctx, c, ipc.PathUpdate, &st)
				}),
			},
			{
				Name:  "check",
				Usage: "Look for a newer release now",
				Action: run(connect, nil, func(ctx context.Context, _ *cli.Command, c daemonclient.Caller) (ipc.UpdateStatus, error) {
					var st ipc.UpdateStatus
					return st, daemonclient.Local(ctx, c, ipc.PathUpdateCheck, &st)
				}),
			},
			{
				Name:  "apply",
				Usage: "Download, verify and install the newer release",
				Description: "The daemon restarts as the new version when it has installed it. Attached clients\n" +
					"are disconnected for a moment, and an older `norite` left open is told to restart.",
				Action: run(connect, nil, apply),
			},
			{
				Name:      "mode",
				Usage:     "Choose what happens when a release is out: auto, notify or off",
				ArgsUsage: "auto|notify|off",
				Description: "auto installs a verified release by itself. notify checks, says so, and waits for\n" +
					"`norite update apply`. off checks nothing unless you ask.",
				Action: run(connect, []string{ipc.UpdateAuto, ipc.UpdateNotify, ipc.UpdateOff},
					func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.UpdateStatus, error) {
						var st ipc.UpdateStatus
						return st, daemonclient.Local(ctx, c, ipc.PathUpdateMode+cmd.Args().First(), &st)
					}),
			},
			{
				Name:      "channel",
				Usage:     "Choose which releases are offered: stable or prerelease",
				ArgsUsage: "stable|prerelease",
				Description: "stable is releases only. prerelease also offers alphas, betas and release\n" +
					"candidates, which are for testing and may not talk to your instance.",
				Action: run(connect, []string{ipc.ChannelStable, ipc.ChannelPrerelease},
					func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.UpdateStatus, error) {
						var st ipc.UpdateStatus
						return st, daemonclient.Local(ctx, c, ipc.PathUpdateChannel+cmd.Args().First(), &st)
					}),
			},
			{
				// For the installer scripts, which know where they put the programs and should not be
				// writing JSON with a path in it from a shell.
				Name:   "receipt",
				Hidden: true,
				Usage:  "Record where an installer put the programs, or forget it",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "bin-dir", Usage: "the `DIR` holding norite and norite-daemon"},
					&cli.StringFlag{Name: "by", Usage: "the installer's `NAME`"},
					&cli.BoolFlag{Name: "forget", Usage: "remove the receipt, if it names --bin-dir"},
				},
				Action: receipt,
			},
		},
	}
}

// run wraps one request to the daemon and the printing of its answer. words is the fixed set a command's
// one argument is taken from, or nil for a command that takes none.
//
// The argument is checked here, before anything is attached to, for two reasons. A mistyped word is a
// usage error on a machine with no daemon running too, and not "the daemon is not running". And a request
// to the daemon carries nothing of the user's in its path, because a daemon from before these requests
// would relay that path to its instance: only one of the words is ever sent.
func run(connect Connector, words []string, ask func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.UpdateStatus, error)) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		switch {
		case words == nil && cmd.Args().Len() != 0:
			return clierr.Usage("norite update %s takes no arguments", cmd.Name)
		case words != nil && cmd.Args().Len() != 1:
			return clierr.Usage("norite update %s takes one word: %s", cmd.Name, cmd.ArgsUsage)
		case words != nil && !slices.Contains(words, cmd.Args().First()):
			return clierr.Usage("%q is not one of: %s", output.Clean(cmd.Args().First()), cmd.ArgsUsage)
		}
		if connect == nil {
			return errors.New("this build has no way to reach the daemon")
		}
		c, detach, err := connect(ctx)
		if err != nil {
			return err
		}
		defer detach()
		st, err := ask(ctx, cmd, c)
		if err != nil {
			return err
		}
		return output.Render(output.Writer(cmd.Root().Writer), cmd.Root().Bool("json"), view{UpdateStatus: st, applying: cmd.Name == "apply"})
	}
}

// apply asks for the newer release to be installed. A daemon that installs nothing refuses, with its
// reason.
func apply(ctx context.Context, _ *cli.Command, c daemonclient.Caller) (ipc.UpdateStatus, error) {
	var st ipc.UpdateStatus
	return st, daemonclient.Local(ctx, c, ipc.PathUpdateApply, &st)
}

func receipt(_ context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() != 0 {
		return clierr.Usage("norite update receipt takes no arguments")
	}
	dir := cmd.String("bin-dir")
	if dir == "" {
		return clierr.Usage("--bin-dir is needed")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	// One receipt per account, for the install its daemon runs from. Forgetting names a directory so that
	// an installer run against another one, a server's programs in /opt or a test's in a temporary
	// directory, leaves the real install's receipt alone (M24 /code-review).
	if cmd.Bool("forget") {
		return forgetReceipt(abs)
	}
	// A receipt names a directory that holds the two programs, or the daemon would be told it may write
	// somewhere it has never run from.
	for _, name := range []string{"norite", "norite-daemon"} {
		if !isFile(filepath.Join(abs, name)) && !isFile(filepath.Join(abs, name+".exe")) {
			return fmt.Errorf("%s holds no %s, so it is not where Norite was installed", output.Clean(abs), name)
		}
	}
	by := cmd.String("by")
	if by == "" {
		by = "an installer"
	}
	return recordReceipt(installreceipt.Receipt{BinDir: abs, By: by})
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// view is an UpdateStatus as these commands print it (contracts/cli-json/update.schema.json).
type view struct {
	ipc.UpdateStatus
	// applying is that this is the answer to `apply`: the daemon is about to install Available.
	applying bool
}

// Text draws it for a person.
func (v view) Text(t *output.Text) {
	version, available := output.Clean(v.Version), output.Clean(v.Available)
	switch {
	case v.applying && v.Staging != "":
		// Not "installing": the download may yet fail, or its signature be refused.
		t.Line("Downloading %s. It is installed once it is verified; `norite update status` says how it went.", output.Clean(v.Staging))
		return
	case v.applying:
		t.Line("Installing %s. The daemon restarts as it in a moment; `norite update status` says how it went.", available)
		return
	case v.Trial != "":
		t.Line("Norite %s was just installed. It is kept once it has stayed up for ten minutes.", output.Clean(v.Trial))
	default:
		t.Line("Norite %s is running.", version)
	}
	if v.Staging != "" {
		t.Line("%s is being downloaded and verified.", output.Clean(v.Staging))
	}
	if v.RolledBack != "" {
		t.Line("%s was installed, did not stay up, and was undone. It will not be installed again.", output.Clean(v.RolledBack))
	}

	switch {
	case v.Available != "" && v.Installs && v.Mode == ipc.UpdateAuto:
		t.Line("%s is out and will be installed by itself. `norite update apply` installs it now.", available)
	case v.Available != "" && v.Installs:
		t.Line("%s is out. `norite update apply` installs it.", available)
	case v.Available != "":
		t.Line("%s is out.", available)
	case v.LastCheck == "":
		t.Line("No check for a newer release has been made yet. `norite update check` makes one.")
	default:
		t.Line("No newer release, as of %s.", when(v.LastCheck))
	}
	if !v.Installs {
		t.Line("This daemon does not install releases: %s.", output.Clean(v.Reason))
	}

	channel := "releases only"
	if v.Channel == ipc.ChannelPrerelease {
		channel = "pre-releases too"
	}
	switch v.Mode {
	case ipc.UpdateOff:
		t.Line("Updates are off: nothing is checked unless you ask (%s).", channel)
	case ipc.UpdateNotify:
		t.Line("Updates: checked once a day and installed when you ask (%s).", channel)
	default:
		t.Line("Updates: checked once a day and installed by themselves (%s).", channel)
	}
	if v.LastError != "" {
		t.Line("The last check or installation failed: %s.", output.Clean(v.LastError))
	}
}

// when draws an RFC 3339 time in the reader's zone, or as it came when it is not one.
func when(stamp string) string {
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return output.Clean(stamp)
	}
	return at.In(zone).Format("2006-01-02 15:04")
}
