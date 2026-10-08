// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Source says where a value comes from.
type Source string

// Where a value can come from, nearest first.
const (
	// FromFile: the section asked about sets it.
	FromFile Source = "file"
	// FromShared: a client's section does not set it, and [shared] does.
	FromShared Source = "shared"
	// FromDefault: nothing in the file sets it.
	FromDefault Source = "default"
	// NotSet: nothing sets it and it has no default, which is every key with no reader yet.
	NotSet Source = "unset"
)

// Entry is one key as a section sees it.
type Entry struct {
	Section Section
	// Name is the key without its section. For a key inside a table it runs on past the table's name:
	// "keys.C-x b".
	Name string
	Key  Key
	// Value is a string, or a map[string]any for a whole table, or nil when Source is NotSet. A color is
	// the string the file would hold: "6" or "#1e90ff".
	Value  any
	Source Source
	// notString marks a table's entry the file sets to something other than a string. Value is then how
	// it reads written out, so a caller printing it prints a string as it is promised one, and an import
	// knows the two are not the same value however alike they are spelled.
	notString bool
}

// ErrMachineLocal reports an import that sets a key the contract marks machine-local.
var ErrMachineLocal = errors.New("is a setting for one machine and cannot be imported")

// Get returns name as section sees it.
func (f *File) Get(section Section, name string) (Entry, error) {
	if !validSection(string(section)) {
		return Entry{}, fmt.Errorf("[%s]: %w; the sections are [shared], [tui] and [gui]", section, ErrUnknownKey)
	}
	key, ok := lookup(section, name)
	if !ok {
		return Entry{}, fmt.Errorf("%s.%s: %w", section, name, ErrUnknownKey)
	}
	e := Entry{Section: section, Name: name, Key: key, Source: NotSet}
	inner, isInner := strings.CutPrefix(name, key.Name+".")

	from := []struct {
		section Section
		source  Source
	}{{section, FromFile}}
	if key.Section == Shared && section != Shared {
		from = append(from, struct {
			section Section
			source  Source
		}{Shared, FromShared})
	}
	for _, place := range from {
		value, set := f.set[place.section][key.Name]
		if !set {
			continue
		}
		if isInner {
			table, _ := value.(map[string]any)
			if value, set = table[inner]; !set {
				continue
			}
			if _, isString := value.(string); !isString {
				// An entry's meaning is its milestone's to define, and so far every one is a string. `a = 5`
				// is shown as "5": what the file says, in the one type an entry's value is promised to be.
				value, e.notString = fmt.Sprint(value), true
			}
		}
		e.Value, e.Source = plain(value), place.source
		return e, nil
	}
	if key.Default != "" && !isInner {
		e.Value, e.Source = key.Default, FromDefault
	}
	return e, nil
}

// Entries lists every key the contract defines, each under its own section, followed by any shared key a
// client's section overrides. It is what `norite config get` prints with no key named.
func (f *File) Entries() []Entry { return f.EntriesFor("") }

// readFor lists the sections a file is read for: all of them when it is config.toml, which both clients
// read, and [shared] with one client's own when it is that client's file while the toggle is on. The
// other client's section in such a file is a copy of how things stood at the split, in force nowhere.
func readFor(client Section) []Section {
	if client == TUI || client == GUI {
		return []Section{Shared, client}
	}
	return []Section{Shared, TUI, GUI}
}

// EntriesFor is Entries for the file one client reads while the toggle is on, client being whose; "" is
// config.toml. A listing that showed the other client's stale section as set would be showing settings
// that are not in force, beside a `get` of the same key that refuses to (M21 /code-review).
func (f *File) EntriesFor(client Section) []Entry {
	read := readFor(client)
	var out []Entry
	for _, key := range keys {
		if !slices.Contains(read, key.Section) {
			continue
		}
		e, _ := f.Get(key.Section, key.Name)
		out = append(out, e)
	}
	for _, section := range []Section{TUI, GUI} {
		if !slices.Contains(read, section) {
			continue
		}
		for _, key := range keys {
			if key.Section != Shared {
				continue
			}
			if _, set := f.set[section][key.Name]; set {
				e, _ := f.Get(section, key.Name)
				out = append(out, e)
			}
		}
	}
	return out
}

