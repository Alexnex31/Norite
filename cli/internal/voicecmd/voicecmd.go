// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package voicecmd is `norite voice`: joining a voice channel, leaving it, and how the call stands (M28;
// M34 adds mute, deafen and the rest).
//
// The daemon does the work: it starts the voice-worker, which owns the call's media, and carries its
// signaling. These commands ask it over the attach socket and print what it answers. A reason in an
// answer may be the voice server's words or the worker's, so every string is cleaned before it is printed
// as text (rule 19).
package voicecmd

import (
	"context"
	"errors"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/cli/internal/output"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// Connector attaches to the daemon. verbs.Daemon is the real one.
type Connector func(ctx context.Context) (daemonclient.Caller, func(), error)

// Command returns the `norite voice` group.
func Command(connect Connector) *cli.Command {
	return &cli.Command{
		Name:  "voice",
		Usage: "Join a voice channel, leave it, and see how the call stands",
		Description: "The daemon runs each call in a process of its own, norite-voice, so a problem with\n" +
			"audio never takes messaging down with it. One call at a time: joining another channel\n" +
			"leaves the one you are in.",
		Commands: []*cli.Command{
			{
				Name:   "status",
				Usage:  "Say whether you are in a call, and how the last one ended",
				Action: run(connect, false, status),
			},
			{
				Name:      "join",
				Usage:     "Join a voice channel",
				ArgsUsage: "CHANNEL_ID",
				Action:    run(connect, true, join),
			},
			{
				Name:   "leave",
				Usage:  "Leave the call you are in",
				Action: run(connect, false, leave),
			},
		},
	}
}

func status(ctx context.Context, _ *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error) {
	var st ipc.VoiceStatus
	return st, daemonclient.LocalRead(ctx, c, ipc.PathVoice, &st)
}

// join asks how voice stands before it sends the channel: the id is the user's, and a daemon from before
// these requests would relay the path to its instance (the ledger's entry on the prefix). A daemon that
// answers GET knows the join too. One that cannot join at all is refused here, and is never sent the id.
func join(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error) {
	st, err := status(ctx, cmd, c)
	if err != nil {
		return st, err
	}
	if !st.Available {
		return st, &clierr.RefusedError{Message: output.Clean(st.Unavailable)}
	}
	return st, daemonclient.Local(ctx, c, ipc.PathVoiceJoin(cmd.Args().First()), &st)
}

func leave(ctx context.Context, _ *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error) {
	var st ipc.VoiceStatus
	return st, daemonclient.Local(ctx, c, ipc.PathVoiceLeave, &st)
}

// run wraps one command: its argument checked before anything is attached to, so a mistyped id is a usage
// error on a machine with no daemon running too, then the request, then its answer printed.
func run(connect Connector, takesChannel bool,
	ask func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error)) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		switch {
		case !takesChannel && cmd.Args().Len() != 0:
			return clierr.Usage("norite voice %s takes no arguments", cmd.Name)
		case takesChannel && cmd.Args().Len() != 1:
			return clierr.Usage("norite voice %s takes one channel id", cmd.Name)
		case takesChannel && !ops.IsID(cmd.Args().First()):
			return clierr.Usage("%q is not a channel id", output.Clean(cmd.Args().First()))
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
		return output.Render(output.Writer(cmd.Root().Writer), cmd.Root().Bool("json"), view{st})
	}
}

// view is a VoiceStatus as these commands print it (contracts/cli-json/voice.schema.json).
type view struct {
	ipc.VoiceStatus
}

// Text draws it for a person.
func (v view) Text(t *output.Text) {
	channel := output.Clean(v.Channel)
	switch v.State {
	case ipc.VoiceInCall:
		t.Line("In a voice call in channel %s.", channel)
	case ipc.VoiceJoining:
		t.Line("Joining voice channel %s.", channel)
	default:
		t.Line("Not in a voice call.")
		if v.Reason != "" && v.Reason != "left" {
			t.Line("The last call ended: %s.", output.Clean(v.Reason))
		}
	}
	if !v.Available {
		t.Line("This daemon cannot join voice calls: %s.", output.Clean(v.Unavailable))
	}
}
