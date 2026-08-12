package auth

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Username length bounds, counted in runes rather than bytes — a 32-byte cap would allow eight Han
// characters and thirty-two Latin ones, which is not a rule anyone would choose on purpose.
const (
	MinUsernameLength = 2
	MaxUsernameLength = 32
)

// NormalizeUsername puts a username into the one form that is stored, compared, and rendered.
//
// NFKC, because visually identical strings must not become distinct accounts. Without it "ﬁnn" (U+FB01
// ligature) and "finn" are two rows that no reader can tell apart, which is an impersonation primitive
// rather than a cosmetic problem. `citext` already collapses case; this collapses the rest of the
// compatibility equivalences on top of it.
//
// Normalization happens *before* validation, never after: NFKC can change a string's length, so validating
// first would let a name pass the bounds check and then be stored outside them.
func NormalizeUsername(raw string) string {
	return norm.NFKC.String(strings.TrimSpace(raw))
}

// ValidUsername reports whether a normalized username is acceptable.
//
// An allow-list, not a deny-list, and that is the whole design. The rule this replaced was
// `excludesall= `, which excluded exactly one character — U+0020 — and therefore admitted tabs, newlines,
// C0/C1 control bytes, DEL, ANSI escape introducers and the bidi override characters into a string every
// client renders. CLAUDE.md rule 19 requires a terminal-safe sanitizer in front of untrusted text; that
// sanitizer does not exist yet, so until it does this function is the only thing standing between a
// crafted username and a terminal.
//
// A deny-list would have to enumerate those classes correctly and stay correct as Unicode grows. The
// allow-list is the inverse and cannot rot: letters and digits in any script, plus the three separators
// people actually use in names. Bidi overrides and format characters need no explicit mention — they are
// neither letters nor digits, so they are already out.
//
// Emoji and symbols are excluded deliberately. Display names (`display_name`) are free-form for exactly
// that; the username is the identifier people type, quote in a report, and read to decide who they are
// talking to.
func ValidUsername(s string) bool {
	if n := utf8.RuneCountInString(s); n < MinUsernameLength || n > MaxUsernameLength {
		return false
	}
	// Rejects invalid UTF-8 too: a bad byte decodes to RuneError, which is not a letter or a digit.
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
		case r == '_', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}
