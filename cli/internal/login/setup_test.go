// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package login

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

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
	// inviteOnly answers invite_required to a registration without invite_code, and with validCode set,
	// invite_invalid to one with any other.
	inviteOnly bool
	validCode  string
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
		inviteOnly, valid := f.inviteOnly, f.validCode
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if inviteOnly && req.InviteCode == "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"invite_required","message":"an invite code is required"}}`))
			return
		}
		if inviteOnly && valid != "" && req.InviteCode != valid {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"invite_invalid","message":"that invite code is not valid"}}`))
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
	// slowToSay leaves the state as it was after a successful install, as a service manager that has not
	// reported the unit active yet does.
	slowToSay bool
}

func (d *fakeService) State(context.Context) DaemonState { return d.state }

func (d *fakeService) Install(_ context.Context, out io.Writer) error {
	d.installs++
	if d.installErr != nil {
		return d.installErr
	}
	_, _ = io.WriteString(out, "Installed the daemon.\n")
	if !d.slowToSay {
		d.state = DaemonRunning
	}
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

// newSetupRun wires a Setup to f, a temp store, a fake service and a fake daemon signed in to whatever the
// store holds, at each request as at each attach, as the real daemon is a moment after a sign-in is stored.
func newSetupRun(t *testing.T, f *setupInstance, opts SetupOptions, lines, secrets []string) *setupRun {
	t.Helper()
	store, err := credentials.OpenLocalForTest(t.TempDir())
	require.NoError(t, err)
	run := &setupRun{store: store, out: &bytes.Buffer{}, lines: &script{answers: lines},
		secrets: &script{answers: secrets}, service: &fakeService{state: DaemonAbsent}, daemon: daemontest.New(t)}
	run.daemon.SignInFrom(func() string {
		if rec, err := store.LoadRecord(); err == nil {
			return rec.InstanceURL
		}
		return ""
	}).On("getCurrentUser", daemontest.OK(apicontract.User{Id: "123", Username: "ada", DisplayName: "ada",
		Email: "ada@example.com", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}))
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
		// Never a real browser: a guard removed in a mutation once sent a provider sign-in to the desktop's.
		openBrowser: func(context.Context, string) error { return errors.New("no browser in tests") },
		pollEvery:   time.Millisecond, waitFor: 200 * time.Millisecond,
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
		[]string{"", "", "ada", "ada@example.com", "", "", ""}, []string{"a correct passphrase", "a correct passphrase"})
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
		"Press Enter to sign in, once you have opened the link if it emailed you one: ",
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
	// The daemon has just said the instance takes the sign-in, so the join does not ask it again.
	assert.Len(t, requestsTo(run.daemon, "getCurrentUser"), 1)
}

// TestALinkWritingTheStoredInstanceAnotherWaySignsInWhereItIsStored: a link may write the instance this
// machine is signed in to with a final dot, which a resolver may send to another server. The comparison
// calls the two one instance, so nothing is asked, and what a sign-in then reaches is the instance as it was
// stored, never the link's spelling of it.
func TestALinkWritingTheStoredInstanceAnotherWaySignsInWhereItIsStored(t *testing.T) {
	f := newSetupInstance(t)
	stored := strings.Replace(f.server.URL, "127.0.0.1", "localhost", 1)
	link := strings.Replace(f.server.URL, "127.0.0.1", "localhost.", 1) + "/join#BCDFGHJKMNPQRSTV"
	run := newSetupRun(t, f, SetupOptions{Invite: link, NoDaemon: true}, []string{"ada@example.com"},
		[]string{"a correct passphrase"})
	seed(t, run.store, stored, "ada")
	run.daemon.SignInFrom(func() string { return "" })
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Equal(t, []string{"Email: "}, run.lines.asked)
	_, logins, _ := f.seen()
	assert.Len(t, logins, 1)
	rec, err := run.store.LoadRecord()
	require.NoError(t, err)
	assert.Equal(t, stored, rec.InstanceURL)
	assert.Contains(t, run.out.String(), "Setting up Norite on this machine, for "+stored+".\n")
}

// TestAnAnswerAboveTheTokensCheckSaysNothingOfTheSignIn: only a 2xx says the instance took the sign-in and
// only a 401 that it did not. A 503 while it migrates, a 429 or a proxy's 502 are answered before the token
// is looked at; the 403 here stands for them, being what the contract declares for this route.
func TestAnAnswerAboveTheTokensCheckSaysNothingOfTheSignIn(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL}, nil, nil)
	seed(t, run.store, f.server.URL, "ada")
	run.service.state = DaemonRunning
	run.daemon.On("getCurrentUser", func(daemontest.Request) (int, any) {
		var refusal apicontract.Error
		refusal.Error.Code, refusal.Error.Message = "forbidden", "missing scope"
		return 403, refusal
	})
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Contains(t, run.out.String(), "Signed in as @ada, as far as this machine knows: the instance answered "+
		"HTTP 403, so whether the instance still accepts it is not known. Running `norite setup` again asks "+
		"again.\n", "the daemon answered, so it is not the daemon status to look at")
	assert.NotContains(t, run.out.String(), "Signed in as @ada already.")
	_, logins, _ := f.seen()
	assert.Empty(t, logins)

	// A 200 that is not the instance's answer, a captive portal's page the relay dropped as not JSON, says
	// no more than the 403 does.
	portal := newSetupRun(t, f, SetupOptions{Instance: f.server.URL}, nil, nil)
	seed(t, portal.store, f.server.URL, "ada")
	portal.service.state = DaemonRunning
	portal.daemon.On("getCurrentUser", daemontest.OK(nil))
	require.NoError(t, portal.setup.Run(context.Background()), portal.out.String())
	assert.Contains(t, portal.out.String(), "\nSigned in as @ada, as far as this machine knows: the instance "+
		"answered HTTP 200 with no account in it, so whether the instance still accepts it is not known. "+
		"Running `norite setup` again asks again.\n")

	// Signed in to the instance as somebody else than the record names: a daemon that has not read the
	// store since another sign-in, which is not this machine's sign-in being accepted.
	other := newSetupRun(t, f, SetupOptions{Instance: f.server.URL}, nil, nil)
	seed(t, other.store, f.server.URL, "ada")
	other.service.state = DaemonRunning
	other.daemon.On("getCurrentUser", daemontest.OK(apicontract.User{Id: "999", Username: "bob",
		DisplayName: "bob", Email: "bob@example.com", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}))
	require.NoError(t, other.setup.Run(context.Background()), other.out.String())
	assert.Contains(t, other.out.String(), "\nSigned in as @ada, as far as this machine knows: the daemon is "+
		"signed in to it as another account than this machine's record names, so whether the instance still "+
		"accepts it is not known. `norite daemon status` says which.\n")
}

// TestTheJoinWaitsOutAnAnswerThatSaysNothingOfTheSignIn: right after a sign-in, the instance may answer
// the daemon with a 503 while it migrates, or a 429, and the daemon may still be signed in as the account
// it held before; the join waits those out as it waits for the daemon. A 401 reaches it only when a renewed
// sign-in is refused again, and ends the wait. Either way the join is left for later with the command.
func TestTheJoinWaitsOutAnAnswerThatSaysNothingOfTheSignIn(t *testing.T) {
	user := func(id string) apicontract.User {
		return apicontract.User{Id: id, Username: "ada", DisplayName: "ada", Email: "ada@example.com",
			CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	}
	refusal := func(code string) apicontract.Error {
		var e apicontract.Error
		e.Error.Code, e.Error.Message = apicontract.ErrorErrorCode(code), "no"
		return e
	}
	const later = " Join it later: `norite invite join '"
	for _, tc := range []struct {
		name   string
		answer func(n int) (int, any)
		want   string
	}{
		{"passing", func(n int) (int, any) {
			if n < 3 {
				return 403, refusal("forbidden")
			}
			return 200, user("123")
		}, "Joined Tea Room.\n"},
		{"for good", func(int) (int, any) { return 403, refusal("forbidden") },
			"\nstopped waiting: the instance answered HTTP 403." + later},
		{"as another account", func(int) (int, any) { return 200, user("999") },
			"\nstopped waiting: the daemon is still signed in to the instance as another account; " +
				"`norite daemon status` says which." + later},
		{"refused after a renewal", func(int) (int, any) { return 401, refusal("unauthorized") },
			"\nthe instance refused the daemon's sign-in; `norite login` signs in again." + later},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetupInstance(t)
			run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV", NoDaemon: true},
				[]string{"", "y", "ada@example.com", "", ""}, []string{"a correct passphrase"})
			asked := 0
			run.daemon.On("getCurrentUser", func(daemontest.Request) (int, any) {
				asked++
				return tc.answer(asked - 1)
			}).On("previewInvite", daemontest.OK(preview("Tea Room"))).
				On("redeemInvite", daemontest.OK(guildNamed("Tea Room")))
			require.NoError(t, run.setup.Run(context.Background()), run.out.String())
			assert.Contains(t, run.out.String(), tc.want)
		})
	}
}

