// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
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

	mu        sync.Mutex
	pc        *webrtc.PeerConnection
	closed    bool
	connected bool
	timer     *time.Timer
	done      chan struct{}
}

func newConn(s *SFU, r *room, id, user, issued snowflake.ID) *conn {
	c := &conn{
		sfu:    s,
		room:   r,
		id:     id,
		user:   user,
		issued: issued,
		log:    s.log.With().Stringer("room", r.id).Stringer("conn", id).Logger(),
		done:   make(chan struct{}),
	}
	// A provision nobody connects to is dropped, or a client could hold places in rooms without ever
	// sending a packet: unbounded growth in the shape M6's sweeper fixed.
	c.timer = time.AfterFunc(s.lim.ProvisionTimeout, func() {
		c.mu.Lock()
		connected := c.connected
		c.mu.Unlock()
		if !connected {
			c.close("not connected within the provision timeout")
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
	pc, err := c.sfu.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = pc.Close()
		return "", ErrEvicted
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

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
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

	// Pion's own refusal is logged and not returned: its text can quote the offer, and what the client is
	// told says which rule was broken, not what it sent.
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		c.log.Debug().Err(err).Msg("an offer Pion would not apply")
		return "", fmt.Errorf("%w: it could not be applied", ErrBadOffer)
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
		return "", ErrEvicted
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return "", ErrEvicted
	}
	return pc.LocalDescription().SDP, nil
}

// close ends the connection, once, for the reason given. It is safe from any goroutine but Pion's own
// callbacks, which go through a goroutine of their own.
func (c *conn) close(reason string) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
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
