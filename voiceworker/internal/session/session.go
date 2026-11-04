// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package session is the voice-worker's connection to the SFU: one Pion peer connection, shaped as M26's
// SFU admits (one audio track sent, N slots received, one data channel with the label voiceproto.Label).
//
// It is backend/internal/voice/voicetest's client made the worker's, and the two are held to one protocol by
// contracts/voice-datachannel.schema.json rather than shared: the SFU's tests run that client in process,
// and the backend cannot import a client module, which may take copyleft (rule 22).
//
// From M28 to M29 it sends nothing on its track and drops what it receives. M29 adds the audio.
//
// Its DTLS certificate is Pion's, made for each peer connection and never kept, so every call has its own
// keypair (ADR 0023): nothing a worker held outlives its call.
package session

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"

	"github.com/Alexnex31/Norite/backend/voiceproto"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// Slots is how many audio slots the worker receives on. M26's SFU admits up to 16; three speakers at
// once is a conversation, and M34's subscription is where a client asks for more or fewer.
const Slots = 3

// Options configures a session.
type Options struct {
	// Version is what the data channel's hello names.
	Version string
	// Log receives Pion's own lines at warning and above. Pion's default writes to stdout.
	Log io.Writer
}

// Session is one call's connection.
type Session struct {
	opts Options
	pc   *webrtc.PeerConnection

	ready    chan struct{}
	ended    chan string
	endOnce  sync.Once
	readyOne sync.Once
}

// opus is the codec both ends register, the SFU's own.
var opus = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeOpus,
	ClockRate:   voiceproto.ClockRateAudio,
	Channels:    2,
	SDPFmtpLine: "minptime=10;useinbandfec=1",
}

// New builds a session's peer connection from a JOIN. Nothing is sent until Offer.
func New(join voiceipc.Join, opts Options) (*Session, error) {
	if opts.Log == nil {
		opts.Log = os.Stderr
	}
	media := &webrtc.MediaEngine{}
	if err := media.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: opus, PayloadType: 111},
		webrtc.RTPCodecTypeAudio); err != nil {
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

	logs := logging.NewDefaultLoggerFactory()
	logs.Writer = opts.Log
	logs.DefaultLogLevel = logging.LogLevelWarn

	se := webrtc.SettingEngine{LoggerFactory: logs}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetIncludeLoopbackCandidate(join.Loopback)
	se.SetRemoteIPFilter((&remotes{loopback: join.Loopback}).admit)

	servers := make([]webrtc.ICEServer, 0, len(join.ICEServers))
	for _, s := range join.ICEServers {
		servers = append(servers, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(media), webrtc.WithInterceptorRegistry(reg),
		webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: servers})
	if err != nil {
		return nil, err
	}
	s := &Session{opts: opts, pc: pc, ready: make(chan struct{}), ended: make(chan string, 1)}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { go drain(tr) })
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateFailed:
			s.end("the connection to the voice server failed")
		case webrtc.PeerConnectionStateClosed:
			s.end("the connection to the voice server closed")
		}
	})
	return s, nil
}

// drain reads a slot's packets and drops them, so they do not pile up in Pion's buffers. M29 decodes them.
func drain(tr *webrtc.TrackRemote) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := tr.Read(buf); err != nil {
			return
		}
	}
}

// end reports the call over, once, with why.
func (s *Session) end(reason string) {
	s.endOnce.Do(func() { s.ended <- reason })
}

// Offer adds the call's sections and returns the offer, every candidate gathered.
func (s *Session) Offer(ctx context.Context) (string, error) {
	track, err := webrtc.NewTrackLocalStaticRTP(opus, "audio", "norite-voice")
	if err != nil {
		return "", err
	}
	tr, err := s.pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	})
	if err != nil {
		return "", err
	}
	go drainRTCP(tr.Sender())
	for range Slots {
		if _, err := s.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			return "", err
		}
	}
	dc, err := s.pc.CreateDataChannel(voiceproto.Label, nil)
	if err != nil {
		return "", err
	}
	dc.OnOpen(func() {
		b, err := voiceproto.Encode(voiceproto.Hello{Op: voiceproto.OpHello, Version: s.opts.Version})
		if err == nil {
			err = dc.SendText(string(b))
		}
		if err != nil {
			s.end("the voice server's channel could not be greeted")
		}
	})
	dc.OnMessage(func(m webrtc.DataChannelMessage) { s.message(m.Data) })
	dc.OnClose(func() { s.end("the voice server closed the call") })

	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(s.pc)
	if err := s.pc.SetLocalDescription(offer); err != nil {
		return "", err
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	sdp := offered(s.pc.LocalDescription().SDP, voiceproto.MaxCandidates)
	if len(sdp) > voiceproto.MaxOffer {
		return "", fmt.Errorf("the offer is %d bytes, past the SFU's %d", len(sdp), voiceproto.MaxOffer)
	}
	return sdp, nil
}

