// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package output is how a command's result reaches stdout: as text for a person, or as JSON for a script,
// from one value either way (M20).
//
// # One result, two presentations
//
// A command produces a Result and nothing prints it but Render. Its JSON form is the value itself, whose
// shape contracts/cli-json/ pins (rule 15); its text form is its Text method. Neither is a second code path
// to the data, which is what M48 needs to draw the same result in a TUI pane, and what stops the text and
// the JSON disagreeing about what a command found.
//
// # JSON is lossless and inert
//
// encoding/json escapes the C0 controls and leaves the C1 controls, DEL and the bidi overrides raw. So a
// listing printed to a terminal — `norite guild list --json` with no pipe after it is the ordinary case —
// could drive the terminal with a name a stranger chose. WriteJSON escapes every rune termsafe would remove
// as \uXXXX: a parser reads the original rune back, so nothing is lost, and the terminal sees six inert
// characters, so nothing acts (rule 19). That is safe on encoded JSON because outside a string valid JSON
// holds only ASCII punctuation and whitespace, so every such rune is inside one.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Result is what a command produces. It marshals as its --json form and draws itself as text.
type Result interface {
	Text(t *Text)
}

// Render writes r to w: JSON when asJSON, text otherwise.
func Render(w io.Writer, asJSON bool, r Result) error {
	if asJSON {
		return WriteJSON(w, r)
	}
	t := &Text{w: w}
	r.Text(t)
	return t.err
}

// WriteJSON writes v as indented JSON with every rune termsafe removes escaped, and a final newline.
//
// Indented because this output is read by people as often as by scripts, and jq does not care. HTML
// escaping is off: `<` in a guild name is a character, and < in a terminal is noise.
func WriteJSON(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encoding output: %w", err)
	}
	_, err := w.Write(Inert(buf.Bytes()))
	return err
}

// WriteJSONLine writes v as one line of JSON, escaped as WriteJSON escapes, for a command whose output is
// a stream of objects rather than one: each line is a whole value, so a reader takes them as they come.
func WriteJSONLine(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encoding output: %w", err)
	}
	_, err := w.Write(Inert(buf.Bytes()))
	return err
}

// Inert rewrites every rune termsafe removes, in already-encoded JSON, as a \u escape.
//
// Only runes from DEL up are candidates. Below it, a control character in valid JSON is structural
// whitespace — the indentation's newlines — because inside a string encoding/json has already escaped them.
func Inert(encoded []byte) []byte {
	if !bytes.ContainsFunc(encoded, needsEscape) {
		return encoded
	}
	out := make([]byte, 0, len(encoded)+32)
	for len(encoded) > 0 {
		r, size := utf8.DecodeRune(encoded)
		if needsEscape(r) {
			if r > 0xFFFF {
				hi, lo := utf16.EncodeRune(r)
				out = fmt.Appendf(out, `\u%04x\u%04x`, hi, lo)
			} else {
				out = fmt.Appendf(out, `\u%04x`, r)
			}
		} else {
			out = append(out, encoded[:size]...)
		}
		encoded = encoded[size:]
	}
	return out
}

func needsEscape(r rune) bool {
	return r >= 0x7f && r != utf8.RuneError && termsafe.Removes(r)
}

// Text is where a Result draws itself for a person. Every value from an instance goes through Clean or
// Block before it reaches Line; nothing here sanitizes on the caller's behalf, because the caller is the one
// who knows which values are foreign.
type Text struct {
	w   io.Writer
	err error
}

// NewText returns a Text writing to w, for a command that draws something outside its result: what an
// import would change, shown on stderr beside the question that follows it.
func NewText(w io.Writer) *Text { return &Text{w: w} }

// Err returns the first write error, if any.
func (t *Text) Err() error { return t.err }

// Line writes one line. The first write error is kept and later writes are skipped, so a closed pipe stops
// the output rather than failing at every line.
func (t *Text) Line(format string, args ...any) {
	if t.err != nil {
		return
	}
	_, t.err = fmt.Fprintf(t.w, format+"\n", args...)
}

// Clean is termsafe.Text: for a value printed inside a line — a name, an id, a code.
func Clean(s string) string { return termsafe.Text(s) }

// Block is termsafe.Block: for a value that spans lines — message content, a report's detail.
func Block(s string) string { return termsafe.Block(s) }

// OrNone renders an optional value: the sanitized string, or "-" when there is none.
func OrNone(s *string) string {
	if s == nil {
		return "-"
	}
	return Clean(*s)
}
