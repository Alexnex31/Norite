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
func (f *File) Entries() []Entry {
	var out []Entry
	for _, key := range keys {
		e, _ := f.Get(key.Section, key.Name)
		out = append(out, e)
	}
	for _, section := range []Section{TUI, GUI} {
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
func (f *File) Export() ([]byte, error) {
	var edits []edit
	doc := []byte("# Exported by `norite config export`. Merge it into another machine's config with\n" +
		"# `norite config import <this file>`, which shows what it would change before changing it.\n")
	for _, section := range []Section{Shared, TUI, GUI} {
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
	have, err := Inspect(current)
	if err != nil {
		return nil, err
	}
	want, err := Inspect(incoming)
	if err != nil {
		return nil, fmt.Errorf("the file to import: %w", err)
	}

	plan := &ImportPlan{Skipped: want.Warnings, MoreSkipped: want.MoreWarnings}
	for _, section := range []Section{Shared, TUI, GUI} {
		for _, name := range slices.Sorted(maps.Keys(want.set[section])) {
			key, _ := lookup(section, name)
			if !key.Portable {
				return nil, fmt.Errorf("%s.%s %w", section, name, ErrMachineLocal)
			}
			for inner, to := range flatten(section, name, want.set[section][name], plan) {
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
		if len(plan.Skipped) < MaxWarnings {
			plan.Skipped = append(plan.Skipped, Warning{
				Key: termsafe.Text(string(section) + "." + name + "." + inner), Problem: problem,
			})
		} else {
			plan.MoreSkipped++
		}
	}
	for inner, v := range table {
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

// Import merges incoming into the file at path and returns what it did.
//
// The plan is worked out again inside the write, on the file as it is under the lock, so what is returned
// is what happened even if the file changed after a caller showed somebody an earlier plan.
func Import(path string, incoming []byte, overwrite bool) (*ImportPlan, error) {
	var done *ImportPlan
	err := Update(path, func(current []byte) ([]byte, error) {
		plan, err := PlanImport(current, incoming, overwrite)
		if err != nil {
			return nil, err
		}
		edits := make([]edit, 0, len(plan.Apply))
		for _, change := range plan.Apply {
			segments, literal, err := resolve(change.Section, change.name, change.to, true)
			if err != nil {
				return nil, err
			}
			edits = append(edits, edit{segments, literal})
		}
		next, err := setAll(current, edits)
		if err != nil {
			return nil, err
		}
		done = plan
		return next, nil
	})
	if err != nil {
		return nil, named(path, err)
	}
	return done, nil
}
