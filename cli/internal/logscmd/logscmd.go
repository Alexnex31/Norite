// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package logscmd is `norite logs`: reading the daemon's log (M23).
//
// It reads a file and asks the daemon nothing, because the log is wanted most when the daemon will not
// start. Where the file is comes from daemon/logfile, the same function the daemon writes by.
//
// The file is the daemon's by convention and anybody's by fact, so everything printed from it as text goes
// through termsafe (rule 19), and --json through the escaping every other command's JSON gets. Rule 8 is
// the daemon's to keep: this prints what the log holds, and nothing puts a token there.
package logscmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/output"
	"github.com/Alexnex31/Norite/daemon/logfile"
)

// defaultLines is how many lines `tail` prints unless told.
const defaultLines = 50

// The seams the tests use: where the log is, how often a follow looks, and the zone times are shown in.
var (
	defaultPath = logfile.Path
	pollEvery   = 300 * time.Millisecond
	zone        = time.Local
)

// Command returns the `norite logs` group.
func Command() *cli.Command {
	return &cli.Command{
		Name:  "logs",
		Usage: "read the daemon's log",
		Description: "The daemon writes what it does to a file, which it rotates so it never grows without\n" +
			"bound. These commands read that file. They work whether or not the daemon is running.",
		Commands: []*cli.Command{
			{
				Name:  "tail",
				Usage: "Print the newest lines of the daemon's log",
				Description: "Prints the last lines and exits, or with --follow keeps printing as the daemon\n" +
					"writes. When the log has just been rotated, the lines come from the copy before it.\n\n" +
					"With --json each line is one JSON object, so the output can be read as it arrives.",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "lines", Aliases: []string{"n"}, Value: defaultLines,
						Usage: fmt.Sprintf("how many lines to print, up to %d", logfile.MaxLines)},
					&cli.BoolFlag{Name: "follow", Aliases: []string{"f"},
						Usage: "keep printing new lines until interrupted"},
					&cli.StringFlag{Name: "level",
						Usage: "print only lines at this `LEVEL` or above: " + strings.Join(logfile.Levels(), ", ")},
					&cli.StringFlag{Name: "file",
						Usage: "read this `FILE` instead, for a daemon started with -log-file"},
				},
				Action: tail,
			},
		},
	}
}

func tail(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() != 0 {
		return clierr.Usage("norite logs tail takes no arguments")
	}
	lines := cmd.Int("lines")
	if lines < 0 || lines > logfile.MaxLines {
		return clierr.Usage("--lines must be between 0 and %d", logfile.MaxLines)
	}
	var keep func(logfile.Entry) bool
	if level := cmd.String("level"); cmd.IsSet("level") {
		var ok bool
		if keep, ok = logfile.AtLeast(strings.ToLower(level)); !ok {
			return clierr.Usage("--level %q is not a level: use one of %s",
				output.Clean(level), strings.Join(logfile.Levels(), ", "))
		}
	}
	path := cmd.String("file")
	if cmd.IsSet("file") && path == "" {
		return clierr.Usage("--file needs a path")
	}
	if path == "" {
		resolved, err := defaultPath()
		if err != nil {
			return err
		}
		path = resolved
	}
	follow := cmd.Bool("follow")

	emit := printer(writer(cmd), cmd.Root().Bool("json"))
	entries, follower, err := logfile.Tail(path, logfile.Options{Lines: lines, Keep: keep, Follow: follow})
	switch {
	case errors.Is(err, fs.ErrNotExist) && follow:
		// Followed, a log that is not there yet is waited for: start this, then start the daemon.
		fmt.Fprintf(errWriter(cmd), "No log at %s yet; waiting for the daemon to write one.\n", output.Clean(path)) //nolint:errcheck // a note
		follower = logfile.Follow(path, keep)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("there is no log at %s: the daemon writes one when it first starts, and "+
			"`norite daemon status` says whether it is installed. A daemon started with -log-file "+
			"is read with --file", output.Clean(path))
	case err != nil:
		// The error names the path, which is the filesystem's text.
		return fmt.Errorf("cannot read the daemon's log: %s", output.Clean(err.Error()))
	}
	for _, entry := range entries {
		if emit(entry) != nil {
			// Whoever was reading has stopped: `norite logs tail | head`.
			return nil
		}
	}
	if !follow {
		return nil
	}

	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		entries, err := follower.Next()
		for _, entry := range entries {
			if emit(entry) != nil {
				return nil
			}
		}
		if err != nil {
			return fmt.Errorf("cannot go on reading the daemon's log: %s", output.Clean(err.Error()))
		}
		if len(entries) > 0 {
			// More may be waiting already: a read is bounded, and a log can grow by more than one.
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		select {
		case <-ctx.Done():
			// Interrupted, which is how a follow ends.
			return nil
		case <-tick.C:
		}
	}
}

