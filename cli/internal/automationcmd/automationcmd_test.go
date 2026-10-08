// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package automationcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// A token shaped like a real one that says it is not.
const botToken = "nat_EXAMPLEexampleEXAMPLEexampleEXAMPLEexample0"

// TestMain points the state directory at a temporary one, so that a test which forgot to replace `located`
// reads nothing of whoever is running the suite.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "norite-automationcmd-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, name := range []string{"XDG_STATE_HOME", "XDG_CONFIG_HOME", "LOCALAPPDATA", "APPDATA", "HOME"} {
		_ = os.Setenv(name, dir)
	}
	for _, name := range []string{EnvAddress, EnvSecret, EnvToken} {
		_ = os.Unsetenv(name)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// ---------- the harness ----------

// fakeDaemon answers the three requests about the port, and records what it was asked.
type fakeDaemon struct {
	status ipc.AutomationStatus
	// refuse, when set, is the daemon's answer to a POST.
	refuse *ipc.RelayError
	// older makes it a daemon from before these requests: it relays the path and reports an instance's 404.
	older bool
	asked []string
}

func (f *fakeDaemon) Do(_ context.Context, method, path string, _ any) (ipc.Result, error) {
	f.asked = append(f.asked, method+" "+path)
	if f.older {
		return ipc.Result{Status: 404, Body: json.RawMessage(`{"error":{"code":"not_found","message":"x","request_id":"r"}}`)}, nil
	}
	if method == "POST" {
		if f.refuse != nil {
			return ipc.Result{}, f.refuse
		}
		if port, ok := ipc.AutomationEnablePort(path); ok {
			f.status = ipc.AutomationStatus{Enabled: true, Port: port, Instance: "https://chat.example", Open: true,
				Address: fmt.Sprintf("127.0.0.1:%d", port)}
		} else {
			f.status = ipc.AutomationStatus{Port: f.status.Port}
		}
	}
	body, _ := json.Marshal(f.status)
	return ipc.Result{Status: 200, Body: body}, nil
}

type ran struct {
	out, errOut string
	err         error
}

// norite runs `norite [--json] automation args...` against daemon, which may be nil.
func norite(t *testing.T, daemon *fakeDaemon, stdin string, asJSON bool, args ...string) ran {
	t.Helper()
	var connect Connector
	if daemon != nil {
		connect = func(context.Context) (daemonclient.Caller, func(), error) { return daemon, func() {}, nil }
	}
	var out, errOut bytes.Buffer
	root := &cli.Command{
		Name: "norite", Writer: &out, ErrWriter: &errOut, Reader: strings.NewReader(stdin),
		Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands:       []*cli.Command{Command(connect)},
	}
	argv := []string{"norite"}
	if asJSON {
		argv = append(argv, "--json")
	}
	err := root.Run(context.Background(), append(append(argv, "automation"), args...))
	return ran{out: out.String(), errOut: errOut.String(), err: err}
}

func requireUsage(t *testing.T, err error, what string) {
	t.Helper()
	var usage *clierr.UsageError
	require.ErrorAs(t, err, &usage, what)
}

func requireUnavailable(t *testing.T, err error, what string) {
	t.Helper()
	var unavailable *clierr.UnavailableError
	require.ErrorAs(t, err, &unavailable, what)
}

// ---------- enable, disable, status ----------

// TestEnableAsksHowThePortStandsBeforeItSendsAPortNumber: the number is the one thing of the user's that a
// request to the daemon carries in its path, and a daemon from before these requests would relay that path
// to its instance. So a request that says nothing goes first, and an older daemon is found out by it.
func TestEnableAsksHowThePortStandsBeforeItSendsAPortNumber(t *testing.T) {
	d := &fakeDaemon{status: ipc.AutomationStatus{Port: ipc.DefaultAutomationPort}}
	r := norite(t, d, "", true, "enable")
	require.NoError(t, r.err)
	assert.Equal(t, []string{"GET " + ipc.PathAutomation, "POST " + ipc.PathAutomationEnable(7717)}, d.asked)
	daemontest.MatchesCLISchema(t, r.out, "automation.schema.json", "status")

	// The port chosen before is the one used when none is named, and a named one wins.
	d = &fakeDaemon{status: ipc.AutomationStatus{Port: 9000}}
	require.NoError(t, norite(t, d, "", false, "enable").err)
	require.Len(t, d.asked, 2)
	assert.Equal(t, "POST "+ipc.PathAutomationEnable(9000), d.asked[1])
	d = &fakeDaemon{status: ipc.AutomationStatus{Port: 9000}}
	require.NoError(t, norite(t, d, "", false, "enable", "--port", "9100").err)
	require.Len(t, d.asked, 2)
	assert.Equal(t, "POST "+ipc.PathAutomationEnable(9100), d.asked[1])

	old := &fakeDaemon{older: true}
	r = norite(t, old, "", false, "enable", "--port", "9100")
	requireUnavailable(t, r.err, "an older daemon")
	assert.Contains(t, r.err.Error(), "norite daemon restart")
	assert.Equal(t, []string{"GET " + ipc.PathAutomation}, old.asked, "the port number was never sent to it")
}

func TestAPortThatIsNotOneIsAUsageErrorBeforeAnyDaemonIsAsked(t *testing.T) {
	for _, port := range []string{"0", "65536", "-1"} {
		d := &fakeDaemon{}
		r := norite(t, d, "", false, "enable", "--port", port)
		requireUsage(t, r.err, port)
		assert.Empty(t, d.asked)
	}
	for _, verb := range []string{"enable", "disable", "status"} {
		d := &fakeDaemon{}
		requireUsage(t, norite(t, d, "", false, verb, "extra").err, verb)
		assert.Empty(t, d.asked)
	}
}

func TestTheDaemonsRefusalIsARefusalInItsOwnWords(t *testing.T) {
	d := &fakeDaemon{refuse: &ipc.RelayError{Code: ipc.RelayConflict, Message: "something else is listening on 127.0.0.1 port 7717. The port was not turned on"}}
	r := norite(t, d, "", false, "enable")
	var refused *clierr.RefusedError
	require.ErrorAs(t, r.err, &refused)
	assert.Contains(t, r.err.Error(), "port 7717")
	assert.Empty(t, r.out)
}

// TestStatusSaysWhatAPersonNeedsNextAndNeverTheSecret covers the three ways the port can stand, as text
// and as JSON held to the contract.
func TestStatusSaysWhatAPersonNeedsNextAndNeverTheSecret(t *testing.T) {
	for name, tc := range map[string]struct {
		status ipc.AutomationStatus
		want   []string
	}{
		"off": {ipc.AutomationStatus{Port: 7717}, []string{"is off", "norite automation enable", "7717"}},
		"on": {ipc.AutomationStatus{Enabled: true, Port: 7717, Instance: "https://chat.example", Open: true, Address: "127.0.0.1:7717"},
			[]string{"127.0.0.1:7717", "https://chat.example", "norite automation run", EnvToken}},
		"on and not open": {ipc.AutomationStatus{Enabled: true, Port: 7717, Instance: "https://chat.example", Problem: "something else is listening on 127.0.0.1 port 7717"},
			[]string{"not open", "something else is listening"}},
	} {
		text := norite(t, &fakeDaemon{status: tc.status}, "", false, "status")
		require.NoError(t, text.err, name)
		for _, want := range tc.want {
			assert.Contains(t, text.out, want, name)
		}
		js := norite(t, &fakeDaemon{status: tc.status}, "", true, "status")
		require.NoError(t, js.err, name)
		daemontest.MatchesCLISchema(t, js.out, "automation.schema.json", "status")
		assert.NotContains(t, js.out, "secret", name)
	}

	// disable prints the same shape.
	d := &fakeDaemon{status: ipc.AutomationStatus{Enabled: true, Port: 9000, Open: true, Address: "127.0.0.1:9000", Instance: "https://chat.example"}}
	r := norite(t, d, "", false, "disable")
	require.NoError(t, r.err)
	assert.Contains(t, r.out, "is off")
	assert.Contains(t, r.out, "9000")
}

// TestWhatTheDaemonNamesIsDrawnInert: the instance in the state file came from a file a person can edit.
func TestWhatTheDaemonNamesIsDrawnInert(t *testing.T) {
	d := &fakeDaemon{status: ipc.AutomationStatus{Enabled: true, Port: 7717, Open: true, Address: "127.0.0.1:7717",
		Instance: "https://chat.example\x1b[2J\nThe automation port is off."}}
	r := norite(t, d, "", false, "status")
	require.NoError(t, r.err)
	assert.NotContains(t, r.out, "\x1b")
	assert.Equal(t, 2, strings.Count(r.out, "\n"), "a name cannot add a line of its own: %q", r.out)
}

func TestWithNoDaemonTheThreeSayHowToStartOne(t *testing.T) {
	connect := func(context.Context) (daemonclient.Caller, func(), error) {
		return nil, nil, clierr.Unavailable("the daemon is not running; start it with `norite daemon start`")
	}
	root := &cli.Command{Name: "norite", Flags: []cli.Flag{&cli.BoolFlag{Name: "json"}},
		ExitErrHandler: func(context.Context, *cli.Command, error) {}, Commands: []*cli.Command{Command(connect)}}
	err := root.Run(context.Background(), []string{"norite", "automation", "status"})
	requireUnavailable(t, err, "status")
}

// ---------- run ----------

type started struct {
	path string
	argv []string
	env  []string
}

// capture replaces what `run` would start with a record of it, and says where the port is.
func capture(t *testing.T, file ipc.AutomationFile, fileErr error) *started {
	t.Helper()
	got := &started{}
	realReplace, realLocated := replace, located
	replace = func(path string, argv, env []string) error {
		got.path, got.argv, got.env = path, argv, env
		return nil
	}
	located = func() (ipc.AutomationFile, error) { return file, fileErr }
	t.Cleanup(func() { replace, located = realReplace, realLocated })
	return got
}

func envOf(env []string, name string) []string {
	var out []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			out = append(out, v)
		}
	}
	return out
}

