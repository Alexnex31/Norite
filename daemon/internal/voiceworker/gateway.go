// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"sync"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// GatewayLink is the daemon's gateway connection as the voice signaler needs it: *gatewayclient.Client.
type GatewayLink interface {
	// SendVoice sends op 4 on the established connection, refusing at once when there is none.
	SendVoice(ctx context.Context, v gatewayproto.VoiceStateUpdate) error
	// Connected reports whether a connection is established now.
	Connected() bool
}

// Gateway signals calls over the daemon's own gateway connection (M33): op 4 with the worker's offer,
// answered by VOICE_SERVER_UPDATE or VOICE_JOIN_REFUSED, which the gateway client hands to Deliver and to
// nobody attached. Every release's signaler.
//
// What the instance says is a stranger's (M19): a reply is decoded strictly, matched to a join by a nonce
// this daemon chose and to the channel it asked for, and bounded before it reaches the worker, which bounds
// it again.
type Gateway struct {
	Link GatewayLink
	// Instance is the instance the daemon is signed in to, whose host being this machine admits loopback
	// candidates: an instance on the same machine announces loopback addresses.
	Instance func() string

	mu sync.Mutex
	// waiting is the join whose reply is expected, by nonce; one at a time, since the daemon joins one call
	// at a time (Voice).
	waiting map[string]chan reply
	// conn is the call's connection id, once answered, and changes where its rights go.
	conn    string
	changes chan voiceipc.Rights
}

type reply struct {
	event string
	data  json.RawMessage
}

var _ Signaler = (*Gateway)(nil)

// Prepare refuses a join while the daemon has no connection to its instance, before any worker is started:
// op 4 would have nowhere to go.
func (g *Gateway) Prepare(context.Context, string) (Route, error) {
	if !g.Link.Connected() {
		return Route{}, errors.New("the daemon is not connected to its instance; try again once it is")
	}
	return Route{Loopback: localInstance(g.instance())}, nil
}

func (g *Gateway) instance() string {
	if g.Instance == nil {
		return ""
	}
	return g.Instance()
}

// localInstance is an instance URL whose host is a loopback address or exactly localhost, read as a host
// and never as text (the harness's rule).
func localInstance(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

// Join sends op 4 and waits for its reply.
func (g *Gateway) Join(ctx context.Context, _, channel, offer string) (Answer, error) {
	nonce, err := newNonce()
	if err != nil {
		return Answer{}, err
	}
	replies := make(chan reply, 1)
	g.mu.Lock()
	g.waiting = map[string]chan reply{nonce: replies}
	g.conn, g.changes = "", nil
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.waiting, nonce)
		g.mu.Unlock()
	}()

	video := false
	if err := g.Link.SendVoice(ctx, gatewayproto.VoiceStateUpdate{
		Nonce: nonce, ChannelID: &channel, Offer: offer, SupportsVideo: &video,
	}); err != nil {
		return Answer{}, fmt.Errorf("the join could not be sent to the instance: %w", err)
	}
	var r reply
	select {
	case r = <-replies:
	case <-ctx.Done():
		return Answer{}, errors.New("the instance did not answer the join in time")
	}

	if r.event == gatewayproto.EventVoiceJoinRefused {
		var refused gatewayproto.VoiceJoinRefused
		if err := strictJSON(r.data, &refused); err != nil {
			return Answer{}, errors.New("the instance refused the join in a shape this daemon does not know")
		}
		return Answer{}, errors.New(refusalWords(refused.Code))
	}
	var s gatewayproto.VoiceServer
	if err := strictJSON(r.data, &s); err != nil || s.ChannelID != channel || s.ConnectionID == "" {
		return Answer{}, errors.New("the instance answered the join in a shape this daemon does not know")
	}
	servers := make([]voiceipc.ICEServer, 0, len(s.ICEServers))
	for _, srv := range s.ICEServers {
		servers = append(servers, voiceipc.ICEServer{URLs: srv.URLs, Username: srv.Username, Credential: srv.Credential})
	}
	a := Answer{SDP: s.Answer, ICEServers: servers, Bitrate: s.Bitrate, Rights: rights(s.Mute, s.Deaf, s.Suppress)}
	if err := (voiceipc.Answer{SDP: a.SDP, ICEServers: a.ICEServers, Bitrate: a.Bitrate}).Check(); err != nil {
		return Answer{}, fmt.Errorf("the instance's answer is past a bound: %w", err)
	}
	changes := make(chan voiceipc.Rights, 1)
	g.mu.Lock()
	g.conn, g.changes = s.ConnectionID, changes
	g.mu.Unlock()
	a.Changes = changes
	return a, nil
}

// rights is what VoiceState's three flags leave the account able to do.
func rights(mute, deaf, suppress bool) voiceipc.Rights {
	return voiceipc.Rights{Send: !mute && !suppress, Hear: !deaf}
}

// refusalWords is a refusal code as a person reads it. A code this daemon does not know is said as one.
func refusalWords(code string) string {
	switch code {
	case gatewayproto.VoiceNotFound:
		return "there is no such voice channel, or this account cannot see it"
	case gatewayproto.VoiceNotVoice:
		return "that channel is not a voice channel"
	case gatewayproto.VoiceForbidden:
		return "this account may not join that voice channel"
	case gatewayproto.VoiceFull:
		return "the voice channel is full"
	case gatewayproto.VoiceBadOffer:
		return "the voice server refused this client's offer"
	case gatewayproto.VoiceBusy:
		return "another join from this daemon is under way"
	case gatewayproto.VoiceRateLimited:
		return "this account has joined too often in the last minute; wait a moment"
	case gatewayproto.VoiceDisabled:
		return "the instance runs no voice server"
	case gatewayproto.VoiceUnavailable:
		return "the voice server could not take the call; try again"
	}
	return "the instance refused the join"
}

// Leave sends op 4 with no channel. Best effort: a connection that is not there has no session, and an
// instance whose session ends ends the call with it.
func (g *Gateway) Leave(ctx context.Context, _ string) {
	g.mu.Lock()
	g.conn, g.changes = "", nil
	g.mu.Unlock()
	_ = g.Link.SendVoice(ctx, gatewayproto.VoiceStateUpdate{})
}

// Deliver takes a voice dispatch from the gateway client's read loop, so it never blocks.
func (g *Gateway) Deliver(event string, data json.RawMessage) {
	switch event {
	case gatewayproto.EventVoiceServerUpdate, gatewayproto.EventVoiceJoinRefused:
		var n struct {
			Nonce string `json:"nonce"`
		}
		if json.Unmarshal(data, &n) != nil {
			return
		}
		g.mu.Lock()
		replies := g.waiting[n.Nonce]
		delete(g.waiting, n.Nonce)
		g.mu.Unlock()
		if replies != nil {
			// Copied: the read loop's buffer is not the signaler's to keep.
			replies <- reply{event: event, data: bytes.Clone(data)}
		}
	case gatewayproto.EventVoiceStateUpdate:
		var s struct {
			ConnectionID string `json:"connection_id"`
			Mute         bool   `json:"mute"`
			Deaf         bool   `json:"deaf"`
			Suppress     bool   `json:"suppress"`
		}
		if json.Unmarshal(data, &s) != nil {
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.changes == nil || s.ConnectionID != g.conn {
			return
		}
		// The latest only: a change waiting unread is replaced.
		select {
		case <-g.changes:
		default:
		}
		g.changes <- rights(s.Mute, s.Deaf, s.Suppress)
	}
}

func newNonce() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// strictJSON decodes a reply the way the gateway's contract shapes it: no field it does not name.
func strictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