// TestASignInTheInstanceRefusesIsWaitedForAndSaidAsEither: the relay renews on a 401 and answers a renewal
// refused as signed out, which is also what a daemon that has not read the store yet answers. The two
// cannot be told apart from here, so the wait ends saying both, never blaming the daemon alone.
func TestASignInTheInstanceRefusesIsWaitedForAndSaidAsEither(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV", NoDaemon: true},
		[]string{"", "y", "ada@example.com", ""}, []string{"a correct passphrase"})
	run.daemon.SignInFrom(func() string { return "" })
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Contains(t, run.out.String(), "\nstopped waiting: the daemon is not signed in, having not read the "+
		"sign-in stored on this machine yet or had it refused by the instance; `norite daemon status` says "+
		"which. Join it later: `norite invite join '"+f.server.URL+"/join#BCDFGHJKMNPQRSTV'`.\n")
	assert.Empty(t, requestsTo(run.daemon, "previewInvite"))
}

// TestTheInstanceTypedIsTheSpellingUsed: --instance and a link that name one instance in two spellings are
// one instance, and the one typed is where the account goes: a link's final dot may be resolved elsewhere.
func TestTheInstanceTypedIsTheSpellingUsed(t *testing.T) {
	f := newSetupInstance(t)
	typed := strings.Replace(f.server.URL, "127.0.0.1", "localhost", 1)
	link := strings.Replace(f.server.URL, "127.0.0.1", "localhost.", 1) + "/join#BCDFGHJKMNPQRSTV"
	run := newSetupRun(t, f, SetupOptions{Instance: typed, Invite: link, NoDaemon: true},
		[]string{"", "y", "ada@example.com", "", ""}, []string{"a correct passphrase"})
	run.daemon.On("previewInvite", daemontest.OK(preview("Tea Room"))).
		On("redeemInvite", daemontest.OK(guildNamed("Tea Room")))
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Equal(t, "The invite is for "+typed+": your account and password go there. Is that the instance "+
		"you meant? [Y/n] ", run.lines.asked[0])
	rec, err := run.store.LoadRecord()
	require.NoError(t, err)
	assert.Equal(t, typed, rec.InstanceURL)
}

