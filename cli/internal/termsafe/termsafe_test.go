package termsafe

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// The mark a removed run leaves. Spelled out here rather than imported from the code under test, so a
// change to the sentinel has to be made twice and meant both times.
const mark = "�"

func TestBlockRemovesWhatATerminalWouldActOn(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text is untouched", "active", "active"},
		{"newlines and tabs survive", "TaskName:\t\\X\nStatus:\tReady", "TaskName:\t\\X\nStatus:\tReady"},

		// The attack this exists to stop: a sequence that repaints the line, so what the user reads is not
		// what the command actually reported. The `[2K` left behind is ordinary printable text — this
		// removes the bytes a control function is built from, it does not parse sequences.
		{"ANSI color", "\x1b[31mactive\x1b[0m", mark + "[31mactive" + mark + "[0m"},
		{"cursor movement", "stopped\x1b[2K\x1b[1Grunning", "stopped" + mark + "[2K" + mark + "[1Grunning"},
		{"carriage return overwrite", "inactive\ractive", "inactive" + mark + "active"},
		{"backspace overwrite", "admin\b\b\b\b\bguest", "admin" + mark + "guest"},

		// 0x9b is CSI on its own in some terminals, with no ESC in front of it to look for — which is why
		// this covers the whole C1 range rather than hunting for 0x1b.
		{"bare CSI with no ESC", "a\u009b31mb", "a" + mark + "31mb"},

		{"DEL", "a\x7fb", "a" + mark + "b"},
		{"NUL and bell", "a\x00b\ac", "a" + mark + "b" + mark + "c"},

		// One mark per run: a kilobyte of NULs is one thing that happened, and answering it with a kilobyte
		// of U+FFFD would flood the screen in the sanitizer's own name.
		{"a run collapses to one mark", "a\x00\x00\x00\x1b\x1bb", "a" + mark + "b"},

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
		// that sentence and start a line of its own, which is the same lie ESC [ 2 K buys.
		{"forged line", "wrong password\nnorite: signed in as admin",
			"wrong password" + mark + "norite: signed in as admin"},
		{"tab", "ada\tadmin", "ada" + mark + "admin"},
		{"CRLF is one run", "ada\r\nadmin", "ada" + mark + "admin"},

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

// ---------- bidirectional text ----------

// The Trojan Source class: an override picks up the text after it and prints it backwards, so every
// character is present and the reader is shown an order nobody sent.
func TestBidiOverridesAreRemoved(t *testing.T) {
	for _, r := range []rune{
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e', // embeddings, the pop, and the overrides
		'\u2066', '\u2067', '\u2068', '\u2069', // isolates and their pop
	} {
		in := "safe" + string(r) + "name"
		if got := Text(in); got != "safe"+mark+"name" {
			t.Errorf("Text(%q) = %q, want %U removed", in, got, r)
		}
	}
}

// The three bidi *marks* are kept. A mark only sets the direction of the neutral characters around it — it
// cannot reprint a run backwards — and all three appear in ordinary Arabic and Hebrew text, where replacing
// one with a visible mark would put a hole in a message that was never an attack.
func TestBidiMarksAreKept(t *testing.T) {
	for _, r := range []rune{'\u061c', '\u200e', '\u200f'} {
		in := "شارع" + string(r) + "Main"
		if got := Text(in); got != in {
			t.Errorf("Text(%q) = %q, want %U kept", in, got, r)
		}
	}
}

// Every rune the Bidi_Control table holds is accounted for deliberately, so a future Unicode adding one
// arrives covered rather than silently allowed — and so the three exceptions stay a decision rather than an
// oversight.
func TestEveryBidiControlIsEitherRemovedOrAKnownMark(t *testing.T) {
	marks := map[rune]bool{'\u061c': true, '\u200e': true, '\u200f': true}

	found := 0
	for _, rng := range unicode.Bidi_Control.R16 {
		for c := rune(rng.Lo); c <= rune(rng.Hi); c += rune(rng.Stride) {
			found++
			removed := Text(string(c)) != string(c)
			if removed == marks[c] {
				t.Errorf("%U: removed=%v, but it is %s", c, removed,
					map[bool]string{true: "a mark, which must be kept", false: "a reordering control"}[marks[c]])
			}
		}
	}
	if found == 0 {
		t.Fatal("walked no bidi controls at all; the table's shape must have changed")
	}
}

// ---------- text that must survive ----------

// The Cf category holds the zero-width characters *and* parts of ordinary writing. Removing the category
// wholesale — the obvious implementation — corrupts Arabic, Persian, Indic and composed emoji, which is why
// this filter is defined by what acts on a terminal rather than by what happens to be invisible.
func TestLegitimateFormatCharactersSurvive(t *testing.T) {
	cases := map[string]string{
		"Arabic number sign":       "\u0600٧",
		"Arabic end of ayah":       "\u06dd٧",
		"Syriac abbreviation mark": "\u070fܐ",
		"Persian ZWNJ":             "می\u200cرود",
		"emoji ZWJ sequence":       "👨\u200d👩\u200d👧",
		"variation selector":       "❤\ufe0f",
		"zero-width space":         "a\u200bb",
		"soft hyphen":              "co\u00adoperate",
		"tag characters in a flag": "\U0001F3F4\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got := Text(in); got != in {
				t.Errorf("Text(%q) = %q, want it unchanged", in, got)
			}
		})
	}
}

