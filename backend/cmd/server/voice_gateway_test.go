// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/gateway"
	"github.com/Alexnex31/Norite/backend/internal/guilds"
	"github.com/Alexnex31/Norite/backend/internal/platform/ratelimit"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/reports"
	"github.com/Alexnex31/Norite/backend/internal/roles"
	"github.com/Alexnex31/Norite/backend/internal/voice"
	"github.com/Alexnex31/Norite/backend/internal/voice/coordinator"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
	"github.com/Alexnex31/Norite/backend/voiceproto"
)

// voiceFixture is a guild with a voice channel, a real SFU on loopback, and a gateway whose op 4 reaches a
// real coordinator over the fixture's database.
type voiceFixture struct {
	*guildFixture
	url     string
	sfu     *voice.SFU
	coord   *coordinator.Coordinator
	channel string
	text    string
}

func newVoiceFixture(t *testing.T, mutate func(*gateway.Options)) *voiceFixture {
	t.Helper()
	f := newGuildFixture(t)
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	sfu, err := voice.New(voice.Options{Conn: conn, Loopback: true, Version: gatewayproto.DevVersion,
		Logger: zerolog.Nop()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sfu.Close() })

	ids, err := snowflake.NewGenerator(7)
	require.NoError(t, err)
	coord, err := coordinator.New(coordinator.Options{
		Pool: f.api.pool, SFU: sfu, IDs: ids, Events: dispatch.NewPublisher(f.api.bus, nil), Bus: f.api.bus,
		RateLimitBackend: ratelimit.MemoryBackend(), TURNSecret: []byte("a turn secret for these tests only"),
		TURNURLs: []string{"turn:chat.example.com:3478?transport=udp"}, Logger: zerolog.Nop(),
	})
	require.NoError(t, err)
	require.NoError(t, coord.Sweep(t.Context()))
	t.Cleanup(coord.Close)

	url, _ := customGateway(t, f, func(o *gateway.Options) {
		o.Voice = coord
		if mutate != nil {
			mutate(o)
		}
	})
	vf := &voiceFixture{guildFixture: f, url: url, sfu: sfu, coord: coord}
	vf.channel = vf.createChannel(t, map[string]any{"name": "lounge", "type": 2})
	vf.text = vf.createChannel(t, map[string]any{"name": "general", "type": 0})
	return vf
}

func (f *voiceFixture) createChannel(t *testing.T, body map[string]any) string {
	t.Helper()
	res := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID), body,
		withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, res.Code, res)
	return res.field(t, "id")
}

// caller is one account's gateway connection and its voice-worker stand-in.
type caller struct {
	gw     *gatewayClient
	client *voicetest.Client
}

func (f *voiceFixture) connect(t *testing.T, token string) *gatewayClient {
	t.Helper()
	gw := dialGateway(t, f.url, nil)
	gw.hello()
	gw.identify(token, "dev")
	gw.ready()
	return gw
}

// join sends op 4 and returns the reply's type and payload, skipping the voice states the channel is told
// about meanwhile.
func (g *gatewayClient) join(channel, nonce, offer string) (string, json.RawMessage) {
	g.t.Helper()
	g.send(gatewayproto.OpVoiceStateUpdate, map[string]any{
		"nonce": nonce, "channel_id": channel, "offer": offer, "supports_video": false,
		"self_mute": false, "self_deaf": false,
	})
	return g.until(gatewayproto.EventVoiceServerUpdate, gatewayproto.EventVoiceJoinRefused)
}

// until reads dispatches until one of types arrives.
func (g *gatewayClient) until(types ...string) (string, json.RawMessage) {
	g.t.Helper()
	for {
		f := g.read()
		if f.Op != gatewayproto.OpDispatch || f.T == nil {
			continue
		}
		for _, t := range types {
			if *f.T == t {
				return t, f.D
			}
		}
	}
}

