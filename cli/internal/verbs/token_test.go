// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
)

// TestCreatingATokenPrintsItsValueOnce: the value is in the answer to create and nowhere after it.
func TestCreatingATokenPrintsItsValueOnce(t *testing.T) {
	f := newFake(t).On("mintApiToken", created(apiMinted("95", "status bot")))

	r := runVerb(t, f, "", "token", "create", "--name", " status bot ", "--scope", "messages.write",
		"--scope", "messages.read")
	require.NoError(t, r.err)
	assert.Contains(t, r.out, tokenValue)
	assert.Contains(t, r.out, "shown this once")

	calls := f.Requests()
	require.Len(t, calls, 1)
	assert.JSONEq(t, `{"name":"status bot","scopes":["messages.write","messages.read"]}`, string(calls[0].Body),
		"the name is trimmed, and the scopes go as asked")
}

// TestOutKeepsTheValueOffTheScreen: with --out the value is in a file only its owner reads, and in no
// output, text or JSON.
func TestOutKeepsTheValueOffTheScreen(t *testing.T) {
	for _, json := range []bool{false, true} {
		f := newFake(t).On("mintApiToken", created(apiMinted("95", "status bot")))
		path := filepath.Join(t.TempDir(), "bot.token")

		argv := []string{"token", "create", "--name", "status bot", "--scope", "messages.write", "--out", path}
		if json {
			argv = append([]string{"--json"}, argv...)
		}
		r := runVerb(t, f, "", argv...)
		require.NoError(t, r.err)
		assert.NotContains(t, r.out, tokenValue, "json=%v", json)
		assert.NotContains(t, r.out, "nat_", "json=%v", json)
		assert.Contains(t, r.out, path)
		if json {
			matchesCLISchema(t, r.out, "token.schema.json", "minted")
		}

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, tokenValue+"\n", string(got))
		if runtime.GOOS != "windows" {
			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, tokenFileMode, info.Mode().Perm())
		}
	}
}

// TestOutNeverReplacesAFileAndAsksNothingFirst: the file is claimed before a token exists. A value minted
// and then not written is a live credential nobody holds, so a path that cannot be written is found out
// while there is still nothing to lose.
func TestOutNeverReplacesAFileAndAsksNothingFirst(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(existing, []byte("mine\n"), 0o600))

	for _, path := range []string{existing, filepath.Join(dir, "no-such-dir", "bot.token")} {
		f := newFake(t).On("mintApiToken", created(apiMinted("95", "status bot")))
		r := runVerb(t, f, "", "token", "create", "--name", "bot", "--scope", "identify", "--out", path)
		var usage *clierr.UsageError
		require.ErrorAs(t, r.err, &usage, path)
		assert.Empty(t, f.Requests(), "nothing may be minted when its value has nowhere to go: %s", path)
	}
	got, err := os.ReadFile(existing)
	require.NoError(t, err)
	assert.Equal(t, "mine\n", string(got))
}