// TestRunHandsTheProgramThePortAndPrintsNothing is the "or an environment variable" of the roadmap: the
// address and the secret reach one program through its environment, and nothing else sees them.
func TestRunHandsTheProgramThePortAndPrintsNothing(t *testing.T) {
	// A stale pair in this process's own environment, as inside an outer `run` from before a restart.
	t.Setenv(EnvAddress, "127.0.0.1:1")
	t.Setenv(EnvSecret, "last-runs-secret")
	t.Setenv(EnvToken, botToken)
	self, err := os.Executable()
	require.NoError(t, err)

	got := capture(t, ipc.AutomationFile{Address: "127.0.0.1:7717", Secret: "this-runs-secret"}, nil)
	r := norite(t, nil, "", false, "run", "--", self, "--flag", "value")
	require.NoError(t, r.err)

	assert.Equal(t, self, got.path)
	assert.Equal(t, []string{self, "--flag", "value"}, got.argv, "the program's own flags are its own")
	assert.Equal(t, []string{"127.0.0.1:7717"}, envOf(got.env, EnvAddress), "set once, replacing what was there")
	assert.Equal(t, []string{"this-runs-secret"}, envOf(got.env, EnvSecret))
	assert.Equal(t, []string{botToken}, envOf(got.env, EnvToken), "the token is the caller's and passes through")
	assert.Empty(t, r.out)
	assert.Empty(t, r.errOut)
}