// untilState reads VOICE_STATE_UPDATE until one matches: the caller's own join is announced to it first.
func (g *gatewayClient) untilState(t *testing.T, match func(map[string]any) bool) map[string]any {
	t.Helper()
	for {
		_, raw := g.until(gatewayproto.EventVoiceStateUpdate)
		var state map[string]any
		require.NoError(t, json.Unmarshal(raw, &state))
		if match(state) {
			return state
		}
	}
}

func newVoiceClient(t *testing.T) *voicetest.Client {
	t.Helper()
	c, err := voicetest.New(voicetest.Options{Slots: 1, Loopback: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func offer(t *testing.T, c *voicetest.Client) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sdp, err := c.Offer(ctx)
	require.NoError(t, err)
	return sdp
}

// joinCall joins channel as token's account and waits for the media to be up and the SFU's hello answered.
func (f *voiceFixture) joinCall(t *testing.T, token, channel string) (*caller, gatewayproto.VoiceServer) {
	t.Helper()
	gw := f.connect(t, token)
	c := newVoiceClient(t)
	typ, raw := gw.join(channel, "n1", offer(t, c))
	require.Equal(t, gatewayproto.EventVoiceServerUpdate, typ, "%s", raw)
	var server gatewayproto.VoiceServer
	require.NoError(t, json.Unmarshal(raw, &server))
	require.NoError(t, c.Accept(server.Answer))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, c.WaitConnected(ctx))
	_, err := c.WaitFor(ctx, func(op string, _ []byte) bool { return op == voiceproto.OpReady })
	require.NoError(t, err, "the SFU did not answer the hello")
	return &caller{gw: gw, client: c}, server
}

func (f *voiceFixture) voiceStates(t *testing.T, token, channel string) []map[string]any {
	t.Helper()
	res := f.api.call(http.MethodGet, "/api/v1/channels/"+channel+"/voice-states", nil, withToken(token))
	require.Equal(t, http.StatusOK, res.Code, res)
	var out []map[string]any
	res.decode(&out)
	return out
}

// dropped waits for the SFU to have ended the client's connection.
func dropped(t *testing.T, c *voicetest.Client, what string) {
	t.Helper()
	select {
	case <-c.ChannelClosed():
	case <-c.Failed():
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not end the call", what)
	}
}

// droppedSaying is dropped, the SFU having told the client why first, in exactly these words (M33's manual
// pass: every end read "the voice server closed the call").
func droppedSaying(t *testing.T, c *voicetest.Client, what, reason string) {
	t.Helper()
	dropped(t, c, what)
	var said []string
	for _, m := range c.Messages() {
		if op, _ := voiceproto.OpOf(m); op == voiceproto.OpRefused {
			var r voiceproto.Refused
			require.NoError(t, voiceproto.Decode(m, &r))
			said = append(said, r.Reason)
		}
	}
	assert.Equal(t, []string{reason}, said, "what %s told the client", what)
}

// The done-when: a join over the real gateway path allocates a real SFU connection, end to end, and the
// channel is told who is in it.
func TestAJoinOverTheGatewayIsProvisionedOnTheSFU(t *testing.T) {
	f := newVoiceFixture(t, nil)
	watcher := f.connect(t, f.ownerToken)

	member, server := f.joinCall(t, f.memberToken, f.channel)
	assert.Equal(t, "n1", server.Nonce)
	assert.Equal(t, f.channel, server.ChannelID)
	assert.Equal(t, f.guildID, server.GuildID)
	assert.Equal(t, coordinator.DefaultBitrate, server.Bitrate, "a channel naming no bitrate is 128 kbps")
	require.Len(t, server.ICEServers, 1)
	assert.Equal(t, []string{"turn:chat.example.com:3478?transport=udp"}, server.ICEServers[0].URLs)
	assert.Contains(t, server.ICEServers[0].Username, ":"+f.memberID, "the credential is the account's")
	assert.False(t, server.Mute || server.Deaf || server.Suppress)

	_, raw := watcher.until(gatewayproto.EventVoiceStateUpdate)
	var state map[string]any
	require.NoError(t, json.Unmarshal(raw, &state))
	assert.Equal(t, server.ConnectionID, state["connection_id"])
	assert.Equal(t, f.memberID, state["user_id"])

	states := f.voiceStates(t, f.ownerToken, f.channel)
	require.Len(t, states, 1)
	assert.Equal(t, server.ConnectionID, states[0]["connection_id"])

	// Leaving ends the connection on the SFU, and the channel is told.
	member.gw.send(gatewayproto.OpVoiceStateUpdate, map[string]any{"channel_id": nil})
	dropped(t, member.client, "a leave")
	for _, m := range member.client.Messages() {
		op, _ := voiceproto.OpOf(m)
		assert.NotEqual(t, voiceproto.OpRefused, op, "a call its client left was told why: %s", m)
	}
	_, raw = watcher.until(gatewayproto.EventVoiceStateDelete)
	require.NoError(t, json.Unmarshal(raw, &state))
	assert.Equal(t, server.ConnectionID, state["connection_id"])
	assert.Empty(t, f.voiceStates(t, f.ownerToken, f.channel))
}

// The done-when's second clause: a kick mid-call drops the connection.
func TestAKickMidCallDropsTheConnection(t *testing.T) {
	f := newVoiceFixture(t, nil)
	member, _ := f.joinCall(t, f.memberToken, f.channel)

	res := f.api.call(http.MethodDelete,
		fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.memberID), nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusNoContent, res.Code, res)
	droppedSaying(t, member.client, "a kick", "you are no longer a member of this guild")
	require.Eventually(t, func() bool { return len(f.voiceStates(t, f.ownerToken, f.channel)) == 0 },
		5*time.Second, 50*time.Millisecond, "the kicked member is still listed in the channel")
}

