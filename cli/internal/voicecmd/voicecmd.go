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
	"fmt"
	"path/filepath"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/cli/internal/output"
	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Connector attaches to the daemon. verbs.Daemon is the real one.
type Connector func(ctx context.Context) (daemonclient.Caller, func(), error)

// Settings reads the call's audio processing as config.toml sets it (M30), with a line for each thing the
// file got wrong about it. FileSettings is the real one.
type Settings func() (ipc.VoiceJoin, []string, error)

// voiceKeys are the switches a join reads.
var voiceKeys = []string{config.KeyVoiceEchoCancellation, config.KeyVoiceNoiseSuppression, config.KeyVoiceAutoGain}

// FileSettings reads the user's config.toml as the terminal client does, since the command tree is the one
// M-x runs there (ADR 0026): [shared], then [tui], or config.tui.toml while the toggle has split them. A
// file that is not there is every switch on.
func FileSettings() (ipc.VoiceJoin, []string, error) {
	path, _, err := config.PathFor(config.TUI)
	if err != nil {
		return ipc.VoiceJoin{}, nil, fmt.Errorf("finding the config file: %s", termsafe.Text(err.Error()))
	}
	cfg, err := config.Load(path, config.TUI)
	if err != nil {
		// Not the defaults: somebody who turned a switch off and broke the file meanwhile would be put in a
		// call with it on. The path in the error is the filesystem's.
		return ipc.VoiceJoin{}, nil, fmt.Errorf("the voice settings could not be read: %s", termsafe.Text(err.Error()))
	}
	return settingsOf(cfg, filepath.Base(path))
}

// settingsOf is a join's settings out of a config that loaded, and its warnings about them.
func settingsOf(cfg *config.Config, file string) (ipc.VoiceJoin, []string, error) {
	var warnings []string
	for _, w := range cfg.Warnings {
		for _, k := range voiceKeys {
			// A warning's key is the section's and the key's. The GUI's section applies to nothing here, so
			// what it got wrong leaves nothing on.
			if w.Key == string(config.Shared)+"."+k || w.Key == string(config.TUI)+"."+k {
				warnings = append(warnings, fmt.Sprintf("%s: %s; it stays on", termsafe.Text(file), w.String()))
			}
		}
	}
	return ipc.VoiceJoin{
		EchoCancellation: cfg.On(config.KeyVoiceEchoCancellation),
		NoiseSuppression: cfg.On(config.KeyVoiceNoiseSuppression),
		AutoGain:         cfg.On(config.KeyVoiceAutoGain),
	}, warnings, nil
}

// Command returns the `norite voice` group. settings is FileSettings unless a test sets it.
func Command(connect Connector, settings Settings) *cli.Command {
	if settings == nil {
		settings = FileSettings
	}
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
				Description: "The call's audio processing is read from config.toml as the call is joined:\n" +
					"voice.echo_cancellation, voice.noise_suppression and voice.auto_gain, each \"on\" or\n" +
					"\"off\", all on by default. A change applies at the next join.",
				Action: run(connect, true, joinWith(settings)),
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

// joinWith asks how voice stands before it sends the channel and the settings: the id is the user's, the
// settings too, and a daemon from before these requests would relay the path and the body to its instance
// (the ledger's entry on the prefix). A daemon that answers GET knows the join too. One that cannot join at
// all is refused here, and is never sent either.
//
// The settings are read before anything is asked, so a config that does not load sends nothing.
func joinWith(settings Settings) func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error) {
	return func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error) {
		body, warnings, err := settings()
		if err != nil {
			return ipc.VoiceStatus{}, err
		}
		for _, w := range warnings {
			_, _ = fmt.Fprintln(cmd.Root().ErrWriter, w)
		}
		st, err := status(ctx, cmd, c)
		if err != nil {
			return st, err
		}
		if !st.Available {
			return st, &clierr.RefusedError{Message: output.Clean(st.Unavailable)}
		}
		return st, daemonclient.LocalWith(ctx, c, ipc.PathVoiceJoin(cmd.Args().First()), body, &st)
	}
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
	if p := v.Processing; p != nil && v.State != ipc.VoiceIdle {
		t.Line("Echo cancellation %s, noise suppression %s, automatic gain %s.", onOff(p.EchoCancellation),
			onOff(p.NoiseSuppression), onOff(p.AutoGain))
	}
	if !v.Available {
		t.Line("This daemon cannot join voice calls: %s.", output.Clean(v.Unavailable))
	}
}

func onOff(on bool) string {
	if on {
		return config.On
	}
	return config.Off
}
