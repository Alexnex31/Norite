// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// ---------- the boundary ----------

func TestTargetRefusesWhatCouldLeaveTheInstance(t *testing.T) {
	for _, path := range []string{
		"",
		"guilds/1",
		"//evil.example/guilds",
		"https://evil.example/guilds",
		"/guilds/../auth/tokens",
		"/guilds/./1",
		"/guilds/%2e%2e/auth/tokens",
		"/guilds%2F..%2Fauth",
		`/guilds\1`,
		"/guilds/1#frag",
		"/" + strings.Repeat("a", 2048),
		"/auth/login",
		"/AUTH/login",
		"/auth",
		"/instance/invites",
		"/users/@me/sessions",
		"/users/@me/sessions/123",
		"/users//@me/sessions",
		"/guilds/1/",
		"/guilds/1\x00",
	} {
		_, err := Target(path)
		assert.Error(t, err, "%q must be refused", path)
	}

	for path, want := range map[string]string{
		"/guilds/1":                     "/guilds/1",
		"/guilds/1/members?limit=50":    "/guilds/1/members",
		"/users/@me/guilds":             "/users/@me/guilds",
		"/authors/1":                    "/authors/1", // a prefix of a refused name is not the name
		"/channels/2/messages?before=9": "/channels/2/messages",
	} {
		u, err := Target(path)
		require.NoError(t, err, path)
		assert.Equal(t, want, u.Path)
	}
}

func TestBuildKeepsTheInstancesPathPrefix(t *testing.T) {
	target, err := Target("/guilds/1/members?limit=5")
	require.NoError(t, err)

	u, err := Build("https://example.com/norite", target)
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/norite/api/v1/guilds/1/members?limit=5", u.String())

	u, err = Build("https://chat.example", target)
	require.NoError(t, err)
	assert.Equal(t, "https://chat.example/api/v1/guilds/1/members?limit=5", u.String())

	_, err = Build("file:///etc/passwd", target)
	assert.Error(t, err)
}

// TestEveryContractPathIsDecided walks openapi.yaml and requires an explicit decision for every path, so a
// route added to the contract is relayed or refused because somebody said so — never by default. It fails
// in both directions: a path with no decision here, and a decision for a path the contract no longer has.
// The daemon's own requests live under a first path segment the instance's API does not have. The relay
// refuses it, so a request for the daemon that reached the relay is not told to the instance; and no REST
// route may ever be given it, or a client asking the instance for that route would be answered by the
// daemon instead.
func TestTheDaemonsOwnPathsAreNeitherRelayedNorInTheContract(t *testing.T) {
	for _, path := range []string{
		ipc.PathConfigSplit, ipc.PathConfigUnsplit, "/@daemon", "/@daemon/anything", "/@DAEMON/config/split",
		ipc.PathUpdate, ipc.PathUpdateCheck, ipc.PathUpdateApply, ipc.PathUpdateMode + "auto", ipc.PathUpdateChannel + "stable",
	} {
		_, err := Target(path)
		require.Error(t, err, path)
		assert.Contains(t, err.Error(), "the daemon's own", path)
	}
	_, err := Target("/users/@me")
	require.NoError(t, err, "an @ elsewhere in a path is the API's own")

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "openapi.yaml"))
	require.NoError(t, err)
	paths := regexp.MustCompile(`(?m)^  (/[^:]*):\s*$`).FindAllStringSubmatch(string(raw), -1)
	require.NotEmpty(t, paths)
	for _, m := range paths {
		assert.False(t, strings.HasPrefix(strings.ToLower(m[1])+"/", ipc.LocalPathPrefix),
			"%s is under the prefix the daemon answers itself", m[1])
	}
}

