// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/gateway"
	"github.com/Alexnex31/Norite/backend/internal/platform/events"
)

// Every path that ends a sign-in closes the connections opened with it. Five paths end one, and a connection
// left open by any of them keeps a signed-out device receiving the account's events for as long as its
// client likes: a request is bounded by the access token's fifteen minutes, a connection by nothing.

// stillOpen asserts c is still attached to a live stream, by sending a message and receiving it. A heartbeat
// ack would not do: a connection whose session was dropped still answers heartbeats until it is closed.
func stillOpen(t *testing.T, f *guildFixture, c *gatewayClient, channelID string) {
	t.Helper()
	send(t, f, f.ownerToken, channelID, "still here")
	assert.Equal(t, "still here", c.expect("MESSAGE_CREATE").field(t, "content"))
}

// revokedClose asserts the server closed c for a revoked sign-in.
func revokedClose(c *gatewayClient) {
	c.t.Helper()
	assert.Equal(c.t, "signed out", c.expectClose(gatewayproto.CloseSessionRevoked))
}

// The primitive's close, through its commonest caller. The device that asked keeps its stream.
func TestSigningOutEverywhereElseClosesTheOtherDevicesConnections(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})

	laptop := connected(t, url, f.ownerToken)
	phone := f.api.login("owner@example.com", "owner-phone")
	onPhone := connected(t, url, phone.AccessToken)

	res := f.api.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(phone.AccessToken))
	require.Equal(t, http.StatusOK, res.Code, res)

	revokedClose(laptop)
	stillOpen(t, f, onPhone, general)
}

// Signing out on a machine ends that machine's stream.
func TestLoggingOutClosesThatDevicesConnection(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})

	pair := f.api.login("member@example.com", "member-laptop")
	laptop := connected(t, url, pair.AccessToken)
	other := connected(t, url, f.memberToken)

	res := f.api.call(http.MethodPost, "/api/v1/auth/logout", map[string]string{"refresh_token": pair.RefreshToken})
	require.Equal(t, http.StatusNoContent, res.Code, res)

	revokedClose(laptop)
	stillOpen(t, f, other, general)
}

// "Sign out that laptop" from the sessions list: most of what it is for is the laptop's live stream.
func TestRevokingADeviceClosesItsConnection(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})

	laptop := connected(t, url, f.memberToken)
	phone := f.api.login("member@example.com", "member-phone")
	onPhone := connected(t, url, phone.AccessToken)

	list := f.api.call(http.MethodGet, "/api/v1/users/@me/sessions", nil, withToken(phone.AccessToken))
	require.Equal(t, http.StatusOK, list.Code, list)
	var devices []struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
	}
	list.decode(&devices)
	require.Len(t, devices, 2)
	var laptopID string
	for _, d := range devices {
		if !d.Current {
			laptopID = d.ID
		}
	}

	res := f.api.call(http.MethodDelete, "/api/v1/users/@me/sessions/"+laptopID, nil, withToken(phone.AccessToken))
	require.Equal(t, http.StatusNoContent, res.Code, res)

	revokedClose(laptop)
	stillOpen(t, f, onPhone, general)
}

// The path that matters most. A refresh token presented twice means one of two parties holding the family is
// a thief, and nothing can say which, so both lose the device: the stream opened before the rotation and the
// one opened after it alike.
func TestRefreshTokenReuseClosesTheDevicesConnections(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})

	first := f.api.login("member@example.com", "member-laptop")
	before := connected(t, url, first.AccessToken)

	rotated := f.api.call(http.MethodPost, "/api/v1/auth/refresh", map[string]string{"refresh_token": first.RefreshToken})
	require.Equal(t, http.StatusOK, rotated.Code, rotated)
	var second tokenPair
	rotated.decode(&second)
	after := connected(t, url, second.AccessToken)
	other := connected(t, url, f.memberToken)

	replay := f.api.call(http.MethodPost, "/api/v1/auth/refresh", map[string]string{"refresh_token": first.RefreshToken})
	require.Equal(t, http.StatusUnauthorized, replay.Code, replay)

	revokedClose(before)
	revokedClose(after)
	stillOpen(t, f, other, general)
}

// gatedBus holds revocations until the test releases them, and reports each one handled, so a test can put
// a connection in place between a revocation's commit and its delivery. That window is real on every
// instance and too narrow to hit by racing.
type gatedBus struct {
	events.Bus
	release chan struct{}
	handled chan struct{}
}

func newGatedBus(inner events.Bus) *gatedBus {
	return &gatedBus{Bus: inner, release: make(chan struct{}), handled: make(chan struct{}, 8)}
}

func (b *gatedBus) Subscribe(topic string, handle func([]byte)) (events.Subscription, error) {
	if topic != dispatch.RevocationTopic {
		return b.Bus.Subscribe(topic, handle)
	}
	return b.Bus.Subscribe(topic, func(payload []byte) {
		<-b.release
		handle(payload)
		b.handled <- struct{}{}
	})
}

// A fresh sign-in on a device closes the connection opened with the sign-in it replaced, and never the one
// opened with the new sign-in, even when that connection is in place before the revocation arrives.
func TestSigningInAgainClosesOnlyTheReplacedSignInsConnection(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	bus := newGatedBus(f.api.bus)
	url, _ := customGateway(t, f, func(o *gateway.Options) { o.Bus = bus })
	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})

	old := connected(t, url, f.memberToken)
	again := f.api.login("member@example.com", "member-device")
	fresh := connected(t, url, again.AccessToken)

	close(bus.release)
	select {
	case <-bus.handled:
	case <-time.After(5 * time.Second):
		t.Fatal("the revocation never arrived")
	}

	revokedClose(old)
	stillOpen(t, f, fresh, general)
}

// droppingBus loses every revocation, as an at-most-once bus may.
type droppingBus struct{ events.Bus }

func (b droppingBus) Subscribe(topic string, handle func([]byte)) (events.Subscription, error) {
	if topic == dispatch.RevocationTopic {
		handle = func([]byte) {}
	}
	return b.Bus.Subscribe(topic, handle)
}

// A revocation the bus lost is caught by the periodic re-check, on the next heartbeat after the interval.
func TestALostRevocationIsCaughtByTheLivenessCheck(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url, _ := customGateway(t, f, func(o *gateway.Options) {
		o.Bus = droppingBus{f.api.bus}
		o.LivenessInterval = time.Millisecond
	})

	laptop := connected(t, url, f.ownerToken)
	phone := f.api.login("owner@example.com", "owner-phone")
	res := f.api.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(phone.AccessToken))
	require.Equal(t, http.StatusOK, res.Code, res)

	time.Sleep(10 * time.Millisecond)
	laptop.send(gatewayproto.OpHeartbeat, nil)
	revokedClose(laptop)
}

// A revoked session is gone, not merely disconnected: its stream is not waiting to be resumed by anybody.
func TestARevokedSessionIsNotLeftToBeResumed(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)

	pair := f.api.login("member@example.com", "member-laptop")
	laptop, sessionID := identified(t, url, pair.AccessToken)

	res := f.api.call(http.MethodPost, "/api/v1/auth/logout", map[string]string{"refresh_token": pair.RefreshToken})
	require.Equal(t, http.StatusNoContent, res.Code, res)
	revokedClose(laptop)

	// Signed in again on the same device, which passes liveness: only the session being gone refuses this.
	again := f.api.login("member@example.com", "member-laptop")
	back := dialGateway(t, url, nil)
	back.hello()
	back.resume(again.AccessToken, sessionID, 1)
	back.notResumable()
}
