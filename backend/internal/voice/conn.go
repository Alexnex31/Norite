// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/voiceproto"
)

// conn is one admitted connection: a peer connection the SFU answers, sending its tracks and receiving
// others' over its slots.
type conn struct {
	sfu    *SFU
	room   *room
	id     snowflake.ID
	user   snowflake.ID
	issued snowflake.ID
	log    zerolog.Logger
	// userText and idText are the ids as the data channel names them, made once rather than by every
	// receiver on every selection tick.
	userText, idText string

	mu            sync.Mutex
	pc            *webrtc.PeerConnection
	slots         map[string][]*slot
	dc            *webrtc.DataChannel
	extraChannels int
	helloed       bool
	// inRoom is set by an accepted hello just before helloed, and is what the room's selection reads
	// (takesPart); helloed is what the sends read. Two flags, so the room counts the connection before
	// ready goes out and sends it nothing before ready has.
	inRoom          bool
	refused         bool
	sub             subscription
	slotMap         []voiceproto.SlotEntry
	mapVersion      uint64
	speaking        []string
	speakingVersion uint64
	closed          bool
	// cause is what closed the connection, when that is something Provision reports: an eviction, the SFU
	// stopping, the provision timing out.
	cause     error
	connected bool
	timer     *time.Timer
	done      chan struct{}

	// tracks counts the tracks the peer has sent, against what its offer said it would.
	tracks    int
	maxTracks int
	// streams counts the SRTP and SRTCP streams the connection has opened, against maxStreams.
	streams    int
	maxStreams int
	// candidates counts the remote candidates ICE has been allowed to add (streams.go).
	candidates int
	// rtcp and messages meter what the peer sends besides media, under mu.
	rtcp     meter
	messages meter

	// sendMu orders what goes out on the data channel; mapSent is the newest slot map sent, once one has
	// been, and speakingSent the newest speaking list.
	sendMu       sync.Mutex
	mapSent      uint64
	mapSentOnce  bool
	speakingSent uint64
}

func newConn(s *SFU, r *room, id, user, issued snowflake.ID) *conn {
	c := &conn{
		sfu:      s,
		room:     r,
		id:       id,
		user:     user,
		issued:   issued,
		log:      s.log.With().Stringer("room", r.id).Stringer("conn", id).Logger(),
		userText: user.String(),
		idText:   id.String(),
		sub:      noSubscription,
		done:     make(chan struct{}),
	}
	// A provision nobody connects to is dropped, or a client could hold places in rooms without ever
	// sending a packet: unbounded growth in the shape M6's sweeper fixed. So is one that connects and never
	// says hello, which takes no part in the room and is never refused for its version.
	c.timer = time.AfterFunc(s.lim.ProvisionTimeout, func() {
		defer c.contain("the provision timeout")
		c.mu.Lock()
		connected, helloed := c.connected, c.helloed
		c.mu.Unlock()
		switch {
		case !connected:
			c.closeFor(ErrStale, "not connected within the provision timeout")
		case !helloed:
			c.close("no hello within the provision timeout")
		}
	})
	return c
}

// negotiate builds the peer connection and answers the offer.
//
// The SFU's transceivers are added before the offer is applied, so Pion matches them to the offer's
// sections by kind and direction: each section the client sends on gets a receiving transceiver, and each
// slot the client receives on gets a sending one.
func (c *conn) negotiate(ctx context.Context, offer string, shape offerShape) (string, error) {
	c.mu.Lock()
	for _, n := range shape.sends {
		c.maxTracks += n
	}
	slotCount := 0
	for _, n := range shape.slots {
		slotCount += n
	}
	c.maxStreams = streamsFor(c.maxTracks, slotCount)
	c.mu.Unlock()

	pc, err := c.sfu.apiFor(c).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = pc.Close()
		return "", errClosedWhileProvisioned
	}
	c.pc = pc
	c.mu.Unlock()

	for kind, n := range shape.sends {
		for range n {
			if _, err := pc.AddTransceiverFromKind(mediaKind(kind), webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionRecvonly,
			}); err != nil {
				return "", err
			}
		}
	}
	slots := map[string][]*slot{}
	transceivers := map[*slot]*webrtc.RTPTransceiver{}
	for kind, n := range shape.slots {
		codec, ok := codecFor(kind)
		if !ok {
			return "", fmt.Errorf("%w: the SFU carries no %s codec", ErrBadOffer, kind)
		}
		for i := range n {
			st := newSlotTrack(kind, i, codec)
			tr, err := pc.AddTransceiverFromTrack(st, webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionSendonly,
			})
			if err != nil {
				return "", err
			}
			s := &slot{conn: c, index: i, kind: kind, codec: strings.ToLower(codec.MimeType), track: st}
			slots[kind] = append(slots[kind], s)
			transceivers[s] = tr
			go c.drainSender(tr.Sender())
		}
	}
	pc.OnTrack(c.onTrack)
	pc.OnDataChannel(c.onDataChannel)

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		defer c.contain("a state change")
		switch st {
		case webrtc.PeerConnectionStateConnected:
			c.mu.Lock()
			c.connected = true
			c.mu.Unlock()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			// Closing from inside Pion's own callback can wait on the goroutine running it.
			go c.close("the connection " + st.String())
		}
	})

	panicHook("negotiating", nil)

	// Pion's own refusal is logged and not returned: its text can quote the offer, and what the client is
	// told says which rule was broken, not what it sent.
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		c.log.Debug().Err(err).Msg("an offer Pion would not apply")
		return "", fmt.Errorf("%w: it could not be applied", ErrBadOffer)
	}
	// Pion matched the slots to the offer's receiving sections in order; the mid is how the slot map
	// names each slot to the client.
	for s, tr := range transceivers {
		s.mid = tr.Mid()
		if s.mid == "" {
			return "", fmt.Errorf("%w: a slot matched no section", ErrBadOffer)
		}
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		c.log.Debug().Err(err).Msg("an offer Pion would not answer")
		return "", fmt.Errorf("%w: it could not be answered", ErrBadOffer)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	// With one UDP mux there is nothing to wait on but the host candidates of the socket, so the answer
	// carries every candidate and no trickling is needed.
	select {
	case <-gathered:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-c.done:
		return "", errClosedWhileProvisioned
	}
	c.mu.Lock()
	closed := c.closed
	c.slots = slots
	c.mu.Unlock()
	if closed {
		return "", errClosedWhileProvisioned
	}
	c.room.changed()
	return pc.LocalDescription().SDP, nil
}

