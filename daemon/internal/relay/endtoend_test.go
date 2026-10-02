// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/internal/attach"
	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/internal/state"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// standIn is an instance that does what M4's reuse detection does: a refresh token presented a second time
// revokes the device's sign-in, after which no refresh and no access token works.
type standIn struct {
	srv *httptest.Server

	mu        sync.Mutex
	current   string          // the one refresh token that may be presented
	spent     map[string]bool // every refresh token already rotated
	valid     map[string]bool // access tokens the API accepts
	revoked   bool
	refreshes int
}

func newStandIn(t *testing.T) *standIn {
	t.Helper()
	s := &standIn{current: "nrt_0", spent: map[string]bool{}, valid: map[string]bool{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			var body struct {
				RefreshToken string `json:"refresh_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.spent[body.RefreshToken] {
				s.revoked = true // replay: the device's whole sign-in goes
			}
			if s.revoked || body.RefreshToken != s.current {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			s.spent[body.RefreshToken] = true
			s.refreshes++
			access := fmt.Sprintf("eyJ.access.%d", s.refreshes)
			s.current = fmt.Sprintf("nrt_%d", s.refreshes)
			s.valid[access] = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": access, "refresh_token": s.current, "token_type": "Bearer",
				"expires_at": time.Now().Add(15 * time.Minute).Format(time.RFC3339Nano),
			})
		case "/api/v1/guilds/1":
			s.mu.Lock()
			ok := !s.revoked && s.valid[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
			s.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"id":"1","name":"Guild"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// expire makes the instance refuse every access token issued so far, as their expiry would.
func (s *standIn) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.valid = map[string]bool{}
}

func (s *standIn) refresh(token string) int {
	body, _ := json.Marshal(map[string]string{"refresh_token": token})
	resp, err := http.Post(s.srv.URL+"/api/v1/auth/refresh", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestTheStandInRevokesOnReuse checks the fixture fails the way the real instance does before trusting what
// it measures: M19's lesson, after a proxy that passed a FIN through while "dead".
func TestTheStandInRevokesOnReuse(t *testing.T) {
	s := newStandIn(t)
	require.Equal(t, http.StatusOK, s.refresh("nrt_0"))
	require.Equal(t, http.StatusUnauthorized, s.refresh("nrt_0"), "the replay is refused")
	assert.Equal(t, http.StatusUnauthorized, s.refresh("nrt_1"), "and took the live token with it")
	s.mu.Lock()
	assert.True(t, s.revoked)
	s.mu.Unlock()
}

// jumpClock is the real clock, moved forward on demand. The session will not refresh twice within ten
// seconds, and the test needs its renewal to be due now rather than wait that out in real time.
type jumpClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *jumpClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *jumpClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (c *jumpClock) jump(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// recordingConn keeps every byte that crosses the socket, both ways.
type recordingConn struct {
	net.Conn
	mu   sync.Mutex
	seen bytes.Buffer
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.seen.Write(p[:n])
	c.mu.Unlock()
	return n, err
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.seen.Write(p)
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *recordingConn) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen.String()
}

// M20's third done-when: an attach client's request is performed with the daemon's credential and answered,
// no token crosses the socket, and two verbs run concurrently leave the account signed in.
//
// The two verbs are fired at the moment the instance stops accepting the access token, so both are refused,
// both report it, and the session has to renew under both of them. Two presenters of the refresh token would
// be read by the stand-in as theft and the device signed out; the session's single owner makes it one.
func TestTwoVerbsAtOnceLeaveTheAccountSignedInAndNoTokenCrossesTheSocket(t *testing.T) {
	inst := newStandIn(t)
	store, err := credentials.OpenLocalForTest(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Save(credentials.Record{
		InstanceURL: inst.srv.URL, UserID: "1", Username: "ada", DeviceID: "dev_test", DeviceName: "laptop",
	}, "nrt_0"))

	dir, err := os.MkdirTemp("", "nrel")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := attach.Listen(dir)
	require.NoError(t, err)

	log := zerolog.Nop()
	clock := &jumpClock{}
	src := session.New(session.Options{Store: store, HTTP: inst.srv.Client(), Log: log, Clock: clock})
	srv := attach.New(attach.Options{
		Session: src, State: state.New(log, state.DefaultLimits),
		Relay: New(Options{Credentials: src, Log: log}), Version: "dev", Log: log,
	})
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { src.Run(ctx) })
	wg.Go(func() { srv.Serve(ctx, l) })
	t.Cleanup(func() { cancel(); wg.Wait() })

	require.Eventually(t, func() bool { s, _ := src.Status(); return s == session.Live },
		5*time.Second, 5*time.Millisecond)
	clock.jump(11 * time.Second)
	inst.expire()

	attachOne := func() (*ipc.Client, *recordingConn) {
		actx, acancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer acancel()
		nc, err := ipc.DialAt(actx, ipc.SocketPath(dir))
		require.NoError(t, err)
		rec := &recordingConn{Conn: nc}
		c, err := ipc.Attach(actx, rec, ipc.Options{Client: "norite-test", Version: "dev"})
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		return c, rec
	}
	first, firstWire := attachOne()
	second, secondWire := attachOne()

	type answer struct {
		res ipc.Result
		err error
	}
	answers := make(chan answer, 2)
	start := make(chan struct{})
	for _, c := range []*ipc.Client{first, second} {
		go func() {
			<-start
			rctx, rcancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer rcancel()
			res, err := c.Do(rctx, "GET", "/guilds/1", nil)
			answers <- answer{res, err}
		}()
	}
	close(start)
	for range 2 {
		a := <-answers
		require.NoError(t, a.err)
		assert.Equal(t, http.StatusOK, a.res.Status)
		assert.JSONEq(t, `{"id":"1","name":"Guild"}`, string(a.res.Body))
	}

	inst.mu.Lock()
	revoked, refreshes := inst.revoked, inst.refreshes
	inst.mu.Unlock()
	assert.False(t, revoked, "the instance saw a refresh token presented twice and signed the device out")
	assert.Equal(t, 2, refreshes, "one sign-in and one renewal, however many verbs were waiting on it")
	standing, _ := src.Status()
	assert.Equal(t, session.Live, standing)

	for _, wire := range []*recordingConn{firstWire, secondWire} {
		got := wire.String()
		require.Contains(t, got, `"name":"Guild"`, "the recording saw the answer, so it saw the traffic")
		assert.NotContains(t, got, "eyJ", "an access token crossed the socket")
		assert.NotContains(t, got, "nrt_", "a refresh token crossed the socket")
	}
}
