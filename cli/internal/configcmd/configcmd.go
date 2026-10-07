// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package configcmd is `norite config`: the client's config.toml as a scriptable interface.
//
// The verbs read and write the same file a person edits by hand, through daemon/config, so they are a
// second way to reach one source of truth and never a second source. None of them needs the daemon: the
// file is on this machine and a setting is worth changing before the first login.
package configcmd

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/output"
	"github.com/Alexnex31/Norite/cli/internal/prompt"
	"github.com/Alexnex31/Norite/daemon/atomicfile"
	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// Connector attaches to the daemon for one command, returning a caller and a function that detaches. Only
// split and unsplit use it: every other verb here reads and writes files, and works with no daemon running.
type Connector func(ctx context.Context) (daemonclient.Caller, func(), error)

// clientFlag names whose config file a command means, while the two clients have one each.
const clientFlag = "client"

// Command returns the `norite config` group.
func Command(connect Connector) *cli.Command {
	return &cli.Command{
		Name: "config",
		Flags: []cli.Flag{&cli.StringFlag{
			Name:  clientFlag,
			Value: string(config.TUI),
			Usage: "while the config is split, the client whose file is meant: `tui` or gui",
		}},
		Usage: "Read and change this machine's client settings (config.toml)",
		Description: "The client's settings live in one file you can edit by hand. These commands read and\n" +
			"change the same file, and never touch a comment or a line they were not asked about.\n\n" +
			"A key is written section.name, as in tui.colors.accent or shared.clock. `norite config get`\n" +
			"with no key lists every one. This is not the instance's configuration, which is instance.toml.\n\n" +
			"The terminal client and the GUI read the same file unless `norite config split` gave each its\n" +
			"own. While split, these commands mean the terminal client's file, and --client gui means the\n" +
			"GUI's.",
		Commands: []*cli.Command{
			{
				Name:   "path",
				Usage:  "Print where config.toml is",
				Action: run(0, 0, pathVerb),
			},
			{
				Name:      "get",
				Usage:     "Print one setting, or all of them",
				ArgsUsage: "[key]",
				Action:    run(0, 1, getVerb),
			},
			{
				Name:      "set",
				Usage:     "Change one setting",
				ArgsUsage: "<key> <value>",
				Action:    run(2, 2, setVerb),
			},
			{
				Name:      "unset",
				Usage:     "Remove one setting, so its default applies again",
				ArgsUsage: "<key>",
				Action:    run(1, 1, unsetVerb),
			},
			{
				Name:  "export",
				Usage: "Write the settings that can move to another machine",
				Description: "Prints a config.toml holding what this machine's file sets and another machine could\n" +
					"use. Defaults you never changed are left out, and so is anything tied to this machine.",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "write to `FILE` instead of printing"},
					&cli.BoolFlag{Name: "force", Usage: "replace FILE if it exists"},
				},
				Action: run(0, 0, exportVerb),
			},
			{
				Name:      "import",
				Usage:     "Merge an exported file into this machine's settings",
				ArgsUsage: "<file>",
				Description: "Shows what the file would change, then asks. A setting you already have is kept unless\n" +
					"--overwrite is passed. The file is treated as somebody else's: a setting tied to one\n" +
					"machine is refused, and nothing it says is acted on beyond the keys Norite defines.",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "overwrite", Usage: "replace settings you already have with the file's"},
					&cli.BoolFlag{Name: "dry-run", Usage: "show what would change and change nothing"},
					&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "apply without asking"},
				},
				Action: run(1, 1, importVerb),
			},
			{
				Name:  "split",
				Usage: "Give the terminal client and the GUI a config file each",
				Description: "Copies config.toml to config.tui.toml and config.gui.toml beside it, and from then on\n" +
					"each client reads its own. config.toml stays where it is and is read by neither.\n" +
					"The running daemon does this, so it has to be running.",
				Action: run(0, 0, toggleVerb(connect, ipc.PathConfigSplit)),
			},
			{
				Name:  "unsplit",
				Usage: "Fold the two clients' config files back into one",
				Description: "Merges config.tui.toml and config.gui.toml onto config.toml, key by key: the file saved\n" +
					"more recently is the starting point, and every setting only the other has is added to\n" +
					"it. Nothing is deleted. Both files, and config.toml as it was, are kept beside it with\n" +
					".before-unsplit after their names. The running daemon does this.",
				Action: run(0, 0, toggleVerb(connect, ipc.PathConfigUnsplit)),
			},
		},
	}
}

