// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package voicecmd is `norite voice`: joining a voice channel, leaving it, and how the call stands (M28;
// M34 adds mute, deafen and the rest, M35 the input mode).
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
	"runtime"
	"strconv"
	"strings"

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

// Settings reads the call's audio processing (M30) and how many people it is heard from at once (M34) as
// config.toml sets them, with a line for each thing the file got wrong about them. FileSettings is the
// real one.
type Settings func() (Read, []string, error)

// Read is what config.toml says of the calls.
type Read struct {
	Processing ipc.VoiceJoin
	// Streams is how many people a call is heard from at once, 1 to ipc.MaxStreams, or 0 for every one it
	// has room for.
	Streams int
	// Input is how the microphone is let through (M35), the key as ipc.CanonicalKey writes it.
	Input ipc.VoiceInput
}

// Write sets voice keys in config.toml's [shared], as the terminal client reads it, all of them in one write
// or none. FileWrite is the real one.
type Write func(settings ...config.Setting) error

// FileWrite sets the keys in the file the terminal client reads, which the command tree shares (ADR 0026).
func FileWrite(settings ...config.Setting) error {
	if _, err := config.SetAllFor(config.TUI, config.Shared, settings...); err != nil {
		return fmt.Errorf("config.toml could not be written: %s", termsafe.Text(err.Error()))
	}
	return nil
}

// voiceKeys are the switches a join reads, and how a warning names each.
var voiceKeys = []struct{ key, words string }{
	{config.KeyVoiceEchoCancellation, "echo cancellation"},
	{config.KeyVoiceNoiseSuppression, "noise suppression"},
	{config.KeyVoiceAutoGain, "automatic gain"},
}

// FileSettings reads the user's config.toml as the terminal client does, since the command tree is the one
// M-x runs there (ADR 0026): [shared], then [tui], or config.tui.toml while the toggle has split them. A
// file that is not there is every switch on.
func FileSettings() (Read, []string, error) {
	path, _, err := config.PathFor(config.TUI)
	if err != nil {
		return Read{}, nil, fmt.Errorf("finding the config file: %s", termsafe.Text(err.Error()))
	}
	cfg, err := config.Load(path, config.TUI)
	if err != nil {
		// Not the defaults: somebody who turned a switch off and broke the file meanwhile would be put in a
		// call with it on. The path in the error is the filesystem's.
		return Read{}, nil, fmt.Errorf("the voice settings could not be read: %s", termsafe.Text(err.Error()))
	}
	return settingsOf(cfg, filepath.Base(path))
}

// settingsOf is a join's settings out of a config that loaded, and its warnings about them: anything the
// file got wrong under voice in the sections this client reads, a misspelled switch included, which would
// otherwise leave the switch meant on without a word. The GUI's section applies to nothing here.
func settingsOf(cfg *config.Config, file string) (Read, []string, error) {
	var warnings []string
	for _, w := range cfg.Warnings {
		for _, section := range []config.Section{config.Shared, config.TUI} {
			voice := string(section) + ".voice"
			if w.Key != voice && !strings.HasPrefix(w.Key, voice+".") {
				continue
			}
			// What applies, rather than what a bad value would leave on its own: the other section may set
			// the switch, off included.
			outcome := "it changes nothing"
			for _, k := range voiceKeys {
				if w.Key == string(section)+"."+k.key {
					outcome = fmt.Sprintf("so %s is %s", k.words, onOff(cfg.On(k.key)))
				}
			}
			if w.Key == string(section)+"."+config.KeyVoiceMaxStreams {
				outcome = "so " + streamsWords(streamsOf(cfg))
			}
			if w.Key == string(section)+"."+config.KeyVoiceInputMode {
				outcome = "so " + modeWords(modeOf(cfg))
			}
			warnings = append(warnings, fmt.Sprintf("%s: %s; %s", termsafe.Text(file), w.String(), outcome))
		}
	}
	// The file's problems past the bound are counted and not kept, and one of them may be a switch's.
	if cfg.MoreWarnings > 0 {
		warnings = append(warnings, fmt.Sprintf("%s: %d more problems, not listed, may concern the voice settings; "+
			"`norite config get` says what applies", termsafe.Text(file), cfg.MoreWarnings))
	}
	// The key is a string to the file, and its syntax is the daemon's (ipc.CanonicalKey): one it refuses is a
	// line, as a bad value of any other voice key is, and the default holds.
	key := ipc.DefaultPushToTalkKey
	if v, _ := cfg.Value(config.KeyVoicePushToTalkKey); v != nil {
		written, _ := v.(string)
		if k, err := ipc.CanonicalKey(written); err == nil {
			key = k
		} else {
			warnings = append(warnings, fmt.Sprintf("%s: %s: %s; so push-to-talk holds %s", termsafe.Text(file),
				config.KeyVoicePushToTalkKey, termsafe.Text(err.Error()), ipc.DefaultPushToTalkKey))
		}
	}
	return Read{Processing: ipc.VoiceJoin{
		EchoCancellation: cfg.On(config.KeyVoiceEchoCancellation),
		NoiseSuppression: cfg.On(config.KeyVoiceNoiseSuppression),
		AutoGain:         cfg.On(config.KeyVoiceAutoGain),
	}, Streams: streamsOf(cfg), Input: ipc.VoiceInput{Mode: modeOf(cfg), Key: key}}, warnings, nil
}