func TestRunStartsNothingItCannot(t *testing.T) {
	got := capture(t, ipc.AutomationFile{}, ipc.ErrAutomationOff)
	self, err := os.Executable()
	require.NoError(t, err)
	r := norite(t, nil, "", false, "run", "--", self)
	requireUnavailable(t, r.err, "the port is off")
	assert.Contains(t, r.err.Error(), "norite automation enable")
	assert.Empty(t, got.path, "a script is not started without the port it was to use")

	got = capture(t, ipc.AutomationFile{Address: "127.0.0.1:7717", Secret: "s3cr3t-value"}, nil)
	requireUsage(t, norite(t, nil, "", false, "run").err, "no program")
	r = norite(t, nil, "", false, "run", "--", filepath.Join(t.TempDir(), "no-such-program"))
	requireUsage(t, r.err, "a program that does not exist")
	assert.NotContains(t, r.err.Error(), "s3cr3t-value")
	assert.Empty(t, got.path)
}

// ---------- request ----------

// fakePort speaks the automation port's protocol, answering each request with what answer returns.
type fakePort struct {
	t      *testing.T
	file   ipc.AutomationFile
	answer func(req ipc.Request) ipc.Response

	mu         sync.Mutex
	identified []ipc.AutomationIdentify
	requests   []ipc.Request
}

