// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"fmt"
	"slices"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/voiceproto"
)

// maxBuffered is how much the SFU lets wait unsent on a data channel. Pion's send queue has no bound of
// its own, so a peer that never acknowledges would otherwise make the SFU hold every slot map it was ever
// sent; past this the connection is too slow to serve and is closed.
const maxBuffered = 64 << 10

// subscription is the client's own preference over who fills its slots.
type subscription struct {
	pin  []snowflake.ID
	drop map[snowflake.ID]bool
	// cap is how many slots the client wants filled, at most; -1 is all of them.
	cap int
}

var noSubscription = subscription{cap: -1}

// refusal is why a connection is refused, in a sentence for whoever wrote the client: it is sent to them
// as it is.
type refusal string

func (r refusal) Error() string { return string(r) }

func refusalf(format string, a ...any) error { return refusal(fmt.Sprintf(format, a...)) }

// onDataChannel takes the peer's one data channel and closes any other.
func (c *conn) onDataChannel(dc *webrtc.DataChannel) {
	defer c.contain("a new data channel")
	c.mu.Lock()
	extra := c.dc != nil || dc.Label() != voiceproto.Label
	if extra {
		c.extraChannels++
	} else {
		c.dc = dc
	}
	tooMany := c.extraChannels > c.sfu.lim.ExtraChannels
	c.mu.Unlock()

	if extra {
		_ = dc.Close()
		if tooMany {
			// From a goroutine of its own, like every close in a Pion callback: pc.Close here would run on
			// the goroutine Pion's accept loop is waiting on.
			go c.close("too many data channels")
		}
		return
	}
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		defer c.contain("a data-channel message")
		if err := c.onMessage(msg); err != nil {
			go c.refuse(err.Error())
		}
	})
}

// onMessage handles one message from the client.
func (c *conn) onMessage(msg webrtc.DataChannelMessage) error {
	// Past the bound a message is dropped unread, as a packet past its bound is; only a peer past twice it
	// for StrikesToEvict seconds is refused. A client that bursts, a few pins clicked at once, keeps its
	// call, and since a subscribe replaces the last whole, its next one makes good what was dropped.
	lim := c.sfu.lim
	c.mu.Lock()
	ok, out := c.messages.admit(time.Now(), 0, lim.MessagesPerSecond, 0, lim.StrikesToEvict)
	c.mu.Unlock()
	if out {
		return refusalf("more than %d messages a second", lim.MessagesPerSecond)
	}
	if !ok {
		return nil
	}
	panicHook("a data-channel message", msg.Data)
	if !msg.IsString || len(msg.Data) > voiceproto.MaxMessage {
		return refusalf("a message must be JSON text of at most %d bytes", voiceproto.MaxMessage)
	}
	op, err := voiceproto.OpOf(msg.Data)
	if err != nil {
		return refusalf("a message must name its op")
	}

	c.mu.Lock()
	helloed := c.helloed
	c.mu.Unlock()
	if !helloed {
		if op != voiceproto.OpHello {
			return refusalf("the first message must be hello")
		}
		return c.onHello(msg.Data)
	}

	switch op {
	case voiceproto.OpSubscribe:
		return c.onSubscribe(msg.Data)
	default:
		return refusalf("%q is not a message the client sends after hello", truncate(op, 32))
	}
}

func (c *conn) onHello(b []byte) error {
	var h voiceproto.Hello
	if err := voiceproto.Decode(b, &h); err != nil || len(h.Version) > voiceproto.MaxVersion {
		return refusalf("hello is malformed")
	}
	if compat := gatewayproto.Check(c.sfu.opts.Version, h.Version); !compat.Compatible {
		return refusal(compat.Reason)
	}

	ready := voiceproto.Ready{Op: voiceproto.OpReady, Version: c.sfu.opts.Version, Slots: []voiceproto.SlotIdent{}}
	for _, kind := range []string{voiceproto.KindAudio, voiceproto.KindVideo} {
		for _, s := range c.slotsByKind()[kind] {
			ready.Slots = append(ready.Slots, voiceproto.SlotIdent{Slot: s.index, Kind: s.kind, Mid: s.mid})
		}
	}
	// The connection takes part in its room from here, so the room chooses again now, before ready goes
	// out: a client may send as soon as it reads ready, and a packet arriving before the room counted it
	// would be dropped. Once per connection, unlike a subscribe. What the choice gives this connection is
	// recorded, not sent, until it is helloed below.
	c.mu.Lock()
	c.inRoom = true
	c.mu.Unlock()
	c.room.changed()
	// Ready goes first: a reassignment racing this waits on sendMu, and finds the client helloed only once
	// ready is out.
	func() {
		// Deferred, for the reason room.assign's is: a contained panic must not leave sendMu held.
		c.sendMu.Lock()
		defer c.sendMu.Unlock()
		c.sendLocked(ready)
		c.mu.Lock()
		c.helloed = true
		c.mu.Unlock()
	}()
	// Then the slot map, sent even when the choice left it empty, since the client has had none yet, and
	// who is speaking if anybody is.
	c.flushSlots()
	c.flushSpeaking()
	return nil
}