// ---------- bytes ----------

// Invalid bytes must not survive, and one of them is the whole reason: 0x9b is CSI, is not valid UTF-8 on
// its own, and a rune-by-rune filter sees only a printable U+FFFD where it sits — so the raw byte would be
// copied through to a terminal that decodes bytes. Each run of invalid bytes becomes one mark.
func TestInvalidBytesCannotReachTheTerminal(t *testing.T) {
	cases := map[string]string{
		"a\x9bb":      "a" + mark + "b",
		"ada\xff":     "ada" + mark,
		"ada\xff\xfe": "ada" + mark,
	}
	for in, want := range cases {
		got := Text(in)
		if got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("Text(%q) = %q, which is not valid UTF-8", in, got)
		}
	}
}

// ---------- the properties the callers rely on ----------

// Removing rather than dropping is what keeps two different strings from rendering as one. An impostor
// whose name is "ada" plus an override must not read as the account called "ada".
func TestARemovedRunLeavesAVisibleMark(t *testing.T) {
	for _, in := range []string{"ada\u202e", "ada\x00", "ada\x1b", "ada\u009b"} {
		got := Text(in)
		if got == "ada" {
			t.Errorf("Text(%q) = %q, which is indistinguishable from the real name", in, got)
		}
		if !strings.Contains(got, mark) {
			t.Errorf("Text(%q) = %q, with nothing to show that something was removed", in, got)
		}
	}
}

// The ASCII branch in mustRemove is an optimization, and an optimization that disagrees with the tables it
// stands in for is a hole. Walking every code point is cheap and settles it exactly.
func TestTheASCIIFastPathAgreesWithTheUnicodeTables(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue // surrogates cannot appear in a Go string decoded from UTF-8
		}
		byTable := unicode.Is(unicode.Cc, r) || isBidiReordering(r)
		if got := mustRemove(r, false); got != byTable {
			t.Fatalf("mustRemove(%U) = %v, but the tables say %v", r, got, byTable)
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

// Whatever comes in, what comes out can only print. The invariants are stated once here and hold for every
// input the fuzzer reaches, which is the only way to be confident about a filter with this many cases.
func FuzzOutputIsAlwaysInert(f *testing.F) {
	for _, seed := range []string{
		"", "ada", "\x1b[2K\rSigned in as admin", "a\x9bb", "\xff\xfe", "شارع\u200eMain",
		"safe\u202ename", "👨\u200d👩\u200d👧", "line\nbreak\ttab", strings.Repeat("\x00", 64),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, in string) {
		for _, tc := range []struct {
			name string
			fn   func(string) string
			// layout reports whether newlines and tabs are allowed to survive.
			layout bool
		}{
			{"Text", Text, false},
			{"Block", Block, true},
		} {
			got := tc.fn(in)

			if !utf8.ValidString(got) {
				t.Fatalf("%s(%q) = %q, which is not valid UTF-8", tc.name, in, got)
			}
			for _, r := range got {
				if tc.layout && (r == '\n' || r == '\t') {
					continue
				}
				if unicode.Is(unicode.Cc, r) || isBidiReordering(r) {
					t.Fatalf("%s(%q) = %q, which still carries %U", tc.name, in, got, r)
				}
			}

			// Sanitizing twice must be sanitizing once: the output is text like any other, and callers
			// layer this — an ingest boundary cleans a value that a print site cleans again.
			if second := tc.fn(got); second != got {
				t.Fatalf("%s is not idempotent: %q became %q", tc.name, got, second)
			}
		}
	})
}