func newFakePort(t *testing.T, answer func(ipc.Request) ipc.Response) *fakePort {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	p := &fakePort{t: t, answer: answer, file: ipc.AutomationFile{Address: l.Addr().String(), Secret: "the-port-secret"}}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go p.serve(conn)
		}
	}()
	return p
}

func (p *fakePort) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	first, err := ipc.ReadFrame(conn, ipc.MaxAutomationIdentify)
	if err != nil {
		return
	}
	var id ipc.AutomationIdentify
	if ipc.Decode(first, &id) != nil {
		return
	}
	p.mu.Lock()
	p.identified = append(p.identified, id)
	p.mu.Unlock()
	if id.Secret != p.file.Secret {
		f, _ := ipc.Encode(ipc.OpClose, ipc.Close{Code: ipc.CloseAutomationRefused, Reason: "the automation port did not accept that secret"})
		_ = ipc.WriteFrame(conn, f)
		return
	}
	ready, _ := ipc.Encode(ipc.OpAutomationReady, ipc.AutomationReady{Version: "dev"})
	_ = ipc.WriteFrame(conn, ready)
	for {
		f, err := ipc.ReadFrame(conn, ipc.MaxClientFrame)
		if err != nil {
			return
		}
		var req ipc.Request
		if ipc.Decode(f, &req) != nil {
			return
		}
		p.mu.Lock()
		p.requests = append(p.requests, req)
		p.mu.Unlock()
		resp := p.answer(req)
		resp.ID = req.ID
		out, _ := ipc.Encode(ipc.OpResponse, resp)
		_ = ipc.WriteFrame(conn, out)
	}
}

func (p *fakePort) seen() []ipc.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ipc.Request(nil), p.requests...)
}

func status(code int, body string) func(ipc.Request) ipc.Response {
	return func(ipc.Request) ipc.Response {
		r := ipc.Response{Status: &code}
		if body != "" {
			r.Body = json.RawMessage(body)
		}
		return r
	}
}

// inRun sets the environment `run` gives a program, for this port.
func (p *fakePort) inRun(t *testing.T) {
	t.Helper()
	t.Setenv(EnvAddress, p.file.Address)
	t.Setenv(EnvSecret, p.file.Secret)
	t.Setenv(EnvToken, botToken)
}

// TestARequestGoesThroughThePortAndPrintsTheInstancesAnswer is what the roadmap's shell script runs.
func TestARequestGoesThroughThePortAndPrintsTheInstancesAnswer(t *testing.T) {
	// The escape arrives escaped, as JSON requires; the override arrives as itself, as JSON allows.
	p := newFakePort(t, status(201, "{\"id\":\"30\",\"type\":1,\"content\":\"hi\\u001b[2J\",\"name\":\"a\u202eb\"}"))
	p.inRun(t)

	r := norite(t, nil, "", false, "request", "post", "/channels/20/messages", "--body", ` {"content":"hi"} `)
	require.NoError(t, r.err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.out), &got), "%s", r.out)
	assert.Equal(t, "30", got["id"])
	assert.Equal(t, "hi\x1b[2J", got["content"], "a parser reads the instance's value exactly")
	assert.Equal(t, "a\u202eb", got["name"])
	assert.NotContains(t, r.out, "\x1b", "and a terminal is sent nothing it would act on")
	assert.NotContains(t, r.out, "\u202e", "nor anything that reorders what it shows")

	reqs := p.seen()
	require.Len(t, reqs, 1)
	assert.Equal(t, "POST", reqs[0].Method)
	assert.Equal(t, "/channels/20/messages", reqs[0].Path)
	assert.JSONEq(t, `{"content":"hi"}`, string(reqs[0].Body))
	assert.Equal(t, ipc.AutomationIdentify{Secret: "the-port-secret", Token: botToken}, p.identified[0])
	assert.NotContains(t, r.out+r.errOut, botToken)
	assert.NotContains(t, r.out+r.errOut, "the-port-secret")

	// A body from standard input and from a file, and an answer with no body.
	p.answer = status(204, "")
	r = norite(t, nil, `{"content":"from stdin"}`, false, "request", "PATCH", "/channels/20/messages/30", "--body-file", "-")
	require.NoError(t, r.err)
	assert.Empty(t, r.out)
	file := filepath.Join(t.TempDir(), "body.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"content":"from a file"}`), 0o600))
	require.NoError(t, norite(t, nil, "", false, "request", "POST", "/channels/20/messages", "--body-file", file).err)
	reqs = p.seen()
	assert.JSONEq(t, `{"content":"from stdin"}`, string(reqs[1].Body))
	assert.JSONEq(t, `{"content":"from a file"}`, string(reqs[2].Body))
}

