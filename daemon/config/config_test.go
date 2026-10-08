// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const contractPath = "../../contracts/client-config.toml"

type contractKey struct {
	Name     string   `toml:"name"`
	Section  string   `toml:"section"`
	Type     string   `toml:"type"`
	Values   []string `toml:"values"`
	Default  string   `toml:"default"`
	Portable bool     `toml:"portable"`
	Consumer string   `toml:"consumer"`
	Live     bool     `toml:"live"`
}

func readContract(t *testing.T) []contractKey {
	t.Helper()
	data, err := os.ReadFile(contractPath)
	require.NoError(t, err)
	var doc struct {
		Key []contractKey `toml:"key"`
	}
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&doc), "the contract has a field this test does not compare")
	require.NotEmpty(t, doc.Key)
	return doc.Key
}

// Both directions, in order: a key added to the table and not the contract, or the reverse, or changed in
// one, fails here. Comparing whole values is what makes a changed default or a flipped `portable` visible.
func TestTheKeyTableIsTheContract(t *testing.T) {
	var fromContract []Key
	for _, k := range readContract(t) {
		fromContract = append(fromContract, Key{
			Name: k.Name, Section: Section(k.Section), Kind: Kind(k.Type), Values: k.Values,
			Default: k.Default, Portable: k.Portable, Consumer: k.Consumer, Live: k.Live,
		})
	}
	assert.Equal(t, fromContract, Keys())
}

func TestEveryKeyIsWellFormed(t *testing.T) {
	milestone := regexp.MustCompile(`^M[0-9]+[a-z]?$`)
	secretish := regexp.MustCompile(`(?i)secret|token|password|passwd|credential|api[_-]?key`)
	seen := map[string]bool{}
	for _, k := range Keys() {
		id := string(k.Section) + "." + k.Name
		assert.False(t, seen[id], "%s is listed twice", id)
		seen[id] = true
		assert.True(t, validSection(string(k.Section)), "%s: unknown section", id)
		assert.Regexp(t, milestone, k.Consumer, "%s: consumer must be a milestone", id)
		// Rule 8. A secret in a file people publish in dotfiles repositories is a secret published.
		assert.NotRegexp(t, secretish, k.Name, "%s is named like a secret, and no secret is a config key", id)

		switch k.Kind {
		case KindEnum:
			assert.NotEmpty(t, k.Values, "%s: an enum lists its values", id)
		case KindColor, KindString, KindTable:
			assert.Empty(t, k.Values, "%s: only an enum has values", id)
		default:
			t.Errorf("%s: unknown kind %q", id, k.Kind)
		}
		if k.Live {
			require.NotEmpty(t, k.Default, "%s is live, so it needs a default to fall back to", id)
		}
		if k.Default != "" {
			// A default the loader would itself refuse is a default nobody can restore by typing it.
			var raw any = k.Default
			if k.Kind == KindColor && !strings.HasPrefix(k.Default, "#") {
				var n int64
				for _, r := range k.Default {
					n = n*10 + int64(r-'0')
				}
				raw = n
			}
			_, problem := check(k, raw)
			assert.Empty(t, problem, "%s: its own default does not validate", id)
		}
	}
}

func TestNoFileIsTheDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.toml"), TUI)
	require.NoError(t, err)
	assert.Equal(t, Clock24h, c.Clock())
	assert.Equal(t, Color("6"), c.Color(KeyColorAccent))
	assert.Empty(t, c.Warnings)
}

func TestAClientSectionOverridesShared(t *testing.T) {
	doc := []byte("[shared]\nclock = \"12h\"\n\n[gui]\nclock = \"24h\"\n")

	tui, err := Parse(doc, TUI)
	require.NoError(t, err)
	assert.Equal(t, Clock12h, tui.Clock(), "the terminal client has no override, so [shared] applies")

	gui, err := Parse(doc, GUI)
	require.NoError(t, err)
	assert.Equal(t, Clock24h, gui.Clock(), "[gui] overrides [shared] for the GUI")
	assert.Empty(t, tui.Warnings)
}

