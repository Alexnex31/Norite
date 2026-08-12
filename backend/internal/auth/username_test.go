package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The rule this replaced was `excludesall= `, which excluded exactly one character. Everything in the
// rejected table below was accepted before, and every one of them reaches a terminal renderer unescaped
// while CLAUDE.md rule 19's sanitizer does not yet exist.
func TestValidUsernameRejectsUnrenderableInput(t *testing.T) {
	rejected := map[string]string{
		"tab":                  "ada\tlovelace",
		"newline":              "ada\nlovelace",
		"carriage return":      "ada\rlovelace",
		"null":                 "ada\x00lovelace",
		"ansi escape":          "ada\x1b[31mred",
		"c1 control":           "ada\u0085lovelace",
		"delete":               "ada\x7flovelace",
		"bidi override":        "ada\u202elovelace",
		"bidi isolate":         "ada\u2066lovelace",
		"zero width joiner":    "ada\u200dlovelace",
		"zero width space":     "ada\u200blovelace",
		"non-breaking space":   "ada\u00a0lovelace",
		"ordinary space":       "ada lovelace",
		"emoji":                "ada🙂",
		"symbol":               "ada+lovelace",
		"slash":                "ada/lovelace",
		"at sign":              "ada@lovelace",
		"invalid utf-8":        string([]byte{0x61, 0xff, 0x62}),
		"too short":            "a",
		"too long":             strings.Repeat("a", MaxUsernameLength+1),
		"empty":                "",
		"whitespace only":      "   ",
		"combining mark alone": "\u0301",
	}

	for name, candidate := range rejected {
		t.Run(name, func(t *testing.T) {
			assert.False(t, ValidUsername(candidate), "%q must be rejected", candidate)
		})
	}
}

// Letters and digits in any script. Excluding non-Latin names would be a product decision nobody made.
func TestValidUsernameAcceptsRealNames(t *testing.T) {
	accepted := []string{
		"ada",
		"ada_lovelace",
		"ada.lovelace",
		"ada-lovelace",
		"ada2",
		"алексей",
		"田中",
		"Ωμέγα",
		"עברית",
		"日本語ユーザー",
		strings.Repeat("a", MaxUsernameLength),
		strings.Repeat("a", MinUsernameLength),
	}

	for _, candidate := range accepted {
		t.Run(candidate, func(t *testing.T) {
			assert.True(t, ValidUsername(candidate), "%q must be accepted", candidate)
		})
	}
}

// Two names that render identically must not become two accounts — that is an impersonation primitive,
// not a cosmetic issue. citext collapses case; NFKC collapses the rest.
func TestNormalizeUsernameCollapsesVisuallyIdenticalForms(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"ﬁnn", "finn"},    // U+FB01 LATIN SMALL LIGATURE FI
		{"ａｄａ", "ada"},     // fullwidth Latin
		{"  ada  ", "ada"}, // surrounding whitespace
		{"ada⁵", "ada5"},   // superscript five
		{"Å", "Å"},        // A + combining ring => Å
		{"ada", "ada"},     // already normal
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			assert.Equal(t, tc.want, NormalizeUsername(tc.raw))
		})
	}
}

// Normalization runs before validation because NFKC can change length. Validating first would let a name
// pass the bounds check and then be stored outside them.
func TestNormalizationHappensBeforeLengthIsJudged(t *testing.T) {
	// 32 ligatures normalize to 64 runes — inside the cap before, outside it after.
	raw := strings.Repeat("ﬁ", MaxUsernameLength)

	assert.False(t, ValidUsername(NormalizeUsername(raw)),
		"a name that only fits before normalization must be rejected")
}