// closedBy says whether the connection is closed, and what Provision answers for it if so.
func (c *conn) closedBy() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cause != nil {
		return c.closed, c.cause
	}
	return c.closed, errClosedWhileProvisioned
}

// errClosedWhileProvisioned is a connection closed during its provision for no reason Provision has a
// sentinel for, such as a panic contained in one of its callbacks.
var errClosedWhileProvisioned = errors.New("voice: the connection was closed while it was provisioned")

// closeAll closes conns, each with the cause and reason given, all at once and waiting for all of them:
// one at a time, each peer connection's close (DTLS's alert, SCTP's abort) waits on the last.
func closeAll(conns []*conn, cause error, reason string) {
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.contain("closing")
			c.closeFor(cause, reason)
		}()
	}
	wg.Wait()
}

// codecFor is the codec the SFU carries for a kind. Video has none until M107.
func codecFor(kind string) (webrtc.RTPCodecCapability, bool) {
	if kind == voiceproto.KindAudio {
		return opusCapability, true
	}
	return webrtc.RTPCodecCapability{}, false
}

// takesPart says whether the client's hello has been accepted. Until it has, the connection takes no part in
// its room: nothing is forwarded to it and nothing it sends is forwarded, so a client whose version would
// be refused gains nothing by never saying which it is.
func (c *conn) takesPart() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inRoom
}

// slotsByKind is the connection's slots, empty until it is negotiated.
func (c *conn) slotsByKind() map[string][]*slot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.slots
}

// onTrack starts forwarding a track the connection sends.
//
// No more tracks than the offer's sending sections: Pion matches a track to a section, so this holds by
// construction, and the count makes it hold whatever Pion does with an SSRC nobody declared.
func (c *conn) onTrack(track *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
	defer c.contain("a new track")
	c.mu.Lock()
	c.tracks++
	over := c.tracks > c.maxTracks
	c.mu.Unlock()
	if over {
		go c.close("sending more tracks than its offer said")
		return
	}
	src := c.room.addSource(c, track, r)
	if src == nil {
		return
	}
	c.room.changed()
	go c.drainReceiver(r)
	go func() {
		src.run()
		c.room.removeSource(src)
		c.room.changed()
	}()
}

// drainSender reads a slot's RTCP, which is the receiver's reports about this hop. Reading it is what runs
// the interceptors over it; nothing read here goes to the sender.
func (c *conn) drainSender(s *webrtc.RTPSender) {
	defer c.contain("a slot's RTCP")
	buf := make([]byte, 1500)
	for {
		if _, _, err := s.Read(buf); err != nil || !c.meterRTCP() {
			return
		}
	}
}

// drainReceiver reads a sent track's RTCP, the sender's reports about its own hop, for the same reason.
func (c *conn) drainReceiver(r *webrtc.RTPReceiver) {
	defer c.contain("a track's RTCP")
	buf := make([]byte, 1500)
	for {
		if _, _, err := r.Read(buf); err != nil || !c.meterRTCP() {
			return
		}
	}
}

// meterRTCP counts one RTCP datagram against the connection's bound, and closes a connection that strikes
// out. It says whether to go on reading.
func (c *conn) meterRTCP() bool {
	lim := c.sfu.lim
	c.mu.Lock()
	_, out := c.rtcp.admit(time.Now(), 0, lim.RTCPPerSecond, 0, lim.StrikesToEvict)
	c.mu.Unlock()
	if out {
		go c.close("sending RTCP past its bound")
		return false
	}
	return true
}

// testPanic, set only by tests, is called where a stranger's input is handled, to prove a panic there is
// contained. Atomic, since a test sets it while read loops run.
var testPanic atomic.Pointer[func(where string, input []byte)]

func panicHook(where string, input []byte) {
	if f := testPanic.Load(); f != nil {
		(*f)(where, input)
	}
}

// close ends the connection, once, for the reason given. It is safe from any goroutine but Pion's own
// callbacks, which go through a goroutine of their own.
func (c *conn) close(reason string) { c.closeFor(nil, reason) }

// closeFor is close, recording cause as what Provision answers if the connection was still being
// negotiated.
func (c *conn) closeFor(cause error, reason string) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.cause = cause
	pc := c.pc
	c.mu.Unlock()

	c.timer.Stop()
	close(c.done)
	c.sfu.forget(c)
	c.log.Debug().Str("reason", reason).Msg("voice connection closed")
	if pc != nil {
		if err := pc.Close(); err != nil && !errors.Is(err, webrtc.ErrConnectionClosed) {
			c.log.Warn().Err(err).Msg("closing a voice connection")
		}
	}
}