func TestColorsAreAnIndexOrHex(t *testing.T) {
	c, err := Parse([]byte("[tui.colors]\naccent = 208\nwarn = \"#1E90FF\"\n"), TUI)
	require.NoError(t, err)
	assert.Equal(t, Color("208"), c.Color(KeyColorAccent))
	assert.Equal(t, Color("#1e90ff"), c.Color(KeyColorWarn))
	assert.Equal(t, Color("1"), c.Color(KeyColorDanger), "an unset role keeps its default")
	assert.Empty(t, c.Warnings)
}

// One bad line must not cost the rest of the file, and must not be applied either.
func TestABadValueIsRefusedAloneAndTheRestLoads(t *testing.T) {
	cases := map[string]string{
		"an index out of range": "accent = 256",
		"a negative index":      "accent = -1",
		"a color name":          "accent = \"red\"",
		"short hex":             "accent = \"#fff\"",
		"an escape sequence":    "accent = \"\\u001b[31m\"",
		"a float":               "accent = 6.0",
		"a table":               "accent = { r = 1 }",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := Parse([]byte("[shared]\nclock = \"12h\"\n[tui.colors]\n"+line+"\nwarn = 4\n"), TUI)
			require.NoError(t, err)
			assert.Equal(t, Color("6"), c.Color(KeyColorAccent), "the bad value must not apply")
			assert.Equal(t, Color("4"), c.Color(KeyColorWarn), "the key after it still loads")
			assert.Equal(t, Clock12h, c.Clock())
			require.Len(t, c.Warnings, 1)
			assert.Equal(t, "tui.colors.accent", c.Warnings[0].Key)
		})
	}
}

func TestAnEnumOutsideItsValuesIsRefused(t *testing.T) {
	c, err := Parse([]byte("[shared]\nclock = \"13h\"\n"), TUI)
	require.NoError(t, err)
	assert.Equal(t, Clock24h, c.Clock())
	require.Len(t, c.Warnings, 1)
	assert.Contains(t, c.Warnings[0].Problem, `"24h" or "12h"`)
}

// The opposite of the instance config's rule: an export from a newer client is read by an older one.
func TestAnUnknownKeyIsAWarningNotAnError(t *testing.T) {
	doc := "future = 1\n[shared]\nnewer_setting = true\n[tui]\ntheme = \"norite-dark\"\n" +
		"[tui.colors]\nacent = 3\n[tui.keys]\n\"C-x b\" = \"buffers\"\n[plugins]\nx = 1\n"
	c, err := Parse([]byte(doc), TUI)
	require.NoError(t, err)

	var keys []string
	for _, w := range c.Warnings {
		keys = append(keys, w.Key)
	}
	assert.ElementsMatch(t, []string{"future", "plugins", "shared.newer_setting", "tui.colors.acent"}, keys,
		"reserved keys (theme, keys) are known and warn about nothing")
}

func TestAClientKeyUnderTheWrongSectionIsNotApplied(t *testing.T) {
	c, err := Parse([]byte("[shared.colors]\naccent = 9\n[gui.colors]\naccent = 9\n"), TUI)
	require.NoError(t, err)
	assert.Equal(t, Color("6"), c.Color(KeyColorAccent))
	assert.Len(t, c.Warnings, 2)
}

// A quoted key containing a dot is a different key from the path it resembles.
func TestAQuotedDottedKeyIsNotAPath(t *testing.T) {
	c, err := Parse([]byte("[tui]\n\"colors.accent\" = 9\n"), TUI)
	require.NoError(t, err)
	assert.Equal(t, Color("6"), c.Color(KeyColorAccent))
	require.Len(t, c.Warnings, 1)

	// The warning names the key as the file holds it: the path, then the quoted key.
	c, err = Parse([]byte("[tui.colors]\n\"a.b\" = 1\n"), TUI)
	require.NoError(t, err)
	require.Len(t, c.Warnings, 1)
	assert.Equal(t, `tui.colors."a.b"`, c.Warnings[0].Key)
}

