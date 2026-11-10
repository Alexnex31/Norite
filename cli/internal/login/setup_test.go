// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package login

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// setupInstance is an instance over loopback that registers, signs in and names the account.
type setupInstance struct {
	server *httptest.Server

	mu sync.Mutex
	// registers and logins are what each endpoint received, in order.
	registers []registerRequest
	logins    []loginRequest
	// inviteOnly answers invite_required to a registration without invite_code.
	inviteOnly bool
	// refuseLogins is how many sign-ins are refused before one is accepted.
	refuseLogins int
	// requests counts every request, so a test can assert nothing reached the instance.
	requests int
}

func newSetupInstance(t *testing.T) *setupInstance {
	t.Helper()
	f := &setupInstance{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
		var req registerRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.registers = append(f.registers, req)
		inviteOnly := f.inviteOnly
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if inviteOnly && req.InviteCode == "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"invite_required","message":"an invite code is required"}}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"message":"Your account is ready. You can sign in now."}`))
	})
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.logins = append(f.logins, req)
		refuse := f.refuseLogins > 0
		if refuse {
			f.refuseLogins--
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if refuse {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"invalid credentials"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"eyJ.access","refresh_token":"nrt_setup","token_type":"Bearer",` +
			`"expires_at":"2026-01-01T00:00:00Z"}`))
	})
	mux.HandleFunc("/api/v1/users/@me", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"123","username":"ada","email":"ada@example.com"}`))
	})
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *setupInstance) seen() (registers []registerRequest, logins []loginRequest, requests int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]registerRequest(nil), f.registers...), append([]loginRequest(nil), f.logins...), f.requests
}

// fakeService is the daemon's service manager: installing and starting make it running.
type fakeService struct {
	state            DaemonState
	installs, starts int
	installErr       error
}

func (d *fakeService) State(context.Context) DaemonState { return d.state }

func (d *fakeService) Install(_ context.Context, out io.Writer) error {
	d.installs++
	if d.installErr != nil {
		return d.installErr
	}
	_, _ = io.WriteString(out, "Installed the daemon.\n")
	d.state = DaemonRunning
	return nil
}

func (d *fakeService) Start(context.Context) error {
	d.starts++
	d.state = DaemonRunning
	return nil
}

// script answers questions in order, and records each one asked.
type script struct {
	answers []string
	asked   []string
}