// TestOutsideRunTheDaemonsFileSaysWhereThePortIs: and half of the environment's pair is not used with half
// of the file's.
func TestOutsideRunTheDaemonsFileSaysWhereThePortIs(t *testing.T) {
	p := newFakePort(t, status(200, `{"id":"1"}`))
	t.Setenv(EnvToken, botToken)
	capture(t, p.file, nil)

	require.NoError(t, norite(t, nil, "", false, "request", "GET", "/users/@me").err)
	assert.Equal(t, "the-port-secret", p.identified[0].Secret)

	t.Setenv(EnvSecret, "half-of-another-runs-pair")
	require.NoError(t, norite(t, nil, "", false, "request", "GET", "/users/@me").err)
	assert.Equal(t, "the-port-secret", p.identified[1].Secret)

	capture(t, ipc.AutomationFile{}, ipc.ErrAutomationOff)
	r := norite(t, nil, "", false, "request", "GET", "/users/@me")
	requireUnavailable(t, r.err, "the port is off")
	assert.Contains(t, r.err.Error(), "norite automation enable")
}

// TestEachOutcomeOfARequestHasItsExitCode: refused is 4, worth another try is 3, the instance failing is 1.
func TestEachOutcomeOfARequestHasItsExitCode(t *testing.T) {
	failed := func(code, msg string) func(ipc.Request) ipc.Response {
		return func(ipc.Request) ipc.Response { return ipc.Response{Error: &ipc.RelayError{Code: code, Message: msg}} }
	}
	instanceError := `{"error":{"code":"forbidden","message":"you may not post here","request_id":"req-1"}}`
	for name, tc := range map[string]struct {
		answer func(ipc.Request) ipc.Response
		kind   string
		says   string
	}{
		"403 from the instance":      {status(403, instanceError), "refused", "you may not post here"},
		"404 from the instance":      {status(404, `{"error":{"code":"not_found","message":"not found","request_id":"req-2"}}`), "refused", "req-2"},
		"401: the token":             {status(401, instanceError), "unavailable", EnvToken},
		"429 from the instance":      {status(429, instanceError), "unavailable", "rate-limiting"},
		"500 from the instance":      {status(500, `{"error":{"code":"internal","message":"internal error","request_id":"req-3"}}`), "failure", "req-3"},
		"a path the port refuses":    {failed(ipc.RelayRefused, "the automation port does not carry /auth"), "refused", "does not carry /auth"},
		"the port's own rate":        {failed(ipc.RelayTooManyRequests, "the automation port takes 5 requests a second"), "unavailable", "5 requests a second"},
		"a daemon signed in nowhere": {failed(ipc.RelayNotSignedIn, "the daemon is not signed in to an instance"), "unavailable", "not signed in"},
		"an instance out of reach":   {failed(ipc.RelayUnreachable, "the instance did not answer"), "unavailable", "did not answer"},
	} {
		p := newFakePort(t, tc.answer)
		p.inRun(t)
		r := norite(t, nil, "", false, "request", "POST", "/channels/20/messages", "--body", `{"content":"x"}`)
		require.Error(t, r.err, name)
		var refused *clierr.RefusedError
		var unavailable *clierr.UnavailableError
		var usage *clierr.UsageError
		switch tc.kind {
		case "refused":
			require.ErrorAs(t, r.err, &refused, name)
		case "unavailable":
			require.ErrorAs(t, r.err, &unavailable, name)
		default:
			require.NotErrorAs(t, r.err, &refused, name)
			require.NotErrorAs(t, r.err, &unavailable, name)
			require.NotErrorAs(t, r.err, &usage, name)
		}
		assert.Contains(t, r.err.Error(), tc.says, name)
		assert.Empty(t, r.out, name)
		assert.NotContains(t, r.err.Error(), botToken, name)
	}
}