func TestEveryContractPathIsDecided(t *testing.T) {
	const (
		relayed = "relayed"
		refused = "refused"
		// notAPI is a route outside /api/v1 — a page or the gateway — which the relay cannot reach whatever
		// it decides, since it puts /api/v1 in front of every path.
		notAPI = "not under /api/v1"
	)
	decisions := map[string]string{
		"/gateway": notAPI, "/verify": notAPI, "/oauth/continue": notAPI, "/reset": notAPI,
		"/oauth/signup": notAPI, "/device": notAPI, "/device/signin": notAPI, "/device/2fa": notAPI,
		"/device/approve": notAPI, "/join": notAPI,

		"/meta": relayed, "/healthz": relayed,

		"/auth/register": refused, "/auth/login": refused, "/auth/refresh": refused, "/auth/logout": refused,
		"/auth/verify/request": refused, "/auth/password/reset/request": refused,
		"/auth/password/reset": refused, "/auth/oauth/{provider}/authorize": refused,
		"/auth/oauth/{provider}/callback": refused, "/auth/oauth/exchange": refused,
		"/auth/oauth/complete": refused, "/auth/device/code": refused, "/auth/device/token": refused,
		"/auth/2fa/verify": refused, "/auth/2fa/totp": refused, "/auth/2fa/totp/confirm": refused,
		"/auth/2fa/recovery-codes": refused, "/auth/logout/all": refused,
		// M22. The one exception under /auth: see the package comment, and the test below this one.
		"/auth/tokens": relayed, "/auth/tokens/{tokenId}": relayed,
		"/instance/bootstrap": refused, "/instance/invites": refused, "/instance/invites/revoke": refused,
		"/users/@me/sessions": refused, "/users/@me/sessions/{sessionID}": refused,

		"/users/@me": relayed, "/users/@me/guilds": relayed,
		"/guilds": relayed, "/guilds/{guild_id}": relayed, "/guilds/{guild_id}/owner": relayed,
		"/guilds/{guild_id}/channels": relayed, "/channels/{channel_id}": relayed,
		"/guilds/{guild_id}/members/{user_id}/roles/{role_id}": relayed,
		"/channels/{channel_id}/permissions/{overwrite_id}":    relayed,
		"/guilds/{guild_id}/roles":                             relayed, "/guilds/{guild_id}/roles/{role_id}": relayed,
		"/channels/{channel_id}/messages": relayed, "/channels/{channel_id}/messages/{message_id}": relayed,
		"/channels/{channel_id}/messages/{message_id}/history": relayed,
		"/guilds/{guild_id}/message-audit":                     relayed,
		"/guilds/{guild_id}/tags":                              relayed, "/guilds/{guild_id}/tags/{tag_id}": relayed,
		"/channels/{channel_id}/messages/{message_id}/tags":          relayed,
		"/channels/{channel_id}/messages/{message_id}/tags/{tag_id}": relayed,
		"/guilds/{guild_id}/audit-log":                               relayed, "/reports": relayed,
		"/guilds/{guild_id}/reports": relayed, "/guilds/{guild_id}/reports/{report_id}": relayed,
		"/guilds/{guild_id}/reports/{report_id}/resolve": relayed,

		// M20a. Guild invites are the account acting in guilds like every route above, and joining one is
		// the client's whole reason to reach them; none manages a credential.
		"/guilds/{guild_id}/invites": relayed, "/channels/{channel_id}/invites": relayed,
		"/invites/preview": relayed, "/invites/redeem": relayed, "/invites/revoke": relayed,
		"/guilds/{guild_id}/members": relayed, "/guilds/{guild_id}/members/{user_id}": relayed,
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "openapi.yaml"))
	require.NoError(t, err)
	paths := regexp.MustCompile(`(?m)^  (/[^:]*):\s*$`).FindAllStringSubmatch(string(raw), -1)
	require.NotEmpty(t, paths)
	param := regexp.MustCompile(`\{[^}]+\}`)

	seen := map[string]bool{}
	for _, m := range paths {
		path := m[1]
		seen[path] = true
		want, ok := decisions[path]
		if !ok {
			t.Errorf("the contract has %s and the relay has no decision for it: relay it or refuse it, here "+
				"and in relay.refused", path)
			continue
		}
		if want == notAPI {
			continue
		}
		_, err := Target(param.ReplaceAllString(path, "1"))
		assert.Equal(t, want == refused, err != nil, "%s should be %s", path, want)
	}
	for path := range decisions {
		if !seen[path] {
			t.Errorf("a decision for %s, which the contract no longer has", path)
		}
	}
}