// A role change that takes PermConnectVoice away ends the call; one that takes Speak away tells the caller
// it is suppressed, and leaves the call up.
func TestALostPermissionEndsTheCallAndALostSpeakSuppressesIt(t *testing.T) {
	f := newVoiceFixture(t, nil)
	member, server := f.joinCall(t, f.memberToken, f.channel)

	deny := func(perm string) {
		res := f.api.call(http.MethodPut,
			fmt.Sprintf("/api/v1/channels/%s/permissions/%s", f.channel, f.everyoneID),
			map[string]any{"type": 0, "allow": "0", "deny": perm}, withToken(f.ownerToken))
		require.Equal(t, http.StatusOK, res.Code, res)
	}
	deny(fmt.Sprint(roles.PermSpeakVoice.Int64()))
	state := member.gw.untilState(t, func(s map[string]any) bool { return s["suppress"] == true })
	assert.Equal(t, server.ConnectionID, state["connection_id"])
	select {
	case <-member.client.ChannelClosed():
		t.Fatal("losing SPEAK ended the call")
	case <-time.After(300 * time.Millisecond):
	}

	deny(fmt.Sprint((roles.PermSpeakVoice | roles.PermConnectVoice).Int64()))
	droppedSaying(t, member.client, "a lost CONNECT", "you may no longer join this voice channel")
}

// A server mute reaches the caller's voice state; the SFU enforces nothing (M33's D6).
func TestAServerMuteReachesTheCallersVoiceState(t *testing.T) {
	f := newVoiceFixture(t, nil)
	member, _ := f.joinCall(t, f.memberToken, f.channel)
	res := f.api.call(http.MethodPatch,
		fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.memberID),
		map[string]any{"mute": true}, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, res.Code, res)
	state := member.gw.untilState(t, func(s map[string]any) bool { return s["mute"] == true })
	assert.Equal(t, f.memberID, state["user_id"])
}

// updateSpacing is the gateway's spacing of one session's updates, which this package cannot read.
const updateSpacing = 250 * time.Millisecond