func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}

// message reads one message from the SFU. Ready is the one this milestone acts on; a refusal ends the
// call with the SFU's reason, which is a stranger's text and is cut to what a STATE carries here and
// cleaned by the daemon where it is shown. The slot map and the speaking list are M34's.
func (s *Session) message(b []byte) {
	op, err := voiceproto.OpOf(b)
	if err != nil {
		return
	}
	switch op {
	case voiceproto.OpReady:
		s.readyOne.Do(func() { close(s.ready) })
	case voiceproto.OpRefused:
		var m voiceproto.Refused
		if voiceproto.Decode(b, &m) != nil {
			s.end("the voice server refused the call")
			return
		}
		s.end(voiceipc.CutReason("the voice server refused the call: " + m.Reason))
	}
}

// Accept applies the SFU's answer, which starts ICE.
func (s *Session) Accept(sdp string) error {
	return s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp})
}

// Ready is closed once the SFU has said ready, which it does only once connected.
func (s *Session) Ready() <-chan struct{} { return s.ready }

// Ended delivers why the call ended by itself, once.
func (s *Session) Ended() <-chan string { return s.ended }

// Close closes the peer connection. The SFU sees the data channel and DTLS close at once rather than
// waiting out ICE's timeout.
func (s *Session) Close() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.pc.GracefulClose()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// The worker is a full ICE agent and the controlling one, so it sends connectivity checks to every
// candidate the answer names, and the answer is the voice server's: from M33 an instance's, a stranger's
// server (M19). Unfiltered, an answer naming this machine's loopback, its link or a cloud's metadata
// service turns every client in a call into a sender of checks to that address, on the instance's
// say-so; and with no count, an answer near its 64 KiB names hundreds, each checked up to seven times,
// with a peer-reflexive candidate added for every further address a check arrives from. M27 found the
// same in the SFU, from the other side (backend/internal/voice/streams.go). Found by /security-sweep.
//
// Private ranges stay: a self-hosted instance on a LAN is in one, which is what its clients reach.

// maxRemotes is how many times the filter says yes: Pion asks about each signaled candidate twice, once as
// the answer is applied and once as ICE starts, and once about each peer-reflexive one. An SFU announces a
// candidate an address; voiceproto.MaxCandidates of them is more than any announces.
const maxRemotes = 3 * voiceproto.MaxCandidates

// metadata are cloud instance-metadata services: two sit in ranges the filter otherwise admits, AWS's
// IPv6 address in fc00::/7 and Alibaba's in 100.64/10. The SFU's list, which the backend's internal
// package keeps and this module cannot import.
var metadata = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"),
	netip.MustParseAddr("fd00:ec2::254"),
	netip.MustParseAddr("100.100.100.200"),
}

// remotes is a call's remote IP filter. Pion calls it on ICE's own goroutines.
type remotes struct {
	loopback bool
	mu       sync.Mutex
	admitted int
}

// admit is whether a remote candidate's address is one a voice server can have, and the bound is not
// spent.
func (r *remotes) admit(ip net.IP) bool {
	if !reachable(ip, r.loopback) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.admitted >= maxRemotes {
		return false
	}
	r.admitted++
	return true
}

// reachable refuses what is never a voice server: an unspecified, multicast or broadcast address, this
// host's link, a metadata service, and loopback unless the join admits it (M26's harness, and tests).
func reachable(ip net.IP, loopback bool) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return loopback
	case a.IsUnspecified(), a.IsMulticast(), a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(),
		a.IsInterfaceLocalMulticast():
		return false
	case a.Is4() && (a.As4()[0] == 0 || a == netip.AddrFrom4([4]byte{255, 255, 255, 255})):
		return false
	}
	return !slices.Contains(metadata, a)
}