// env is what a verb runs with.
type env struct {
	ctx         context.Context
	out, errOut io.Writer
	in          io.Reader
	json        bool
	interactive bool
	// path is the config file these commands mean: config.toml, or one client's own while split.
	path string
	// split reports that the two clients have a file each, and client whose file path is.
	split  bool
	client config.Section
}

type verb func(cmd *cli.Command, e *env) (output.Result, error)

// run checks the argument count, runs the verb and renders what it produced.
func run(minArgs, maxArgs int, v verb) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		if n := cmd.Args().Len(); n < minArgs || n > maxArgs {
			return clierr.Usage("usage: norite config %s %s", cmd.Name, cmd.ArgsUsage)
		}
		root := cmd.Root()
		e := &env{ctx: ctx, out: root.Writer, errOut: root.ErrWriter, in: root.Reader, json: root.Bool("json")}
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
		e.client = config.Section(cmd.String(clientFlag))
		if e.client != config.TUI && e.client != config.GUI {
			return clierr.Usage("--%s %q is not a client: it is tui or gui", clientFlag, output.Clean(string(e.client)))
		}
		// The toggle is read from the daemon's state file, with or without a daemon running.
		path, split, err := config.PathFor(e.client)
		if err != nil {
			return err
		}
		e.path, e.split = path, split

		result, err := v(cmd, e)
		if err != nil {
			return classify(err)
		}
		if result == nil {
			return nil
		}
		return output.Render(e.out, e.json, result)
	}
}

// classify gives each failure the exit code its cause deserves. A key or a value the caller got wrong is
// theirs to fix (2). A lock somebody else holds is a reason to try again (3). A file that is not valid
// TOML, or that Norite will not edit, stays an ordinary failure (1): the command line was right and the
// file is what needs attention.
func classify(err error) error {
	var value *config.ValueError
	var usage *clierr.UsageError
	switch {
	case errors.As(err, &usage), errors.Is(err, clierr.ErrNoTerminal):
		return err
	case errors.Is(err, config.ErrUnknownKey):
		return clierr.Usage("%s; `norite config get` lists the keys", err)
	case errors.As(err, &value), errors.Is(err, config.ErrMachineLocal):
		return clierr.Usage("%s", err)
	case errors.Is(err, config.ErrLocked):
		return clierr.Unavailable("%s", err)
	}
	return err
}

// key reads a key argument and checks it is one this command's file is read for.
//
// While split, each client reads [shared] and its own section from its own file. A [gui] setting written
// into the terminal client's file, or a [tui] one into the GUI's, is read by nobody: a setting that
// silently does nothing, which is what an unknown key is refused for. It is refused the same way, naming
// the flag that means the other file.
func (e *env) key(arg string) (config.Section, string, error) {
	section, name, err := splitKey(arg)
	if err != nil {
		return "", "", err
	}
	if e.split && (section == config.TUI || section == config.GUI) && section != e.client {
		return "", "", clierr.Usage("the config is split, and [%s] is read from the %s client's own file: "+
			"pass --%s %s", section, section, clientFlag, section)
	}
	return section, name, nil
}

// splitKey reads "tui.colors.accent" as section "tui" and name "colors.accent".
func splitKey(arg string) (config.Section, string, error) {
	section, name, ok := strings.Cut(arg, ".")
	if !ok || section == "" || name == "" {
		return "", "", clierr.Usage("%q is not a key: write it section.name, as in shared.clock or tui.colors.accent",
			output.Clean(arg))
	}
	return config.Section(section), name, nil
}