// TestARefusedMintLeavesNoFile: the claimed file is not left behind, empty, looking like a token.
func TestARefusedMintLeavesNoFile(t *testing.T) {
	f := newFake(t).On("mintApiToken", func(request) (int, any) {
		return http.StatusForbidden, refusal("forbidden", "minting requires a logged-in session")
	})
	path := filepath.Join(t.TempDir(), "bot.token")
	r := runVerb(t, f, "", "token", "create", "--name", "bot", "--scope", "identify", "--out", path)
	require.Error(t, r.err)
	_, err := os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestAMintWithoutAValueIsNotASuccess: the fake holds answers to the contract, which requires `value`, but
// not to its being non-empty. A hostile or broken instance can send "".
func TestAMintWithoutAValueIsNotASuccess(t *testing.T) {
	empty := apiMinted("95", "bot")
	empty.Value = ""
	path := filepath.Join(t.TempDir(), "bot.token")
	f := newFake(t).On("mintApiToken", created(empty))
	r := runVerb(t, f, "", "token", "create", "--name", "bot", "--scope", "identify", "--out", path)
	require.Error(t, r.err)
	assert.Contains(t, r.err.Error(), "revoke token 95")
	_, err := os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestWhatCreateRefusesBeforeAsking: each is a usage error on a machine with no daemon running.
func TestWhatCreateRefusesBeforeAsking(t *testing.T) {
	for name, argv := range map[string][]string{
		"no name":          {"--scope", "identify"},
		"a blank name":     {"--name", "   ", "--scope", "identify"},
		"a long name":      {"--name", longName(), "--scope", "identify"},
		"no scope":         {"--name", "bot"},
		"an unknown scope": {"--name", "bot", "--scope", "everything"},
		"a scope's casing": {"--name", "bot", "--scope", "Identify"},
		"an argument":      {"--name", "bot", "--scope", "identify", "extra"},
	} {
		f := newFake(t)
		r := runVerb(t, f, "", append([]string{"token", "create"}, argv...)...)
		var usage *clierr.UsageError
		require.ErrorAs(t, r.err, &usage, name)
		assert.Empty(t, f.Requests(), name)
	}
}

func longName() string {
	b := make([]rune, maxTokenName+1)
	for i := range b {
		b[i] = 'é' // two bytes each: the bound counts characters, as the instance's does
	}
	return string(b)
}

// TestANameOfExactlyTheBoundInANonLatinScriptIsAccepted is M15's lesson about a bound held in two places.
func TestANameOfExactlyTheBoundInANonLatinScriptIsAccepted(t *testing.T) {
	f := newFake(t).On("mintApiToken", created(apiMinted("95", "bot")))
	name := longName()[:2*maxTokenName] // maxTokenName characters, twice that in bytes
	require.NoError(t, runVerb(t, f, "", "token", "create", "--name", name, "--scope", "identify").err)
}

// TestRevokingATokenIsAskedAbout: whatever holds it stops working, so a script says --yes.
func TestRevokingATokenIsAskedAbout(t *testing.T) {
	f := newFake(t).On("revokeApiToken", noContent())
	r := runVerb(t, f, "", "token", "revoke", "95")
	require.ErrorIs(t, r.err, clierr.ErrNoTerminal)
	assert.Empty(t, f.Requests())

	require.NoError(t, runVerb(t, f, "", "token", "revoke", "95", "--yes").err)
	calls := f.Requests()
	require.Len(t, calls, 1)
	assert.Equal(t, "/auth/tokens/95", calls[0].Path)
}

// TestATokensNameIsDrawnSafely: a name is its owner's own text, but it comes back from an instance.
func TestATokensNameIsDrawnSafely(t *testing.T) {
	hostile := apiToken("95", "bot\x1b[2J\u202eevil")
	f := newFake(t).On("listApiTokens", ok([]any{hostile}))
	r := runVerb(t, f, "", "token", "list")
	require.NoError(t, r.err)
	assert.NotContains(t, r.out, "\x1b")
	assert.NotContains(t, r.out, "\u202e")
	assert.Contains(t, r.out, "never used")
}

// TestTheScopeHelpListsEveryScope holds allScopes to the contract's enum in both directions, so a scope
// added to the instance is one `token create --help` names.
func TestTheScopeHelpListsEveryScope(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(daemontest.Contracts(), "openapi.yaml"))
	require.NoError(t, err)
	var doc struct {
		Components struct {
			Schemas struct {
				Scope struct {
					Enum []string `yaml:"enum"`
				} `yaml:"Scope"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	want := doc.Components.Schemas.Scope.Enum
	require.NotEmpty(t, want)

	got := regexp.MustCompile(`, `).Split(scopeList(), -1)
	sort.Strings(got)
	sort.Strings(want)
	assert.Equal(t, want, got)
}