// modeOf is voice.input_mode, ipc's word for it. A value the file got wrong is a warning and leaves the
// default, by voice.
func modeOf(cfg *config.Config) string {
	v, _ := cfg.Value(config.KeyVoiceInputMode)
	if s, _ := v.(string); s == config.InputPushToTalk {
		return ipc.VoiceInputPushToTalk
	}
	return ipc.VoiceInputVoice
}

// modeWords says how the microphone is let through.
func modeWords(mode string) string {
	if mode == ipc.VoiceInputPushToTalk {
		return "you are heard while you hold the push-to-talk key"
	}
	return "you are heard whenever you speak"
}

// streamsOf is voice.max_streams as a number, 0 for every one a call has room for. A value the file got
// wrong is a warning and leaves the default.
func streamsOf(cfg *config.Config) int {
	v, _ := cfg.Value(config.KeyVoiceMaxStreams)
	s, _ := v.(string)
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > ipc.MaxStreams {
		return 0
	}
	return n
}

// streamsWords says how many people a call is heard from at once.
func streamsWords(n int) string {
	switch n {
	case 0:
		return "you hear everybody the call has room for at once"
	case 1:
		return "you hear one person at a time"
	}
	return fmt.Sprintf("you hear at most %d people at once", n)
}

// Command returns the `norite voice` group. settings is FileSettings and write FileWrite unless a test sets
// them.
func Command(connect Connector, settings Settings, write Write) *cli.Command {
	if settings == nil {
		settings = FileSettings
	}
	if write == nil {
		write = FileWrite
	}
	readSettings, join := joinWith(settings)
	self := func(name, usage, path string) *cli.Command {
		return &cli.Command{Name: name, Usage: usage,
			Description: "What you set on yourself holds in the call you are in and in every one after it, until\n" +
				"you sign out. Everybody in the channel sees it. Deafening mutes you too, and undeafening\n" +
				"puts back the mute you had before.",
			Action: run(connect, argNone, nil, local(path))}
	}
	prepareStreams, streams := streamsWith(write)
	prepareProcessing, processing := processingWith(settings, write)
	prepareMode, mode, nextMode := modeWith(settings, write)
	return &cli.Command{
		Name:  "voice",
		Usage: "Join a voice channel, leave it, and see how the call stands",
		Description: "The daemon runs each call in a process of its own, norite-voice, so a problem with\n" +
			"audio never takes messaging down with it. One call at a time: joining another channel\n" +
			"leaves the one you are in.",
		Commands: []*cli.Command{
			{
				Name:   "status",
				Usage:  "Say whether you are in a call, who is speaking, and how the last one ended",
				Action: runNext(connect, argNone, nil, status, fileInput(settings)),
			},
			{
				Name:      "join",
				Usage:     "Join a voice channel",
				ArgsUsage: "CHANNEL_ID",
				Description: "The call's audio processing is read from config.toml as the call is joined:\n" +
					"shared.voice.echo_cancellation, shared.voice.noise_suppression and\n" +
					"shared.voice.auto_gain, each \"on\" or \"off\" and all on by default, set with\n" +
					"`norite config set shared.voice.auto_gain off` for instance. A change applies at the\n" +
					"next join.",
				Action: run(connect, argChannel, readSettings, join),
			},
			{
				Name:   "leave",
				Usage:  "Leave the call you are in",
				Action: run(connect, argNone, nil, leave),
			},
			self("mute", "Mute yourself, for everybody in the call", ipc.PathVoiceMute),
			self("unmute", "Unmute yourself, which undeafens you too", ipc.PathVoiceUnmute),
			self("deafen", "Deafen yourself: hear nobody, and be heard by nobody", ipc.PathVoiceDeafen),
			self("undeafen", "Undeafen yourself, back to the mute you had before", ipc.PathVoiceUndeafen),
			{
				Name:      "local-mute",
				Usage:     "Stop hearing somebody, for you alone",
				ArgsUsage: "USER_ID",
				Description: "Nobody else is told, and they are not reported: `norite report user` does that,\n" +
					"separately. The voice server stops sending you their audio, in this call and every one\n" +
					"after it until you sign out. `norite voice status` lists whom you muted.",
				Action: run(connect, argUser, nil, idAsked(ipc.PathVoiceLocalMute)),
			},
			{
				Name:      "local-unmute",
				Usage:     "Hear somebody you muted for yourself again",
				ArgsUsage: "USER_ID",
				Action:    run(connect, argUser, nil, idAsked(ipc.PathVoiceLocalUnmute)),
			},
			{
				Name:      "streams",
				Usage:     "Hear at most this many people at once",
				ArgsUsage: "1|2|3|all",
				Description: "Fewer is less of your downlink: each person is up to the channel's bitrate. Set in\n" +
					"config.toml as shared.voice.max_streams, so it holds for every call, and applied to the\n" +
					"call you are in at once.",
				Action: run(connect, argStreams, prepareStreams, streams),
			},
			{
				Name:  "processing",
				Usage: "Turn the microphone's echo cancellation, noise suppression or automatic gain on or off",
				Description: "Set in config.toml, as shared.voice.echo_cancellation, shared.voice.noise_suppression\n" +
					"and shared.voice.auto_gain, so it holds for every call, and applied to the call you are\n" +
					"in at once. Echo cancellation turned back on mid-call starts converging again, so the\n" +
					"far end may hear a moment of echo.",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "echo-cancellation", Usage: "`on` or off"},
					&cli.StringFlag{Name: "noise-suppression", Usage: "`on` or off"},
					&cli.StringFlag{Name: "auto-gain", Usage: "`on` or off"},
				},
				Action: run(connect, argNone, prepareProcessing, processing),
			},
			{
				Name:      "mode",
				Usage:     "Be heard whenever you speak, or only while a key is held",
				ArgsUsage: "voice|push-to-talk",
				Description: "voice sends what the noise suppressor takes for a voice, and nothing between your\n" +
					"sentences. push-to-talk sends only while you hold a key, ctrl+shift+space unless --key\n" +
					"names another: modifiers among ctrl, alt, shift and super, then a letter, a digit, space\n" +
					"or f1 to f24. The key works whatever program is in front, and is held only while a call\n" +
					"by push-to-talk lasts, since nothing else can use it meanwhile. On Linux the desktop may\n" +
					"ask once, and let you bind another key; on macOS it needs ctrl or super.\n\n" +
					"Set in config.toml as shared.voice.input_mode and shared.voice.push_to_talk_key, so it\n" +
					"holds for every call, and applied to the call you are in at once.",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "key", Usage: "the push-to-talk `KEY`, as in ctrl+shift+space"},
				},
				Action: runNext(connect, argMode, prepareMode, mode, nextMode),
			},
		},
	}
}

