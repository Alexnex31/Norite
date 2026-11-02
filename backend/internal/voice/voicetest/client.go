// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package voicetest is a voice client for the SFU's tests and the harness: a Pion peer connection shaped
// as the voice-worker's will be (one track sent, N slots received, one data channel), sending real Opus
// from an Ogg file and recording what it receives.
//
// It does not import the voice package, so the SFU's own tests can use it.
package voicetest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/Alexnex31/Norite/backend/voiceproto"
)

// Options configures a Client.
type Options struct {
	// Slots is how many audio slots the client receives on.
	Slots int
	// Loopback admits loopback candidates, for a client and an SFU on one machine.
	Loopback bool
	// Version is what the client's hello names; empty is "dev".
	Version string
	// NoHello leaves the hello unsent, for a test that sends something else first.
	NoHello bool
}

// Client is one voice connection's client end.
type Client struct {
	opts       Options
	pc         *webrtc.PeerConnection
	send       *webrtc.TrackLocalStaticRTP
	sender     *webrtc.RTPSender
	slotMids   []string
	recordings sync.WaitGroup

	dc *webrtc.DataChannel

	// seq and ts carry on from one Send to the next, as a real sender's do. A fresh random start per call
	// jumps the sequence by up to 2^16, which SRTP's index estimate can read as an old packet and drop
	// the whole burst as a replay.
	sendMu sync.Mutex
	seq    uint16
	ts     uint32
	lastAt time.Time

	mu       sync.Mutex
	received map[string][]Packet
	// messages are what arrived on the data channel, and sent what went out, raw, in order.
	messages [][]byte
	sent     [][]byte
	arrived  chan struct{}
	openDC   chan struct{}
	closedDC chan struct{}

	connected chan struct{}
	failed    chan struct{}
	once      sync.Once
	failOnce  sync.Once
}

// New builds a client and its peer connection. Nothing is sent until Offer.
func New(opts Options) (*Client, error) {
	if opts.Slots < 1 {
		opts.Slots = 1
	}
	media := &webrtc.MediaEngine{}
	if err := media.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: OpusCapability,
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := media.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: voiceproto.AudioLevelURI}, webrtc.RTPCodecTypeAudio,
	); err != nil {
		return nil, err
	}
	reg := &interceptor.Registry{}
	if err := webrtc.ConfigureRTCPReports(reg); err != nil {
		return nil, err
	}

	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetIncludeLoopbackCandidate(opts.Loopback)
	se.SetICETimeouts(3*time.Second, 6*time.Second, time.Second)

	api := webrtc.NewAPI(webrtc.WithMediaEngine(media), webrtc.WithInterceptorRegistry(reg),
		webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	c := &Client{
		opts:      opts,
		pc:        pc,
		received:  map[string][]Packet{},
		seq:       uint16(rand.Uint32()),
		ts:        rand.Uint32(),
		arrived:   make(chan struct{}, 1),
		openDC:    make(chan struct{}),
		closedDC:  make(chan struct{}),
		connected: make(chan struct{}),
		failed:    make(chan struct{}),
	}
	pc.OnTrack(c.record)
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateConnected:
			c.once.Do(func() { close(c.connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			c.failOnce.Do(func() { close(c.failed) })
		}
	})
	return c, nil
}

// OpusCapability is the codec both ends register.
var OpusCapability = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeOpus,
	ClockRate:   voiceproto.ClockRateAudio,
	Channels:    2,
	SDPFmtpLine: "minptime=10;useinbandfec=1",
}

// Offer adds the client's sections (one track sent, the slots, the data channel) and returns the offer,
// with every candidate gathered.
func (c *Client) Offer(ctx context.Context) (string, error) {
	track, err := webrtc.NewTrackLocalStaticRTP(OpusCapability, "audio", "voicetest")
	if err != nil {
		return "", err
	}
	tr, err := c.pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	})
	if err != nil {
		return "", err
	}
	c.send, c.sender = track, tr.Sender()
	go drainRTCP(tr.Sender())
	slots := make([]*webrtc.RTPTransceiver, 0, c.opts.Slots)
	for range c.opts.Slots {
		tr, err := c.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		})
		if err != nil {
			return "", err
		}
		slots = append(slots, tr)
	}
	dc, err := c.pc.CreateDataChannel(voiceproto.Label, nil)
	if err != nil {
		return "", err
	}
	c.dc = dc
	dc.OnOpen(func() {
		defer close(c.openDC)
		if c.opts.NoHello {
			return
		}
		version := c.opts.Version
		if version == "" {
			version = "dev"
		}
		_ = c.SendMessage(voiceproto.Hello{Op: voiceproto.OpHello, Version: version})
	})
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		c.mu.Lock()
		c.messages = append(c.messages, append([]byte(nil), m.Data...))
		c.mu.Unlock()
		select {
		case c.arrived <- struct{}{}:
		default:
		}
	})
	dc.OnClose(func() { close(c.closedDC) })
	offer, err := c.pc.CreateOffer(nil)
	if err != nil {
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(c.pc)
	if err := c.pc.SetLocalDescription(offer); err != nil {
		return "", err
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	c.mu.Lock()
	for _, tr := range slots {
		c.slotMids = append(c.slotMids, tr.Mid())
	}
	c.mu.Unlock()
	return c.pc.LocalDescription().SDP, nil
}

// Accept applies the SFU's answer, which starts ICE.
func (c *Client) Accept(answer string) error {
	return c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer})
}

