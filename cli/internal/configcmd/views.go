// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package configcmd

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/Alexnex31/Norite/cli/internal/output"
	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// The shapes here are contracts/cli-json/config.schema.json's (rule 15). Every string that came out of a
// file passes output.Clean on its way to text: a config is hand-edited and often somebody else's, and a
// value is printed verbatim. --json goes through output.WriteJSON, which escapes the same runes.

type pathView struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	// Split is the same-machine toggle, and Client whose file Path is: both clients' while it is off.
	Split  bool   `json:"split"`
	Client string `json:"client"`
}

func (v pathView) Text(t *output.Text) {
	t.Line("%s", output.Clean(v.Path))
	if v.Split {
		whose := "terminal client"
		if v.Client == string(config.GUI) {
			whose = "GUI"
		}
		t.Line("(the config is split; this is the %s's own file)", whose)
	}
	if !v.Exists {
		t.Line("(not created yet; `norite config set` creates it)")
	}
}

type entryView struct {
	Key string `json:"key"`
	// Value is a string, an object for a whole table, or null when nothing sets the key.
	Value    any    `json:"value"`
	Source   string `json:"source"`
	Live     bool   `json:"live"`
	Consumer string `json:"consumer"`
}

func viewOf(e config.Entry) entryView {
	return entryView{
		Key:      string(e.Section) + "." + e.Name,
		Value:    encodable(e.Value),
		Source:   string(e.Source),
		Live:     e.Key.Live,
		Consumer: e.Key.Consumer,
	}
}

// encodable makes a table's values safe to write as JSON. TOML has inf and nan and JSON does not, so one
// hand-typed `a = inf` in a table nothing reads yet failed every `--json config get` that listed it. They
// are written as the words the file used.
func encodable(v any) any {
	switch v := v.(type) {
	case float64:
		switch {
		case math.IsNaN(v):
			return "nan"
		case math.IsInf(v, 1):
			return "inf"
		case math.IsInf(v, -1):
			return "-inf"
		}
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, inner := range v {
			out[k] = encodable(inner)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, inner := range v {
			out[i] = encodable(inner)
		}
		return out
	}
	return v
}

func (v entryView) Text(t *output.Text) {
	note := v.Source
	if !v.Live {
		note += fmt.Sprintf("; nothing reads this until %s", v.Consumer)
	}
	switch value := v.Value.(type) {
	case nil:
		if v.Live {
			t.Line("%s is not set", output.Clean(v.Key))
		} else {
			t.Line("%s is not set  (nothing reads this until %s)", output.Clean(v.Key), v.Consumer)
		}
	case map[string]any:
		t.Line("%s  (%s)", output.Clean(v.Key), note)
		for _, inner := range slices.Sorted(maps.Keys(value)) {
			t.Line("  %s = %s", output.Clean(inner), output.Clean(fmt.Sprint(value[inner])))
		}
	default:
		t.Line("%s = %s  (%s)", output.Clean(v.Key), output.Clean(fmt.Sprint(value)), note)
	}
}

type entriesView struct {
	Items        []entryView `json:"items"`
	Warnings     []string    `json:"warnings"`
	MoreWarnings int         `json:"more_warnings"`
}

func (v entriesView) Text(t *output.Text) {
	for _, item := range v.Items {
		item.Text(t)
	}
	if len(v.Warnings) == 0 {
		return
	}
	t.Line("")
	t.Line("Not applied:")
	for _, w := range v.Warnings {
		// Already sanitized where the warning was made (daemon/config), and cleaned again here because
		// this is the line that reaches the terminal.
		t.Line("  %s", output.Clean(w))
	}
	if v.MoreWarnings > 0 {
		t.Line("  and %d more", v.MoreWarnings)
	}
}

type exportedView struct {
	Path string `json:"path"`
}

func (v exportedView) Text(t *output.Text) { t.Line("wrote %s", output.Clean(v.Path)) }

type changeView struct {
	Key string `json:"key"`
	// From is null when this machine's file did not set the key.
	From *string `json:"from"`
	To   string  `json:"to"`
}

type importView struct {
	// Written is false for a dry run, and for an import with nothing to change.
	Written bool         `json:"written"`
	Applied []changeView `json:"applied"`
	Kept    []changeView `json:"kept"`
	Skipped []string     `json:"skipped"`
	// MoreSkipped counts what Skipped did not have room for.
	MoreSkipped int `json:"more_skipped"`
}

func planView(p *config.ImportPlan, written bool) importView {
	v := importView{
		Written:     written,
		Applied:     changesOf(p.Apply),
		Kept:        changesOf(p.Kept),
		Skipped:     warnings(p.Skipped),
		MoreSkipped: p.MoreSkipped,
	}
	return v
}

func changesOf(changes []config.Change) []changeView {
	out := make([]changeView, 0, len(changes))
	for _, c := range changes {
		view := changeView{Key: c.Key(), To: c.To}
		if c.Replace {
			from := c.From
			view.From = &from
		}
		out = append(out, view)
	}
	return out
}

// changes draws what the import would do or did, without the closing line.
func (v importView) changes(t *output.Text) {
	for _, c := range v.Applied {
		if c.From == nil {
			t.Line("  add      %s = %s", output.Clean(c.Key), output.Clean(c.To))
		} else {
			t.Line("  replace  %s = %s  (was %s)", output.Clean(c.Key), output.Clean(c.To), output.Clean(*c.From))
		}
	}
	for _, c := range v.Kept {
		t.Line("  keep     %s = %s  (the file has %s; --overwrite takes it)",
			output.Clean(c.Key), output.Clean(*c.From), output.Clean(c.To))
	}
	for _, s := range v.Skipped {
		t.Line("  skip     %s", output.Clean(s))
	}
	if v.MoreSkipped > 0 {
		t.Line("  skip     and %d more", v.MoreSkipped)
	}
}

func (v importView) Text(t *output.Text) {
	v.changes(t)
	switch {
	case v.Written:
		t.Line("imported %d setting(s)", len(v.Applied))
	case len(v.Applied) == 0:
		t.Line("nothing to import")
	default:
		t.Line("%d setting(s) would be imported; nothing was changed", len(v.Applied))
	}
}

// toggledView is what `norite config split` and `unsplit` did. Its fields are the daemon's answer.
type toggledView ipc.ConfigToggle

func (v toggledView) MarshalJSON() ([]byte, error) { return json.Marshal(ipc.ConfigToggle(v)) }

func (v toggledView) Text(t *output.Text) {
	if v.Split {
		t.Line("split: the terminal client and the GUI now read a config file each")
		for _, f := range v.Files {
			t.Line("  %s", output.Clean(f))
		}
		t.Line("config.toml is left where it is, and is read by neither. `norite config unsplit` undoes this.")
		return
	}
	t.Line("unsplit: both clients read one file again")
	for _, f := range v.Files {
		t.Line("  %s", output.Clean(f))
	}
	if v.Base != "" {
		t.Line("started from %s, the more recently saved", output.Clean(v.Base))
	}
	for _, k := range v.Merged {
		t.Line("  take     %s  (from the other client's file)", output.Clean(k))
	}
	for _, k := range v.Kept {
		t.Line("  keep     %s  (both set it differently; the more recent file's value stays)", output.Clean(k))
	}
	for _, s := range v.Skipped {
		t.Line("  skip     %s", output.Clean(s))
	}
	if len(v.Backups) > 0 {
		t.Line("nothing was deleted; what was replaced is kept as:")
		for _, f := range v.Backups {
			t.Line("  %s", output.Clean(f))
		}
	}
}