// local is a request that carries nothing, so it needs no question first.
func local(path string) asker {
	return func(ctx context.Context, _ *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error) {
		var st ipc.VoiceStatus
		return st, daemonclient.Local(ctx, c, path, &st)
	}
}

// idAsked is a request carrying the argument, a user id, in its path: asked only of a daemon that answered
// PathVoice, as the join's channel is.
func idAsked(path func(string) string) asker {
	return func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error) {
		st, err := status(ctx, cmd, c)
		if err != nil {
			return st, err
		}
		return st, daemonclient.Local(ctx, c, path(cmd.Args().First()), &st)
	}
}

// streamsWith writes voice.max_streams before anything is attached to, then tells a daemon that answered
// PathVoice, so the call there is follows at once.
func streamsWith(write Write) (preparer, asker) {
	var n int
	prepare := func(cmd *cli.Command) (string, error) {
		arg := cmd.Args().First()
		value := arg
		if arg == config.StreamsAll {
			n = 0
		} else {
			n, _ = strconv.Atoi(arg)
		}
		return "shared." + config.KeyVoiceMaxStreams, write(config.Setting{Key: config.KeyVoiceMaxStreams, Value: value})
	}
	return prepare, func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error) {
		st, err := status(ctx, cmd, c)
		if err != nil {
			return st, err
		}
		return st, daemonclient.Local(ctx, c, ipc.PathVoiceStreams(n), &st)
	}
}

