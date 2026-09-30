// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package redistest gives tests a real Redis-protocol server, for the half of the event bus and rate
// limiter that only the flagship activates (M114).
//
// It exists so that half is exercised before production does it. Until M18 the Redis paths were seams no
// code had ever run, on the two components whose failure modes only appear across processes, and
// docker-compose had shipped the server since M0 so the swap could be tried without a compose change.
//
// Unlike dbtest it starts its container on first use rather than from TestMain, so a package needing
// Postgres and Redis both keeps dbtest.Main as its TestMain. The container is removed by testcontainers'
// reaper when the test binary exits.
package redistest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the server tests run against. Keep it on the line docker/docker-compose.yml pins, so the tests
// exercise the server a developer's stack actually runs; that file explains why it is Valkey.
const Image = "valkey/valkey:9.1-alpine"

var (
	startOnce sync.Once
	serverURL string
	startErr  error
)

// URL returns the shared server's redis:// URL, starting it on first use. Skips in -short mode.
func URL(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping container-backed test in -short mode")
	}
	startOnce.Do(start)
	if startErr != nil {
		t.Fatalf("redistest: could not start %s: %v (run with -short to skip container-backed tests)", Image, startErr)
	}
	return serverURL
}

func start() {
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp"),
		},
		Started: true,
	})
	if err != nil {
		startErr = err
		return
	}
	endpoint, err := container.PortEndpoint(ctx, "6379/tcp", "")
	if err != nil {
		startErr = err
		return
	}
	serverURL = fmt.Sprintf("redis://%s/0", endpoint)
}

// Namespace returns a prefix unique to this test, for channels and keys.
//
// Pub/sub channels are global to the server and every test shares one, so two tests publishing on the same
// topic would hear each other. A per-test prefix makes each test its own world, which is the isolation
// dbtest gets from a database per test.
func Namespace(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, r := range strings.ToLower(t.Name()) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return "t:" + b.String() + ":"
}