// printer returns what writes one entry: a JSON object on a line of its own, or a line of text.
func printer(w io.Writer, asJSON bool) func(logfile.Entry) error {
	if asJSON {
		return func(e logfile.Entry) error { return output.WriteJSONLine(w, viewOf(e)) }
	}
	return func(e logfile.Entry) error {
		_, err := fmt.Fprintln(w, textOf(e))
		return err
	}
}

// entryView is one line of the log under --json (contracts/cli-json/logs.schema.json). Every field is
// always present.
type entryView struct {
	// Time is RFC 3339, or empty when the line has no time that parses.
	Time      string `json:"time"`
	Level     string `json:"level"`
	Subsystem string `json:"subsystem"`
	Message   string `json:"message"`
	// Fields is everything else the line holds, each value as it was written.
	Fields    map[string]json.RawMessage `json:"fields"`
	Unparsed  bool                       `json:"unparsed"`
	Truncated bool                       `json:"truncated"`
}

func viewOf(e logfile.Entry) entryView {
	v := entryView{Level: e.Level, Subsystem: e.Subsystem, Message: e.Message, Fields: e.Fields,
		Unparsed: e.Unparsed, Truncated: e.Truncated}
	if !e.Time.IsZero() {
		v.Time = e.Time.Format(time.RFC3339Nano)
	}
	if v.Fields == nil {
		v.Fields = map[string]json.RawMessage{}
	}
	return v
}

// textOf draws one entry as a line: when, how loud, which part of the daemon, what, and then whatever else
// the line carries as key=value. Every piece is the file's, and is cleaned before it is joined.
func textOf(e logfile.Entry) string {
	if e.Unparsed {
		line := output.Clean(e.Message)
		if e.Truncated {
			line += fmt.Sprintf(" [cut at %d bytes]", logfile.MaxLine)
		}
		return line
	}

	when := "-"
	if !e.Time.IsZero() {
		when = e.Time.In(zone).Format("2006-01-02 15:04:05")
	}
	level := "-"
	if e.Level != "" {
		level = strings.ToUpper(output.Clean(e.Level))
	}
	part := "daemon"
	if e.Subsystem != "" {
		part = output.Clean(e.Subsystem)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-19s %-5s %-10s %s", when, level, part, output.Clean(e.Message))

	keys := make([]string, 0, len(e.Fields))
	for key := range e.Fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw := e.Fields[key]
		// On every line the daemon writes, and saying nothing a reader of its log does not know.
		if key == "component" && string(raw) == `"daemon"` {
			continue
		}
		fmt.Fprintf(&b, "  %s=%s", output.Clean(key), valueText(raw))
	}
	return b.String()
}

// valueText draws one field's value: a string as itself, quoted when it would otherwise read as two
// things, and anything else as the JSON it is.
func valueText(raw json.RawMessage) string {
	var text string
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &text) == nil {
		clean := output.Clean(text)
		if clean == "" || strings.ContainsAny(clean, " \t=\"") {
			return fmt.Sprintf("%q", clean)
		}
		return clean
	}
	return output.Clean(string(raw))
}

func writer(cmd *cli.Command) io.Writer {
	if w := cmd.Root().Writer; w != nil {
		return w
	}
	return os.Stdout
}

func errWriter(cmd *cli.Command) io.Writer {
	if w := cmd.Root().ErrWriter; w != nil {
		return w
	}
	return os.Stderr
}