// processingWith reads the switches config.toml has, applies the flags, writes what changed and then,
// when there is a call, sends the call the whole of it (D5): the next join reads the file.
func processingWith(settings Settings, write Write) (preparer, asker) {
	var body ipc.VoiceJoin
	prepare := func(cmd *cli.Command) (string, error) {
		read, _, err := settings()
		if err != nil {
			return "", err
		}
		body = read.Processing
		flags := []struct {
			name, key string
			field     *bool
		}{
			{"echo-cancellation", config.KeyVoiceEchoCancellation, &body.EchoCancellation},
			{"noise-suppression", config.KeyVoiceNoiseSuppression, &body.NoiseSuppression},
			{"auto-gain", config.KeyVoiceAutoGain, &body.AutoGain},
		}
		named := 0
		for _, f := range flags {
			if !cmd.IsSet(f.name) {
				continue
			}
			named++
			v := cmd.String(f.name)
			if v != config.On && v != config.Off {
				return "", clierr.Usage("--%s is on or off", f.name)
			}
			*f.field = v == config.On
		}
		if named == 0 {
			return "", clierr.Usage("norite voice processing names --echo-cancellation, --noise-suppression or " +
				"--auto-gain, each on or off")
		}
		var settings []config.Setting
		var keys []string
		for _, f := range flags {
			if cmd.IsSet(f.name) {
				settings = append(settings, config.Setting{Key: f.key, Value: cmd.String(f.name)})
				keys = append(keys, "shared."+f.key)
			}
		}
		return strings.Join(keys, ", "), write(settings...)
	}
	return prepare, func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error) {
		st, err := status(ctx, cmd, c)
		if err != nil || st.State == ipc.VoiceIdle {
			return st, err
		}
		return st, daemonclient.LocalWith(ctx, c, ipc.PathVoiceProcessing, body, &st)
	}
}