// unreachableFirst answers its first n requests as a daemon that has a sign-in and no session from the
// instance yet, and passes the rest to InstanceCaller.
type unreachableFirst struct {
	daemonclient.InstanceCaller
	n int
}

func (u *unreachableFirst) DoFor(ctx context.Context, instance, method, path string, body any) (ipc.Result, error) {
	if u.n > 0 {
		u.n--
		return ipc.Result{}, &ipc.RelayError{Code: ipc.RelayUnreachable, Message: "the daemon has no usable " +
			"session from its instance yet"}
	}
	return u.InstanceCaller.DoFor(ctx, instance, method, path, body)
}

// TestADaemonWithoutASessionYetIsWaitedFor: just after a sign-in is stored, the daemon may still be getting
// its session from a slow instance, and says it cannot reach it. That is waited out like a daemon that has
// not read the sign-in; one that never gets there leaves the join for later, setup having done the rest.
func TestADaemonWithoutASessionYetIsWaitedFor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first int
		want  string
	}{
		{"it comes", 3, "Joined Tea Room.\n"},
		{"it never comes", 1 << 30, "\nstopped waiting: the daemon has no usable session from its instance yet. " +
			"Join it later: `norite invite join '"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetupInstance(t)
			run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV", NoDaemon: true},
				[]string{"", "y", "ada@example.com", "", ""}, []string{"a correct passphrase"})
			run.daemon.On("previewInvite", daemontest.OK(preview("Tea Room"))).
				On("redeemInvite", daemontest.OK(guildNamed("Tea Room")))
			connect := run.setup.Connect
			run.setup.Connect = func(ctx context.Context) (daemonclient.Caller, func(), error) {
				c, detach, err := connect(ctx)
				return &unreachableFirst{InstanceCaller: c.(daemonclient.InstanceCaller), n: tc.first}, detach, err
			}
			require.NoError(t, run.setup.Run(context.Background()), run.out.String())
			assert.Contains(t, run.out.String(), tc.want)
		})
	}
}

