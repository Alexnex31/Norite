// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package session is the voice-worker's connection to the SFU: one Pion peer connection, shaped as M26's
// SFU admits (one audio track sent, N slots received, one data channel with the label voiceproto.Label).
//
// It is backend/internal/voice/voicetest's client made the worker's, and the two are held to one protocol by
// contracts/voice-datachannel.schema.json rather than shared: the SFU's tests run that client in process,
// and the backend cannot import a client module, which may take copyleft (rule 22).
//
// From M29 it carries the call's audio (media.go), once the SFU says ready.
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
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"

	"github.com/Alexnex31/Norite/backend/voiceproto"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

// Slots is how many audio slots the worker receives on. M26's SFU admits up to 16; three speakers at
// once is a conversation, and M34's subscription is where a client asks for more or fewer.
const Slots = 3

// Options configures a session.
type Options struct {
	// Version is what the data channel's hello names.
	Version string
	// Log receives Pion's own lines at warning and above, and the session's. Pion's default writes to
	// stdout.
	Log io.Writer
	// Device is the microphone and speakers. Nil carries no audio, and drops what the slots receive.
	Device audio.Device
	// Codec makes the call's codec, opus.New unless set. It starts compiling as the session is made, so
	// that its ~100 ms run while ICE connects.
	Codec func(context.Context) (opus.Codec, error)
	// Lossy, when set, is asked before each packet is sent and drops it on true: a test's loss.
	Lossy func() bool
	// RelayOnly gathers relayed candidates alone, so the call crosses TURN: a test's network that refuses
	// UDP to the SFU.
	RelayOnly bool
}

// Session is one call's connection.
type Session struct {
	opts Options
	pc   *webrtc.PeerConnection

	ready    chan struct{}
	ended    chan string
	endOnce  sync.Once
	readyOne sync.Once

	track  *webrtc.TrackLocalStaticRTP
	sender *webrtc.RTPSender
	level  atomic.Uint32 // the audio level's extension id, once negotiated; zero before

	codecDone chan struct{}
	codec     opus.Codec
	codecErr  error

	mediaMu       sync.Mutex
	media         *media
	mediaStarting bool
	closing       bool
	pending       []remote
}

// remote is a slot's track and the receiver it arrived on, whose parameters say which id the audio level
// came under.
type remote struct {
	tr  *webrtc.TrackRemote
	rcv *webrtc.RTPReceiver
}

// opusCapability is the codec both ends register, the SFU's own.
var opusCapability = webrtc.RTPCodecCapability{
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
	engine := &webrtc.MediaEngine{}
	if err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: opusCapability, PayloadType: 111},
		webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := engine.RegisterHeaderExtension(
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
	api := webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(reg),
		webrtc.WithSettingEngine(se))
	cfg := webrtc.Configuration{ICEServers: servers}
	if opts.RelayOnly {
		cfg.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, err
	}
	s := &Session{opts: opts, pc: pc, ready: make(chan struct{}), ended: make(chan string, 1),
		codecDone: make(chan struct{})}
	if opts.Device != nil {
		mk := opts.Codec
		if mk == nil {
			mk = opus.New
		}
		go func() {
			defer close(s.codecDone)
			s.codec, s.codecErr = mk(context.Background())
		}()
	} else {
		close(s.codecDone)
	}
	pc.OnTrack(func(tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) { s.trackArrived(remote{tr, rcv}) })
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

// trackArrived is a slot's first packet. Before the audio has started it waits, a slot's worth at most.
func (s *Session) trackArrived(tr remote) {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	switch {
	case s.media != nil:
		go s.media.attach(tr)
	case s.opts.Device == nil || s.closing || len(s.pending) >= Slots:
		go drain(tr.tr)
	default:
		s.pending = append(s.pending, tr)
	}
}

// adopt makes m the call's audio, unless the call closed while it started, and gives it the slots that
// arrived first.
func (s *Session) adopt(m *media) bool {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	if s.closing {
		return false
	}
	s.media = m
	for _, tr := range s.pending {
		go m.attach(tr)
	}
	s.pending = nil
	return true
}

func (s *Session) slotCount() int { return Slots }

// runMedia starts the audio once the SFU has said ready.
func (s *Session) runMedia() {
	s.mediaMu.Lock()
	if s.closing {
		s.mediaMu.Unlock()
		return
	}
	s.mediaStarting = true
	s.mediaMu.Unlock()
	if err := s.startMedia(context.Background()); err != nil {
		s.end(voiceipc.CutReason("the call's audio could not start: " + err.Error()))
	}
}

func (s *Session) awaitCodec(ctx context.Context) (opus.Codec, error) {
	select {
	case <-s.codecDone:
		if s.codecErr != nil {
			return nil, s.codecErr
		}
		return s.codec, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// resolveLevel reads the audio level's extension id from the sender once the answer is in, so a frame
// never asks Pion for it: GetParameters builds the sender's parameters afresh under a lock, and an answer
// that left the extension out would otherwise have had every frame ask again.
func (s *Session) resolveLevel() {
	if s.sender == nil {
		return
	}
	for _, e := range s.sender.GetParameters().HeaderExtensions {
		if e.URI == voiceproto.AudioLevelURI && e.ID > 0 && e.ID < 256 {
			s.level.Store(uint32(e.ID)) //nolint:gosec // checked above
			return
		}
	}
}

// levelID is the audio level's extension id as negotiated, zero if the answer left it out.
func (s *Session) levelID() uint8 {
	return uint8(s.level.Load()) //nolint:gosec // an extension id, at most 255
}

func (s *Session) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.opts.Log, "norite-voice: "+format+"\n", args...)
}

// drain reads a slot's packets and drops them, so they do not pile up in Pion's buffers: a session with
// no audio, or a slot past the mix's.
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
	track, err := webrtc.NewTrackLocalStaticRTP(opusCapability, "audio", "norite-voice")
	if err != nil {
		return "", err
	}
	tr, err := s.pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	})
	if err != nil {
		return "", err
	}
	s.track, s.sender = track, tr.Sender()
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
		s.readyOne.Do(func() {
			close(s.ready)
			if s.opts.Device != nil {
				go s.runMedia()
			}
		})
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
	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		return err
	}
	s.resolveLevel()
	return nil
}

// Ready is closed once the SFU has said ready, which it does only once connected.
func (s *Session) Ready() <-chan struct{} { return s.ready }

// Ended delivers why the call ended by itself, once.
func (s *Session) Ended() <-chan string { return s.ended }

// Close closes the peer connection. The SFU sees the data channel and DTLS close at once rather than
// waiting out ICE's timeout.
func (s *Session) Close() {
	s.mediaMu.Lock()
	s.closing = true
	m, starting := s.media, s.mediaStarting
	s.mediaMu.Unlock()
	if m != nil {
		m.close()
	} else if !starting {
		// The codec compiled for a call that never got as far as its audio.
		go func() {
			<-s.codecDone
			if s.codec != nil {
				_ = s.codec.Close(context.Background())
			}
		}()
	}
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
