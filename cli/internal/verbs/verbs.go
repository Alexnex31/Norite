// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package verbs is the command tree's account-facing half: guilds, channels, roles, members, overwrites,
// messages, reports and tags, each verb relayed through the daemon as the signed-in account (M20, folding
// in M17a).
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
	"regexp"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
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

// env is what a verb runs with.
type env struct {
	out  io.Writer
	in   io.Reader
	json bool
	// interactive reports whether in is a terminal a question can be asked on.
	interactive bool
	call        daemonclient.Caller
}

// run attaches, runs the verb, and renders what it produced. A verb that produces nothing to show — a
// confirmation it printed itself — returns a nil Result.
func run(connect Connector, verb func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error)) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		root := cmd.Root()
		e := &env{out: root.Writer, in: root.Reader, json: root.Bool("json")}
		if e.out == nil {
			e.out = os.Stdout
		}
		if e.in == nil {
			e.in = os.Stdin
		}
		if f, ok := e.in.(*os.File); ok {
			e.interactive = term.IsTerminal(int(f.Fd()))
		}

		// Arguments are checked before the daemon is attached to, so a typo is a usage error on a machine
		// with no daemon running, rather than "the daemon is not running".
		if err := checkArgs(cmd); err != nil {
			return err
		}

		c, detach, err := connect(ctx)
		if err != nil {
			return err
		}
		defer detach()
		e.call = c

		result, err := verb(ctx, cmd, e)
		if err != nil || result == nil {
			return err
		}
		return output.Render(e.out, e.json, result)
	}
}

// ---------- arguments ----------

// snowflake is what an id argument may be: digits, and no more than a 64-bit integer has. Checked here
// because an id goes into a request path, and the relay refusing a path that climbs out of /api/v1 is the
// second line, not the first.
var snowflake = regexp.MustCompile(`^[0-9]{1,20}$`)

// argsKey is where a command records the ids it takes, for checkArgs to read.
const argsKey = "ids"

// ids declares a command's positional arguments, all of them ids, by name: `ids("guild", "role")` takes
// `<guild-id> <role-id>`.
func ids(names ...string) (usage string, meta map[string]any) {
	for i, n := range names {
		if i > 0 {
			usage += " "
		}
		usage += "<" + n + "-id>"
	}
	return usage, map[string]any{argsKey: names}
}

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
		if !snowflake.MatchString(a) {
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
	if !snowflake.MatchString(v) {
		return nil, clierr.Usage("--%s %q is not an id", name, output.Clean(v))
	}
	return &v, nil
}