// What a caller sets on themselves reaches everyone who can see the channel, and the listing a client
// connecting later reads (M34).
func TestASelfMuteReachesTheChannel(t *testing.T) {
	f := newVoiceFixture(t, nil)
	member, _ := f.joinCall(t, f.memberToken, f.channel)
	watcher := f.connect(t, f.ownerToken)
	set := func(mute, deaf bool) {
		member.gw.send(gatewayproto.OpVoiceStateUpdate, map[string]any{
			"channel_id": f.channel, "self_mute": mute, "self_deaf": deaf,
		})
	}
	set(true, false)
	state := watcher.untilState(t, func(s map[string]any) bool { return s["self_mute"] == true })
	assert.Equal(t, f.memberID, state["user_id"])
	assert.Equal(t, false, state["self_deaf"])
	assert.Equal(t, false, state["mute"], "a self mute is not a server mute")
	// The same again is no change, and tells nobody: the next state the watcher reads is the third's. Each
	// is sent past the spacing, so the second runs rather than being replaced by the third.
	set(true, false)
	time.Sleep(2 * updateSpacing)
	set(true, true)
	state = watcher.untilState(t, func(map[string]any) bool { return true })
	assert.Equal(t, true, state["self_deaf"], "an update that changed nothing was announced")
	states := f.voiceStates(t, f.ownerToken, f.channel)
	require.Len(t, states, 1)
	assert.Equal(t, true, states[0]["self_mute"])
}

// The end of a sign-in ends its call: the revocation drops the session, and the session's end reaches the
// coordinator (M33's D3).
func TestALogoutEndsTheCall(t *testing.T) {
	f := newVoiceFixture(t, nil)
	pair := f.api.login("member@example.com", "member-laptop")
	member, _ := f.joinCall(t, pair.AccessToken, f.channel)
	res := f.api.call(http.MethodPost, "/api/v1/auth/logout", map[string]string{"refresh_token": pair.RefreshToken})
	require.Equal(t, http.StatusNoContent, res.Code, res)
	droppedSaying(t, member.client, "a logout", "this device's session with the instance ended")
}

// A session that is not resumed in its window ends its call with it.
func TestASessionExpiredPastItsResumeWindowEndsItsCall(t *testing.T) {
	f := newVoiceFixture(t, func(o *gateway.Options) { o.ResumeWindow = 200 * time.Millisecond })
	member, _ := f.joinCall(t, f.memberToken, f.channel)
	require.NoError(t, member.gw.ws.CloseNow())
	droppedSaying(t, member.client, "the resume window running out",
		"this device's session with the instance ended")
}

// A second join from the same session replaces its call, and one from a new session of the same device
// replaces the call the older session left.
func TestAJoinReplacesTheSessionsAndTheDevicesEarlierCall(t *testing.T) {
	f := newVoiceFixture(t, nil)
	first, _ := f.joinCall(t, f.memberToken, f.channel)

	c := newVoiceClient(t)
	typ, raw := first.gw.join(f.channel, "n2", offer(t, c))
	require.Equal(t, gatewayproto.EventVoiceServerUpdate, typ, "%s", raw)
	dropped(t, first.client, "a second join from the session")

	// The member's token is for member-device; a new connection identifies a new session of it.
	again, _ := f.joinCall(t, f.memberToken, f.channel)
	_ = again
	require.Eventually(t, func() bool { return len(f.voiceStates(t, f.ownerToken, f.channel)) == 1 },
		5*time.Second, 50*time.Millisecond, "the device's earlier call is still listed")
}

