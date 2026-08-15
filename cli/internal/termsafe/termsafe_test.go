package termsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBlockStripsEscapeSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text is untouched", "active", "active"},
		{"newlines and tabs survive", "TaskName:\t\\X\nStatus:\tReady", "TaskName:\t\\X\nStatus:\tReady"},

		// The attack this exists to stop: a sequence that repaints the line, so what the user reads is not
		// what the command actually reported.
		{"ANSI color", "\x1b[31mactive\x1b[0m", "[31mactive[0m"},
		{"cursor movement", "stopped\x1b[2K\x1b[1Grunning", "stopped[2K[1Grunning"},
		{"carriage return overwrite", "inactive\ractive", "inactiveactive"},

		// 0x9b is CSI on its own in some terminals, with no ESC in front of it to look for — which is why
		// the filter covers the whole C1 range rather than just hunting for 0x1b.
		{"bare CSI with no ESC", "a\u009b31mb", "a31mb"},

		{"DEL", "a\x7fb", "ab"},
		{"NUL and bell", "a\x00b\ac", "abc"},

		// Sanitizing must not mangle legitimate text. A localized Windows reports its task status in the
		// system language, and those words have to survive intact.
		{"non-ASCII text is preserved", "Prêt — 日本語", "Prêt — 日本語"},
		{"emoji survive", "shipped 🚀", "shipped 🚀"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Block(tc.in); got != tc.want {
				t.Errorf("Block(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTextAlsoRemovesLineBreaksAndTabs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// The whole point of the stricter form: a value printed inside a sentence must not be able to end
		// that sentence and start a line of its own, which is the same forgery ESC [ 2 K buys.
		{"forged line", "wrong password\nnorite: signed in as admin", "wrong passwordnorite: signed in as admin"},
		{"tab", "ada\tadmin", "adaadmin"},
		{"CRLF", "ada\r\nadmin", "adaadmin"},

		// The bytes a sequence is *built* from are gone; the ordinary characters that followed the ESC are
		// left in place, visibly inert. This is not an escape-sequence parser and does not need to be.
		{"escape sequences go too", "\x1b[2K\rada\x1b[31m", "[2Kada[31m"},
		{"ordinary name is untouched", "ada", "ada"},
		{"non-ASCII name is untouched", "Ada Lovelace 日本語", "Ada Lovelace 日本語"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Text(tc.in); got != tc.want {
				t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Invalid bytes must not survive, and one of them is the whole reason: 0x9b is CSI, and it is not valid
// UTF-8 on its own, so a rune-by-rune filter sees a printable U+FFFD and passes the raw byte straight
// through to a terminal that decodes bytes. Each run of invalid bytes becomes one U+FFFD.
func TestInvalidBytesCannotReachTheTerminal(t *testing.T) {
	cases := map[string]string{
		"a\x9bb":      "a�b",
		"ada\xff":     "ada�",
		"ada\xff\xfe": "ada�",
	}
	for in, want := range cases {
		got := Text(in)
		if got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("Text(%q) = %q, which is not valid UTF-8", in, got)
		}
		if strings.ContainsRune(got, 0x9b) {
			t.Errorf("Text(%q) = %q still carries CSI", in, got)
		}
	}
}

// The common path allocates nothing, because every piece of foreign text the CLI prints goes through here.
func TestCleanTextIsReturnedUnchanged(t *testing.T) {
	const in = "a perfectly ordinary message"
	if got := Text(in); got != in {
		t.Errorf("Text(%q) = %q", in, got)
	}
	if n := testing.AllocsPerRun(100, func() { _ = Text(in) }); n != 0 {
		t.Errorf("Text allocated %v times on clean input", n)
	}
}
