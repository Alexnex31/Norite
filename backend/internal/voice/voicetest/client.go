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
}

// Client is one voice connection's client end.
type Client struct {
	opts       Options
	pc         *webrtc.PeerConnection
	send       *webrtc.TrackLocalStaticRTP
	sender     *webrtc.RTPSender
	slotMids   []string
	recordings sync.WaitGroup

	mu       sync.Mutex
	received map[string][]Packet

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
	if _, err := c.pc.CreateDataChannel(voiceproto.Label, nil); err != nil {
		return "", err
	}
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
				Mid: mid, Seq: p.SequenceNumber, TS: p.Timestamp, CSRC: append([]uint32(nil), p.CSRC...),
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
	seq := uint16(rand.Uint32())
	ts := rand.Uint32()
	tick := time.NewTicker(opts.Pace)
	defer tick.Stop()
	for i, payload := range packets {
		p := &rtp.Packet{
			Header:  rtp.Header{Version: 2, Marker: i == 0, SequenceNumber: seq, Timestamp: ts},
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
		seq++
		ts += FrameSamples
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
