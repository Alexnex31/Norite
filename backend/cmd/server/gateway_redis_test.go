// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/gateway"
	"github.com/Alexnex31/Norite/backend/internal/platform/events"
	"github.com/Alexnex31/Norite/backend/internal/platform/redistest"
)

// redisBus is one process's connection to the shared Redis. The prefix keeps concurrent tests apart, and
// every replica in one test shares it, as every replica on one instance shares a server.
func redisBus(t *testing.T, prefix string) events.Bus {
	t.Helper()
	b, err := events.NewRedis(t.Context(), events.RedisOptions{URL: redistest.URL(t), Prefix: prefix})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// nextMessage reads past whatever else arrives (an event published before this connection identified can
// reach it a moment later over Redis) to the next MESSAGE_CREATE, and returns its content. A message that
// should not have arrived would be read here first, so the negative check still holds.
func nextMessage(t *testing.T, c *gatewayClient) any {
	t.Helper()
	for {
		d := c.nextDispatch()
		if d.t == "MESSAGE_CREATE" {
			return d.field(t, "content")
		}
	}
}

// The done-when's last clause: the gateway over the Redis bus, the way the flagship runs it (M114). Two
// replicas, each with its own Redis connection, as two processes would have. Mutations commit through one;
// a connection held by the other receives the events it may see and not the others, and is closed when its
// sign-in ends — the two things the bus exists to carry between processes.
func TestTheGatewayWorksAcrossReplicasOverRedis(t *testing.T) {
	t.Parallel()
	prefix := "norite:test:" + t.Name() + ":"
	a := newAPIOnBus(t, auth.RegistrationOpen, &captureMailer{}, nil, "https://chat.example.com", redisBus(t, prefix))
	f := newGuildFixtureOn(t, a)
	replica, _ := customGateway(t, f, func(o *gateway.Options) { o.Bus = redisBus(t, prefix) })

	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})
	staff := createChannel(t, f, map[string]any{"name": "staff", "type": 0})
	denyView(t, f, staff, 1, f.memberID)

	member := connected(t, replica, f.memberToken)
	owner := connected(t, serveGateway(t, f.api.handler), f.ownerToken)

	send(t, f, f.ownerToken, staff, "staff only")
	send(t, f, f.ownerToken, general, "across replicas")
	assert.Equal(t, "across replicas", nextMessage(t, member),
		"the other replica's member hears the visible channel, and never the hidden one before it")
	assert.Equal(t, "staff only", nextMessage(t, owner))
	assert.Equal(t, "across replicas", nextMessage(t, owner))

	laptop := f.api.login("member@example.com", "member-laptop")
	onReplica := connected(t, replica, laptop.AccessToken)
	res := f.api.call(http.MethodPost, "/api/v1/auth/logout", map[string]string{"refresh_token": laptop.RefreshToken})
	require.Equal(t, http.StatusNoContent, res.Code, res)
	revokedClose(onReplica)

	send(t, f, f.ownerToken, general, "still here")
	assert.Equal(t, "still here", nextMessage(t, member), "the member's other device keeps its stream")
}