// ErrFailed is a connection that failed or was closed before it connected.
var ErrFailed = errors.New("voicetest: the connection failed")

// WaitConnected waits for ICE and DTLS to finish.
func (c *Client) WaitConnected(ctx context.Context) error {
	select {
	case <-c.connected:
		return nil
	case <-c.failed:
		return ErrFailed
	case <-ctx.Done():
		return fmt.Errorf("voicetest: not connected: %w", ctx.Err())
	}
}

// Failed is closed once the connection has failed or been closed.
func (c *Client) Failed() <-chan struct{} { return c.failed }

// Connected is closed once the connection is up.
func (c *Client) Connected() <-chan struct{} { return c.connected }

// Close closes the peer connection and waits for its recordings to stop.
func (c *Client) Close() error {
	err := c.pc.Close()
	c.recordings.Wait()
	return err
}

// Packet is one packet received on a slot.
type Packet struct {
	Mid    string
	SSRC   uint32
	Seq    uint16
	TS     uint32
	CSRC   []uint32
	Marker bool
	// Level is the RFC 6464 level the packet carried, or -1 for none.
	Level   int
	Payload []byte
	// Extensions counts the header extensions the packet carried.
	Extensions int
	At         time.Time
}

// SendMessage encodes and sends a message on the data channel.
func (c *Client) SendMessage(v any) error {
	b, err := voiceproto.Encode(v)
	if err != nil {
		return err
	}
	return c.SendRaw(b)
}

// SendRaw sends bytes on the data channel as they are.
func (c *Client) SendRaw(b []byte) error {
	c.mu.Lock()
	c.sent = append(c.sent, append([]byte(nil), b...))
	c.mu.Unlock()
	return c.dc.SendText(string(b))
}

// SendRawUnchecked sends bytes and keeps them out of Sent, for a message the contract excludes on purpose.
func (c *Client) SendRawUnchecked(b []byte) error { return c.dc.SendText(string(b)) }

// Subscribe sends a subscription naming users by id.
func (c *Client) Subscribe(pin, drop []string, slotCap *int) error {
	if pin == nil {
		pin = []string{}
	}
	if drop == nil {
		drop = []string{}
	}
	return c.SendMessage(voiceproto.Subscribe{Op: voiceproto.OpSubscribe, Pin: pin, Drop: drop, Cap: slotCap})
}

// Messages is everything that arrived on the data channel, raw.
func (c *Client) Messages() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.messages...)
}

// Sent is everything the client sent on the data channel, raw.
func (c *Client) Sent() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.sent...)
}

// WaitFor waits for a message for which match is true, looking at every message so far and then at each
// as it arrives, and returns it.
func (c *Client) WaitFor(ctx context.Context, match func(op string, raw []byte) bool) ([]byte, error) {
	seen := 0
	for {
		msgs := c.Messages()
		for _, m := range msgs[seen:] {
			if op, err := voiceproto.OpOf(m); err == nil && match(op, m) {
				return m, nil
			}
		}
		seen = len(msgs)
		select {
		case <-c.arrived:
		case <-ctx.Done():
			return nil, fmt.Errorf("voicetest: no such message: %w", ctx.Err())
		}
	}
}

// LatestSlots is the newest slot map that arrived, and whether one has.
func (c *Client) LatestSlots() (voiceproto.Slots, bool) {
	msgs := c.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if op, _ := voiceproto.OpOf(msgs[i]); op == voiceproto.OpSlots {
			var m voiceproto.Slots
			if voiceproto.Decode(msgs[i], &m) == nil {
				return m, true
			}
		}
	}
	return voiceproto.Slots{}, false
}

// OpenChannel opens another data channel, which the SFU should close.
func (c *Client) OpenChannel(label string) (*webrtc.DataChannel, error) {
	return c.pc.CreateDataChannel(label, nil)
}

// ChannelOpen is closed once the data channel is open, and the hello, unless asked not to, sent.
func (c *Client) ChannelOpen() <-chan struct{} { return c.openDC }

// ChannelClosed is closed once the data channel is.
func (c *Client) ChannelClosed() <-chan struct{} { return c.closedDC }

