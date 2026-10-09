// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/statefile"
)

const testToken = "nat_EXAMPLEexampleEXAMPLEexampleEXAMPLEexample0"

// fakeSignIn is the session as the port sees it: a standing and an instance.
type fakeSignIn struct {
	mu       sync.Mutex
	standing session.Standing
	url      string
}

func (f *fakeSignIn) Status() (session.Standing, session.Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.standing, session.Account{InstanceURL: f.url}
}

func (f *fakeSignIn) set(standing session.Standing, url string) {
	f.mu.Lock()
	f.standing, f.url = standing, url
	f.mu.Unlock()
}

type portFixture struct {
	t        *testing.T
	dir      string
	signIn   *fakeSignIn
	instance *httptest.Server
	control  *automationControl

	mu   sync.Mutex
	auth []string
}

func newPortFixture(t *testing.T) *portFixture {
	t.Helper()
	f := &portFixture{t: t, dir: t.TempDir(), signIn: &fakeSignIn{standing: session.Live}}
	f.instance = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"1"}`))
	}))
	t.Cleanup(f.instance.Close)
	f.signIn.url = f.instance.URL
	f.control = f.boot()
	return f
}

// boot is a daemon starting on this state directory.
func (f *portFixture) boot() *automationControl {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c := newAutomation(ctx, f.dir, f.signIn, "dev", zerolog.Nop())
	c.start()
	f.t.Cleanup(func() {
		cancel()
		c.stop()
	})
	return c
}

func (f *portFixture) ask(method, path string) ipc.Response {
	f.t.Helper()
	return f.control.Do(context.Background(), ipc.Request{ID: "1", Method: method, Path: path})
}

// status asks and requires a 200, holding the body to the contract.
func (f *portFixture) ok(method, path string) ipc.AutomationStatus {
	f.t.Helper()
	resp := f.ask(method, path)
	require.Nil(f.t, resp.Error, "%s %s: %+v", method, path, resp.Error)
	require.Equal(f.t, http.StatusOK, *resp.Status)
	matchesAutomationStatus(f.t, resp.Body)
	var out ipc.AutomationStatus
	require.NoError(f.t, json.Unmarshal(resp.Body, &out))
	return out
}

func matchesAutomationStatus(t *testing.T, body []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "daemon-ipc.schema.json"))
	require.NoError(t, err)
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Contains(t, doc.Defs, "AutomationStatus")
	def, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc.Defs["AutomationStatus"]))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource("status.json", def))
	schema, err := c.Compile("status.json")
	require.NoError(t, err)
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, schema.Validate(inst), "%s", body)
}

// freePort is a port nothing holds at the moment it is asked.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func (f *portFixture) secret() string {
	f.t.Helper()
	file, err := ipc.LoadAutomationFile(f.dir)
	require.NoError(f.t, err)
	return file.Secret
}

func (f *portFixture) fileExists() bool {
	_, err := os.Stat(ipc.AutomationFilePath(f.dir))
	return err == nil
}

// send performs one request through the port as a script would, and reports whether it reached the instance.
func (f *portFixture) send() *ipc.RelayError {
	f.t.Helper()
	file, err := ipc.LoadAutomationFile(f.dir)
	require.NoError(f.t, err)
	c, err := ipc.DialAutomation(context.Background(), file, testToken)
	require.NoError(f.t, err)
	defer func() { _ = c.Close() }()
	resp, err := c.Do(context.Background(), "GET", "/users/@me", nil)
	require.NoError(f.t, err)
	return resp.Error
}

// TestThePortIsClosedUntilItsUserTurnsItOn: a daemon nobody asked has no listener and no file, and says the
// port it would use.
func TestThePortIsClosedUntilItsUserTurnsItOn(t *testing.T) {
	f := newPortFixture(t)
	st := f.ok("GET", ipc.PathAutomation)
	assert.Equal(t, ipc.AutomationStatus{Port: ipc.DefaultAutomationPort}, st)
	assert.False(t, f.fileExists())

	// Asking changes nothing, the state file included.
	_, err := os.Stat(statefile.PathIn(f.dir))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestEnablingOpensThePortForTheSignedInInstanceAndRecordsBoth is the switch doing its job, end to end: a
// script reads the file, presents its secrets, and its request reaches the instance with its own token.
func TestEnablingOpensThePortForTheSignedInInstanceAndRecordsBoth(t *testing.T) {
	f := newPortFixture(t)
	port := freePort(t)

	st := f.ok("POST", ipc.PathAutomationEnable(port))
	assert.Equal(t, ipc.AutomationStatus{
		Enabled: true, Port: port, Instance: f.instance.URL, Open: true,
		Address: net.JoinHostPort("127.0.0.1", itoa(port)),
	}, st)
	assert.Equal(t, st, f.ok("GET", ipc.PathAutomation))

	require.Nil(t, f.send())
	f.mu.Lock()
	assert.Equal(t, []string{"Bearer " + testToken}, f.auth)
	f.mu.Unlock()

	saved, err := statefile.ReadIn(f.dir)
	require.NoError(t, err)
	assert.True(t, saved.AutomationEnabled)
	assert.Equal(t, port, saved.AutomationPort)
	assert.Equal(t, f.instance.URL, saved.AutomationInstance)
}

// TestThePortSurvivesARestartAndItsSecretDoesNot: the switch is in the state file, so the next daemon opens
// the port without being asked; the secret is minted per run, so the last one's is no use.
func TestThePortSurvivesARestartAndItsSecretDoesNot(t *testing.T) {
	f := newPortFixture(t)
	f.ok("POST", ipc.PathAutomationEnable(freePort(t)))
	before, err := ipc.LoadAutomationFile(f.dir)
	require.NoError(t, err)

	f.control.stop()
	assert.False(t, f.fileExists(), "a stopped daemon leaves no file naming its port")

	f.control = f.boot()
	st := f.ok("GET", ipc.PathAutomation)
	assert.True(t, st.Open)
	after, err := ipc.LoadAutomationFile(f.dir)
	require.NoError(t, err)
	assert.Equal(t, before.Address, after.Address)
	assert.NotEqual(t, before.Secret, after.Secret)
	require.Nil(t, f.send())
}

// TestThePortIsTurnedOnForOneInstance: nobody signed in is nothing to turn it on for, and a sign-in that
// moves afterwards does not take the port with it.
func TestThePortIsTurnedOnForOneInstance(t *testing.T) {
	f := newPortFixture(t)
	f.signIn.set(session.SignedOut, "")
	resp := f.ask("POST", ipc.PathAutomationEnable(freePort(t)))
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayConflict, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "norite login")
	assert.False(t, f.fileExists())
	assert.False(t, f.ok("GET", ipc.PathAutomation).Enabled)

	// A daemon still establishing its sign-in already names its instance, and that is enough.
	f.signIn.set(session.Starting, f.instance.URL)
	st := f.ok("POST", ipc.PathAutomationEnable(freePort(t)))
	assert.Equal(t, f.instance.URL, st.Instance)
	require.Nil(t, f.send())

	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an instance the port was not turned on for was sent a script's token")
	}))
	defer other.Close()
	f.signIn.set(session.Live, other.URL)
	refused := f.send()
	require.NotNil(t, refused)
	assert.Equal(t, ipc.RelayRefused, refused.Code)
	st = f.ok("GET", ipc.PathAutomation)
	assert.Equal(t, f.instance.URL, st.Instance, "status still names the first")
	assert.True(t, st.Open)
	assert.Contains(t, st.Problem, other.URL, "an open port that refuses everything must say so")
	assert.Contains(t, st.Problem, "norite automation enable")

	f.signIn.set(session.SignedOut, "")
	st = f.ok("GET", ipc.PathAutomation)
	assert.True(t, st.Open)
	assert.Contains(t, st.Problem, "norite login")

	// Turning it on again is how it is moved.
	f.signIn.set(session.Live, f.instance.URL)
	assert.Empty(t, f.ok("POST", ipc.PathAutomationEnable(freePort(t))).Problem)
	require.Nil(t, f.send())
}

// TestATakenPortIsRefusedAndNothingChanges: the daemon does not walk past a port something holds. Turning
// the port on is refused with the reason, the state file is as it was, and a port that was open before is
// open again.
func TestATakenPortIsRefusedAndNothingChanges(t *testing.T) {
	squatter, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = squatter.Close() }()
	taken := squatter.Addr().(*net.TCPAddr).Port

	f := newPortFixture(t)
	resp := f.ask("POST", ipc.PathAutomationEnable(taken))
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayConflict, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, itoa(taken))
	assert.Contains(t, resp.Error.Message, "Nothing was changed")
	assert.Equal(t, ipc.AutomationStatus{Port: ipc.DefaultAutomationPort}, f.ok("GET", ipc.PathAutomation))
	assert.False(t, f.fileExists())

	good := freePort(t)
	f.ok("POST", ipc.PathAutomationEnable(good))
	secret := f.secret()
	resp = f.ask("POST", ipc.PathAutomationEnable(taken))
	require.NotNil(t, resp.Error)
	assert.Contains(t, resp.Error.Message, "Nothing was changed")
	assert.Equal(t, secret, f.secret(), "\"nothing was changed\" after the secret every running script holds was replaced")
	st := f.ok("GET", ipc.PathAutomation)
	assert.True(t, st.Open)
	assert.Equal(t, good, st.Port)
	assert.Empty(t, st.Problem)
	require.Nil(t, f.send(), "the port that was open is open again")
}

// TestAnEnabledPortThatCannotOpenAtStartSaysWhy: the one case where enabled and open differ. The daemon
// starts, the port stays closed, no file names it, and status carries the reason.
func TestAnEnabledPortThatCannotOpenAtStartSaysWhy(t *testing.T) {
	f := newPortFixture(t)
	port := freePort(t)
	f.ok("POST", ipc.PathAutomationEnable(port))
	f.control.stop()

	squatter, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", itoa(port)))
	require.NoError(t, err)
	defer func() { _ = squatter.Close() }()

	f.control = f.boot()
	st := f.ok("GET", ipc.PathAutomation)
	assert.True(t, st.Enabled)
	assert.False(t, st.Open)
	assert.Empty(t, st.Address)
	assert.Contains(t, st.Problem, itoa(port))
	assert.False(t, f.fileExists(), "no file may name a port another program holds")
}

// TestDisablingClosesThePortAndKeepsItsNumber: and asked of a port already off, it changes nothing.
func TestDisablingClosesThePortAndKeepsItsNumber(t *testing.T) {
	f := newPortFixture(t)
	port := freePort(t)
	f.ok("POST", ipc.PathAutomationEnable(port))
	file, err := ipc.LoadAutomationFile(f.dir)
	require.NoError(t, err)

	for range 2 {
		st := f.ok("POST", ipc.PathAutomationDisable)
		assert.Equal(t, ipc.AutomationStatus{Port: port}, st)
		assert.False(t, f.fileExists())
	}
	_, err = ipc.DialAutomation(context.Background(), file, testToken)
	require.ErrorIs(t, err, ipc.ErrAutomationOff, "nothing listens where the port was")

	f.control.stop()
	f.control = f.boot()
	assert.False(t, f.ok("GET", ipc.PathAutomation).Open, "a restart does not reopen a port that was turned off")
}

// TestStartingRemovesWhatAKilledDaemonLeft: with the port off, too. The file names a port this daemon does
// not hold and anything may since have bound.
func TestStartingRemovesWhatAKilledDaemonLeft(t *testing.T) {
	f := newPortFixture(t)
	require.NoError(t, os.WriteFile(ipc.AutomationFilePath(f.dir),
		[]byte(`{"address":"127.0.0.1:7717","secret":"left-behind"}`), 0o600))
	f.control = f.boot()
	assert.False(t, f.fileExists())
}

// TestAPortTurnedOffStaysClosedWhateverElseTheFileSays: the switch is what opens it, not a port number and
// an instance left in the file.
func TestAPortTurnedOffStaysClosedWhateverElseTheFileSays(t *testing.T) {
	f := newPortFixture(t)
	port := freePort(t)
	state := `{"version":1,"automation_enabled":false,"automation_port":` + itoa(port) +
		`,"automation_instance":"` + f.instance.URL + `"}`
	require.NoError(t, os.WriteFile(statefile.PathIn(f.dir), []byte(state), 0o600))

	f.control = f.boot()
	assert.False(t, f.fileExists())
	l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", itoa(port)))
	require.NoError(t, err, "the daemon is listening on a port that is turned off")
	_ = l.Close()
}

func TestWhatTheAutomationRequestsRefuse(t *testing.T) {
	f := newPortFixture(t)
	for _, tc := range [][2]string{
		{"POST", ipc.PathAutomation},
		{"GET", ipc.PathAutomationDisable},
		{"GET", ipc.PathAutomationEnable(7717)},
		{"DELETE", ipc.PathAutomation},
		{"POST", ipc.PathAutomation + "/enable"},
		{"POST", ipc.PathAutomation + "/enable/"},
		{"POST", ipc.PathAutomation + "/enable/0"},
		{"POST", ipc.PathAutomation + "/enable/65536"},
		{"POST", ipc.PathAutomation + "/enable/07717"},
		{"POST", ipc.PathAutomation + "/enable/7717/x"},
		{"POST", ipc.PathAutomation + "/enable/-1"},
		{"POST", ipc.PathAutomation + "/enable/77a"},
		{"POST", ipc.PathAutomation + "/nonsense"},
	} {
		resp := f.ask(tc[0], tc[1])
		require.NotNil(t, resp.Error, "%s %s", tc[0], tc[1])
		assert.Equal(t, ipc.RelayBadRequest, resp.Error.Code, "%s %s", tc[0], tc[1])
	}
	assert.False(t, f.ok("GET", ipc.PathAutomation).Enabled)
	assert.False(t, f.fileExists())
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestStartingLeavesAPortARequestAlreadyOpened: the attach socket is served before start runs, so an enable
// can arrive first. start then finds this run's own port, and must not remove its file or try to bind it a
// second time: a port open with no file is one no script can find (M22 review).
func TestStartingLeavesAPortARequestAlreadyOpened(t *testing.T) {
	f := newPortFixture(t)
	f.ok("POST", ipc.PathAutomationEnable(freePort(t)))

	f.control.start()
	require.True(t, f.fileExists(), "start removed the file of a port this daemon had just opened")
	st := f.ok("GET", ipc.PathAutomation)
	assert.True(t, st.Open)
	assert.Empty(t, st.Problem)
	require.Nil(t, f.send())
}

// TestARefusedEnableKeepsWhyTheEnabledPortIsNotOpen: a port that is on and could not be bound says why. An
// enable that is refused changes nothing, and that includes the reason.
func TestARefusedEnableKeepsWhyTheEnabledPortIsNotOpen(t *testing.T) {
	f := newPortFixture(t)
	port := freePort(t)
	f.ok("POST", ipc.PathAutomationEnable(port))
	f.control.stop()

	squatter, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", itoa(port)))
	require.NoError(t, err)
	defer func() { _ = squatter.Close() }()
	other, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = other.Close() }()

	f.control = f.boot()
	resp := f.ask("POST", ipc.PathAutomationEnable(other.Addr().(*net.TCPAddr).Port))
	require.NotNil(t, resp.Error)
	st := f.ok("GET", ipc.PathAutomation)
	assert.True(t, st.Enabled)
	assert.False(t, st.Open)
	assert.Equal(t, port, st.Port)
	assert.Contains(t, st.Problem, itoa(port), "the reason the enabled port is not open was lost")
}

// TestAnEnableThatFailsOnTheSameNumberSaysTheSecretChanged: the port has to be closed to be bound again, so
// a failure there cannot leave things as they were. What was open is opened again, and the refusal says
// its secret is new rather than that nothing changed.
func TestAnEnableThatFailsOnTheSameNumberSaysTheSecretChanged(t *testing.T) {
	f := newPortFixture(t)
	port := freePort(t)
	f.ok("POST", ipc.PathAutomationEnable(port))
	secret := f.secret()

	// An instance the port cannot be opened for: the open fails after the old listener has gone.
	f.signIn.set(session.Live, "not-a-url")
	resp := f.ask("POST", ipc.PathAutomationEnable(port))
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayConflict, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "new secret")
	assert.NotContains(t, resp.Error.Message, "Nothing was changed")
	assert.NotEqual(t, secret, f.secret())

	f.signIn.set(session.Live, f.instance.URL)
	st := f.ok("GET", ipc.PathAutomation)
	assert.True(t, st.Open)
	assert.Equal(t, f.instance.URL, st.Instance)
	require.Nil(t, f.send())
}

// TestADaemonStillReadingItsSignInIsNotToldToLogIn: started before its keyring unlocks, a daemon names no
// instance and is not signed out. Sending its user to `norite login` would replace a good sign-in.
func TestADaemonStillReadingItsSignInIsNotToldToLogIn(t *testing.T) {
	f := newPortFixture(t)
	f.ok("POST", ipc.PathAutomationEnable(freePort(t)))

	f.signIn.set(session.Starting, "")
	resp := f.ask("POST", ipc.PathAutomationEnable(freePort(t)))
	require.NotNil(t, resp.Error)
	assert.Contains(t, resp.Error.Message, "try again")
	assert.NotContains(t, resp.Error.Message, "norite login")

	st := f.ok("GET", ipc.PathAutomation)
	assert.Contains(t, st.Problem, "try again")
	assert.NotContains(t, st.Problem, "norite login")
}

// TestNothingOpensThePortOnceTheDaemonIsStopping: a request still on its way in when the daemon stops must
// not open the port behind it, which would leave a file naming a port nothing holds after a clean exit.
func TestNothingOpensThePortOnceTheDaemonIsStopping(t *testing.T) {
	f := newPortFixture(t)
	f.ok("POST", ipc.PathAutomationEnable(freePort(t)))
	f.control.stop()
	require.False(t, f.fileExists())

	resp := f.ask("POST", ipc.PathAutomationEnable(freePort(t)))
	require.NotNil(t, resp.Error)
	assert.Contains(t, resp.Error.Message, "stopping")
	assert.False(t, f.fileExists(), "a stopped daemon wrote a file naming a port")
	assert.False(t, f.ok("GET", ipc.PathAutomation).Open)
}

// TestAPortWhoseRecordCouldNotBeWrittenIsPutBackAsTheFileSays: the port is opened before the state file is
// written. If the write fails, the file is what status reports and what the next start reads, so a port
// left open would be one its user is told is off.
func TestAPortWhoseRecordCouldNotBeWrittenIsPutBackAsTheFileSays(t *testing.T) {
	f := newPortFixture(t)
	real := updateState
	t.Cleanup(func() { updateState = real })
	updateState = func(_ context.Context, dir string, fn func(*statefile.State) error, _ func()) error {
		st, err := statefile.ReadIn(dir)
		require.NoError(t, err)
		if err := fn(&st); err != nil {
			return err
		}
		return errors.New("no space left on device")
	}

	resp := f.ask("POST", ipc.PathAutomationEnable(freePort(t)))
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayFailed, resp.Error.Code)
	assert.False(t, f.fileExists(), "the port stayed open with nothing recording it")

	updateState = real
	st := f.ok("GET", ipc.PathAutomation)
	assert.False(t, st.Enabled)
	assert.False(t, st.Open)
}

// TestStatusIsOneMomentsAnswer: the state file is written just after the port moves and is read by a
// request that may land in between, and the attach socket answers before start has run. No answer may say
// the port is off while it is open, or on and not open with no reason given.
func TestStatusIsOneMomentsAnswer(t *testing.T) {
	f := newPortFixture(t)
	port := freePort(t)
	enabled := `{"version":1,"automation_enabled":true,"automation_port":` + itoa(port) +
		`,"automation_instance":"` + f.instance.URL + `"}`
	disabled := `{"version":1,"automation_enabled":false,"automation_port":` + itoa(port) + `}`

	// Open, with the file not yet saying so: what is open is what is reported.
	f.ok("POST", ipc.PathAutomationEnable(port))
	require.NoError(t, os.WriteFile(statefile.PathIn(f.dir), []byte(disabled), 0o600))
	st := f.ok("GET", ipc.PathAutomation)
	assert.True(t, st.Enabled, "the port is open and status says it is off")
	assert.True(t, st.Open)
	assert.Equal(t, f.instance.URL, st.Instance)
	f.ok("POST", ipc.PathAutomationDisable)

	// Recorded as on, asked before start has run.
	require.NoError(t, os.WriteFile(statefile.PathIn(f.dir), []byte(enabled), 0o600))
	early := newAutomation(context.Background(), f.dir, f.signIn, "dev", zerolog.Nop())
	f.control = early
	st = f.ok("GET", ipc.PathAutomation)
	assert.True(t, st.Enabled)
	assert.False(t, st.Open)
	assert.Contains(t, st.Problem, "still starting")

	// Started and then stopped: still a reason.
	early.start()
	assert.True(t, f.ok("GET", ipc.PathAutomation).Open)
	early.stop()
	st = f.ok("GET", ipc.PathAutomation)
	assert.False(t, st.Open)
	assert.Contains(t, st.Problem, "stopping")
}