// Every refusal is a code, the connection stays up, and nothing is provisioned.
func TestAJoinIsRefusedWithACode(t *testing.T) {
	f := newVoiceFixture(t, nil)
	c := newVoiceClient(t)
	sdp := offer(t, c)

	stranger := f.connect(t, f.strangerToken)
	member := f.connect(t, f.memberToken)
	cases := []struct {
		name    string
		gw      *gatewayClient
		channel string
		offer   string
		code    string
	}{
		{"a channel nobody has", member, f.next(t), sdp, gatewayproto.VoiceNotFound},
		{"a guild the account is not in", stranger, f.channel, sdp, gatewayproto.VoiceNotFound},
		{"a text channel", member, f.text, sdp, gatewayproto.VoiceNotVoice},
		{"an offer the SFU refuses", member, f.channel, "v=0\r\n", gatewayproto.VoiceBadOffer},
	}
	for _, tc := range cases {
		typ, raw := tc.gw.join(tc.channel, "x", tc.offer)
		require.Equal(t, gatewayproto.EventVoiceJoinRefused, typ, "%s: %s", tc.name, raw)
		var refused gatewayproto.VoiceJoinRefused
		require.NoError(t, json.Unmarshal(raw, &refused))
		assert.Equal(t, gatewayproto.VoiceJoinRefused{Nonce: "x", ChannelID: tc.channel, Code: tc.code}, refused,
			tc.name)
	}
	assert.Empty(t, f.voiceStates(t, f.ownerToken, f.channel))

	// Without CONNECT, a member who can see the channel is told forbidden.
	res := f.api.call(http.MethodPut,
		fmt.Sprintf("/api/v1/channels/%s/permissions/%s", f.channel, f.everyoneID),
		map[string]any{"type": 0, "allow": "0", "deny": fmt.Sprint(roles.PermConnectVoice.Int64())}, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, res.Code, res)
	typ, raw := member.join(f.channel, "y", sdp)
	require.Equal(t, gatewayproto.EventVoiceJoinRefused, typ, "%s", raw)
	assert.Contains(t, string(raw), `"forbidden"`)
}

// A channel's user_limit refuses the account that would exceed it.
func TestUserLimitRefusesTheAccountPastIt(t *testing.T) {
	f := newVoiceFixture(t, nil)
	small := f.createChannel(t, map[string]any{"name": "booth", "type": 2, "user_limit": 1})
	f.joinCall(t, f.ownerToken, small)
	member := f.connect(t, f.memberToken)
	typ, raw := member.join(small, "z", offer(t, newVoiceClient(t)))
	require.Equal(t, gatewayproto.EventVoiceJoinRefused, typ, "%s", raw)
	assert.Contains(t, string(raw), `"full"`)
}

// An instance with no voice server answers every join voice_disabled, and a leave nothing.
func TestAnInstanceWithoutVoiceRefusesJoins(t *testing.T) {
	f := newGuildFixture(t)
	url, _ := customGateway(t, f, func(*gateway.Options) {})
	gw := dialGateway(t, url, nil)
	gw.hello()
	gw.identify(f.memberToken, "dev")
	gw.ready()
	gw.send(gatewayproto.OpVoiceStateUpdate, map[string]any{"channel_id": nil})
	// An update has no reply, here as anywhere: the refusal read next is the join's.
	gw.send(gatewayproto.OpVoiceStateUpdate, map[string]any{"channel_id": "1", "self_mute": true, "self_deaf": false})
	typ, raw := gw.join("1", "q", "v=0")
	require.Equal(t, gatewayproto.EventVoiceJoinRefused, typ)
	assert.JSONEq(t, `{"nonce":"q","channel_id":"1","code":"voice_disabled"}`, string(raw))
}

// The listing a client starts from sends what VoiceState declares, every field present.
func TestTheVoiceStateListingMatchesTheContract(t *testing.T) {
	f := newVoiceFixture(t, nil)
	f.joinCall(t, f.memberToken, f.channel)
	states := f.voiceStates(t, f.memberToken, f.channel)
	require.Len(t, states, 1)
	var got []string
	for k := range states[0] {
		got = append(got, k)
	}
	sort.Strings(got)
	declared, required := declaredProperties(t, contractSchemas(t)["VoiceState"])
	assert.Equal(t, declared, got)
	assert.Equal(t, declared, required)

	// A text channel has nobody in it, as an array; a stranger is refused as though it were not there.
	assert.JSONEq(t, "[]", string(f.api.call(http.MethodGet, "/api/v1/channels/"+f.text+"/voice-states", nil,
		withToken(f.memberToken)).Body))
	res := f.api.call(http.MethodGet, "/api/v1/channels/"+f.channel+"/voice-states", nil, withToken(f.strangerToken))
	assert.Equal(t, http.StatusNotFound, res.Code, res)
}