func (s *script) read(prompt string) (string, error) {
	s.asked = append(s.asked, prompt)
	if len(s.answers) == 0 {
		return "", errors.New("asked more than the test answered: " + prompt)
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a, nil
}

type setupRun struct {
	setup   *Setup
	store   *credentials.Store
	out     *bytes.Buffer
	lines   *script
	secrets *script
	service *fakeService
	daemon  *daemontest.Daemon
}

// newSetupRun wires a Setup to f, a temp store, a fake service and a fake daemon whose READY names f once a
// sign-in is stored, as the real daemon does a moment after one.
func newSetupRun(t *testing.T, f *setupInstance, opts SetupOptions, lines, secrets []string) *setupRun {
	t.Helper()
	store, err := credentials.OpenLocalForTest(t.TempDir())
	require.NoError(t, err)
	run := &setupRun{store: store, out: &bytes.Buffer{}, lines: &script{answers: lines},
		secrets: &script{answers: secrets}, service: &fakeService{state: DaemonAbsent}, daemon: daemontest.New(t)}
	run.setup = &Setup{
		Options: opts, Store: store, Out: run.out, ReadLine: run.lines.read, ReadSecret: run.secrets.read,
		Interactive: true, Hostname: func() (string, error) { return "ada-laptop", nil },
		Daemon: run.service,
		Connect: func(context.Context) (daemonclient.Caller, func(), error) {
			ready := ipc.Ready{Standing: ipc.StandingSignedOut, Guilds: []ipc.GuildSummary{}}
			if rec, err := store.LoadRecord(); err == nil {
				ready = ipc.Ready{Standing: ipc.StandingSignedIn, Guilds: []ipc.GuildSummary{},
					Account: &ipc.Account{InstanceURL: rec.InstanceURL, UserID: rec.UserID, Username: rec.Username}}
			}
			return run.daemon.Attach(ready, false), func() {}, nil
		},
		browserReachable: func() bool { return true },
		pollEvery:        time.Millisecond, waitFor: 200 * time.Millisecond,
	}
	return run
}

// seed stores a sign-in to instance as username, as a finished login would.
func seed(t *testing.T, store *credentials.Store, instance, username string) {
	t.Helper()
	id, err := store.DeviceID()
	require.NoError(t, err)
	require.NoError(t, store.Save(credentials.Record{InstanceURL: instance, Username: username, UserID: "123",
		DeviceID: id, DeviceName: "ada-laptop"}, "nrt_kept"))
}

func preview(guild string) apicontract.GuildInvitePreview {
	p := apicontract.GuildInvitePreview{Code: "BCDFGHJKMNPQRSTV",
		Inviter: &apicontract.PublicUser{Id: "1", Username: "alice", DisplayName: "Alice"}}
	p.Guild.Id, p.Guild.Name = "10", guild
	p.Channel.Id = "20"
	name := "general"
	p.Channel.Name = &name
	return p
}

func guildNamed(name string) apicontract.Guild {
	return apicontract.Guild{Id: "10", Name: name, OwnerId: "1", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
}

func requestsTo(d *daemontest.Daemon, op string) []daemontest.Request {
	var out []daemontest.Request
	for _, r := range d.Requests() {
		if r.Op == op {
			out = append(out, r)
		}
	}
	return out
}

// TestANewcomerWithALinkEndsInTheGuild: the journey the command exists for. A link, no account: the
// instance is confirmed, the account created, the password typed twice and never again, the daemon
// installed, and the guild joined with the link's code.
func TestANewcomerWithALinkEndsInTheGuild(t *testing.T) {
	f := newSetupInstance(t)
	link := f.server.URL + "/join#bcdf-ghjk-mnpq-rstv"
	run := newSetupRun(t, f, SetupOptions{Invite: link},
		[]string{"", "", "ada", "ada@example.com", "", ""}, []string{"a correct passphrase", "a correct passphrase"})
	run.daemon.On("previewInvite", daemontest.OK(preview("Tea Room"))).
		On("redeemInvite", daemontest.OK(guildNamed("Tea Room")))

	require.NoError(t, run.setup.Run(context.Background()), run.out.String())

	registers, logins, _ := f.seen()
	require.Len(t, registers, 1)
	assert.Equal(t, registerRequest{Username: "ada", Email: "ada@example.com", Password: "a correct passphrase"},
		registers[0])
	require.Len(t, logins, 1)
	assert.Equal(t, "a correct passphrase", logins[0].Password, "the password chosen is the one that signs in")
	assert.Len(t, run.secrets.asked, 2, "typed twice to register, and not asked again to sign in")

	rec, err := run.store.LoadRecord()
	require.NoError(t, err)
	assert.Equal(t, f.server.URL, rec.InstanceURL)
	assert.Equal(t, 1, run.service.installs)

	redeemed := requestsTo(run.daemon, "redeemInvite")
	require.Len(t, redeemed, 1)
	assert.JSONEq(t, `{"code":"bcdf-ghjk-mnpq-rstv"}`, string(redeemed[0].Body))

	out := run.out.String()
	assert.Equal(t, 1, strings.Count(out, "is plain HTTP"), "warned once, not once per step:\n%s", out)
	assert.Contains(t, out, "The invite leads to Tea Room, #general, invited by Alice (@alice).")
	assert.Contains(t, out, "Joined Tea Room.")
	assert.Contains(t, out, "Run `norite` to open Norite")
	assert.NotContains(t, out, "Start the background daemon", "the sign-in leaves the daemon to setup's own step")
	assert.Equal(t, []string{
		"The invite is for " + f.server.URL + ": your account and password go there. Is that the instance you meant? [Y/n] ",
		"Do you already have an account on " + f.server.URL + "? [y/N] ",
		"Username: ", "Email: ",
		"Install and start it? [Y/n] ",
		"Join it? [Y/n] ",
	}, run.lines.asked)
}

// TestTheInstanceALinkNamesIsConfirmedBeforeAnythingIsSent: a link is a stranger's text deciding where a
// password goes, so declining at the first question sends the instance nothing at all.
func TestTheInstanceALinkNamesIsConfirmedBeforeAnythingIsSent(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV"}, []string{"n"}, nil)
	err := run.setup.Run(context.Background())
	require.EqualError(t, err, "stopped; nothing was sent to "+f.server.URL)
	_, _, requests := f.seen()
	assert.Zero(t, requests)
	assert.Zero(t, run.service.installs)
	assert.Empty(t, run.daemon.Requests())
}

// TestASecondRunSaysWhatIsDoneAndAsksNothing: signed in to the same instance, daemon running, no invite.
func TestASecondRunSaysWhatIsDoneAndAsksNothing(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL}, nil, nil)
	seed(t, run.store, f.server.URL, "ada")
	run.service.state = DaemonRunning

	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Empty(t, run.lines.asked)
	assert.Empty(t, run.secrets.asked)
	_, _, requests := f.seen()
	assert.Zero(t, requests)
	assert.Contains(t, run.out.String(), "Signed in as @ada already.\n")
	assert.Contains(t, run.out.String(), "The daemon is running.\n")
	assert.Zero(t, run.service.installs+run.service.starts)
}

