// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"

	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// This file changes a config.toml without rewriting it.
//
// go-toml v2 cannot write a document back with its comments: Marshal emits a value, and a value has none.
// What v2 does have is unstable.Parser, which reports the byte range every key and value was read from. So
// an edit here is a splice. Setting a key replaces the bytes of its value and nothing else; a key that is
// not there yet is one inserted line. Every byte outside the edit is the byte that was there, which makes
// "never destroys a hand-written comment" a property of the method rather than of a serializer.
//
// The package is called unstable and promises nothing about its API. The dependency is pinned, so a change
// arrives as a compile error on an upgrade somebody chose, and the tests here read real documents.

// ErrInlineTable reports a key that is set inside an inline table or an array of tables, which a splice
// cannot safely reach into. The file is left alone and the person is told where to look.
var ErrInlineTable = errors.New("is set inside an inline table or an array of tables, " +
	"which Norite does not edit; change it by hand")

type span struct{ start, end int }

// assignment is one `key = value` line, with the full path it sets.
type assignment struct {
	path  []string
	table int  // index into document.tables, -1 for the root
	expr  span // the whole `key = value`, without a trailing comment
	value span // the value alone
	// opaque is true when the value is an inline table or an array: something may be set inside it.
	opaque bool
}

// header is one [table] line.
type header struct {
	path  []string
	array bool // [[table]]
	// last is the end of the last expression that belongs to the table: its header, or its final key.
	last int
}

type document struct {
	data        []byte
	tables      []header
	assignments []assignment
}

func parseDocument(data []byte) (*document, error) {
	// The ordinary decoder first. It knows what this file's parser does not: that a table is defined
	// twice, that a key is. A fault already in the file is then reported as the file's, with its line,
	// rather than blamed on the edit or rewritten around.
	if err := valid(data); err != nil {
		return nil, err
	}
	return scanDocument(data)
}

// scanDocument is parseDocument without the decoder, for bytes the decoder has already read: it is the
// quadratic one, and a merge that has inspected both files has no reason to pay for it again.
func scanDocument(data []byte) (*document, error) {
	doc := &document{data: data}
	p := unstable.Parser{KeepComments: true}
	p.Reset(data)
	current := -1
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			path, last := keyParts(e.Key())
			// The header's own range is empty in this parser, so its extent is taken from its key: the
			// line that key is on is the header line.
			doc.tables = append(doc.tables, header{path: path, array: e.Kind == unstable.ArrayTable, last: last})
			current = len(doc.tables) - 1
		case unstable.KeyValue:
			rel, keyEnd := keyParts(e.Key())
			var base []string
			if current >= 0 {
				base = doc.tables[current].path
			}
			path := append(append([]string{}, base...), rel...)
			v := e.Value()
			end := int(e.Raw.Offset + e.Raw.Length)
			// The value starts after the key and its "=". Not at v.Raw: this parser leaves an array's
			// range zero, so trusting it would start the splice at byte 0 of the file.
			valueStart := afterEquals(data, keyEnd, end)
			doc.assignments = append(doc.assignments, assignment{
				path:   path,
				table:  current,
				expr:   span{int(e.Raw.Offset), end},
				value:  span{valueStart, end},
				opaque: v.Kind == unstable.InlineTable || v.Kind == unstable.Array,
			})
			if current >= 0 {
				doc.tables[current].last = end
			}
		}
	}
	if err := p.Error(); err != nil {
		return nil, parseError(err)
	}
	return doc, nil
}

// afterEquals returns where a value begins: past the "=" that follows the key ending at keyEnd, and the
// blanks after it. limit is the end of the expression, which the scan never passes.
func afterEquals(data []byte, keyEnd, limit int) int {
	i := keyEnd
	for i < limit && data[i] != '=' {
		i++
	}
	if i < limit {
		i++
	}
	for i < limit && (data[i] == ' ' || data[i] == '\t') {
		i++
	}
	return i
}

func keyParts(it unstable.Iterator) (parts []string, end int) {
	for it.Next() {
		n := it.Node()
		parts = append(parts, string(n.Data))
		end = int(n.Raw.Offset + n.Raw.Length)
	}
	return parts, end
}

// edit is one key to set: its full path, and its value already spelled as the file will hold it.
type edit struct {
	path    []string
	literal string
}

// setRaw returns data with path set to literal, a TOML value already spelled as the file will hold it.
func setRaw(data []byte, path []string, literal string) ([]byte, error) {
	return setAll(data, []edit{{path, literal}})
}