// plain turns a stored value into what a caller prints: a Color becomes its string.
func plain(value any) any {
	if c, ok := value.(Color); ok {
		return string(c)
	}
	return value
}

// Export returns a config.toml holding the portable keys f sets, and only those: nothing machine-local,
// no default the file did not state, no key this version does not know. It is a new document, so it
// carries none of the original's comments; it is for carrying settings to another machine, where
// `norite config import` merges it into a file that keeps its own.
//
// It is written by this package's own editor rather than by the TOML library's marshaler, for one reason:
// the library writes a bidi override or a C1 control raw, which TOML permits, and an export is printed to
// a terminal. BasicString escapes them. A table entry that is not a string is left out, as import leaves
// it out: its milestone has not said what it means.
func (f *File) Export() ([]byte, error) { return f.ExportFor("") }

// ExportFor is Export of the file one client reads while the toggle is on, client being whose; "" is
// config.toml. The other client's section is left out, as EntriesFor leaves it out: an export of a stale
// copy is settings in force nowhere, carried to another machine and applied there.
func (f *File) ExportFor(client Section) ([]byte, error) {
	var edits []edit
	doc := []byte("# Exported by `norite config export`. Merge it into another machine's config with\n" +
		"# `norite config import <this file>`, which shows what it would change before changing it.\n")
	for _, section := range readFor(client) {
		for _, name := range slices.Sorted(maps.Keys(f.set[section])) {
			key, ok := lookup(section, name)
			if !ok || !key.Portable {
				continue
			}
			values := flatten(section, name, f.set[section][name], &ImportPlan{})
			for _, inner := range slices.Sorted(maps.Keys(values)) {
				segments, literal, err := resolve(section, inner, values[inner], true)
				if err != nil {
					return nil, fmt.Errorf("exporting %s.%s: %w", section, inner, err)
				}
				edits = append(edits, edit{segments, literal})
			}
		}
	}
	doc, err := setAll(doc, edits)
	if err != nil {
		return nil, fmt.Errorf("exporting: %w", err)
	}
	return doc, nil
}

// Change is one key an import would touch.
type Change struct {
	Section Section
	Name    string
	// From is the value this machine's file sets, or "" when it sets none. From and To are for showing:
	// both are sanitized, since one of them came out of a file somebody was sent (rule 19).
	From string
	To   string
	// name and to are the key and the incoming value exactly as the file held them, which is what gets
	// written. A table's inner key is a stranger's text as much as its value is.
	name, to string
	// Replace is true when the file already sets the key to something else.
	Replace bool
}

// Key returns the change's full key, "tui.colors.accent".
func (c Change) Key() string { return string(c.Section) + "." + c.Name }

// ImportPlan is what an import would do, worked out before anything is written.
type ImportPlan struct {
	// Apply lists the keys that will be written.
	Apply []Change
	// Kept lists the keys both files set differently, where this machine's value stays because the
	// import was not told to overwrite.
	Kept []Change
	// Skipped lists what the incoming file holds that was not understood, and why.
	Skipped     []Warning
	MoreSkipped int
}

// skip records something the incoming file holds that was not carried over.
func (p *ImportPlan) skip(key, problem string) {
	if len(p.Skipped) < MaxWarnings {
		p.Skipped = append(p.Skipped, Warning{Key: termsafe.Text(key), Problem: problem})
		return
	}
	p.MoreSkipped++
}

// Empty reports an import that would change nothing.
func (p *ImportPlan) Empty() bool { return len(p.Apply) == 0 }

// PlanImport works out what merging incoming into current would do.
//
// The incoming file is somebody else's: bounded, parsed leniently like any config, and every string taken
// from it sanitized before it is put in a Change, since a plan exists to be shown. A machine-local key is
// refused outright, by name. Dropping it quietly would let a file that binds a key to a command look like
// it merely "did not all apply".
//
// A key both files set is kept as this machine has it unless overwrite is true. That is the direction
// under which importing never disturbs settings somebody already made.
func PlanImport(current, incoming []byte, overwrite bool) (*ImportPlan, error) {
	return PlanImportInto(current, incoming, overwrite, "")
}

