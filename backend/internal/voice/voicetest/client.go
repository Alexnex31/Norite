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
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
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
	opts Options
	pc   *webrtc.PeerConnection
	send *webrtc.TrackLocalStaticRTP

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
		connected: make(chan struct{}),
		failed:    make(chan struct{}),
	}
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
	if _, err := c.pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	}); err != nil {
		return "", err
	}
	c.send = track
	for range c.opts.Slots {
		if _, err := c.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			return "", err
		}
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

// Close closes the peer connection.
func (c *Client) Close() error { return c.pc.Close() }