// TestABlankInstanceVariableIsAskedOver: NORITE_INSTANCE set to nothing but spaces names no instance, as
// `norite login` reads it, so the address is asked rather than refused.
func TestABlankInstanceVariableIsAskedOver(t *testing.T) {
	t.Setenv(instanceEnvVar, "  ")
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{}, []string{""}, nil)
	err := run.setup.Run(context.Background())
	require.EqualError(t, err, "an instance address is required: the person who invited you, or the "+
		"instance's administrator, has it")
	assert.Equal(t, []string{"Instance address, such as https://chat.example.com: "}, run.lines.asked)
}

// TestAnInviteOnlyInstanceAsksForItsCodeAndSendsTheAnswersAgain: the instance's invite_required is answered
// with its code, and the registration sent again with the same answers rather than asked again.
func TestAnInviteOnlyInstanceAsksForItsCodeAndSendsTheAnswersAgain(t *testing.T) {
	f := newSetupInstance(t)
	f.inviteOnly = true
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL, NoDaemon: true},
		[]string{"", "ada", "ada@example.com", "QQQNBQQMGYTMQRMX", ""}, []string{"a correct passphrase", "a correct passphrase"})

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
		[]string{"", "ada", "ada@example.com", "", ""}, []string{"a correct passphrase", "a correct passphrase"})
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	_, logins, _ := f.seen()
	assert.Len(t, logins, 2)
	assert.Contains(t, run.lines.asked, "Press Enter to try again: ")

	g := newSetupInstance(t)
	g.refuseLogins = 100
	stuck := newSetupRun(t, g, SetupOptions{Instance: g.server.URL, NoDaemon: true},
		[]string{"", "ada", "ada@example.com", "", "", ""}, []string{"a correct passphrase", "a correct passphrase"})
	err := stuck.setup.Run(context.Background())
	require.EqualError(t, err, "the instance still refuses the sign-in. If it emailed you a link, open the newest "+
		"one, then sign in with `norite login --instance '"+g.server.URL+"' --email 'ada@example.com'`; if that "+
		"address already had an account, the same command signs in with its own password")
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
	run.daemon.SignInFrom(func() string { return "https://other.example" })
	require.NoError(t, run.setup.Run(context.Background()))
	assert.Empty(t, run.daemon.Requests(), "nothing reached the instance the daemon is signed in to")
	assert.Contains(t, run.out.String(), "\nstopped waiting: the daemon is still signed in to another instance; "+
		"`norite daemon status` says which. Join it later: `norite invite join '"+f.server.URL+
		"/join#BCDFGHJKMNPQRSTV'`.\n")
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

// TestARefusedInstanceCodeIsAskedForAgain: the instance answers a wrong code invite_invalid, not
// invite_required, and a guild invite's code typed there is the usual mistake. The code is asked for again,
// the same answers sent with it.
func TestARefusedInstanceCodeIsAskedForAgain(t *testing.T) {
	f := newSetupInstance(t)
	f.inviteOnly, f.validCode = true, "QQQNBQQMGYTMQRMX"
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL, NoDaemon: true, InviteCode: "BCDFGHJKMNPQRSTV"},
		[]string{"", "ada", "ada@example.com", "QQQNBQQMGYTMQRMX", ""}, []string{"a correct passphrase", "a correct passphrase"})
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	registers, logins, _ := f.seen()
	require.Len(t, registers, 2)
	assert.Equal(t, "BCDFGHJKMNPQRSTV", registers[0].InviteCode)
	assert.Equal(t, "QQQNBQQMGYTMQRMX", registers[1].InviteCode)
	assert.Len(t, logins, 1)
	assert.Contains(t, run.out.String(), "the instance did not accept that instance invite code: it is mistyped, "+
		"used up or expired, or it is a guild invite's.\n")

	g := newSetupInstance(t)
	g.inviteOnly, g.validCode = true, "QQQNBQQMGYTMQRMX"
	wrong := newSetupRun(t, g, SetupOptions{Instance: g.server.URL, NoDaemon: true},
		[]string{"", "ada", "ada@example.com", "AAAA", "BBBB", "CCCC"}, []string{"a correct passphrase", "a correct passphrase"})
	assert.EqualError(t, wrong.setup.Run(context.Background()),
		"the instance did not accept an instance invite code; nothing was created")
	registers, _, _ = g.seen()
	assert.Len(t, registers, 1+inviteCodeAttempts)
}

