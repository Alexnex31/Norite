// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const exampleToken = "nat_EXAMPLEexampleEXAMPLEexampleEXAMPLEexample0"

func TestOnlyTheLoopbackLiteralIsAnAutomationAddress(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:7717", "127.0.0.1:1", "127.0.0.1:65535"} {
		assert.NoError(t, CheckAutomationAddress(ok), ok)
	}
	for _, bad := range []string{
		"", "127.0.0.1", "127.0.0.1:", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:http",
		"localhost:7717", // a name, which something else resolves
		"127.0.0.2:7717", // loopback, and not the rule
		"[::1]:7717", "0.0.0.0:7717", ":7717", "192.168.1.10:7717", "evil.example:7717",
		"127.0.0.1.evil.example:7717", "127.0.0.1:7717/x", "127.0.0.1:7717 ",
	} {
		assert.Error(t, CheckAutomationAddress(bad), "%q", bad)
	}
}

func TestWhatLooksLikeAnAPIToken(t *testing.T) {
	assert.True(t, LooksLikeAPIToken(exampleToken))
	assert.True(t, LooksLikeAPIToken("nat_a-b_C9"))
	for _, bad := range []string{
		"", "nat_", "nat", "NAT_abc", "nrt_abc", "eyJhbGciOiJIUzI1NiJ9.e30.c2ln", "Bearer " + exampleToken,
		exampleToken + "\n", exampleToken + "\r\nX: 1", "nat_a b", "nat_a.b", "nat_é", " " + exampleToken,
		"nat_" + strings.Repeat("a", maxAutomationToken),
	} {
		assert.False(t, LooksLikeAPIToken(bad), "%q", bad)
	}
}

func writeAutomationFile(t *testing.T, dir, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(AutomationFilePath(dir), []byte(body), 0o600))
}

func TestLoadingTheAutomationFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadAutomationFile(dir)
	require.ErrorIs(t, err, ErrAutomationOff, "no file is a port that is not open")

	writeAutomationFile(t, dir, `{"address":"127.0.0.1:7717","secret":"s3cret"}`)
	f, err := LoadAutomationFile(dir)
	require.NoError(t, err)
	assert.Equal(t, AutomationFile{Address: "127.0.0.1:7717", Secret: "s3cret"}, f)

	for name, body := range map[string]string{
		"an address on another host": `{"address":"203.0.113.9:7717","secret":"s3cret"}`,
		"a name for an address":      `{"address":"localhost:7717","secret":"s3cret"}`,
		"no secret":                  `{"address":"127.0.0.1:7717","secret":""}`,
		"a secret past the bound":    `{"address":"127.0.0.1:7717","secret":"` + strings.Repeat("a", maxAutomationSecret+1) + `"}`,
		"not JSON":                   `address = "127.0.0.1:7717"`,
		"a file past the bound":      `{"address":"127.0.0.1:7717","secret":"s","pad":"` + strings.Repeat("a", maxAutomationFile) + `"}`,
	} {
		writeAutomationFile(t, dir, body)
		_, err := LoadAutomationFile(dir)
		require.Error(t, err, name)
		assert.NotErrorIs(t, err, ErrAutomationOff, name)
		assert.NotContains(t, err.Error(), "s3cret", "an error never repeats the secret")
	}

	// A directory where the file goes is not the file.
	require.NoError(t, os.Remove(AutomationFilePath(dir)))
	require.NoError(t, os.Mkdir(AutomationFilePath(dir), 0o700))
	_, err = LoadAutomationFile(dir)
	require.Error(t, err)
}

// TestDialingSendsTheSecretsToThisMachineOrToNobody: both checks happen before a connection exists. The
// listener stands where a tampered file would point, and must hear nothing.
func TestDialingSendsTheSecretsToThisMachineOrToNobody(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	heard := make(chan struct{}, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			heard <- struct{}{}
			_ = c.Close()
		}
	}()
	_, port, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)

	_, err = DialAutomation(context.Background(), AutomationFile{Address: "localhost:" + port, Secret: "s"}, exampleToken)
	require.Error(t, err)
	_, err = DialAutomation(context.Background(), AutomationFile{Address: "127.0.0.1:" + port, Secret: "s"},
		"eyJhbGciOiJIUzI1NiJ9.e30.c2ln")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an API token")

	select {
	case <-heard:
		t.Fatal("a connection was made before the address and the token were checked")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestDialingAPortNothingAnswersOnIsThePortBeingOff(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	_, err = DialAutomation(context.Background(), AutomationFile{Address: addr, Secret: "s"}, exampleToken)
	require.ErrorIs(t, err, ErrAutomationOff)
}

// fakePort answers one script the way the daemon does, with each request handed to answer.
func fakePort(t *testing.T, answer func(conn net.Conn, req Request)) AutomationFile {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				if _, err := ReadFrame(conn, MaxAutomationIdentify); err != nil {
					return
				}
				ready, _ := Encode(OpAutomationReady, AutomationReady{Version: "dev"})
				_ = WriteFrame(conn, ready)
				for {
					f, err := ReadFrame(conn, MaxClientFrame)
					if err != nil {
						return
					}
					var req Request
					if Decode(f, &req) != nil {
						return
					}
					answer(conn, req)
				}
			}()
		}
	}()
	return AutomationFile{Address: l.Addr().String(), Secret: "s"}
}