// modeWith writes voice.input_mode, and voice.push_to_talk_key when --key names one, before anything is
// attached to, then tells a daemon in a call that answered PathVoice how the call's microphone is let
// through now, the key the file holds included. With no call, next is what the next join will ask for.
func modeWith(settings Settings, write Write) (preparer, asker, func() *ipc.VoiceInput) {
	var body ipc.VoiceInput
	prepare := func(cmd *cli.Command) (string, error) {
		mode, word := ipc.VoiceInputVoice, config.InputVoice
		if cmd.Args().First() == "push-to-talk" {
			mode, word = ipc.VoiceInputPushToTalk, config.InputPushToTalk
		}
		key := ""
		if cmd.IsSet("key") {
			k, err := ipc.CanonicalKey(cmd.String("key"))
			if err != nil {
				return "", clierr.Usage("--key: %s", err)
			}
			key = k
		}
		read, _, err := settings()
		if err != nil {
			return "", err
		}
		body = ipc.VoiceInput{Mode: mode, Key: read.Input.Key}
		saved := []config.Setting{{Key: config.KeyVoiceInputMode, Value: word}}
		keys := "shared." + config.KeyVoiceInputMode
		if key != "" {
			body.Key = key
			saved = append(saved, config.Setting{Key: config.KeyVoicePushToTalkKey, Value: key})
			keys += ", shared." + config.KeyVoicePushToTalkKey
		}
		return keys, write(saved...)
	}
	return prepare, func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error) {
		st, err := status(ctx, cmd, c)
		if err != nil || st.State == ipc.VoiceIdle {
			return st, err
		}
		return st, daemonclient.LocalWith(ctx, c, ipc.PathVoiceMode, body, &st)
	}, func() *ipc.VoiceInput { return &body }
}

