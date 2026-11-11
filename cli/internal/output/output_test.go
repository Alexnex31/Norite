// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package output

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// hostile carries what a stranger's instance could put in a name: an escape sequence, a C1 CSI, DEL, a
// bidi override and an isolate, and the line and paragraph separators — alongside text that must survive
// exactly, in scripts a sanitizer must not touch.
const hostile = "evil\x1b[2J\u009b31m\x7f\u202eesrever\u2066x\u2069 \u2028\u2029 日本語 العربية 👩‍👩‍👧 <&>"

func TestJSONIsLosslessAndInert(t *testing.T) {
	type item struct {
		Name    string   `json:"name"`
		Aliases []string `json:"aliases"`
	}
	in := item{Name: hostile, Aliases: []string{hostile, "plain"}}

	var buf bytes.Buffer
	require.NoError(t, WriteJSON(&buf, in))
	out := buf.String()

	var back item
	require.NoError(t, json.Unmarshal(buf.Bytes(), &back))
	assert.Equal(t, in, back, "a parser reads back exactly what was encoded")

	for _, r := range out {
		if r == '\n' || r == '\t' {
			continue // the indentation's own
		}
		assert.False(t, termsafe.Removes(r), "U+%04X reached the output raw", r)
	}
	assert.True(t, strings.HasSuffix(out, "}\n"))
	assert.Contains(t, out, "日本語 العربية 👩‍👩‍👧 <&>", "ordinary text, HTML characters included, is left as written")
	assert.Contains(t, out, `\u202e`)
	assert.Contains(t, out, `\u009b`)
}

// TestPlainJSONIsNotEnough is why WriteJSON exists: encoding/json leaves these raw.
func TestPlainJSONIsNotEnough(t *testing.T) {
	plain, err := json.Marshal(hostile)
	require.NoError(t, err)
	raw := false
	for _, r := range string(plain) {
		raw = raw || termsafe.Removes(r)
	}
	assert.True(t, raw, "if encoding/json ever escapes all of these, Inert has become a no-op worth removing")
}

func TestInertLeavesCleanJSONAlone(t *testing.T) {
	clean := []byte("{\n  \"a\": \"b\"\n}\n")
	assert.Equal(t, clean, Inert(clean))
}

type sample struct {
	Name string `json:"name"`
}

func (s sample) Text(t *Text) { t.Line("name: %s", Clean(s.Name)) }

func TestRenderChoosesThePresentation(t *testing.T) {
	var text, js bytes.Buffer
	require.NoError(t, Render(&text, false, sample{Name: "a\x1b[2Jb"}))
	require.NoError(t, Render(&js, true, sample{Name: "a\x1b[2Jb"}))

	assert.Equal(t, "name: a�[2Jb\n", text.String())
	assert.JSONEq(t, `{"name":"a\u001b[2Jb"}`, js.String())
}

// TestAValueInACommandToPasteIsOneWord: an address and an email go into a command a person pastes, quoted
// so that a shell reads neither, a quote inside included.
func TestAValueInACommandToPasteIsOneWord(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		assert.Equal(t, `'http://[fd00::1]:8080'`, shellWord(goos, "http://[fd00::1]:8080"))
		assert.Equal(t, `'it'\''s'`, shellWord(goos, "it's"))
		assert.Equal(t, `'$(id)'`, shellWord(goos, "$(id)"))
	}
	// PowerShell, where a single-quoted string takes no escape but a doubled quote, and reads no $( ).
	assert.Equal(t, `'http://[fd00::1]:8080'`, shellWord("windows", "http://[fd00::1]:8080"))
	assert.Equal(t, `'it''s'`, shellWord("windows", "it's"))
	assert.Equal(t, "'it\u2019\u2019s'", shellWord("windows", "it\u2019s"), "a typographic quote ends one too")
	assert.Equal(t, `'$(id)'`, shellWord("windows", "$(id)"))
	assert.Equal(t, shellWord(runtime.GOOS, "it's"), ShellWord("it's"))
}