func respond(conn net.Conn, id string) {
	status := 200
	f, _ := Encode(OpResponse, Response{ID: id, Status: &status})
	_ = WriteFrame(conn, f)
}

// TestACallGivenUpOnEndsTheConnection: its answer may still arrive, and the next call would read it as its
// own. So there is no next call, and the error says to open another connection.
func TestACallGivenUpOnEndsTheConnection(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	file := fakePort(t, func(conn net.Conn, req Request) {
		<-release
		respond(conn, req.ID)
	})
	c, err := DialAutomation(context.Background(), file, exampleToken)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = c.Do(ctx, "GET", "/users/@me", nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	_, err = c.Do(context.Background(), "GET", "/users/@me", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open another")
}

// TestAnAnswerThatIsNotThisRequestsEndsTheConnection: once an answer does not match its request, the next
// answer would be read by the wrong call. The connection is given up on rather than used again.
func TestAnAnswerThatIsNotThisRequestsEndsTheConnection(t *testing.T) {
	file := fakePort(t, func(conn net.Conn, req Request) { respond(conn, req.ID+"0") })
	c, err := DialAutomation(context.Background(), file, exampleToken)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	_, err = c.Do(context.Background(), "GET", "/users/@me", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "other than this request's response")

	_, err = c.Do(context.Background(), "GET", "/users/@me", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open another")
}

// TestACancelThatLosesTheRaceWithAnAnswerLeavesTheConnectionUsable: the context is canceled the moment
// the answer is written, so the cancel and the read race. Whichever wins, a call that returned an answer
// must leave a connection the next call can use: the cancel moves the connection's deadline to now, and a
// deadline left there fails the next write (M22 /code-review).
func TestACancelThatLosesTheRaceWithAnAnswerLeavesTheConnectionUsable(t *testing.T) {
	var cancelNext func()
	var mu sync.Mutex
	file := fakePort(t, func(conn net.Conn, req Request) {
		respond(conn, req.ID)
		mu.Lock()
		if cancelNext != nil {
			cancelNext()
		}
		mu.Unlock()
	})

	answered := 0
	for range 300 {
		c, err := DialAutomation(context.Background(), file, exampleToken)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		mu.Lock()
		cancelNext = cancel
		mu.Unlock()
		_, err = c.Do(ctx, "GET", "/users/@me", nil)
		mu.Lock()
		cancelNext = nil
		mu.Unlock()
		cancel()

		if err == nil {
			answered++
			_, err = c.Do(context.Background(), "GET", "/users/@me", nil)
			require.NoError(t, err, "a call that was answered left its connection unusable")
		}
		_ = c.Close()
	}
	require.NotZero(t, answered, "the answer never won the race, so the test saw nothing")
}

func TestThePortNumberInAnEnablePath(t *testing.T) {
	for _, port := range []int{1, 80, 7717, 65535} {
		got, ok := AutomationEnablePort(PathAutomationEnable(port))
		require.True(t, ok, "%d", port)
		assert.Equal(t, port, got)
	}
	for _, path := range []string{
		PathAutomation, PathAutomationDisable, PathAutomation + "/enable", PathAutomation + "/enable/",
		PathAutomation + "/enable/0", PathAutomation + "/enable/65536", PathAutomation + "/enable/07717",
		PathAutomation + "/enable/7717/", PathAutomation + "/enable/7717/x", PathAutomation + "/enable/+7717",
		PathAutomation + "/enable/-1", PathAutomation + "/enable/7717 ", PathAutomation + "/enable/999999",
		"/@daemon/automationx/enable/7717", "/automation/enable/7717",
	} {
		_, ok := AutomationEnablePort(path)
		assert.False(t, ok, "%q", path)
	}
}