// TestARequestPrintsNumbersAsTheInstanceWroteThem: an integer above 2^53 is not a float64, and printed
// through one it becomes a neighbor. A script reading the output must get the instance's number.
func TestARequestPrintsNumbersAsTheInstanceWroteThem(t *testing.T) {
	p := newFakePort(t, status(200, `{"count":9007199254740993,"ratio":0.1}`))
	p.inRun(t)
	r := norite(t, nil, "", false, "request", "GET", "/users/@me")
	require.NoError(t, r.err)
	assert.Contains(t, r.out, "9007199254740993")
	assert.Contains(t, r.out, "0.1")
}

// TestAWrongSecretSaysToStartTheScriptAgain: the secret changes with every run of the daemon, so a script
// that outlived one holds a stale pair. The way out is `run`, and the message says so.
func TestAWrongSecretSaysToStartTheScriptAgain(t *testing.T) {
	p := newFakePort(t, status(200, `{}`))
	p.inRun(t)
	t.Setenv(EnvSecret, "a-secret-from-before-the-restart")
	r := norite(t, nil, "", false, "request", "GET", "/users/@me")
	requireUnavailable(t, r.err, "a stale secret")
	assert.Contains(t, r.err.Error(), "norite automation run")
	assert.NotContains(t, r.err.Error(), "a-secret-from-before-the-restart")
	assert.Empty(t, p.seen())
}

// TestWhatARequestRefusesBeforeItConnects: each is a usage error, and the port hears nothing.
func TestWhatARequestRefusesBeforeItConnects(t *testing.T) {
	p := newFakePort(t, status(200, `{}`))
	bigFile := filepath.Join(t.TempDir(), "big.json")
	require.NoError(t, os.WriteFile(bigFile, []byte(`"`+strings.Repeat("a", maxBody)+`"`), 0o600))

	for name, tc := range map[string]struct {
		token string
		args  []string
	}{
		"no token":                 {"", []string{"GET", "/users/@me"}},
		"an access token":          {"eyJhbGciOiJIUzI1NiJ9.e30.c2ln", []string{"GET", "/users/@me"}},
		"a method that is not one": {botToken, []string{"TRACE", "/users/@me"}},
		"a path that is a URL":     {botToken, []string{"GET", "https://evil.example/x"}},
		"one argument":             {botToken, []string{"GET"}},
		"three arguments":          {botToken, []string{"GET", "/users/@me", "extra"}},
		"a body that is not JSON":  {botToken, []string{"POST", "/x", "--body", "content=hi"}},
		"two bodies":               {botToken, []string{"POST", "/x", "--body", "{}", "--body-file", "-"}},
		"a body file that is not":  {botToken, []string{"POST", "/x", "--body-file", t.TempDir()}},
		"a body file too large":    {botToken, []string{"POST", "/x", "--body-file", bigFile}},
	} {
		p.inRun(t)
		t.Setenv(EnvToken, tc.token)
		r := norite(t, nil, "", false, append([]string{"request"}, tc.args...)...)
		requireUsage(t, r.err, name)
		assert.NotContains(t, r.err.Error(), "eyJ", "an error never repeats a credential: %s", name)
		if tc.token == "" {
			// Its own sentence: "that is not a token" would send somebody looking at a value they never set.
			assert.Contains(t, r.err.Error(), "set "+EnvToken, name)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	assert.Empty(t, p.identified, "nothing was presented to the port")
}