// TestTheFirstSignInWaitsForTheLinkToBeOpened: a sign-in refused on an unconfirmed account mails a new link
// and spends the one registration sent, so nothing signs in until the person has had the chance to open it.
func TestTheFirstSignInWaitsForTheLinkToBeOpened(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL, NoDaemon: true}, nil,
		[]string{"a correct passphrase", "a correct passphrase"})
	answers := []string{"", "ada", "ada@example.com", ""}
	loginsAtEnter := -1
	run.setup.ReadLine = func(prompt string) (string, error) {
		if strings.HasPrefix(prompt, "Press Enter to sign in") {
			_, logins, _ := f.seen()
			loginsAtEnter = len(logins)
		}
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Zero(t, loginsAtEnter, "no sign-in before Enter")
	_, logins, _ := f.seen()
	assert.Len(t, logins, 1)
}

// TestAStoredSignInTheDaemonCallsSignedOutIsSignedInAgain: a record is not a sign-in the instance accepts.
func TestAStoredSignInTheDaemonCallsSignedOutIsSignedInAgain(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL, NoDaemon: true}, []string{"ada@example.com"},
		[]string{"a correct passphrase"})
	seed(t, run.store, f.server.URL, "ada")
	run.daemon.SignInFrom(func() string { return "" })
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Contains(t, run.out.String(), "the daemon says the instance no longer accepts it. Signing in again.\n"+
		"If @ada signs in through Google or GitHub, press Ctrl-C and run `norite setup --provider google` or "+
		"`--provider github` instead.\n")
	_, logins, _ := f.seen()
	assert.Len(t, logins, 1)
	assert.NotContains(t, run.out.String(), "Signed in as @ada already.")
}

// TestTheGuildIsJoinedWhateverTheServiceManagerSays: a manager that cannot be asked, or that has not caught
// up with an install, says nothing about the socket, which is what the join asks.
func TestTheGuildIsJoinedWhateverTheServiceManagerSays(t *testing.T) {
	for name, prepare := range map[string]func(*setupRun){
		"unknown":           func(r *setupRun) { r.service.state = DaemonUnknown },
		"installed, unsaid": func(r *setupRun) { r.service.slowToSay = true },
	} {
		f := newSetupInstance(t)
		run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV"}, []string{"", ""}, nil)
		seed(t, run.store, f.server.URL, "ada")
		prepare(run)
		run.daemon.On("previewInvite", daemontest.OK(preview("Tea Room"))).
			On("redeemInvite", daemontest.OK(guildNamed("Tea Room")))
		require.NoError(t, run.setup.Run(context.Background()), "%s: %s", name, run.out.String())
		assert.Len(t, requestsTo(run.daemon, "redeemInvite"), 1, name)
	}
}

// TestADaemonThatCannotBeReachedSaysWhy: the attach's own error, such as a version to restart, is the
// reason given, not that a sign-in was not taken up.
func TestADaemonThatCannotBeReachedSaysWhy(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV"}, nil, nil)
	seed(t, run.store, f.server.URL, "ada")
	run.service.state = DaemonRunning
	run.setup.Connect = func(context.Context) (daemonclient.Caller, func(), error) {
		return nil, nil, clierr.Unavailable("the daemon is version 0.2.0 and this client 0.3.0; restart the daemon")
	}
	require.NoError(t, run.setup.Run(context.Background()))
	assert.Contains(t, run.out.String(), "the daemon could not be reached within 200ms: the daemon is version 0.2.0 "+
		"and this client 0.3.0; restart the daemon. Join it once the daemon runs")
}

