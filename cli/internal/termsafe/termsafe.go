// Package termsafe makes untrusted text safe to write to a terminal.
//
// # Why the CLI needs this and the other clients do not
//
// A terminal executes what it is printed. A username carrying `ESC [ 2 K CR` erases the line it was
// written on and rewrites it, so a failed login can paint itself as a successful one; `ESC ] 0 ;` retitles
// the window; other sequences move the cursor, change colors, or on some terminals report back on the input
// stream. None of that is available to a GUI or a browser, which is why CLAUDE.md rule 19 exists only for
// this client and why docs/architecture.md §4 calls for one blanket function rather than a judgement at each
// print site.
//
// "Untrusted" here means anything the program did not compose itself: a username, an instance's error
// message, a message body, a plugin manifest description, a webhook display name, the output of a tool the
// CLI shelled out to. The trust level of each source is deliberately not re-litigated — the point of a
// blanket function is that nobody has to.
//
// # Where to apply it
//
// At the boundary where foreign text enters the program, not at each place it later leaves: the value is
// then safe wherever it goes — the terminal today, a file it is stored in, a log line. That is what
// daemonctl's Runner does with subprocess output and what the login client does with an instance's
// responses. Text read back out of a file a person can edit is foreign again, so a print site fed from one
// sanitizes at the print.
//
// # What it does not do
//
// It is not an escape-sequence parser and does not try to be: it removes the characters a sequence is built
// from, so there is nothing left to parse. It also has no opinion about width, length, or homoglyphs — a
// name that renders confusingly is a different problem from a name that acts on the terminal.
package termsafe

import (
	"strings"
	"unicode/utf8"
)

// Text returns s with every character that could act on the terminal removed, for a value printed inside a
// line — a username, an account's instance URL, an error message from a server.
//
// Line breaks and tabs go too, which Block keeps. A one-line value that can contain a newline can forge a
// whole line of output ("bad password\nSigned in as admin"), and that is the same forgery ESC [ 2 K buys,
// reached by a duller route.
func Text(s string) string { return filter(s, false) }

// Block is Text for output that is meant to span lines: a tool's captured stdout, anything column-aligned.
//
// Newlines and tabs survive, because mangling them makes the text harder to read for no gain — neither can
// reposition a cursor or repaint the screen. Never use it for a value printed inside a sentence.
func Block(s string) string { return filter(s, true) }

// filter drops the unsafe runes, and does nothing at all in the overwhelmingly common case where there are
// none — this runs on every piece of foreign text the CLI prints.
func filter(s string, keepLayout bool) string {
	// Invalid bytes are replaced before anything looks at runes, and that is load-bearing rather than
	// tidiness. Ranging a string decodes an invalid byte as U+FFFD, which is printable, so a raw 0x9b — CSI
	// itself, and not valid UTF-8 on its own — reads as harmless and is then copied through to the terminal
	// verbatim. A terminal decoding bytes rather than runes acts on it. Text arriving as something other
	// than UTF-8 is ordinary here: a localized Windows tool's output, or a server sending whatever it likes.
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, string(utf8.RuneError))
	}

	if !strings.ContainsFunc(s, func(r rune) bool { return unsafeRune(r, keepLayout) }) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		// Dropped rather than escaped. This text is read by a person, not parsed, so a visible `\x1b` would
		// be noise; what matters is that it cannot act on the terminal.
		if unsafeRune(r, keepLayout) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func unsafeRune(r rune, keepLayout bool) bool {
	if keepLayout && (r == '\n' || r == '\t') {
		return false
	}
	// C0 controls (ESC among them, which starts every ANSI sequence), DEL, and the whole C1 range — the
	// last because a lone 0x9B is CSI in some terminals, with no ESC in front of it to look for.
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}
