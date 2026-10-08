// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"context"
	"net"
	"os"
	"strings"
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