// PlanImportInto is PlanImport for one client's own file while the same-machine toggle is on, client
// being whose it is; "" is config.toml, which both read. The other client's section is then left out,
// each of its keys skipped with the reason: nothing reads [gui] from the terminal client's file, and an
// import that wrote it there would report settings imported that do nothing, which `set` refuses to do.
func PlanImportInto(current, incoming []byte, overwrite bool, client Section) (*ImportPlan, error) {
	return planMerge(current, incoming, overwrite, mergeOptions{
		sections: []Section{Shared, TUI, GUI}, notRead: otherClient(client),
	})
}

// otherClient is the client that is not this one, or "" when given neither.
func otherClient(client Section) Section {
	switch client {
	case TUI:
		return GUI
	case GUI:
		return TUI
	}
	return ""
}

// mergeOptions says which merge planMerge is planning.
type mergeOptions struct {
	// own is set when both files are this machine's, so a machine-local key is carried and not refused.
	own bool
	// sections are the ones taken from the incoming file.
	sections []Section
	// notRead, when set, is a section the target file is not read for: its keys are skipped, and said.
	notRead Section
}

// planMerge works out what taking opt.sections of incoming into current would do.
func planMerge(current, incoming []byte, overwrite bool, opt mergeOptions) (*ImportPlan, error) {
	have, err := Inspect(current)
	if err != nil {
		return nil, err
	}
	want, err := Inspect(incoming)
	if err != nil {
		if opt.own {
			return nil, err
		}
		return nil, fmt.Errorf("the file to import: %w", err)
	}
	return planFiles(have, want, overwrite, opt)
}

// planFiles is planMerge on two files already read.
func planFiles(have, want *File, overwrite bool, opt mergeOptions) (*ImportPlan, error) {
	own := opt.own
	plan := &ImportPlan{Skipped: want.Warnings, MoreSkipped: want.MoreWarnings}
	for _, section := range opt.sections {
		for _, name := range slices.Sorted(maps.Keys(want.set[section])) {
			key, _ := lookup(section, name)
			if opt.notRead != "" && section == opt.notRead {
				plan.skip(string(section)+"."+name, fmt.Sprintf("is the %s client's, and this file is read "+
					"for [shared] and [%s] while the config is split; import it with --client %s",
					section, otherClient(section), section))
				continue
			}
			if !key.Portable && !own {
				return nil, fmt.Errorf("%s.%s %w", section, name, ErrMachineLocal)
			}
			values := flatten(section, name, want.set[section][name], plan)
			for _, inner := range slices.Sorted(maps.Keys(values)) {
				to := values[inner]
				from, isSet, same := "", false, false
				if e, err := have.Get(section, inner); err == nil && e.Source == FromFile {
					from, isSet = fmt.Sprint(e.Value), true
					// `a = 5` and an incoming `a = "5"` are spelled alike here and are not the same value.
					same = from == to && !e.notString
				}
				change := Change{Section: section, Name: termsafe.Text(inner), From: termsafe.Text(from),
					To: termsafe.Text(to), Replace: isSet, name: inner, to: to}
				switch {
				case same:
				case isSet && !overwrite:
					plan.Kept = append(plan.Kept, change)
				default:
					plan.Apply = append(plan.Apply, change)
				}
			}
		}
	}
	slices.SortFunc(plan.Apply, func(a, b Change) int { return strings.Compare(a.Key(), b.Key()) })
	slices.SortFunc(plan.Kept, func(a, b Change) int { return strings.Compare(a.Key(), b.Key()) })
	return plan, nil
}

// ClientFileError is a failure to read one of the two clients' files, saying which.
type ClientFileError struct {
	Client Section
	Err    error
}

func (e *ClientFileError) Error() string { return e.Err.Error() }
func (e *ClientFileError) Unwrap() error { return e.Err }