// FloodRTCP sends n picture-loss indications about ssrc, pace apart: RTCP a hostile peer sends.
func (c *Client) FloodRTCP(ctx context.Context, ssrc uint32, n int, pace time.Duration) error {
	tick := time.NewTicker(pace)
	defer tick.Stop()
	for range n {
		if err := c.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}}); err != nil {
			return err
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// SlotMids are the mids of the client's receiving sections, in slot order.
func (c *Client) SlotMids() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.slotMids...)
}

// Received is every packet received on the slot with that mid, in arrival order.
func (c *Client) Received(mid string) []Packet {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Packet(nil), c.received[mid]...)
}

// ReceivedAll is every packet received on any slot, in arrival order per slot, slot after slot.
func (c *Client) ReceivedAll() []Packet {
	c.mu.Lock()
	defer c.mu.Unlock()
	var all []Packet
	for _, mid := range c.slotMids {
		all = append(all, c.received[mid]...)
	}
	return all
}

// record keeps every packet a slot receives.
func (c *Client) record(track *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
	mid := ""
	for _, tr := range c.pc.GetTransceivers() {
		if tr.Receiver() == r {
			mid = tr.Mid()
		}
	}
	levelID := extensionID(r.GetParameters().HeaderExtensions, voiceproto.AudioLevelURI)
	c.recordings.Add(1)
	go func() {
		defer c.recordings.Done()
		go drainReceiverRTCP(r)
		for {
			p, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			got := Packet{
				Mid: mid, SSRC: p.SSRC, Seq: p.SequenceNumber, TS: p.Timestamp, CSRC: append([]uint32(nil), p.CSRC...),
				Marker: p.Marker, Level: -1, Payload: append([]byte(nil), p.Payload...),
				Extensions: len(p.Extensions), At: time.Now(),
			}
			if levelID != 0 {
				if b := p.GetExtension(levelID); b != nil {
					var lvl rtp.AudioLevelExtension
					if lvl.Unmarshal(b) == nil {
						got.Level = int(lvl.Level)
					}
				}
			}
			c.mu.Lock()
			c.received[mid] = append(c.received[mid], got)
			c.mu.Unlock()
		}
	}()
}

// SendOptions shapes what Send sends.
type SendOptions struct {
	// Level is the RFC 6464 level on every packet: 0 is loudest, 127 silence. Nil sends none.
	Level func(i int) *uint8
	// Pace is the interval between packets; zero is one 20 ms frame.
	Pace time.Duration
	// Extra, when set, adds a header extension of no negotiated meaning under id 14 to every packet: what
	// a sender might put there that the SFU must not forward.
	Extra []byte
}

// Loud is a level a speaking client sends.
func Loud(int) *uint8 { l := uint8(20); return &l }

// Quiet is a level a client sends in silence.
func Quiet(int) *uint8 { l := uint8(127); return &l }

// Send sends Opus packets on the client's track, one frame apart, as a voice-worker would: one sequence
// number and one frame of timestamp each.
func (c *Client) Send(ctx context.Context, packets [][]byte, opts SendOptions) error {
	if opts.Pace == 0 {
		opts.Pace = 20 * time.Millisecond
	}
	levelID := extensionID(c.sender.GetParameters().HeaderExtensions, voiceproto.AudioLevelURI)
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if !c.lastAt.IsZero() {
		// The clock runs on through a pause, as a capture clock does.
		c.ts += uint32(time.Since(c.lastAt).Seconds() * voiceproto.ClockRateAudio)
	}
	defer func() { c.lastAt = time.Now() }()
	tick := time.NewTicker(opts.Pace)
	defer tick.Stop()
	for i, payload := range packets {
		p := &rtp.Packet{
			Header:  rtp.Header{Version: 2, Marker: i == 0, SequenceNumber: c.seq, Timestamp: c.ts},
			Payload: payload,
		}
		if opts.Level != nil && levelID != 0 {
			if lvl := opts.Level(i); lvl != nil {
				b, err := rtp.AudioLevelExtension{Level: *lvl, Voice: *lvl < 127}.Marshal()
				if err != nil {
					return err
				}
				if err := p.SetExtension(levelID, b); err != nil {
					return err
				}
			}
		}
		if opts.Extra != nil {
			if err := p.SetExtension(14, opts.Extra); err != nil {
				return err
			}
		}
		if err := c.send.WriteRTP(p); err != nil {
			return err
		}
		c.seq++
		c.ts += FrameSamples
		select {
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func extensionID(exts []webrtc.RTPHeaderExtensionParameter, uri string) uint8 {
	for _, e := range exts {
		if e.URI == uri {
			return uint8(e.ID)
		}
	}
	return 0
}

// drainRTCP reads a sender's RTCP so its interceptors see it.
func drainRTCP(s *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := s.Read(buf); err != nil {
			return
		}
	}
}

func drainReceiverRTCP(r *webrtc.RTPReceiver) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := r.Read(buf); err != nil {
			return
		}
	}
}
