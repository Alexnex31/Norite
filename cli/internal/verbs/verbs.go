// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package verbs is the command tree's account-facing half: guilds, channels, roles, members, overwrites,
// messages, reports, tags and invites, each verb relayed through the daemon as the signed-in account (M20,
// folding in M17a).
//
// Every verb has the same shape, and that shape is the package:
//
//   - its arguments are ids, never names (Ids). Names are not unique, and matching one can act on the wrong
//     object silently;
//   - it makes its calls through daemonclient.Call, which decides every exit code once;
//   - it returns an output.Result, which output.Render prints as text or as the JSON contracts/cli-json/
//     pins — the one code path ADR 0026 and M48 need;
//   - if it destroys something, it asks first (Confirm), and --yes answers for a script;
//   - if it lists something unbounded, it pages (Page), and there is no --all: the recording log has no
//     ceiling, and a script that wants everything loops on the cursor it is handed.
package verbs

import (
	"context"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/cli/internal/output"
)

// Connector attaches to the daemon for one command, returning a caller and a function that detaches.
type Connector func(ctx context.Context) (daemonclient.Caller, func(), error)

// Daemon is the real Connector: this user's daemon, attached as a CLI of the given version.
func Daemon(version string) Connector {
	return func(ctx context.Context) (daemonclient.Caller, func(), error) {
		c, err := daemonclient.Connect(ctx, version)
		if err != nil {
			return nil, nil, err
		}
		return c, func() { _ = c.Close() }, nil
	}
}

// Commands returns every verb group, to be mounted at the root.
func Commands(connect Connector) []*cli.Command {
	return []*cli.Command{
		guildCommand(connect),
		channelCommand(connect),
		roleCommand(connect),
		memberCommand(connect),
		overwriteCommand(connect),
		messageCommand(connect),
		reportCommand(connect),
		tagCommand(connect),
		inviteCommand(connect),
		tokenCommand(connect),
	}
}

// env is what a verb runs with.
type env struct {
	out    io.Writer
	errOut io.Writer
	in     io.Reader
	json   bool
	// interactive reports whether in is a terminal a question can be asked on.
	interactive bool

	// The daemon is attached to on the verb's first request, not before it: every flag a verb checks, and
	// the confirmation it asks for, comes first, so a mistake is a usage error (2) on a machine with no
	// daemon running rather than "the daemon is not running" (3), which a script would wait out.
	connect Connector
	call    daemonclient.Caller
	detach  func()
}

// run checks the arguments, runs the verb, and renders what it produced.
func run(connect Connector, verb func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error)) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		root := cmd.Root()
		e := &env{out: root.Writer, errOut: root.ErrWriter, in: root.Reader, json: root.Bool("json"), connect: connect}
		if e.out == nil {
			e.out = os.Stdout
		}
		if e.errOut == nil {
			e.errOut = os.Stderr
		}
		if e.in == nil {
			e.in = os.Stdin
		}
		if f, ok := e.in.(*os.File); ok {
			e.interactive = term.IsTerminal(int(f.Fd()))
		}

		if err := checkArgs(cmd); err != nil {
			return err
		}
		defer func() {
			if e.detach != nil {
				e.detach()
			}
		}()

		result, err := verb(ctx, cmd, e)
		if err != nil || result == nil {
			return err
		}
		return output.Render(e.out, e.json, result)
	}
}

// attached returns the verb's connection to the daemon, attaching on first use.
func (e *env) attached(ctx context.Context) (daemonclient.Caller, error) {
	if e.call != nil {
		return e.call, nil
	}
	c, detach, err := e.connect(ctx)
	if err != nil {
		return nil, err
	}
	e.call, e.detach = c, detach
	return c, nil
}

// ---------- arguments ----------

// argsKey is where a command records the ids it takes, for checkArgs to read.
const argsKey = "ids"

// codeArg is the one positional argument that is not an id: an invite code (M20a), as M10's
// `instance invite revoke <code>` takes one. Its name is one no id could plausibly be given, since an id
// declared with it would lose the digits check (M20a's second /code-review).
const codeArg = "invite code"

// maxCodeArg bounds a code as typed. A code is sixteen letters; dashes and spaces a chat client added are
// the instance's to strip, so the bound is loose, and the instance's own parser decides what is a code.
const maxCodeArg = 64

// ids declares a command's positional arguments by name, all of them ids except codeArg:
// `ids("guild", "role")` takes `<guild-id> <role-id>`, and `ids("code")` takes `<code>`.
func ids(names ...string) (usage string, meta map[string]any) {
	for i, n := range names {
		if i > 0 {
			usage += " "
		}
		if n == codeArg {
			usage += "<code>"
			continue
		}
		usage += "<" + n + "-id>"
	}
	return usage, map[string]any{argsKey: names}
}

// checkArgs holds a command's arguments to what it declared. An id must be what ops.IsID accepts — digits,
// and no more than a 64-bit integer has — checked before anything is attached to, because an id goes into
// a request path, and the relay refusing a path that climbs out of /api/v1 is the second line, not the
// first.
func checkArgs(cmd *cli.Command) error {
	names, _ := cmd.Metadata[argsKey].([]string)
	args := cmd.Args().Slice()
	if len(args) != len(names) {
		usage := cmd.ArgsUsage
		if usage == "" {
			usage = "no arguments"
		}
		return clierr.Usage("%s takes %s; see `%s --help`", cmd.FullName(), usage, cmd.FullName())
	}
	for i, a := range args {
		if names[i] == codeArg {
			// Not shaped here beyond what keeps it a code a person could have pasted: the instance answers
			// any code it did not issue with one 404, and a second judgement here would be one that drifts.
			// It goes into a request body, never a path (ADR 0029).
			if a == "" || len(a) > maxCodeArg || strings.ContainsFunc(a, unicode.IsControl) {
				return clierr.Usage("%q is not an invite code", output.Clean(a))
			}
			continue
		}
		if !ops.IsID(a) {
			return clierr.Usage("%q is not a %s id: ids are the numbers the list commands print",
				output.Clean(a), names[i])
		}
	}
	return nil
}

// flagID reads an optional id-valued flag, checking it the way checkArgs checks an argument.
func flagID(cmd *cli.Command, name string) (*string, error) {
	if !cmd.IsSet(name) {
		return nil, nil
	}
	v := cmd.String(name)
	if !ops.IsID(v) {
		return nil, clierr.Usage("--%s %q is not an id", name, output.Clean(v))
	}
	return &v, nil
}