// MergeClients folds the two clients' own config files back into one, and returns it with what it did. It
// is what turning the same-machine toggle off does. newer names the client whose file was written more
// recently, and that file's bytes are the starting point, comments included.
//
// Each client's section is its own file's. A split starts both files as copies of config.toml, so the
// GUI's file holds a [tui] nobody has read since and the terminal's a [gui]: stale copies of how things
// were. Merged key by key with no regard for whose section a key is in, the stale copy brought back a
// setting its owner had removed, and overrode one its owner had changed whenever the other file happened
// to be the newer (M21 /code-review). So [tui] is taken from the terminal client's file and [gui] from the
// GUI's, removals included, whichever is newer.
//
// It is taken as the file wrote it, not through the key table: every assignment under the section, at any
// depth, with its value's own bytes. A number in a table, a nested table and a key from a newer Norite
// are carried like any other, where going through the keys this version knows carried strings only and
// said nothing about the rest. Two things cannot be carried by a splice, a value that spans lines and a
// key under an array of tables, and each is in the plan's Skipped by name.
//
// [shared] is the one section both read, and the one where "last write wins" is a question. It is asked
// per key: a key only one file sets is kept, and where both set one the newer file's value stays and the
// key is in the plan's Kept. What the older file's [shared] holds that this version does not understand
// is in Skipped too; the caller keeps that file.
func MergeClients(tui, gui []byte, newer Section) ([]byte, *ImportPlan, error) {
	base, other, mine, theirs := tui, gui, TUI, GUI
	if newer == GUI {
		base, other, mine, theirs = gui, tui, GUI, TUI
	}
	// Each file is decoded once, here. Everything below works from what that found, or from this
	// package's own parser, which is linear.
	have, err := Inspect(base)
	if err != nil {
		return nil, nil, &ClientFileError{Client: mine, Err: err}
	}
	want, err := Inspect(other)
	if err != nil {
		return nil, nil, &ClientFileError{Client: theirs, Err: err}
	}

	// [shared]: what the base lacks is added, and what both set stays the base's.
	plan, err := planFiles(have, want, false, mergeOptions{own: true, sections: []Section{Shared}})
	if err != nil {
		return nil, nil, err
	}
	// The warnings are about the whole of the other file. Its own section is carried as written, known
	// to this version or not, and its copy of the base's section is nobody's to carry.
	plan.Skipped = slices.DeleteFunc(plan.Skipped, func(w Warning) bool {
		return !strings.HasPrefix(w.Key, string(Shared)+".") && w.Key != string(Shared)
	})
	plan.MoreSkipped = 0
	edits := make([]edit, 0, len(plan.Apply))
	for _, change := range plan.Apply {
		segments, literal, err := resolve(change.Section, change.name, change.to, true)
		if err != nil {
			return nil, nil, err
		}
		edits = append(edits, edit{segments, literal})
	}

	// The other client's section: what its own file assigns, over the base's stale copy.
	baseBody, _ := cutBOM(base)
	stale, err := scanDocument(baseBody)
	if err != nil {
		return nil, nil, err
	}
	otherBody, _ := cutBOM(other)
	theirDoc, err := scanDocument(otherBody)
	if err != nil {
		return nil, nil, err
	}
	was := stale.under(theirs)
	now := map[string]bool{}
	for _, a := range theirDoc.assignments {
		if a.path[0] != string(theirs) {
			continue
		}
		id, key := strings.Join(a.path, "\x00"), strings.Join(a.path, ".")
		now[id] = true
		literal := string(otherBody[a.value.start:a.value.end])
		switch {
		case theirDoc.inArray(a.path):
			plan.skip(key, "is under an array of tables, which cannot be placed in another file; it is in the copy set aside")
			continue
		case oneValue(literal) != nil:
			plan.skip(key, "has a value that spans more than one line, which cannot be placed in another file; it is in the copy set aside")
			continue
		}
		old, had := was[id]
		if had && old == literal {
			continue
		}
		edits = append(edits, edit{a.path, literal})
		plan.Apply = append(plan.Apply, Change{Section: theirs, Name: termsafe.Text(strings.Join(a.path[1:], ".")),
			From: termsafe.Text(old), To: termsafe.Text(literal), Replace: had})
	}
	slices.SortFunc(plan.Apply, func(a, b Change) int { return strings.Compare(a.Key(), b.Key()) })

	// What the stale copy assigns that its owner's file no longer does goes first, so that a table the
	// owner now writes another way is out of the way before it is written.
	var gone [][]string
	for _, a := range stale.assignments {
		if a.path[0] == string(theirs) && !now[strings.Join(a.path, "\x00")] {
			gone = append(gone, a.path)
		}
	}
	merged, err := unsetAll(base, gone)
	if err != nil {
		return nil, nil, err
	}
	merged, err = setAll(merged, edits)
	if err != nil {
		return nil, nil, err
	}
	return merged, plan, nil
}