// setAll returns data with every edit made.
//
// The document is read once and every edit is planned against it: a key that is set has its value's bytes
// replaced, a key that is not is one line placed where a person would have put it, and keys of a table
// that does not exist yet are gathered under one new header at the end. The plan is then applied in one
// pass and the result read back once, checking every path.
//
// It was one edit at a time at first, each re-reading the document the last one left. The decoder is
// quadratic in one table's keys and this file's own parser allocates per key, so an import of n keys was
// cubic: a file inside the size bound took two minutes to import, holding the config's lock throughout
// (M21 /code-review). Planned against one read, the same import is the cost of that read.
//
// Two edits to one path are one edit, the later value.
func setAll(data []byte, edits []edit) ([]byte, error) {
	body, bom := cutBOM(data)
	// The decoder reads the file as it was given, so a fault already in it is reported as the file's.
	doc, err := parseDocument(body)
	if err != nil {
		return nil, err
	}
	nl := newline(body)

	seen := make(map[string]int, len(edits))
	unique := make([]edit, 0, len(edits))
	for _, e := range edits {
		if len(e.path) < 2 {
			return nil, errors.New("a key lives in a section; the root of config.toml holds none")
		}
		if err := oneValue(e.literal); err != nil {
			return nil, err
		}
		if err := doc.reachable(e.path); err != nil {
			return nil, err
		}
		id := strings.Join(e.path, "\x00")
		if i, dup := seen[id]; dup {
			unique[i].literal = e.literal
			continue
		}
		seen[id] = len(unique)
		unique = append(unique, e)
	}

	// Where each edit lands in the bytes as they are now. A replacement has a width; an inserted line has
	// none, and several at one offset stay in the order they were asked for.
	type change struct {
		at   span
		text string
	}
	var changes []change
	// New tables, in the order first asked for, each with the lines that go under its header.
	var newTables []string
	newLines := map[string][]string{}
	existing := doc.index()
	paths := make([][]string, 0, len(unique))
	for _, e := range unique {
		paths = append(paths, e.path)
		if a, ok := existing[strings.Join(e.path, "\x00")]; ok {
			changes = append(changes, change{a.value, e.literal})
			continue
		}
		if at, line, ok := doc.place(e.path, e.literal); ok {
			changes = append(changes, change{span{at, at}, line})
			continue
		}
		parent, leaf := e.path[:len(e.path)-1], e.path[len(e.path)-1]
		parts := make([]string, len(parent))
		for i, part := range parent {
			parts[i] = formatKey(part)
		}
		header := "[" + strings.Join(parts, ".") + "]"
		if _, started := newLines[header]; !started {
			newTables = append(newTables, header)
		}
		newLines[header] = append(newLines[header], formatKey(leaf)+" = "+e.literal)
	}
	slices.SortStableFunc(changes, func(a, b change) int { return a.at.start - b.at.start })

	out := make([]byte, 0, len(body)+64*len(unique))
	at := 0
	for _, c := range changes {
		if c.at.start < at {
			// Two edits claiming the same bytes. The planner never produces it; a file is not worth risking
			// on that being true.
			return nil, errors.New("the edits overlap, so nothing was written")
		}
		out = append(out, body[at:c.at.start]...)
		if c.at.start == c.at.end {
			// A line, and the line before it may be the last of a file with no final newline.
			if len(out) > 0 && out[len(out)-1] != '\n' {
				out = append(out, nl...)
			}
			out = append(out, c.text+nl...)
		} else {
			out = append(out, c.text...)
		}
		at = c.at.end
	}
	out = append(out, body[at:]...)
	for _, header := range newTables {
		if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
			out = append(out, nl...)
		}
		if len(bytes.TrimSpace(out)) > 0 {
			out = append(out, nl...)
		}
		out = append(out, header+nl...)
		for _, line := range newLines[header] {
			out = append(out, line+nl...)
		}
	}
	if err := sameAfter(out, paths...); err != nil {
		return nil, err
	}
	return restoreBOM(out, bom), nil
}

func restoreBOM(body []byte, had bool) []byte {
	if !had {
		return body
	}
	return append(append([]byte{}, utf8BOM...), body...)
}