// TestFlagsTheSignInCannotUseAreUsageErrors: as `norite login` refuses them, and --invite-code beside a
// browser's sign-in, which would never send it.
func TestFlagsTheSignInCannotUseAreUsageErrors(t *testing.T) {
	for _, opts := range []SetupOptions{
		{Provider: "github", Email: "ada@example.com"},
		{DeviceCode: true, Email: "ada@example.com"},
		{Provider: "google", InviteCode: "QQQNBQQMGYTMQRMX"},
		{DeviceCode: true, InviteCode: "QQQNBQQMGYTMQRMX"},
	} {
		f := newSetupInstance(t)
		opts.Instance = f.server.URL
		run := newSetupRun(t, f, opts, nil, nil)
		// Bounded, so that the refusal removed fails this test rather than hanging it in a sign-in.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := run.setup.Run(ctx)
		cancel()
		var usage *clierr.UsageError
		require.ErrorAs(t, err, &usage, "%+v", opts)
		assert.Empty(t, run.lines.asked, "%+v", opts)
		_, _, requests := f.seen()
		assert.Zero(t, requests, "%+v", opts)
	}
}

// TestARefusedInviteKeepsItsExitCode: an expired invite is a refusal, exit 4, as `norite invite join` reports
// it, and the codes the verbs give are kept through setup's own wrapping.
func TestARefusedInviteKeepsItsExitCode(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV"}, nil, nil)
	seed(t, run.store, f.server.URL, "ada")
	run.service.state = DaemonRunning
	run.daemon.On("previewInvite", func(daemontest.Request) (int, any) {
		return 404, map[string]any{"error": map[string]any{"code": "not_found", "message": "not found",
			"request_id": "req-1"}}
	})
	err := run.setup.Run(context.Background())
	var refused *clierr.RefusedError
	require.ErrorAs(t, err, &refused)
	require.ErrorAs(t, setupExit(err), &refused)

	for _, kept := range []error{clierr.Usage("u"), clierr.Unavailable("u"), fmt.Errorf("%w: x", ErrNoTerminal)} {
		assert.Same(t, kept, setupExit(kept))
	}
	var coder cli.ExitCoder
	require.ErrorAs(t, setupExit(errors.New("plain")), &coder)
	assert.Equal(t, 1, coder.ExitCode())
	assert.NoError(t, setupExit(nil))
}

// TestAnInviteToJoinLaterIsPrintedQuoted: a code may hold spaces, and a link a '#' some shells read.
func TestAnInviteToJoinLaterIsPrintedQuoted(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL, Invite: "PGTQ RBXG TCDF FWBH"}, []string{"n"}, nil)
	seed(t, run.store, f.server.URL, "ada")
	run.service.state = DaemonAbsent
	require.NoError(t, run.setup.Run(context.Background()))
	// Written back as checked, as a link to the instance set up for: a bare code would go to whichever
	// instance the daemon is on by then.
	assert.Contains(t, run.out.String(), "\nJoin it once the daemon runs: `norite invite join '"+f.server.URL+
		"/join#PGTQRBXGTCDFFWBH'`.\n")
}

// TestAPreviewSaysWhatInviteShowSays: the expiry and the description, which a person deciding to join reads.
func TestAPreviewSaysWhatInviteShowSays(t *testing.T) {
	p := preview("Tea Room")
	at := time.Date(2026, 10, 11, 12, 26, 0, 0, time.Local)
	description := "a quiet \x1b[2Jroom"
	p.ExpiresAt, p.Guild.Description = &at, &description
	assert.Equal(t, "The invite leads to Tea Room, #general, invited by Alice (@alice), and expires 2026-10-11 12:26."+
		"\n  a quiet \ufffd[2Jroom", describePreview(p))
}