// The coordinator's copy of the voice channel type is guilds', which it cannot import.
func TestTheVoiceChannelTypeAgreesAcrossPackages(t *testing.T) {
	require.Equal(t, guilds.ChannelGuildVoice, coordinator.ChannelGuildVoice)
	require.Equal(t, guilds.ChannelGuildVoice, reports.ChannelGuildVoice)
}

// The bitrate's bounds are one set: what a channel accepts, what the gateway says it carries, and what the
// coordinator clamps a stored value to.
func TestTheBitrateBoundsAgreeAcrossTheContracts(t *testing.T) {
	for _, name := range []string{"CreateChannelRequest", "UpdateChannelRequest"} {
		schema, ok := contractSchemas(t)[name]
		require.True(t, ok, "openapi.yaml has no %s", name)
		bitrate := schema["properties"].(map[string]any)["bitrate"].(map[string]any)
		assert.EqualValues(t, coordinator.MinBitrate, bitrate["minimum"], name)
		assert.EqualValues(t, coordinator.MaxBitrate, bitrate["maximum"], name)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "gateway-events.schema.json"))
	require.NoError(t, err)
	var gw struct {
		Defs map[string]struct {
			Properties map[string]map[string]any `json:"properties"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &gw))
	b := gw.Defs["VoiceServer"].Properties["bitrate"]
	assert.EqualValues(t, coordinator.MinBitrate, b["minimum"])
	assert.EqualValues(t, coordinator.MaxBitrate, b["maximum"])

	f := newGuildFixture(t)
	for bitrate, want := range map[int]int{coordinator.MaxBitrate: http.StatusCreated,
		coordinator.MaxBitrate + 1: http.StatusBadRequest, coordinator.MinBitrate - 1: http.StatusBadRequest} {
		res := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
			map[string]any{"name": "booth", "type": 2, "bitrate": bitrate}, withToken(f.ownerToken))
		assert.Equal(t, want, res.Code, "bitrate %d: %s", bitrate, res)
	}
	stored := int32(384_000)
	assert.Equal(t, coordinator.MaxBitrate, coordinator.Bitrate(&stored), "a row written before M33 is clamped")
}

// Every refusal code the gateway can send is one the contract names, and the reverse.
func TestTheVoiceRefusalCodesAreTheContracts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "gateway-events.schema.json"))
	require.NoError(t, err)
	var gw struct {
		Defs map[string]struct {
			Properties map[string]struct {
				OneOf []struct {
					Const string `json:"const"`
				} `json:"oneOf"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &gw))
	var contract []string
	for _, c := range gw.Defs["VoiceJoinRefused"].Properties["code"].OneOf {
		contract = append(contract, c.Const)
	}
	assert.ElementsMatch(t, contract, gatewayproto.VoiceRefusalCodes())
}

// Deleting a voice channel ends its calls and closes its room.
func TestADeletedVoiceChannelEndsItsCalls(t *testing.T) {
	f := newVoiceFixture(t, nil)
	member, _ := f.joinCall(t, f.memberToken, f.channel)
	res := f.api.call(http.MethodDelete, "/api/v1/channels/"+f.channel, nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusNoContent, res.Code, res)
	droppedSaying(t, member.client, "deleting the channel", "the voice channel was deleted")
}

// A client that goes without leaving, its worker killed and its gateway session still there, ends its call
// once the SFU loses its connection: the channel is told, and the listing no longer shows it (M33's
// /code-review, where it stayed listed for its session's life).
func TestAClientThatGoesWithoutLeavingEndsItsCall(t *testing.T) {
	f := newVoiceFixture(t, nil)
	watcher := f.connect(t, f.ownerToken)
	member, server := f.joinCall(t, f.memberToken, f.channel)
	watcher.until(gatewayproto.EventVoiceStateUpdate)

	require.NoError(t, member.client.Close())
	_, raw := watcher.until(gatewayproto.EventVoiceStateDelete)
	var state map[string]any
	require.NoError(t, json.Unmarshal(raw, &state))
	assert.Equal(t, server.ConnectionID, state["connection_id"])
	assert.Empty(t, f.voiceStates(t, f.ownerToken, f.channel))
}