// oneValue refuses a literal that is anything but a single one-line TOML value.
//
// Reading the result back is not enough to catch this: "1\n[other]\nx = 2" splices into a file that is
// perfectly valid TOML and sets the key it was aimed at, along with a table nobody asked for. Every caller
// builds its literal with Literal, so this never fires in practice. It is here because the property the
// package promises is "one value changed", and a promise about bytes should not rest on callers behaving.
func oneValue(literal string) error {
	bad := errors.New("not a single TOML value, so nothing was written")
	if strings.ContainsAny(literal, "\r\n") {
		return bad
	}
	line := []byte("v = " + literal)
	p := unstable.Parser{KeepComments: true}
	p.Reset(line)
	if !p.NextExpression() {
		return bad
	}
	e := p.Expression()
	if e.Kind != unstable.KeyValue || int(e.Raw.Offset+e.Raw.Length) != len(line) {
		return bad
	}
	if p.NextExpression() || p.Error() != nil {
		return bad
	}
	return nil
}

// unsetRaw returns data without path's line. A key that is not set is no change.
//
// The whole line goes, its trailing comment included, since that comment was about the value. A comment
// on the lines above stays: nothing says which key it belonged to, and deleting somebody's words on a
// guess is the thing this file exists not to do.
func unsetRaw(data []byte, path []string) ([]byte, error) {
	return unsetAll(data, [][]string{path})
}

// unsetAll returns data without the lines of every path that is set. The document is read once and every
// line to go is found in it, for the reason setAll plans against one read: an unsplit may have a whole
// table's worth of keys to remove, and one read per key is the cubic import again.
func unsetAll(data []byte, paths [][]string) ([]byte, error) {
	body, bom := cutBOM(data)
	doc, err := parseDocument(body)
	if err != nil {
		return nil, err
	}
	existing := doc.index()
	var cuts []span
	for _, path := range paths {
		if err := doc.reachable(path); err != nil {
			return nil, err
		}
		a, ok := existing[strings.Join(path, "\x00")]
		if !ok {
			continue
		}
		start := lineStart(body, a.expr.start)
		// Only when the key has the line to itself. `a = 1; b = 2` is not TOML, so the tail can only be
		// blanks and a comment, but the head is checked rather than assumed.
		if len(bytes.TrimSpace(body[start:a.expr.start])) != 0 {
			return nil, fmt.Errorf("%s shares its line with something else; remove it by hand", strings.Join(path, "."))
		}
		cuts = append(cuts, span{start, lineEnd(body, a.expr.end)})
	}
	if len(cuts) == 0 {
		return data, nil
	}
	slices.SortFunc(cuts, func(a, b span) int { return a.start - b.start })
	out := make([]byte, 0, len(body))
	at := 0
	for _, c := range cuts {
		if c.start < at {
			continue // the same line asked for twice
		}
		out = append(out, body[at:c.start]...)
		at = c.end
	}
	out = append(out, body[at:]...)
	if err := valid(out); err != nil {
		return nil, fmt.Errorf("removing the key would leave a file that is not valid TOML: %w", err)
	}
	return restoreBOM(out, bom), nil
}

// reachable refuses a path a splice cannot address: one inside an inline table or array, or under an
// array of tables, where "the" key is one of several.
func (d *document) reachable(path []string) error {
	for _, a := range d.assignments {
		if a.opaque && isPrefix(a.path, path) && len(a.path) < len(path) {
			return fmt.Errorf("%s %w", strings.Join(path, "."), ErrInlineTable)
		}
	}
	for _, t := range d.tables {
		if t.array && isPrefix(t.path, path) {
			return fmt.Errorf("%s %w", strings.Join(path, "."), ErrInlineTable)
		}
	}
	return nil
}

// index returns the document's assignments by path, the first for each.
func (d *document) index() map[string]assignment {
	out := make(map[string]assignment, len(d.assignments))
	for _, a := range d.assignments {
		id := strings.Join(a.path, "\x00")
		if _, have := out[id]; !have {
			out[id] = a
		}
	}
	return out
}