// TestOnlyTheTokenRoutesAreRelayedUnderAuth holds M22's exception to its two shapes.
//
// The relay refuses /auth because those routes make and unmake credentials. Minting an API token had to
// become reachable, and an exception written as a prefix would bring whatever is later mounted beside it.
// So it is the collection and one id of digits, on the path as written.
func TestOnlyTheTokenRoutesAreRelayedUnderAuth(t *testing.T) {
	for _, path := range []string{"/auth/tokens", "/auth/tokens/1234567890", "/auth/tokens?limit=5"} {
		_, err := Target(path)
		assert.NoError(t, err, "%q is one of the token routes", path)
	}
	for _, path := range []string{
		"/auth/tokens/",                      // an empty segment
		"/auth/tokens/abc",                   // not an id
		"/auth/tokens/1/rotate",              // nothing below a token
		"/auth/tokens/123456789012345678901", // longer than an id is
		"/AUTH/tokens",                       // the exception does not fold case, though the refusal does
		"/auth/Tokens/1",
		"/auth/tokensx",
		"/auth/login", "/auth/logout", "/auth/logout/all", "/auth/refresh", "/auth/2fa/totp",
	} {
		_, err := Target(path)
		assert.Error(t, err, "%q must stay refused", path)
	}
}

// TestAScriptReachesWhatTheRelayDoesLessAllOfAuth: the automation port's paths (M22). The token routes the
// relay opened for `norite token` are not opened to a script, which uses a credential and manages none.
func TestAScriptReachesWhatTheRelayDoesLessAllOfAuth(t *testing.T) {
	for _, path := range []string{"/users/@me", "/channels/2/messages?before=9", "/guilds/1/members", "/authors/1"} {
		_, err := ScriptTarget(path)
		assert.NoError(t, err, path)
	}
	for _, path := range []string{
		"/auth/tokens", "/auth/tokens/1", "/AUTH/tokens", "/auth", "/auth/login",
		"/instance/invites", "/users/@me/sessions", "/@daemon/config", "//host/x", "/guilds/../auth/tokens",
	} {
		_, err := ScriptTarget(path)
		assert.Error(t, err, "%q must be refused to a script", path)
	}

	// Each surface is told about itself: a script is not on the attach socket (M22 manual pass).
	_, err := ScriptTarget("/auth/login")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the automation port does not carry /auth")
	assert.NotContains(t, err.Error(), "attach socket")
	_, err = Target("/auth/login")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attach socket")
}

// ---------- performing a request ----------

type fakeCreds struct {
	mu       sync.Mutex
	standing session.Standing
	cred     session.Credential
	rejected []string
	onReject func(*fakeCreds)
	noAnswer bool
	// signingIn, when set, holds CurrentUnlessSignedOut until it is closed, as a daemon still starting does.
	signingIn chan struct{}
}

func (f *fakeCreds) Status() (session.Standing, session.Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.standing, session.Account{InstanceURL: f.cred.InstanceURL, UserID: f.cred.UserID}
}

func (f *fakeCreds) CurrentUnlessSignedOut(ctx context.Context) (session.Credential, error) {
	f.mu.Lock()
	signingIn := f.signingIn
	f.mu.Unlock()
	if signingIn != nil {
		select {
		case <-signingIn:
		case <-ctx.Done():
			return session.Credential{}, ctx.Err()
		}
	}
	f.mu.Lock()
	noAnswer, cred, standing := f.noAnswer, f.cred, f.standing
	f.mu.Unlock()
	if standing == session.SignedOut {
		return session.Credential{}, session.ErrSignedOut
	}
	if noAnswer {
		<-ctx.Done()
		return session.Credential{}, ctx.Err()
	}
	return cred, nil
}

func (f *fakeCreds) Rejected(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, token)
	if f.onReject != nil {
		f.onReject(f)
	}
}

type seen struct {
	method, path, query, auth, contentType string
	body                                   string
}

func instance(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, func() []seen) {
	t.Helper()
	var mu sync.Mutex
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"),
			r.Header.Get("Content-Type"), string(body)})
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seen { mu.Lock(); defer mu.Unlock(); return append([]seen(nil), got...) }
}