// fileInput is how config.toml lets the next join's microphone through when it is by push-to-talk, the
// default being by voice and said by nothing, or nil when the file does not load, which a join says as its
// own problem.
func fileInput(settings Settings) func() *ipc.VoiceInput {
	return func() *ipc.VoiceInput {
		read, _, err := settings()
		if err != nil || read.Input.Mode != ipc.VoiceInputPushToTalk {
			return nil
		}
		return &read.Input
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
// The settings are read first, before the daemon is attached to: a config that does not load is a local
// problem, said as one whether or not a daemon is running, and sends nothing.
func joinWith(settings Settings) (prepare preparer, ask asker) {
	var body ipc.VoiceJoinRequest
	var streams int
	prepare = func(cmd *cli.Command) (string, error) {
		read, warnings, err := settings()
		if err != nil {
			return "", err
		}
		for _, w := range warnings {
			_, _ = fmt.Fprintln(cmd.Root().ErrWriter, w)
		}
		body = ipc.VoiceJoinRequest{VoiceJoin: read.Processing, VoiceInput: read.Input}
		streams = read.Streams
		return "", nil
	}
	return prepare, func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error) {
		st, err := status(ctx, cmd, c)
		if err != nil {
			return st, err
		}
		if !st.Available {
			return st, &clierr.RefusedError{Message: output.Clean(st.Unavailable)}
		}
		// How many people the call is heard from goes first, when the file and the daemon disagree, so the
		// call has it from its start.
		if have := st.MaxStreams; (have == nil) != (streams == 0) || (have != nil && *have != streams) {
			if err := daemonclient.Local(ctx, c, ipc.PathVoiceStreams(streams), &st); err != nil {
				return st, err
			}
		}
		err = daemonclient.LocalWith(ctx, c, ipc.PathVoiceJoin(cmd.Args().First()), body, &st)
		if err != nil {
			explainDevices(ctx, cmd, c, err)
		}
		return st, err
	}
}

// explainDevices says on stderr what the devices were doing when a join failed because of them: a call
// that ends as it starts, neither device usable, is a failed join, and the status keeps its devices (M32).
// Only when the status's reason is the join's failure, word for word, so the devices are this call's and
// not an earlier one's.
func explainDevices(ctx context.Context, cmd *cli.Command, c daemonclient.Caller, joinErr error) {
	var failed *ipc.RelayError
	if !errors.As(joinErr, &failed) || failed.Code != ipc.RelayFailed {
		return
	}
	st, err := status(ctx, cmd, c)
	if err != nil || st.State != ipc.VoiceIdle || st.Reason == "" || st.Reason != failed.Message {
		return
	}
	for _, line := range ops.VoiceDevices(st.Microphone, st.Speakers, runtime.GOOS) {
		_, _ = fmt.Fprintln(cmd.Root().ErrWriter, output.Clean(line)+".")
	}
}

func leave(ctx context.Context, _ *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error) {
	var st ipc.VoiceStatus
	return st, daemonclient.Local(ctx, c, ipc.PathVoiceLeave, &st)
}

// asker is one command's requests to the daemon, and what they answered.
type asker func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (ipc.VoiceStatus, error)

// What a command's one argument is, if it has one.
const (
	argNone = iota
	argChannel
	argUser
	argStreams
	argMode
)

// preparer is a command's own local work before it attaches: its checks, and for `streams` and `processing`
// writing config.toml, when saved names the keys it wrote.
type preparer func(*cli.Command) (saved string, err error)

// run wraps one command: its argument checked before anything is attached to, so a mistyped id is a usage
// error on a machine with no daemon running too, then prepare, the command's own local checks when it has
// some, then the request, then its answer printed.
//
// A failure after prepare saved keys says they are saved, keeping the failure's exit code: the file holds
// for the next join whether or not the daemon could be told, and an error alone read as nothing having
// changed (M34's /code-review).
func run(connect Connector, arg int, prepare preparer, ask asker) cli.ActionFunc {
	return runNext(connect, arg, prepare, ask, nil)
}

// runNext is run, and with no call says what the next join's microphone does, as next says: the daemon
// knows the mode only for a call, and the file is the client's (M21). M35's manual pass set push-to-talk
// with no call and was told only "Not in a voice call."
func runNext(connect Connector, arg int, prepare preparer, ask asker, next func() *ipc.VoiceInput) cli.ActionFunc {
	what := map[int]string{argChannel: "channel id", argUser: "user id"}[arg]
	return func(ctx context.Context, cmd *cli.Command) error {
		first := cmd.Args().First()
		switch {
		case arg == argNone && cmd.Args().Len() != 0:
			return clierr.Usage("norite voice %s takes no arguments", cmd.Name)
		case arg == argStreams && (cmd.Args().Len() != 1 || !validStreams(first)):
			return clierr.Usage("norite voice %s takes 1, 2, 3 or all", cmd.Name)
		case arg == argMode && (cmd.Args().Len() != 1 || first != "voice" && first != "push-to-talk"):
			return clierr.Usage("norite voice %s takes voice or push-to-talk", cmd.Name)
		case arg == argMode:
		case arg != argNone && arg != argStreams && cmd.Args().Len() != 1:
			return clierr.Usage("norite voice %s takes one %s", cmd.Name, what)
		case arg != argNone && arg != argStreams && !ops.IsID(first),
			arg == argUser && !ipc.IsUserID(first):
			return clierr.Usage("%q is not a %s", output.Clean(first), what)
		}
		saved := ""
		if prepare != nil {
			var err error
			if saved, err = prepare(cmd); err != nil {
				return err
			}
		}
		told := func(err error) error {
			if err == nil || saved == "" {
				return err
			}
			return fmt.Errorf("%s: saved in config.toml for the next join, but the daemon was not told: %w", saved, err)
		}
		if connect == nil {
			return told(errors.New("this build has no way to reach the daemon"))
		}
		c, detach, err := connect(ctx)
		if err != nil {
			return told(err)
		}
		defer detach()
		st, err := ask(ctx, cmd, c)
		if err != nil {
			return told(err)
		}
		// Every list is [] rather than null in what is printed (contracts/cli-json), whichever daemon sent it.
		if st.LocalMutes == nil {
			st.LocalMutes = []string{}
		}
		if st.Speaking == nil {
			st.Speaking = []string{}
		}
		v := view{VoiceStatus: st, goos: runtime.GOOS}
		if next != nil && st.State == ipc.VoiceIdle {
			v.next = next()
		}
		return output.Render(output.Writer(cmd.Root().Writer), cmd.Root().Bool("json"), v)
	}
}

// view is a VoiceStatus as these commands print it (contracts/cli-json/voice.schema.json).
type view struct {
	ipc.VoiceStatus
	// goos is the OS whose settings a refusal names: this machine's, the daemon's and the worker's too.
	goos string
	// next is how the next join lets the microphone through, said with no call; nil says nothing.
	next *ipc.VoiceInput
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
		// A join that failed is not a call that ended, which the status says apart since M33. A rejoin given up
		// says its reason with it, below.
		switch {
		case v.Reason == "" || v.Reason == "left" || v.Rejoin != nil:
		case v.EndedInCall:
			t.Line("The last call ended: %s.", output.Clean(v.Reason))
		default:
			t.Line("The last join failed: %s.", output.Clean(v.Reason))
		}
	}
	// A call the daemon joins again by itself (M36), and why it was lost: the reason is the worker's or the
	// instance's words.
	for _, line := range ops.VoiceRejoin(v.Rejoin, v.State, v.Reason) {
		t.Line("%s.", output.Clean(line))
	}
	// What the processing is doing, as the worker said (M34), and what was asked before it has said.
	switch d := v.DSP; {
	case v.State == ipc.VoiceIdle:
	case d != nil && d.State == ipc.VoiceDSPRunning:
		t.Line("Processing the microphone with echo cancellation %s, noise suppression %s, automatic gain %s.",
			onOff(d.EchoCancellation), onOff(d.NoiseSuppression), onOff(d.AutoGain))
		// A change the worker has not said it runs yet, which `processing` waits a moment for (M34's manual
		// pass): what runs is said above, and what was asked here, so neither reads as the other.
		if p := v.Processing; p != nil && (p.EchoCancellation != d.EchoCancellation ||
			p.NoiseSuppression != d.NoiseSuppression || p.AutoGain != d.AutoGain) {
			t.Line("Asked for echo cancellation %s, noise suppression %s, automatic gain %s, which has not taken yet.",
				onOff(p.EchoCancellation), onOff(p.NoiseSuppression), onOff(p.AutoGain))
		}
	case d != nil && d.State == ipc.VoiceDSPFailed:
		t.Line("The microphone goes unprocessed: %s.", output.Clean(d.Reason))
	case v.Processing != nil:
		p := v.Processing
		t.Line("Joined with echo cancellation %s, noise suppression %s, automatic gain %s.",
			onOff(p.EchoCancellation), onOff(p.NoiseSuppression), onOff(p.AutoGain))
	}
	if v.State == ipc.VoiceInCall {
		if len(v.Speaking) == 0 {
			t.Line("Nobody is speaking.")
		} else {
			t.Line("Speaking now: %s.", ids(v.Speaking))
		}
	}
	// How the microphone is let through (M35): the key's state and the desktop's binding are its words.
	for _, line := range ops.VoiceInput(v.Input) {
		t.Line("%s.", output.Clean(line))
	}
	if n := v.next; n != nil && n.Mode == ipc.VoiceInputPushToTalk {
		t.Line("The next call is by push-to-talk: you will be heard while you hold %s.", output.Clean(n.Key))
	} else if n != nil {
		t.Line("The next call is by voice: you will be heard whenever you speak.")
	}
	for _, line := range ops.VoiceSelf(v.Self, v.State != ipc.VoiceIdle) {
		t.Line("%s.", line)
	}
	if len(v.LocalMutes) > 0 {
		t.Line("Muted for you alone: %s.", ids(v.LocalMutes))
	}
	if v.MaxStreams != nil {
		t.Line("%s.", capitalized(streamsWords(*v.MaxStreams)))
	}
	// What the devices are doing (M32): during a call, and after one their failing ended. The words are the
	// client's, and the worker's reason in them is cleaned.
	for _, line := range ops.VoiceDevices(v.Microphone, v.Speakers, v.goos) {
		t.Line("%s.", output.Clean(line))
	}
	// What the instance lets the account do in the call (M33), which this client applies.
	for _, line := range ops.VoiceRights(v.Rights) {
		t.Line("%s.", line)
	}
	if !v.Available {
		t.Line("This daemon cannot join voice calls: %s.", output.Clean(v.Unavailable))
	}
}

// ids is a list of user ids for a line, each cleaned.
func ids(users []string) string {
	out := make([]string, len(users))
	for i, u := range users {
		out[i] = output.Clean(u)
	}
	return strings.Join(out, ", ")
}

func capitalized(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func validStreams(s string) bool {
	return s == config.StreamsAll || (len(s) == 1 && s[0] >= '1' && s[0] <= '0'+ipc.MaxStreams)
}

func onOff(on bool) string {
	if on {
		return config.On
	}
	return config.Off
}