// place says where `key = literal` goes for a path that is not set and whose table already exists, in the
// place a person would have put it: the offset of the line to add, and the line. It reports false when
// nothing of the table exists, and the key then belongs under a new header at the end of the file.
func (d *document) place(path []string, literal string) (at int, line string, ok bool) {
	parent, leaf := path[:len(path)-1], path[len(path)-1]

	// 1. Its table has a header: the new key goes after that table's last key.
	for i := len(d.tables) - 1; i >= 0; i-- {
		if equal(d.tables[i].path, parent) {
			return lineEnd(d.data, d.tables[i].last), formatKey(leaf) + " = " + literal, true
		}
	}
	// 2. Its table exists only through dotted keys in a shorter one ([tui] with `colors.warn = 3`). A
	// [tui.colors] header would define that table a second time, which TOML refuses, so the new key
	// joins its siblings in the same spelling.
	for i := len(d.assignments) - 1; i >= 0; i-- {
		a := d.assignments[i]
		if len(a.path) <= len(parent) || !isPrefix(parent, a.path) {
			continue
		}
		// The root counts as a table with no path: `tui.colors.accent = 6` with no header at all is a
		// config, and its sibling goes after the last key before the first header.
		var base []string
		last := a.expr.end
		if a.table >= 0 {
			base = d.tables[a.table].path
			last = d.tables[a.table].last
		} else {
			for _, other := range d.assignments {
				if other.table < 0 {
					last = max(last, other.expr.end)
				}
			}
		}
		if len(base) >= len(parent) {
			continue
		}
		rel := make([]string, 0, len(path)-len(base))
		for _, part := range path[len(base):] {
			rel = append(rel, formatKey(part))
		}
		return lineEnd(d.data, last), strings.Join(rel, ".") + " = " + literal, true
	}
	return 0, "", false
}

// valid reports whether data is a TOML document, as the decoder every reader uses sees it.
func valid(data []byte) error {
	var decoded map[string]any
	if err := toml.Unmarshal(data, &decoded); err != nil {
		return parseError(err)
	}
	return nil
}

// sameAfter checks the edited bytes are TOML and that path now reads back, through the ordinary decoder
// rather than this file's own parser, so a splice that fooled one is caught by the other.
func sameAfter(out []byte, paths ...[]string) error {
	var decoded map[string]any
	if err := toml.Unmarshal(out, &decoded); err != nil {
		return fmt.Errorf("the edit would leave a file that is not valid TOML, so nothing was written: %w",
			parseError(err))
	}
	for _, path := range paths {
		var at any = decoded
		for _, part := range path {
			table, ok := at.(map[string]any)
			if !ok {
				return errors.New("the edit did not land where it was aimed, so nothing was written")
			}
			at, ok = table[part]
			if !ok {
				return errors.New("the edit did not land where it was aimed, so nothing was written")
			}
		}
	}
	return nil
}

func lineStart(data []byte, at int) int {
	return bytes.LastIndexByte(data[:at], '\n') + 1
}

// lineEnd is the offset just past the newline that ends the line at is on, or the end of the data.
func lineEnd(data []byte, at int) int {
	if i := bytes.IndexByte(data[at:], '\n'); i >= 0 {
		return at + i + 1
	}
	return len(data)
}

// newline is the line ending the file already uses, so an edit on Windows does not mix the two.
func newline(data []byte) string {
	if bytes.Contains(data, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// formatKey spells one key segment: bare when TOML allows it, quoted otherwise.
func formatKey(part string) string {
	if bareKey.MatchString(part) {
		return part
	}
	return BasicString(part)
}

// BasicString spells s as a TOML basic string. strconv.Quote is close and wrong: it writes \x1b, which
// TOML does not have. Exported because the instance wizard writes TOML by template and needs the same
// escaping; two encoders of one rule had already drifted on \b and \f.
//
// It escapes more than TOML requires: every rune termsafe would remove, the bidi overrides and the C1
// controls among them, is written as a \u escape. TOML allows those raw, and the decoded value is the
// same either way, but a config is a file people `cat`, and `norite config export` prints one to a
// terminal. A value that came out of somebody else's file should not be able to reorder that output.
func BasicString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || r == 0x7f || termsafe.Removes(r):
			if r > 0xFFFF {
				fmt.Fprintf(&b, `\U%08X`, r)
			} else {
				fmt.Fprintf(&b, `\u%04X`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Literal validates what somebody typed for key and returns it as the file will spell it.
func Literal(key Key, input string) (string, error) {
	var raw any = input
	// A color that is all digits is a palette index, which the file holds as a number.
	if key.Kind == KindColor {
		if n, err := strconv.ParseInt(input, 10, 64); err == nil {
			raw = n
		}
	}
	if key.Kind == KindTable {
		return "", errors.New("is a table; name a key inside it, as in keys.C-x b")
	}
	value, problem := check(key, raw)
	if problem != "" {
		return "", errors.New(problem)
	}
	if c, ok := value.(Color); ok && !strings.HasPrefix(string(c), "#") {
		return string(c), nil
	}
	return BasicString(fmt.Sprint(value)), nil
}

func equal(a, b []string) bool { return slices.Equal(a, b) }

func isPrefix(prefix, path []string) bool {
	return len(prefix) <= len(path) && slices.Equal(prefix, path[:len(prefix)])
}