func pathVerb(_ *cli.Command, e *env) (output.Result, error) {
	_, err := os.Stat(e.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return pathView{Path: e.path, Exists: err == nil, Split: e.split, Client: string(e.client)}, nil
}

func getVerb(cmd *cli.Command, e *env) (output.Result, error) {
	f, err := config.InspectFile(e.path)
	if err != nil {
		return nil, err
	}
	if cmd.Args().Len() == 0 {
		view := entriesView{Items: []entryView{}, Warnings: warnings(f.Warnings), MoreWarnings: f.MoreWarnings}
		for _, entry := range f.Entries() {
			view.Items = append(view.Items, viewOf(entry))
		}
		return view, nil
	}
	section, name, err := e.key(cmd.Args().First())
	if err != nil {
		return nil, err
	}
	entry, err := f.Get(section, name)
	if err != nil {
		return nil, err
	}
	return viewOf(entry), nil
}

func setVerb(cmd *cli.Command, e *env) (output.Result, error) {
	section, name, err := e.key(cmd.Args().Get(0))
	if err != nil {
		return nil, err
	}
	if err := config.Set(e.path, section, name, cmd.Args().Get(1)); err != nil {
		return nil, err
	}
	return after(e, section, name)
}

func unsetVerb(cmd *cli.Command, e *env) (output.Result, error) {
	section, name, err := e.key(cmd.Args().First())
	if err != nil {
		return nil, err
	}
	if err := config.Unset(e.path, section, name); err != nil {
		return nil, err
	}
	return after(e, section, name)
}

// after reads the key back from the file, so what is printed is what is now in force, not what was asked.
func after(e *env, section config.Section, name string) (output.Result, error) {
	f, err := config.InspectFile(e.path)
	if err != nil {
		return nil, err
	}
	entry, err := f.Get(section, name)
	if err != nil {
		return nil, err
	}
	return viewOf(entry), nil
}

func exportVerb(cmd *cli.Command, e *env) (output.Result, error) {
	f, err := config.InspectFile(e.path)
	if err != nil {
		return nil, err
	}
	doc, err := f.Export()
	if err != nil {
		return nil, err
	}
	dest := cmd.String("output")
	if dest == "" {
		// The document itself, whatever --json says: it is TOML, made to be saved and carried.
		_, err := e.out.Write(doc)
		return nil, err
	}
	if !cmd.Bool("force") {
		if _, err := os.Lstat(dest); err == nil {
			return nil, clierr.Usage("%s already exists; pass --force to replace it", output.Clean(dest))
		}
	}
	err = atomicfile.Write(dest, doc, atomicfile.Options{Mode: 0o600})
	if err != nil && !errors.Is(err, atomicfile.ErrNotDurable) {
		return nil, err
	}
	return exportedView{Path: dest}, nil
}

// toggleVerb asks the running daemon to turn the same-machine toggle on or off. The daemon is the state
// file's only writer, so this is the one pair of config commands that needs it running.
func toggleVerb(connect Connector, path string) verb {
	return func(_ *cli.Command, e *env) (output.Result, error) {
		if connect == nil {
			return nil, errors.New("this build has no way to reach the daemon")
		}
		c, detach, err := connect(e.ctx)
		if err != nil {
			return nil, err
		}
		defer detach()
		var done ipc.ConfigToggle
		if err := daemonclient.Local(e.ctx, c, path, &done); err != nil {
			return nil, err
		}
		return toggledView(done), nil
	}
}

// beforeImport runs between the plan somebody was shown and the import itself, which is where another
// writer can get in. It does nothing; a test puts that other writer there.
var beforeImport = func() {}

func importVerb(cmd *cli.Command, e *env) (output.Result, error) {
	source := cmd.Args().First()
	incoming, err := readBounded(source)
	if err != nil {
		return nil, err
	}
	current, err := config.ReadFile(e.path)
	if err != nil {
		return nil, err
	}
	overwrite := cmd.Bool("overwrite")
	plan, err := config.PlanImport(current, incoming, overwrite)
	if err != nil {
		return nil, err
	}
	if plan.Empty() || cmd.Bool("dry-run") {
		return planView(plan, false), nil
	}
	ask := prompt.Confirm{
		Yes: cmd.Bool("yes"), Interactive: e.interactive, In: e.in, Out: e.errOut,
		Question:  "Apply to " + output.Clean(e.path) + "?",
		Otherwise: "pass --yes to import without being asked, or --dry-run to see what it would change",
	}
	if ask.Asks() {
		// On stderr, with the question: stdout is the result, which --json pipes into a parser.
		t := output.NewText(e.errOut)
		planView(plan, false).changes(t)
		if err := t.Err(); err != nil {
			return nil, err
		}
	}
	if err := ask.Ask(); err != nil {
		return nil, err
	}
	beforeImport()
	// The plan is worked out again under the lock, so what is reported is what was written.
	done, err := config.Import(e.path, incoming, overwrite)
	if err != nil {
		return nil, err
	}
	// Written is whether the file changed, and it may not have: somebody else's import or an editor's save
	// can have made every change between the plan shown and the lock.
	return planView(done, !done.Empty()), nil
}

// readBounded reads the file to import, refusing one over the config's own size bound before parsing it.
func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the file the user named to import
	if err != nil {
		return nil, clierr.Usage("cannot read %s: %s", output.Clean(path), output.Clean(errors.Unwrap(err).Error()))
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, config.MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > config.MaxFileSize {
		return nil, clierr.Usage("%s: %s", output.Clean(path), config.ErrTooLarge)
	}
	return data, nil
}

func warnings(ws []config.Warning) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.String())
	}
	return out
}