func TestInvalidTOMLIsAnErrorWithItsLine(t *testing.T) {
	_, err := Parse([]byte("[shared]\nclock = \"12h\"\n[tui.colors\naccent = 3\n"), TUI)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, 3, pe.Line)
}

// The file is foreign text. A key and a parse error both quote it, and both reach a terminal (rule 19).
func TestWhatTheFileSaysIsSanitizedBeforeItIsShown(t *testing.T) {
	c, err := Parse([]byte("[shared]\n\"evil\\u001b[2J\\u202e\" = 1\n"), TUI)
	require.NoError(t, err)
	require.Len(t, c.Warnings, 1)
	// The fields, not only String(): a caller that formats them itself must get safe text too.
	for _, shown := range []string{c.Warnings[0].Key, c.Warnings[0].Problem, c.Warnings[0].String()} {
		assert.NotContains(t, shown, "\x1b")
		assert.NotContains(t, shown, "\u202e")
	}

	_, err = Parse([]byte("[shared]\nclock = \"a\x1b[2Jb\n"), TUI)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "\x1b")
}

func TestAFileOverTheBoundIsRefusedBeforeItIsParsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	big := "[shared]\nclock = \"12h\"\n# " + strings.Repeat("x", MaxFileSize) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(big), 0o600))
	_, err := Load(path, TUI)
	require.ErrorIs(t, err, ErrTooLarge)
	assert.Contains(t, err.Error(), path, "the error names the file")
}

// The bound is a measurement (see MaxFileSize), so raising it is a decision and not an edit. At 1 MiB,
// where it first sat, the decoder took ten seconds on a file of short keys.
func TestTheSizeBoundIsTheOneThatWasMeasured(t *testing.T) {
	assert.Equal(t, 64<<10, MaxFileSize)
}

func TestWarningsAreCountedPastTheirBound(t *testing.T) {
	var doc strings.Builder
	doc.WriteString("[shared]\n")
	for i := range MaxWarnings + 25 {
		doc.WriteString("unknown_" + strconv.Itoa(i) + " = 1\n")
	}
	doc.WriteString("clock = \"12h\"\n")
	c, err := Parse([]byte(doc.String()), TUI)
	require.NoError(t, err)
	assert.Len(t, c.Warnings, MaxWarnings)
	assert.Equal(t, 25, c.MoreWarnings)
	assert.Equal(t, Clock12h, c.Clock(), "the file still loads")
}

func TestWhereTheFileIs(t *testing.T) {
	t.Setenv("HOME", "/home/ada")
	t.Setenv("USERPROFILE", `C:\Users\ada`)

	t.Setenv("XDG_CONFIG_HOME", "")
	for _, goos := range []string{"linux", "darwin"} {
		dir, err := dirFor(goos)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join("/home/ada", ".config", "norite"), dir, goos)
	}

	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	dir, err := dirFor("darwin")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/cfg", "norite"), dir, "XDG_CONFIG_HOME is honored on macOS too")

	// Relative is invalid per the spec. Resolving it against a service's working directory would be
	// a config file in a place nobody chose.
	t.Setenv("XDG_CONFIG_HOME", "relative/dir")
	dir, err = dirFor("linux")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/home/ada", ".config", "norite"), dir)

	t.Setenv("APPDATA", `C:\Users\ada\AppData\Roaming`)
	dir, err = dirFor("windows")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(`C:\Users\ada\AppData\Roaming`, "Norite"), dir)
}

// A config path that leads to something other than a file is refused, by every way of reading one.
func TestAConfigThatIsNotAFileIsRefusedWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	asDir := filepath.Join(dir, "config.toml")
	require.NoError(t, os.Mkdir(asDir, 0o700))
	_, err := Load(asDir, TUI)
	require.ErrorIs(t, err, ErrNotAFile)
	_, err = ReadFile(asDir)
	require.ErrorIs(t, err, ErrNotAFile)

}