func (c *conn) onSubscribe(b []byte) error {
	var m voiceproto.Subscribe
	if err := voiceproto.Decode(b, &m); err != nil {
		return refusalf("subscribe is malformed")
	}
	if len(m.Pin) > voiceproto.MaxSubscribeList || len(m.Drop) > voiceproto.MaxSubscribeList {
		return refusalf("subscribe names more than %d users", voiceproto.MaxSubscribeList)
	}
	sub := subscription{drop: map[snowflake.ID]bool{}, cap: -1}
	for _, s := range m.Pin {
		id, err := snowflake.Parse(s)
		if err != nil {
			return refusalf("subscribe names an id that is not one")
		}
		if !slices.Contains(sub.pin, id) {
			sub.pin = append(sub.pin, id)
		}
	}
	for _, s := range m.Drop {
		id, err := snowflake.Parse(s)
		if err != nil {
			return refusalf("subscribe names an id that is not one")
		}
		sub.drop[id] = true
	}
	if m.Cap != nil {
		if *m.Cap < 0 {
			return refusalf("a cap is not negative")
		}
		sub.cap = *m.Cap
	}
	c.mu.Lock()
	c.sub = sub
	c.mu.Unlock()
	// Applied by the room's next tick, within selectEvery, rather than here: a subscribe arrives on Pion's
	// data-channel read loop, and a room recomputed per message is a peer choosing how much of the
	// room's CPU to spend, twenty times a second each.
	return nil
}

// subscription is the client's preference as it stands.
func (c *conn) subscription() subscription {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sub
}

// setSlotMap records the connection's slot map, to be sent by flushSlots. Called under the room's lock,
// which is why it only records: sending, and closing a connection too slow to take it, happen after.
func (c *conn) setSlotMap(m []voiceproto.SlotEntry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if slices.Equal(c.slotMap, m) {
		return false
	}
	c.slotMap = m
	c.mapVersion++
	return true
}

// flushSlots sends the slot map as it is now, unless that version has gone already. Two reassignments
// racing each flush after the room's lock, in either order; each sends the newest map, never the one it
// computed, so the last to arrive is the current one. The first flush after hello always sends, since an
// empty map is version zero.
func (c *conn) flushSlots() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.mu.Lock()
	m, v, helloed := c.slotMap, c.mapVersion, c.helloed
	c.mu.Unlock()
	if !helloed || (c.mapSentOnce && v <= c.mapSent) {
		return
	}
	c.mapSent, c.mapSentOnce = v, true
	if m == nil {
		m = []voiceproto.SlotEntry{}
	}
	c.sendLocked(voiceproto.Slots{Op: voiceproto.OpSlots, Map: m})
}

// setSpeaking records who the connection hears speaking, to be sent by flushSpeaking, as setSlotMap does
// for the slot map.
func (c *conn) setSpeaking(users []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if slices.Equal(c.speaking, users) {
		return false
	}
	c.speaking = users
	c.speakingVersion++
	return true
}

// flushSpeaking sends who is speaking now, unless that version has gone already.
func (c *conn) flushSpeaking() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.mu.Lock()
	users, v, helloed := c.speaking, c.speakingVersion, c.helloed
	c.mu.Unlock()
	if !helloed || v <= c.speakingSent {
		return
	}
	c.speakingSent = v
	if users == nil {
		users = []string{}
	}
	c.sendLocked(voiceproto.Speaking{Op: voiceproto.OpSpeaking, Users: users})
}

// send sends one message.
func (c *conn) send(v any) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.sendLocked(v)
}

func (c *conn) sendLocked(v any) {
	c.mu.Lock()
	dc := c.dc
	c.mu.Unlock()
	if dc == nil {
		return
	}
	b, err := voiceproto.Encode(v)
	if err != nil {
		c.log.Error().Err(err).Msg("encoding a data-channel message")
		return
	}
	if dc.BufferedAmount() > maxBuffered {
		go c.close("too slow to read its data channel")
		return
	}
	if err := dc.SendText(string(b)); err != nil {
		c.log.Debug().Err(err).Msg("sending on a data channel")
	}
}

// refuse tells the client why, then closes the connection. Closing at once would tear down the association
// with the reason still queued, so it waits, briefly, for the reason to leave.
func (c *conn) refuse(reason string) {
	defer c.contain("a refusal")
	// A reason can quote what the client sent; it is cut so the refusal stays within the contract's size.
	reason = truncate(reason, maxReason)
	// Once: a peer sending one bad message after another gets one refusal and one close.
	c.mu.Lock()
	refused := c.refused
	c.refused = true
	c.mu.Unlock()
	if refused {
		return
	}
	c.send(voiceproto.Refused{Op: voiceproto.OpRefused, Reason: reason})
	c.mu.Lock()
	dc := c.dc
	c.mu.Unlock()
	for range 50 {
		if dc == nil || dc.BufferedAmount() == 0 {
			break
		}
		select {
		case <-c.done:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	// The amount buffered is what SCTP has not handed to DTLS, not what the peer has read; a moment more
	// lets the last datagram land before the alert that ends the connection.
	select {
	case <-c.done:
	case <-time.After(100 * time.Millisecond):
	}
	c.close("refused: " + reason)
}

// maxReason is the longest refusal reason sent, well inside voiceproto.MaxMessage with JSON's escaping.
const maxReason = 512

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