// TestAStoredSignInIsCheckedOnceTheDaemonIsUp: the daemon is what can say whether the instance still accepts
// a stored sign-in, so it comes up first, and one it calls signed out is signed in again rather than reported
// as done. With no daemon at all, nothing is claimed either way.
func TestAStoredSignInIsCheckedOnceTheDaemonIsUp(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL}, []string{"", "ada@example.com"},
		[]string{"a correct passphrase"})
	seed(t, run.store, f.server.URL, "ada")
	run.service.state = DaemonStopped
	run.daemon.SignInFrom(func() string { return "" })
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Equal(t, 1, run.service.starts)
	assert.Equal(t, "Start it? [Y/n] ", strings.TrimPrefix(run.lines.asked[0], "The daemon is installed and not running. "))
	assert.Contains(t, run.out.String(), "the daemon says the instance no longer accepts it. Signing in again.")
	_, logins, _ := f.seen()
	assert.Len(t, logins, 1)

	g := newSetupInstance(t)
	down := newSetupRun(t, g, SetupOptions{Instance: g.server.URL}, []string{"n"}, nil)
	seed(t, down.store, g.server.URL, "ada")
	down.service.state = DaemonStopped
	require.NoError(t, down.setup.Run(context.Background()), down.out.String())
	assert.Contains(t, down.out.String(), "Signed in as @ada, as far as this machine knows: the daemon is not "+
		"running, so whether the instance still accepts it is not known.")
	assert.NotContains(t, down.out.String(), "Signed in as @ada already.")
	_, logins, _ = g.seen()
	assert.Empty(t, logins)
}

// TestADaemonLeftAloneIsAskedOnceAndNotWaitedFor: with --no-daemon nothing is starting one, so a socket
// nobody answers is said at once rather than waited on.
func TestADaemonLeftAloneIsAskedOnceAndNotWaitedFor(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV", NoDaemon: true}, nil, nil)
	seed(t, run.store, f.server.URL, "ada")
	run.setup.waitFor = time.Minute
	attaches := 0
	run.setup.Connect = func(context.Context) (daemonclient.Caller, func(), error) {
		attaches++
		return nil, nil, clierr.Unavailable("the daemon is not running")
	}
	start := time.Now()
	require.NoError(t, run.setup.Run(context.Background()))
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, 2, attaches, "once for the stored sign-in, once for the guild")
	assert.Contains(t, run.out.String(), "no daemon answered (the daemon is not running). Join it once the daemon "+
		"runs: `norite invite join '"+f.server.URL+"/join#BCDFGHJKMNPQRSTV'`.")
}

// TestTheJoinWaitsOnOneAttachment: a daemon that has not yet read the sign-in setup stored answers that it
// is signed out, on this machine, and is asked again on the same attachment until it has.
func TestTheJoinWaitsOnOneAttachment(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV", NoDaemon: true},
		[]string{"", "y", "ada@example.com", ""}, []string{"a correct passphrase"})
	run.setup.waitFor = 10 * time.Second
	asked := 0
	run.daemon.SignInFrom(func() string {
		asked++
		if asked < 5 {
			return ""
		}
		return f.server.URL
	}).On("previewInvite", daemontest.OK(preview("Tea Room"))).
		On("redeemInvite", daemontest.OK(guildNamed("Tea Room")))
	attaches := 0
	connect := run.setup.Connect
	run.setup.Connect = func(ctx context.Context) (daemonclient.Caller, func(), error) {
		attaches++
		return connect(ctx)
	}
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Equal(t, 1, attaches)
	assert.GreaterOrEqual(t, asked, 5)
	assert.Len(t, requestsTo(run.daemon, "redeemInvite"), 1)
}

// TestAMistypedProviderIsRefusedFirst: as `norite login` refuses it, before anything is asked.
func TestAMistypedProviderIsRefusedFirst(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL, Provider: "gihtub"}, nil, nil)
	err := run.setup.Run(context.Background())
	var usage *clierr.UsageError
	require.ErrorAs(t, err, &usage)
	assert.Contains(t, err.Error(), `unknown sign-in provider "gihtub"`)
	assert.Empty(t, run.lines.asked)
	_, _, requests := f.seen()
	assert.Zero(t, requests)
}