// TestASecondRunWithALinkJoinsWithoutAskingAboutTheInstance: signed in to the link's instance already, the
// link's instance needs no confirming; only the join is asked.
func TestASecondRunWithALinkJoinsWithoutAskingAboutTheInstance(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV"}, []string{""}, nil)
	seed(t, run.store, f.server.URL, "ada")
	run.service.state = DaemonRunning
	run.daemon.On("previewInvite", daemontest.OK(preview("Tea Room"))).
		On("redeemInvite", daemontest.OK(guildNamed("Tea Room")))

	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Equal(t, []string{"Join it? [Y/n] "}, run.lines.asked)
	assert.Len(t, requestsTo(run.daemon, "redeemInvite"), 1)
}

// TestAnInviteOnlyInstanceAsksForItsCodeAndSendsTheAnswersAgain: the instance's invite_required is answered
// with its code, and the registration sent again with the same answers rather than asked again.
func TestAnInviteOnlyInstanceAsksForItsCodeAndSendsTheAnswersAgain(t *testing.T) {
	f := newSetupInstance(t)
	f.inviteOnly = true
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL, NoDaemon: true},
		[]string{"", "ada", "ada@example.com", "QQQNBQQMGYTMQRMX"}, []string{"a correct passphrase", "a correct passphrase"})

	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	registers, logins, _ := f.seen()
	require.Len(t, registers, 2)
	assert.Empty(t, registers[0].InviteCode)
	assert.Equal(t, "QQQNBQQMGYTMQRMX", registers[1].InviteCode)
	assert.Equal(t, registers[0].Username, registers[1].Username)
	assert.Equal(t, registers[0].Password, registers[1].Password)
	assert.Len(t, logins, 1)
	assert.Contains(t, run.lines.asked, "Instance invite code: ")
	assert.Len(t, run.secrets.asked, 2)
}

// TestASignInRefusedAfterRegisteringIsTriedAgain: the mailed confirmation link is opened between tries.
// Bounded: after the last, the error names both ways out.
func TestASignInRefusedAfterRegisteringIsTriedAgain(t *testing.T) {
	f := newSetupInstance(t)
	f.refuseLogins = 1
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL, NoDaemon: true},
		[]string{"", "ada", "ada@example.com", ""}, []string{"a correct passphrase", "a correct passphrase"})
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	_, logins, _ := f.seen()
	assert.Len(t, logins, 2)
	assert.Contains(t, run.lines.asked, "Press Enter to try again: ")

	g := newSetupInstance(t)
	g.refuseLogins = 100
	stuck := newSetupRun(t, g, SetupOptions{Instance: g.server.URL, NoDaemon: true},
		[]string{"", "ada", "ada@example.com", "", ""}, []string{"a correct passphrase", "a correct passphrase"})
	err := stuck.setup.Run(context.Background())
	require.EqualError(t, err, "the instance still refuses the sign-in. If it emailed you a link, open it, then "+
		"run `norite setup` again; if that address already had an account, sign in with its own password: "+
		"`norite login`")
	_, logins, _ = g.seen()
	assert.Len(t, logins, signInAttempts)
}

// TestSigningInElsewhereIsAskedAndDefaultsToNo: one daemon holds one sign-in, so moving it is asked.
func TestSigningInElsewhereIsAskedAndDefaultsToNo(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL}, []string{""}, nil)
	seed(t, run.store, "https://other.example", "ada")
	err := run.setup.Run(context.Background())
	require.EqualError(t, err, "stopped; this machine is still signed in to https://other.example")
	_, _, requests := f.seen()
	assert.Zero(t, requests)
	rec, err := run.store.LoadRecord()
	require.NoError(t, err)
	assert.Equal(t, "https://other.example", rec.InstanceURL)
}