func live(instanceURL, token string, gen uint64) *fakeCreds {
	return &fakeCreds{standing: session.Live, cred: session.Credential{
		InstanceURL: instanceURL, UserID: "1", AccessToken: token, Generation: gen,
	}}
}

func newRelay(creds Credentials, srv *httptest.Server) *Relay {
	return New(Options{Credentials: creds, HTTP: srv.Client(), Version: "0.1.0", Log: zerolog.Nop()})
}

func do(r *Relay, method, path, body string) ipc.Response {
	req := ipc.Request{ID: "1", Method: method, Path: path}
	if body != "" {
		req.Body = json.RawMessage(body)
	}
	return r.Do(context.Background(), req)
}

func TestARequestIsMadeWithTheDaemonsTokenAndAnsweredAsIs(t *testing.T) {
	srv, requests := instance(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"5"}`)
	})
	r := newRelay(live(srv.URL+"/norite", "eyJ.access.1", 1), srv)

	resp := do(r, "POST", "/guilds/1/channels?x=1", `{"name":"general"}`)
	require.Nil(t, resp.Error)
	assert.Equal(t, http.StatusCreated, *resp.Status)
	assert.JSONEq(t, `{"id":"5"}`, string(resp.Body))

	got := requests()
	require.Len(t, got, 1)
	assert.Equal(t, seen{"POST", "/norite/api/v1/guilds/1/channels", "x=1", "Bearer eyJ.access.1",
		"application/json", `{"name":"general"}`}, got[0])
}

func TestABodylessRequestSendsNoBody(t *testing.T) {
	srv, requests := instance(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r := newRelay(live(srv.URL, "eyJ.access.1", 1), srv)

	resp := do(r, "DELETE", "/guilds/1", "null")
	require.Nil(t, resp.Error)
	assert.Equal(t, http.StatusNoContent, *resp.Status)
	assert.Nil(t, resp.Body)
	assert.Equal(t, "", requests()[0].body)
	assert.Equal(t, "", requests()[0].contentType)
}

func TestARefusalIsAnAnswerAndANonJSONBodyIsDropped(t *testing.T) {
	status := http.StatusNotFound
	body := `{"code":"not_found","message":"nope","request_id":"r"}`
	srv, _ := instance(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	r := newRelay(live(srv.URL, "eyJ.access.1", 1), srv)

	resp := do(r, "GET", "/guilds/1", "")
	require.Nil(t, resp.Error, "a 404 is the instance's answer")
	assert.Equal(t, http.StatusNotFound, *resp.Status)
	assert.JSONEq(t, body, string(resp.Body))

	status, body = http.StatusBadGateway, "<html>bad gateway</html>"
	resp = do(r, "GET", "/guilds/1", "")
	assert.Equal(t, http.StatusBadGateway, *resp.Status)
	assert.Nil(t, resp.Body, "a proxy's HTML is not passed on")
}

func TestARedirectIsReturnedNotFollowed(t *testing.T) {
	srv, requests := instance(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v1/auth/tokens", http.StatusTemporaryRedirect)
	})
	// The relay's own client, not the test server's, which follows redirects.
	r := New(Options{Credentials: live(srv.URL, "eyJ.access.1", 1), Log: zerolog.Nop()})

	resp := do(r, "POST", "/guilds/1/channels", `{}`)
	require.Nil(t, resp.Error)
	assert.Equal(t, http.StatusTemporaryRedirect, *resp.Status)
	assert.Len(t, requests(), 1, "the token went to the path the relay checked, and nowhere else")
}

func TestASignedOutDaemonRefusesAtOnce(t *testing.T) {
	creds := &fakeCreds{standing: session.SignedOut, noAnswer: true}
	r := New(Options{Credentials: creds, Log: zerolog.Nop()})

	start := time.Now()
	resp := do(r, "GET", "/guilds/1", "")
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayNotSignedIn, resp.Error.Code)
	assert.Less(t, time.Since(start), time.Second, "not waiting for a login")
}

func TestARefusedPathReachesNothing(t *testing.T) {
	srv, requests := instance(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r := newRelay(live(srv.URL, "eyJ.access.1", 1), srv)

	resp := do(r, "POST", "/auth/logout/all", `{}`)
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayRefused, resp.Error.Code)
	assert.Empty(t, requests())
}

// TestAnExpiredTokenIsRenewedByTheSessionAndRetriedOnce: the 401 goes to the session, which is the one
// owner of the refresh token, and the relay retries with what it hands back.
func TestAnExpiredTokenIsRenewedByTheSessionAndRetriedOnce(t *testing.T) {
	srv, requests := instance(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer eyJ.access.2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	creds := live(srv.URL, "eyJ.access.1", 1)
	creds.onReject = func(f *fakeCreds) { f.cred.AccessToken = "eyJ.access.2" }
	r := newRelay(creds, srv)

	resp := do(r, "GET", "/guilds/1", "")
	require.Nil(t, resp.Error)
	assert.Equal(t, http.StatusOK, *resp.Status)
	assert.Equal(t, []string{"eyJ.access.1"}, creds.rejected)
	assert.Len(t, requests(), 2)

	// Still refused after renewal: the second 401 is the answer, not a reason to go round again.
	creds.mu.Lock()
	creds.cred.AccessToken = "eyJ.access.9"
	creds.onReject = func(f *fakeCreds) { f.cred.AccessToken = "eyJ.access.3" }
	creds.mu.Unlock()
	resp = do(r, "GET", "/guilds/1", "")
	require.Nil(t, resp.Error)
	assert.Equal(t, http.StatusUnauthorized, *resp.Status)
	assert.Len(t, requests(), 4)
}

// TestARetryIsNeverMadeAsAnotherSignIn: a login between the attempts changes the generation, and a call made
// as one sign-in is not repeated as another.
func TestARetryIsNeverMadeAsAnotherSignIn(t *testing.T) {
	srv, requests := instance(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	creds := live(srv.URL, "eyJ.access.1", 1)
	creds.onReject = func(f *fakeCreds) { f.cred.AccessToken, f.cred.Generation = "eyJ.other", 2 }
	r := newRelay(creds, srv)

	resp := do(r, "DELETE", "/guilds/1", "")
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayNotSignedIn, resp.Error.Code)
	assert.Len(t, requests(), 1)
}

func TestAnAnswerTooLargeIsRefused(t *testing.T) {
	srv, _ := instance(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `"`+strings.Repeat("x", ipc.MaxResponseBody)+`"`)
	})
	r := newRelay(live(srv.URL, "eyJ.access.1", 1), srv)
	resp := do(r, "GET", "/guilds/1", "")
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayTooLarge, resp.Error.Code)
}

// TestARequestNamingAnInstanceGoesThereOrNowhere: an invite link's code is sent only to the instance the
// link names (easier-setup). The same instance written another way is that instance; another is refused,
// naming both, with nothing sent. Every answer names the instance that gave it.
func TestARequestNamingAnInstanceGoesThereOrNowhere(t *testing.T) {
	srv, requests := instance(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	r := newRelay(live(srv.URL+"/norite", "eyJ.access.1", 1), srv)

	for _, named := range []string{"", srv.URL + "/norite", strings.ToUpper(srv.URL[:4]) + srv.URL[4:] + "/norite/"} {
		req := ipc.Request{ID: "1", Method: "POST", Path: "/invites/preview", Body: json.RawMessage(`{"code":"X"}`),
			InstanceURL: named}
		resp := r.Do(context.Background(), req)
		require.Nil(t, resp.Error, named)
		assert.Equal(t, srv.URL+"/norite", resp.InstanceURL, "the answer names the instance that gave it")
	}
	require.Len(t, requests(), 3)

	for _, other := range []string{"https://other.example", srv.URL, srv.URL + "/elsewhere"} {
		resp := r.Do(context.Background(), ipc.Request{ID: "1", Method: "POST", Path: "/invites/redeem",
			Body: json.RawMessage(`{"code":"X"}`), InstanceURL: other})
		require.NotNil(t, resp.Error, other)
		assert.Equal(t, ipc.RelayOtherInstance, resp.Error.Code, other)
		assert.Equal(t, "that is for "+other+", and this machine is signed in to "+srv.URL+"/norite, so nothing "+
			"was sent", resp.Error.Message)
		assert.Empty(t, resp.InstanceURL)
	}
	assert.Len(t, requests(), 3, "nothing reached the instance for a request naming another")

	// The bound counts characters, as the contract's maxLength does: an address in another script, within
	// it in characters and past it in bytes, is checked as any other, and one past it is refused unsent.
	within := "https://" + strings.Repeat("é", ipc.MaxInstanceURL-len("https://"))
	resp := r.Do(context.Background(), ipc.Request{ID: "1", Method: "GET", Path: "/guilds/1", InstanceURL: within})
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayOtherInstance, resp.Error.Code)
	resp = r.Do(context.Background(), ipc.Request{ID: "1", Method: "GET", Path: "/guilds/1", InstanceURL: within + "é"})
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayBadRequest, resp.Error.Code)
	assert.Equal(t, "the instance named is longer than 2048 characters", resp.Error.Message)
	assert.Len(t, requests(), 3)
}

// TestTheInstanceIsCheckedAgainstTheSignInTheRequestIsSentAs: a daemon still starting is waited for, as for
// any request, and the instance is checked against the sign-in it then has. Checking a READY taken before
// would refuse a good link for a daemon that had not named its instance yet, and pass one for an instance
// the daemon was no longer signed in to.
func TestTheInstanceIsCheckedAgainstTheSignInTheRequestIsSentAs(t *testing.T) {
	srv, requests := instance(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	for _, tc := range []struct {
		signedInTo string
		want       string
	}{
		{srv.URL, ""},
		{"https://other.example", ipc.RelayOtherInstance},
	} {
		creds := &fakeCreds{standing: session.Starting, signingIn: make(chan struct{})}
		r := newRelay(creds, srv)
		answered := make(chan ipc.Response, 1)
		go func() {
			answered <- r.Do(context.Background(), ipc.Request{ID: "1", Method: "POST", Path: "/invites/preview",
				Body: json.RawMessage(`{"code":"X"}`), InstanceURL: srv.URL})
		}()
		select {
		case <-answered:
			t.Fatal("answered before the daemon had a sign-in to check against")
		case <-time.After(50 * time.Millisecond):
		}
		creds.mu.Lock()
		creds.standing = session.Live
		creds.cred = session.Credential{InstanceURL: tc.signedInTo, UserID: "1", AccessToken: "eyJ.a", Generation: 1}
		creds.mu.Unlock()
		close(creds.signingIn)

		var resp ipc.Response
		select {
		case resp = <-answered:
		case <-time.After(5 * time.Second):
			t.Fatal("the request was not answered once the daemon signed in")
		}
		if tc.want == "" {
			require.Nil(t, resp.Error)
			assert.Equal(t, srv.URL, resp.InstanceURL)
		} else {
			require.NotNil(t, resp.Error)
			assert.Equal(t, tc.want, resp.Error.Code)
		}
	}
	assert.Len(t, requests(), 1)

	// Signed out is said as signed out, whatever the request names: the way on is `norite login`.
	out := &fakeCreds{standing: session.SignedOut}
	resp := New(Options{Credentials: out, Log: zerolog.Nop()}).Do(context.Background(),
		ipc.Request{ID: "1", Method: "GET", Path: "/guilds/1", InstanceURL: srv.URL})
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayNotSignedIn, resp.Error.Code)
}

func TestAnUnreachableInstanceSaysSoWithoutTheToken(t *testing.T) {
	srv, _ := instance(t, func(http.ResponseWriter, *http.Request) {})
	url := srv.URL
	srv.Close()
	r := New(Options{Credentials: live(url, "eyJ.secret.token", 1), Log: zerolog.Nop()})

	resp := do(r, "GET", "/guilds/1", "")
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayUnreachable, resp.Error.Code)
	assert.NotContains(t, resp.Error.Message, "eyJ")
}