// TestABareCodeGoesOnlyToTheInstanceSetUpFor: a code given without its link names no instance, and is for
// the one setup was run for. A daemon still signed in elsewhere is never sent it.
func TestABareCodeGoesOnlyToTheInstanceSetUpFor(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL, Invite: "BCDFGHJKMNPQRSTV"}, nil, nil)
	seed(t, run.store, f.server.URL, "ada")
	run.service.state = DaemonRunning
	run.daemon.SignInFrom(func() string { return "https://other.example" }).
		On("previewInvite", daemontest.OK(preview("Tea Room")))
	require.NoError(t, run.setup.Run(context.Background()))
	assert.Empty(t, run.daemon.Requests())
	assert.Contains(t, run.out.String(), "\nstopped waiting: the daemon is still signed in to another instance; "+
		"`norite daemon status` says which. Join it later: `norite invite join '"+f.server.URL+
		"/join#BCDFGHJKMNPQRSTV'`.\n")
}

// TestASignedOutAnswerIsAskedAgainBeforeItIsARefusal: signed out is also a daemon that has not read a
// sign-in stored a moment ago, so a stored sign-in the daemon calls signed out and then takes up is not
// replaced, and nothing asks for a password.
func TestASignedOutAnswerIsAskedAgainBeforeItIsARefusal(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Instance: f.server.URL}, nil, nil)
	seed(t, run.store, f.server.URL, "ada")
	run.service.state = DaemonRunning
	asked := 0
	run.daemon.SignInFrom(func() string {
		asked++
		if asked < 3 {
			return ""
		}
		return f.server.URL
	})
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Contains(t, run.out.String(), "\nSigned in as @ada already.\n")
	assert.Empty(t, run.secrets.asked)
	_, logins, _ := f.seen()
	assert.Empty(t, logins)
}

// TestARecordNamingNoAccountConfirmsNothing: a sign-in whose `GET /users/@me` failed stores no account id,
// and then any account the daemon holds would pass for it, so nothing is claimed and nothing is joined.
func TestARecordNamingNoAccountConfirmsNothing(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV"}, nil, nil)
	id, err := run.store.DeviceID()
	require.NoError(t, err)
	require.NoError(t, run.store.Save(credentials.Record{InstanceURL: f.server.URL, Username: "ada",
		DeviceID: id, DeviceName: "ada-laptop"}, "nrt_kept"))
	run.service.state = DaemonRunning
	run.daemon.On("previewInvite", daemontest.OK(preview("Tea Room")))
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Contains(t, run.out.String(), "\nSigned in as @ada, as far as this machine knows: this machine's "+
		"record does not say which account it is, so whether the instance still accepts it is not known. "+
		"`norite login` signs in again and says.\n")
	assert.Contains(t, run.out.String(), "\nthis machine's sign-in does not say which account it is; "+
		"`norite login` signs in again and says. Join it later: `norite invite join '"+f.server.URL+
		"/join#BCDFGHJKMNPQRSTV'`.\n")
	assert.Empty(t, requestsTo(run.daemon, "previewInvite"))
}

// dropping is an attachment that ends under the first request made on it.
type dropping struct{ daemonclient.InstanceCaller }

func (dropping) DoFor(context.Context, string, string, string, any) (ipc.Result, error) {
	return ipc.Result{}, ipc.ErrClosed
}

// TestAnAttachmentEndingUnderTheWaitSaysTheDaemon: the daemon going away while the join waits is the daemon
// to start again, said as a daemon that never answered is, not as an instance to wait for.
func TestAnAttachmentEndingUnderTheWaitSaysTheDaemon(t *testing.T) {
	f := newSetupInstance(t)
	run := newSetupRun(t, f, SetupOptions{Invite: f.server.URL + "/join#BCDFGHJKMNPQRSTV", NoDaemon: true},
		[]string{"", "y", "ada@example.com", ""}, []string{"a correct passphrase"})
	connect := run.setup.Connect
	run.setup.Connect = func(ctx context.Context) (daemonclient.Caller, func(), error) {
		c, detach, err := connect(ctx)
		return dropping{c.(daemonclient.InstanceCaller)}, detach, err
	}
	require.NoError(t, run.setup.Run(context.Background()), run.out.String())
	assert.Contains(t, run.out.String(), "\nthe daemon could not be asked: "+ipc.ErrClosed.Error()+
		". Join it once the daemon runs: `norite invite join '")
}