// TestALinkAndAnInstanceThatDisagreeAreAUsageError: refused before anything is asked or sent.
func TestALinkAndAnInstanceThatDisagreeAreAUsageError(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Invite: "https://other.example/join#BCDFGHJKMNPQRSTV",
		Instance: f.server.URL}, nil, nil)
	err := run.setup.Run(context.Background())
	var usage *clierr.UsageError
	require.ErrorAs(t, err, &usage)
	assert.Equal(t, "the invite is for https://other.example, and --instance names "+f.server.URL, err.Error())
	assert.Empty(t, run.lines.asked)
}

// TestTheGuildIsJoinedOnlyOnceTheDaemonNamesTheLinksInstance: a daemon that names another instance is never
// sent the code, which is a way into a guild on the link's; setup says how to join later instead.
func TestTheGuildIsJoinedOnlyOnceTheDaemonNamesTheLinksInstance(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV"}, nil, nil)
	seed(t, run.store, f.server.URL, "ada")
	run.service.state = DaemonRunning
	run.setup.Connect = func(context.Context) (daemonclient.Caller, func(), error) {
		return run.daemon.Attach(ipc.Ready{Standing: ipc.StandingSignedIn, Guilds: []ipc.GuildSummary{},
			Account: &ipc.Account{InstanceURL: "https://other.example", UserID: "9", Username: "eve"}}, false), func() {}, nil
	}
	require.NoError(t, run.setup.Run(context.Background()))
	assert.Empty(t, run.daemon.Requests())
	assert.Contains(t, run.out.String(), "the daemon did not take the sign-in up within 200ms; `norite daemon status` "+
		"says where it stands. Join it once the daemon runs: `norite invite join "+f.server.URL+"/join#BCDFGHJKMNPQRSTV`.")
}

// TestAProviderSignInIsNotAskedAboutAnAccount: a provider creates the account in the browser when there is
// none, so there is nothing to register here and the question is not put.
func TestAProviderSignInIsNotAskedAboutAnAccount(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL, Provider: "github", NoDaemon: true}, nil, nil)
	run.setup.browserReachable = func() bool { return false } // the device flow, which this fake does not serve
	_ = run.setup.Run(context.Background())
	for _, q := range run.lines.asked {
		assert.NotContains(t, q, "Do you already have an account")
	}
	registers, _, _ := f.seen()
	assert.Empty(t, registers)
}

// TestWithoutATerminalSetupSaysWhatDoesItsSteps: it asks by nature, so a script is pointed at the commands.
func TestWithoutATerminalSetupSaysWhatDoesItsSteps(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL}, nil, nil)
	run.setup.Interactive = false
	err := run.setup.Run(context.Background())
	require.ErrorIs(t, err, ErrNoTerminal)
	assert.Contains(t, err.Error(), "`norite register`, `norite login` and `norite daemon install`")
	_, _, requests := f.seen()
	assert.Zero(t, requests)
}

// TestAMistypedLinkIsAUsageErrorEvenWithoutATerminal: what was typed is checked before anything else, so
// the mistake is the one reported.
func TestAMistypedLinkIsAUsageErrorEvenWithoutATerminal(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Invite: "https://chat.example.com/joint#BCDF"}, nil, nil)
	run.setup.Interactive = false
	err := run.setup.Run(context.Background())
	var usage *clierr.UsageError
	require.ErrorAs(t, err, &usage)
	assert.NotErrorIs(t, err, ErrNoTerminal)
}

// TestWithNothingNamingTheInstanceItIsAsked: no link, no flag, no environment, no previous sign-in.
func TestWithNothingNamingTheInstanceItIsAsked(t *testing.T) {
	t.Setenv(instanceEnvVar, "")
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{NoDaemon: true}, []string{f.server.URL, "y", "ada@example.com"},
		[]string{"a correct passphrase"})
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Equal(t, "Instance address, such as https://chat.example.com: ", run.lines.asked[0])
	_, logins, _ := f.seen()
	assert.Len(t, logins, 1)

	empty := newSetupRun(t, f, SetupOptions{NoDaemon: true}, []string{""}, nil)
	assert.EqualError(t, empty.setup.Run(context.Background()), "an instance address is required: the person "+
		"who invited you, or the instance's administrator, has it")
}
