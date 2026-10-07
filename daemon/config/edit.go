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

// setRaw returns data with path set to literal, a TOML value already spelled as the file will hold it.
func setRaw(data []byte, path []string, literal string) ([]byte, error) {
	if len(path) < 2 {
		return nil, errors.New("a key lives in a section; the root of config.toml holds none")
	}
	if err := oneValue(literal); err != nil {
		return nil, err
	}
	body, bom := cutBOM(data)
	doc, err := parseDocument(body)
	if err != nil {
		return nil, err
	}
	if err := doc.reachable(path); err != nil {
		return nil, err
	}

	var out []byte
	if a, ok := doc.find(path); ok {
		out = splice(body, a.value, literal)
	} else {
		out = doc.insert(path, literal)
	}
	if err := sameAfter(out, path); err != nil {
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
	body, bom := cutBOM(data)
	doc, err := parseDocument(body)
	if err != nil {
		return nil, err
	}
	if err := doc.reachable(path); err != nil {
		return nil, err
	}
	a, ok := doc.find(path)
	if !ok {
		return data, nil
	}
	data = body
	start := lineStart(data, a.expr.start)
	end := lineEnd(data, a.expr.end)
	// Only when the key has the line to itself. `a = 1; b = 2` is not TOML, so the tail can only be
	// blanks and a comment, but the head is checked rather than assumed.
	if len(bytes.TrimSpace(data[start:a.expr.start])) != 0 {
		return nil, errors.New("shares its line with something else; remove it by hand")
	}
	out := append(append([]byte{}, data[:start]...), data[end:]...)
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

func (d *document) find(path []string) (assignment, bool) {
	for _, a := range d.assignments {
		if equal(a.path, path) {
			return a, true
		}
	}
	return assignment{}, false
}

// insert adds `key = literal` for a path that is not set, in the place a person would have put it.
func (d *document) insert(path []string, literal string) []byte {
	parent, leaf := path[:len(path)-1], path[len(path)-1]
	nl := newline(d.data)

	// 1. Its table has a header: the new key goes after that table's last key.
	for i := len(d.tables) - 1; i >= 0; i-- {
		if equal(d.tables[i].path, parent) {
			at := lineEnd(d.data, d.tables[i].last)
			return insertLine(d.data, at, formatKey(leaf)+" = "+literal, nl)
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
		return insertLine(d.data, lineEnd(d.data, last), strings.Join(rel, ".")+" = "+literal, nl)
	}
	// 3. Nothing of it exists: a new table at the end of the file.
	parts := make([]string, len(parent))
	for i, part := range parent {
		parts[i] = formatKey(part)
	}
	out := append([]byte{}, d.data...)
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, nl...)
	}
	if len(bytes.TrimSpace(out)) > 0 {
		out = append(out, nl...)
	}
	out = append(out, "["+strings.Join(parts, ".")+"]"+nl...)
	out = append(out, formatKey(leaf)+" = "+literal+nl...)
	return out
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
func sameAfter(out []byte, path []string) error {
	var decoded map[string]any
	if err := toml.Unmarshal(out, &decoded); err != nil {
		return fmt.Errorf("the edit would leave a file that is not valid TOML, so nothing was written: %w",
			parseError(err))
	}
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
	return nil
}

func splice(data []byte, at span, with string) []byte {
	out := make([]byte, 0, len(data)-(at.end-at.start)+len(with))
	out = append(out, data[:at.start]...)
	out = append(out, with...)
	return append(out, data[at.end:]...)
}

func insertLine(data []byte, at int, line, nl string) []byte {
	prefix := ""
	// The last line of a file with no final newline: the new line needs one in front of it.
	if at > 0 && data[at-1] != '\n' {
		prefix = nl
	}
	return splice(data, span{at, at}, prefix+line+nl)
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
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
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
		return "", fmt.Errorf("%s is a table; set a key inside it", key.Name)
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
