// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// MaxFileSize bounds what is read. A config written by hand is a few kilobytes; a megabyte is room for
// every key this project will ever define, and a bound means a file somebody was sent cannot be the thing
// that exhausts memory.
const MaxFileSize = 1 << 20

// ErrTooLarge reports a file over MaxFileSize.
var ErrTooLarge = errors.New("the config file is larger than 1 MiB, which no config written for Norite is")

// Color is a color as config.toml gives one: an ANSI palette index or "#rrggbb".
type Color string

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Warning is one thing in the file that was not applied. The file still loads.
type Warning struct {
	// Key is the full path as written, "tui.colors.acent".
	Key string
	// Problem says why, in a sentence fragment.
	Problem string
}

// String is safe to print: the key came from a file, which is text from wherever the file came from.
func (w Warning) String() string {
	return termsafe.Text(w.Key) + ": " + termsafe.Text(w.Problem)
}

// Config is what one client reads out of config.toml: defaults, then [shared], then its own section.
type Config struct {
	values   map[string]any // by Key.Name; string, Color, or map[string]any for a table
	Warnings []Warning
}

// Defaults is the configuration with no file at all.
func Defaults() *Config {
	c := &Config{values: map[string]any{}}
	for _, k := range keys {
		if k.Default == "" {
			continue
		}
		if k.Kind == KindColor {
			c.values[k.Name] = Color(k.Default)
		} else {
			c.values[k.Name] = k.Default
		}
	}
	return c
}

// Clock is "24h" or "12h".
func (c *Config) Clock() string {
	s, _ := c.values[KeyClock].(string)
	return s
}

// Color returns the color for a role key such as KeyColorAccent.
func (c *Config) Color(key string) Color {
	col, _ := c.values[key].(Color)
	return col
}

// Value returns a key's effective value as config.toml would spell a scalar, for `norite config get`.
func (c *Config) Value(name string) (any, bool) {
	v, ok := c.values[name]
	return v, ok
}

// Load reads the file at path as client sees it.
//
// A file that does not exist is the defaults and no error: no config is a supported setup. A file that is
// not valid TOML is an error, and the caller keeps whatever it had, because somebody is probably halfway
// through an edit. Anything else wrong with the file (a key nobody knows, a value of the wrong type) is a
// Warning on a Config that loaded: one bad line must not cost every other setting.
func Load(path string, client Section) (*Config, error) {
	data, err := read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Defaults(), nil
	}
	if err != nil {
		return nil, err
	}
	c, err := Parse(data, client)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func read(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the user's own config file, at the path this package resolved
	if err != nil {
		return nil, err
	}
	// Closed before returning, never held: on Windows a writer cannot replace a file somebody has open.
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	// One byte past the bound is read so that Parse, which owns the check, can see the file is over it.
	return data, nil
}

// Parse is Load for bytes already in hand.
func Parse(data []byte, client Section) (*Config, error) {
	if len(data) > MaxFileSize {
		return nil, ErrTooLarge
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, parseError(err)
	}

	c := Defaults()
	// Sorted, so the warnings come out in one order whatever order a map walks in.
	for _, top := range sortedKeys(doc) {
		if !validSection(top) {
			c.warn(top, "not a section Norite knows; the sections are [shared], [tui] and [gui]")
		}
	}
	// [shared] first and the client's own section over it. The other client's section is checked for
	// problems worth a warning and applies to nothing here.
	for _, section := range []Section{Shared, TUI, GUI} {
		table, ok := doc[string(section)].(map[string]any)
		if !ok {
			if _, present := doc[string(section)]; present {
				c.warn(string(section), "must be a table, written ["+string(section)+"]")
			}
			continue
		}
		apply := section == Shared || section == client
		c.walk(section, "", table, apply)
	}
	return c, nil
}

// walk visits a section's keys, flattening nested tables into dotted names until one is a known key.
func (c *Config) walk(section Section, prefix string, table map[string]any, apply bool) {
	for _, name := range sortedKeys(table) {
		full := prefix + name
		raw := table[name]
		// A quoted key with a dot in it ("colors.accent" = 1) is one key whose name contains a dot, not
		// a path. Joining it into one would let two different spellings mean the same setting, and the
		// editor, which works on paths, would then change one and leave the other in force.
		if strings.Contains(name, ".") {
			c.warn(string(section)+"."+strconv.Quote(full), "not a key this version of Norite knows; it is kept and ignored")
			continue
		}
		key, known := lookup(section, full)
		if !known {
			if nested, ok := raw.(map[string]any); ok && hasKeyUnder(section, full) {
				c.walk(section, full+".", nested, apply)
				continue
			}
			c.warn(string(section)+"."+full, "not a key this version of Norite knows; it is kept and ignored")
			continue
		}
		value, problem := check(key, raw)
		if problem != "" {
			c.warn(string(section)+"."+full, problem)
			continue
		}
		if apply && key.Name == full {
			c.values[key.Name] = value
		}
	}
}

func hasKeyUnder(section Section, prefix string) bool {
	for _, k := range keys {
		if (k.Section == Shared || k.Section == section) && strings.HasPrefix(k.Name, prefix+".") {
			return true
		}
	}
	return false
}

// check validates raw against key's kind and returns the value to keep, or what is wrong with it.
func check(key Key, raw any) (any, string) {
	switch key.Kind {
	case KindEnum:
		s, ok := raw.(string)
		if !ok {
			return nil, "must be a string, one of " + quoted(key.Values)
		}
		for _, v := range key.Values {
			if s == v {
				return s, ""
			}
		}
		return nil, "must be one of " + quoted(key.Values)
	case KindColor:
		return checkColor(raw)
	case KindString:
		s, ok := raw.(string)
		if !ok {
			return nil, "must be a string"
		}
		return s, ""
	case KindTable:
		t, ok := raw.(map[string]any)
		if !ok {
			return nil, "must be a table"
		}
		return t, ""
	}
	return nil, "has a type this version of Norite does not know"
}

func checkColor(raw any) (any, string) {
	const want = "must be an ANSI color number from 0 to 255, or a string like \"#1e90ff\""
	switch v := raw.(type) {
	case int64:
		if v < 0 || v > 255 {
			return nil, want
		}
		return Color(strconv.FormatInt(v, 10)), ""
	case string:
		if !hexColor.MatchString(v) {
			return nil, want
		}
		return Color(strings.ToLower(v)), ""
	}
	return nil, want
}

func (c *Config) warn(key, problem string) {
	c.Warnings = append(c.Warnings, Warning{Key: key, Problem: problem})
}

// ParseError is a file that is not TOML, with where it stopped being.
type ParseError struct {
	Line, Column int
	msg          string
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.msg)
	}
	return e.msg
}

// parseError keeps the position and the library's own one-line reason, sanitized: the reason can quote
// the file, and the file is foreign text on its way to a terminal.
func parseError(err error) error {
	var decode *toml.DecodeError
	if errors.As(err, &decode) {
		line, column := decode.Position()
		return &ParseError{Line: line, Column: column, msg: termsafe.Text(decode.Error())}
	}
	return &ParseError{msg: termsafe.Text(err.Error())}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func quoted(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.Quote(v)
	}
	return strings.Join(parts, " or ")
}