// under returns what the document assigns under a section, by path, each value as its own bytes.
func (d *document) under(section Section) map[string]string {
	out := map[string]string{}
	for _, a := range d.assignments {
		if a.path[0] == string(section) {
			out[strings.Join(a.path, "\x00")] = string(d.data[a.value.start:a.value.end])
		}
	}
	return out
}

// inArray reports a path that lies under an array of tables.
func (d *document) inArray(path []string) bool {
	for _, t := range d.tables {
		if t.array && isPrefix(t.path, path) {
			return true
		}
	}
	return false
}

// flatten yields the settable keys under name: the key itself for a scalar, and each string entry of a
// table. A table entry that is not a string has no meaning yet (its milestone defines one) and is skipped
// with a reason rather than written as something it is not.
func flatten(section Section, name string, value any, plan *ImportPlan) map[string]string {
	table, isTable := value.(map[string]any)
	if !isTable {
		return map[string]string{name: fmt.Sprint(plain(value))}
	}
	out := map[string]string{}
	skip := func(inner, problem string) {
		plan.skip(string(section)+"."+name+"."+inner, problem)
	}
	// In the keys' own order: what is skipped is shown, and which fifty are named when there are more must
	// not change from one run to the next.
	for _, inner := range slices.Sorted(maps.Keys(table)) {
		v := table[inner]
		if inner == "" {
			// TOML allows an empty key. Joined on, it is the table's own name with a dot after it, which names
			// no entry, and one such line would otherwise fail a whole export or import.
			skip(strconv.Quote(inner), "has an empty name, which names no entry")
			continue
		}
		s, ok := v.(string)
		if !ok {
			skip(inner, "is not a string, and this version of Norite imports only strings inside a table")
			continue
		}
		out[name+"."+inner] = s
	}
	return out
}

// importAt merges incoming into the file at path and returns what it did. A caller outside this package
// uses ImportFor, which knows which file a client reads; this is the half of it that writes.
//
// The plan is worked out again inside the write, on the file as it is under the lock, so what is returned
// is what happened even if the file changed after a caller showed somebody an earlier plan.
func importAt(path string, incoming []byte, overwrite bool) (*ImportPlan, error) {
	var done *ImportPlan
	err := Update(path, func(current []byte) ([]byte, error) {
		next, plan, err := importInto(current, incoming, overwrite, "")
		done = plan
		return next, err
	})
	if err != nil {
		return nil, named(path, err)
	}
	return done, nil
}

// ImportFor is Import into the file client reads right now, and returns which that was.
func ImportFor(client Section, incoming []byte, overwrite bool) (string, *ImportPlan, error) {
	var done *ImportPlan
	path, err := UpdateFor(client, func(_ string, split bool, current []byte) ([]byte, error) {
		whose := Section("")
		if split {
			whose = client
		}
		next, plan, err := importInto(current, incoming, overwrite, whose)
		done = plan
		return next, err
	})
	if err != nil {
		return path, nil, named(path, err)
	}
	return path, done, nil
}

func importInto(current, incoming []byte, overwrite bool, client Section) ([]byte, *ImportPlan, error) {
	plan, err := PlanImportInto(current, incoming, overwrite, client)
	if err != nil {
		return nil, nil, err
	}
	edits := make([]edit, 0, len(plan.Apply))
	for _, change := range plan.Apply {
		segments, literal, err := resolve(change.Section, change.name, change.to, true)
		if err != nil {
			return nil, nil, err
		}
		edits = append(edits, edit{segments, literal})
	}
	next, err := setAll(current, edits)
	if err != nil {
		return nil, nil, err
	}
	return next, plan, nil
}
