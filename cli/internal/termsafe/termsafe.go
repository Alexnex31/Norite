// Package termsafe makes untrusted text safe to write to a terminal.
//
// # What it guarantees
//
// That what a terminal displays, and the order it displays it in, is the printable characters that were in
// the string. Nothing else is claimed, and the boundary is deliberate — see "What it does not do".
//
// Two kinds of character break that guarantee, and both are removed:
//
//   - Control characters (Unicode category Cc: C0, DEL, and C1). A terminal *acts* on these instead of
//     printing them. `ESC [ 2 K` erases the line, `CR` returns to its start, `BS` backs over what was
//     written, `ESC ] 0 ;` retitles the window, and `ESC [ 6 n` makes the terminal write a reply onto the
//     program's *input* — which a shell then reads as a typed command. The C1 range matters on its own
//     because a lone 0x9b is CSI with no ESC in front of it to look for.
//   - The bidirectional embeddings, overrides and isolates (U+202A–U+202E, U+2066–U+2069). These reorder
//     what is displayed, so the characters are all present and read in an order nobody sent — the
//     "Trojan Source" class. A cursor-movement escape and an override differ only in mechanism: both make
//     the rendering disagree with the bytes.
//
// # What it does not do
//
// It does not remove characters that are merely invisible — zero-width spaces, word joiners, tag
// characters, soft hyphens — nor confusable letters (Cyrillic "а" for Latin "a"). Those can deceive a
// reader, but not about *which visible characters are present or where they appear*, which is the line this
// function draws. Drawing it wider is not free: the Cf category that holds the zero-width characters also
// holds U+0600 and U+06DD, which are part of written Arabic, and U+200C/U+200D, which decide how Persian
// and Indic letters join and how emoji compose. Removing those corrupts ordinary text. Deciding what is
// legible is a rendering policy, and it belongs to the renderer that has the font and the width rules
// (docs/architecture.md §4, the TUI's markdown subset), not to a filter every string passes through.
//
// It also does not parse escape sequences. Removing the ESC leaves the `[2K` that followed it, visible and
// inert. Recognizing whole sequences would print more cleanly and is not worth it: a parser has to be right
// about DCS, OSC terminated by either BEL or ST, SS2/SS3, charset designations and malformed input, and
// every case it gets wrong is a sequence that reaches the terminal. Deleting the bytes a control function
// is built from cannot be wrong in that direction.
//
// # Removed, not dropped
//
// Every removed run becomes one U+FFFD. Dropping silently would let distinct strings render identically —
// `ada` with a bidi override reads as plain `ada`, and there is then no way to tell the impostor from the
// account. That matters most for the values people identify each other by: usernames, webhook display
// names, plugin manifest descriptions. A visible mark also says that something was taken out, which is
// worth knowing when the text came from a stranger.
//
// # Where to apply it
//
// At the boundary where foreign text enters the program, not at each place it later leaves: the value is
// then safe wherever it goes — the terminal today, a file it is stored in, a log line. That is what
// daemonctl's Runner does with subprocess output and what the login client does with an instance's
// responses. Text read back out of a file a person can edit is foreign again, so a print site fed from one
// sanitizes at the print.
//
// "Untrusted" means anything the program did not compose itself, and the trust level of each source is
// deliberately not re-litigated: a username, an instance's error message, a message body, a plugin manifest
// description, a webhook display name, the output of a tool the CLI shelled out to. Text the operator typed
// at this CLI's own prompt is not in scope — a person cannot attack their own terminal by typing into it.
//
// This is the CLI's package because a terminal is the CLI's problem. The daemon does not import it: what it
// writes is JSON, where zerolog escapes the ASCII controls, and the values it holds were sanitized by the
// login that stored them. When the daemon starts rendering foreign text of its own (M19, where it holds
// the gateway connection rather than reading a file the CLI wrote), this moves
// somewhere both modules can reach rather than being copied — the mistake daemonctl's local version was
// already marked to avoid.
package termsafe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// replacement stands in for each run of removed characters. U+FFFD is the one character that already means
// "something was here that cannot be shown", it is one column wide, and it is printable — so sanitizing an
// already-sanitized string changes nothing.
const replacement = '\uFFFD'

// Text returns s with everything a terminal would act on, or that would reorder what it prints, replaced —
// for a value printed inside a line: a username, an account's instance URL, a server's error message.
//
// Line breaks and tabs are removed too, which Block keeps. A one-line value that can contain a newline can
// forge a whole line of output ("wrong password\nnorite: signed in as admin"), which is the same lie
// `ESC [ 2 K` buys by a more obvious route.
func Text(s string) string { return filter(s, false) }

// Block is Text for output that is meant to span lines: a tool's captured stdout, anything column-aligned.
//
// Newlines and tabs survive, because mangling them makes the text harder to read for no gain — neither can
// reposition a cursor or repaint a line. Never use it for a value printed inside a sentence.
func Block(s string) string { return filter(s, true) }

func filter(s string, keepLayout bool) string {
	// Invalid bytes go first, and that ordering is load-bearing rather than tidiness. Ranging a string
	// decodes an invalid byte as U+FFFD, which is printable, so a raw 0x9b — CSI itself, and not valid
	// UTF-8 on its own — would read as harmless and be copied through verbatim to a terminal that decodes
	// bytes. Text arriving as something other than UTF-8 is ordinary here: a localized Windows tool's
	// output, or a server sending whatever it likes.
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, string(replacement))
	}

	// The overwhelmingly common case is text with nothing to remove, and this runs on every piece of
	// foreign text the CLI prints, so that case allocates nothing.
	if !strings.ContainsFunc(s, func(r rune) bool { return mustRemove(r, keepLayout) }) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	removing := false
	for _, r := range s {
		if mustRemove(r, keepLayout) {
			// One mark per run, not per character: a kilobyte of NULs is one thing that happened, and
			// answering it with a kilobyte of U+FFFD would flood the screen in the sanitizer's own name.
			if !removing {
				b.WriteRune(replacement)
				removing = true
			}
			continue
		}
		removing = false
		b.WriteRune(r)
	}
	return b.String()
}

// mustRemove reports whether r acts on a terminal or reorders what it prints.
func mustRemove(r rune, keepLayout bool) bool {
	if keepLayout && (r == '\n' || r == '\t') {
		return false
	}
	// ASCII is nearly all of every string this sees, and answering it without a table lookup is what keeps
	// the scan cheap. It is exactly unicode.Cc restricted to ASCII, and a test walks every code point to
	// prove this branch and the table below never disagree.
	if r < utf8.RuneSelf {
		return r < 0x20 || r == 0x7f
	}
	return unicode.Is(unicode.Cc, r) || isBidiReordering(r)
}

// isBidiReordering reports whether r is one of the bidi controls that *moves* text.
//
// unicode.Bidi_Control also holds the three marks — U+061C, U+200E, U+200F — and those are kept. A mark
// only sets the direction of the neutral characters around it; it cannot pick up a run of text and print it
// backwards, which is what the embeddings, overrides and isolates do. They are also ordinary content: an
// Arabic letter mark appears in ordinary Arabic, and replacing it with U+FFFD would put a visible hole in
// text that was never an attack. Deriving this from the table rather than listing nine code points means a
// bidi control added to a future Unicode arrives covered.
func isBidiReordering(r rune) bool {
	switch r {
	case '\u061c', '\u200e', '\u200f': // ARABIC LETTER MARK, LEFT-TO-RIGHT MARK, RIGHT-TO-LEFT MARK
		return false
	}
	return unicode.Is(unicode.Bidi_Control, r)
}
