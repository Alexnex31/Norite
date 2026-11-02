// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/voiceproto"
)

// Control is the SFU's half of ADR 0023's control channel: what the backend tells the SFU. On a self-hosted
// instance the SFU shares the backend's process and this is a Go call; on the flagship it is carried over
// mTLS to SFU pods (M116). It is settled whole at M26 because changing it after that changes a wire
// protocol.
//
// A room is a voice channel's id. Admission is keyed by connection, which the backend mints, so one user
// may hold two connections in one room: a second machine, or M105's separate video connection.
type Control interface {
	// Provision admits one connection and returns the SDP answer to the client's offer. issued is a
	// snowflake the backend took when it authorized the connection; Evict compares against it.
	Provision(ctx context.Context, room, user, conn, issued snowflake.ID, offer string) (string, error)
	// Evict closes the user's connections in the room issued at or before cutoff, and refuses any
	// provision issued at or before it that arrives afterwards.
	Evict(ctx context.Context, room, user, cutoff snowflake.ID) error
	// EvictConnection closes one connection, and refuses that connection's provision if it arrives
	// afterwards. A connection already gone is not an error.
	EvictConnection(ctx context.Context, conn snowflake.ID) error
	// SetExclusion sets the users whose tracks are never forwarded to receiver, over the client's own
	// subscription; a pin does not override it. Empty until M70 decides what fills it.
	SetExclusion(ctx context.Context, room, receiver snowflake.ID, excluded []snowflake.ID) error
	// CloseRoom closes every connection in the room, and refuses any provision for the room issued at or
	// before cutoff that arrives afterwards: a deleted channel is not reopened by a join already in flight.
	CloseRoom(ctx context.Context, room, cutoff snowflake.ID) error
}

// Errors Provision answers with. M33 maps each to what the client is told.
var (
	// ErrFull is a bound reached: rooms, participants, or one user's connections.
	ErrFull = errors.New("voice: the SFU is at a limit")
	// ErrEvicted is a provision issued before an eviction's cutoff.
	ErrEvicted = errors.New("voice: the connection was evicted before it was provisioned")
	// ErrStale is a provision issued longer ago than the provision timeout.
	ErrStale = errors.New("voice: the provision is too old")
	// ErrDuplicate is a connection id already provisioned.
	ErrDuplicate = errors.New("voice: the connection is already provisioned")
	// ErrNotFound is a receiver with no connection in the room.
	ErrNotFound = errors.New("voice: no such connection in the room")
	// ErrClosed is an SFU that has been closed.
	ErrClosed = errors.New("voice: the SFU is closed")
)

// Options configures an SFU.
type Options struct {
	// Conn is the one UDP socket every connection's media arrives on, through ICE's UDP mux. The SFU owns
	// it from New and closes it on Close.
	Conn net.PacketConn
	// Loopback admits loopback candidates, for tests and the harness on one machine.
	Loopback bool
	// Version is the server's version, against which a data channel's hello is checked.
	Version string
	Limits  Limits
	Logger  zerolog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// SFU is the selective forwarding unit: rooms of connections, each sending its tracks and receiving others'
// over pre-allocated slots.
type SFU struct {
	opts Options
	lim  Limits
	log  zerolog.Logger
	mux  ice.UDPMux
	now  func() time.Time

	// media, reg and se are what every connection's API is built from. Each connection gets an API of its
	// own because its SettingEngine carries that connection's buffer factory (streams.go).
	media *webrtc.MediaEngine
	reg   *interceptor.Registry
	se    webrtc.SettingEngine

	mu      sync.Mutex
	closed  bool
	rooms   map[snowflake.ID]*room
	conns   map[snowflake.ID]*conn
	cutoffs map[roomUser]cutoff
	// closedRooms and evictedConns are CloseRoom's and EvictConnection's memory, kept as cutoffs are.
	closedRooms  map[snowflake.ID]cutoff
	evictedConns map[snowflake.ID]time.Time

	stop chan struct{}
	wg   sync.WaitGroup
}

type roomUser struct{ room, user snowflake.ID }

// cutoff is an eviction remembered until no provision it refuses can still arrive.
type cutoff struct {
	id    snowflake.ID
	until time.Time
}

var _ Control = (*SFU)(nil)

// New starts an SFU on opts.Conn.
func New(opts Options) (*SFU, error) {
	if opts.Conn == nil {
		return nil, errors.New("voice: no UDP socket")
	}
	// The zero Limits is the defaults. Anything else is the caller's whole, and checked whole: a Limits
	// with one field set and the rest left at zero would admit nobody.
	if reflect.ValueOf(opts.Limits).IsZero() {
		opts.Limits = DefaultLimits()
	}
	if err := opts.Limits.validate(); err != nil {
		return nil, err
	}
	// The hello is checked against it; empty, every client's version is refused.
	if opts.Version == "" {
		return nil, errors.New("voice: no server version")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	logs := pionLogs{log: opts.Logger.With().Str("subsystem", "voice").Logger()}

	media := &webrtc.MediaEngine{}
	if err := media.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: opusCapability,
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := media.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: voiceproto.AudioLevelURI}, webrtc.RTPCodecTypeAudio,
	); err != nil {
		return nil, err
	}

	// RTCP is terminated per hop: these interceptors send receiver reports to senders and sender reports
	// for each slot to receivers, each in that hop's own SSRC and clock, and the SFU forwards no RTCP from
	// one hop into another. Transport-wide congestion control is per hop too, which is the shape M31 needs.
	// No NACK: audio relies on Opus's FEC, and video's NACK and PLI are M107's.
	reg := &interceptor.Registry{}
	if err := webrtc.ConfigureRTCPReports(reg); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureTWCCSender(media, reg); err != nil {
		return nil, err
	}

	mux := webrtc.NewICEUDPMux(logs.NewLogger("ice-mux"), opts.Conn)
	se := webrtc.SettingEngine{LoggerFactory: logs}
	se.SetICEUDPMux(mux)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetIncludeLoopbackCandidate(opts.Loopback)
	se.SetICETimeouts(5*time.Second, 10*time.Second, 2*time.Second)
	se.SetSCTPMaxReceiveBufferSize(4 * voiceproto.MaxMessage)
	se.SetSCTPMaxMessageSize(voiceproto.MaxMessage)

	s := &SFU{
		opts:    opts,
		lim:     opts.Limits,
		log:     opts.Logger.With().Str("subsystem", "voice").Logger(),
		media:   media,
		reg:     reg,
		se:      se,
		mux:     mux,
		now:     opts.Now,
		rooms:   map[snowflake.ID]*room{},
		conns:   map[snowflake.ID]*conn{},
		cutoffs: map[roomUser]cutoff{},

		closedRooms:  map[snowflake.ID]cutoff{},
		evictedConns: map[snowflake.ID]time.Time{},
		stop:         make(chan struct{}),
	}
	s.wg.Add(1)
	go s.sweep()
	return s, nil
}

var opusCapability = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeOpus,
	ClockRate:   voiceproto.ClockRateAudio,
	Channels:    2,
	SDPFmtpLine: "minptime=10;useinbandfec=1",
}

// Close closes every connection and the socket.
func (s *SFU) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	all := make([]*conn, 0, len(s.conns))
	for _, c := range s.conns {
		all = append(all, c)
	}
	// Every room is shut first, so the closes below choose no slots again and send nobody a slot map:
	// all of it would go to connections about to close.
	for _, r := range s.rooms {
		r.shut()
	}
	s.mu.Unlock()

	close(s.stop)
	closeAll(all, ErrClosed, "the SFU is stopping")
	s.wg.Wait()
	return s.mux.Close()
}

// Provision implements Control.
func (s *SFU) Provision(ctx context.Context, roomID, user, connID, issued snowflake.ID, offer string) (string, error) {
	shape, err := readOffer(offer, s.lim)
	if err != nil {
		return "", err
	}

	c, err := s.reserve(roomID, user, connID, issued)
	if err != nil {
		return "", err
	}
	answer, err := c.negotiate(ctx, offer, shape)
	if err != nil {
		// Closed while Pion was busy: its own call then fails on a closed connection, and that is not a
		// bad offer. What closed it is the answer, so a user who was kicked is told so, and one whose join
		// met a restarting server is not told they were kicked.
		closed, cause := c.closedBy()
		c.close("negotiation failed")
		if closed {
			return "", cause
		}
		return "", err
	}
	return answer, nil
}

// reserve checks every bound and the cutoffs, and holds the connection's place in its room, so two
// provisions racing for the last place cannot both have it.
func (s *SFU) reserve(roomID, user, connID, issued snowflake.ID) (*conn, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	// At or past the timeout, not only past it: Evict keeps a cutoff for exactly the timeout, so the two
	// boundaries must not both admit the same instant.
	if !issued.Time().After(now.Add(-s.lim.ProvisionTimeout)) {
		return nil, ErrStale
	}
	if cut, ok := s.cutoffs[roomUser{roomID, user}]; ok && now.Before(cut.until) && issued <= cut.id {
		return nil, ErrEvicted
	}
	if cut, ok := s.closedRooms[roomID]; ok && now.Before(cut.until) && issued <= cut.id {
		return nil, ErrEvicted
	}
	if until, ok := s.evictedConns[connID]; ok && now.Before(until) {
		return nil, ErrEvicted
	}
	if _, ok := s.conns[connID]; ok {
		return nil, ErrDuplicate
	}
	r := s.rooms[roomID]
	if r == nil {
		if len(s.rooms) >= s.lim.Rooms {
			return nil, fmt.Errorf("%w: rooms", ErrFull)
		}
		r = newRoom(s, roomID)
		s.rooms[roomID] = r
		s.wg.Add(1)
		go r.run()
	}
	if !r.admit(user) {
		if r.empty() {
			s.dropRoomLocked(r)
		}
		return nil, fmt.Errorf("%w: the room, or this user's connections in it", ErrFull)
	}
	c := newConn(s, r, connID, user, issued)
	r.add(c)
	s.conns[connID] = c
	return c, nil
}

// forget removes a closed connection, and its room once nobody is left in it.
func (s *SFU) forget(c *conn) {
	s.mu.Lock()
	if s.conns[c.id] == c {
		delete(s.conns, c.id)
	}
	c.room.remove(c)
	if c.room.empty() {
		s.dropRoomLocked(c.room)
	}
	s.mu.Unlock()
	c.room.changed()
}

func (s *SFU) dropRoomLocked(r *room) {
	if s.rooms[r.id] == r {
		delete(s.rooms, r.id)
	}
	r.shut()
}

// Evict implements Control.
func (s *SFU) Evict(_ context.Context, roomID, user, cut snowflake.ID) error {
	now := s.now()
	s.mu.Lock()
	key := roomUser{roomID, user}
	// Kept until a provision it would refuse is refused for its age instead: anything issued at or before
	// the cutoff was issued before this call, so after the provision timeout it is stale. Both checks read
	// one clock on a self-hosted instance; across processes (M116) they need the clocks within a bound.
	prev, ok := s.cutoffs[key]
	if !ok || cut > prev.id {
		prev.id = cut
	}
	prev.until = now.Add(s.lim.ProvisionTimeout)
	s.cutoffs[key] = prev

	var victims []*conn
	if r := s.rooms[roomID]; r != nil {
		for _, c := range r.members() {
			if c.user == user && c.issued <= cut {
				victims = append(victims, c)
			}
		}
	}
	s.mu.Unlock()

	closeAll(victims, ErrEvicted, "evicted")
	return nil
}

// EvictConnection implements Control.
func (s *SFU) EvictConnection(_ context.Context, connID snowflake.ID) error {
	s.mu.Lock()
	c := s.conns[connID]
	s.evictedConns[connID] = s.now().Add(s.lim.ProvisionTimeout)
	s.mu.Unlock()
	if c != nil {
		c.closeFor(ErrEvicted, "evicted")
	}
	return nil
}

// SetExclusion implements Control.
func (s *SFU) SetExclusion(_ context.Context, roomID, receiver snowflake.ID, excluded []snowflake.ID) error {
	s.mu.Lock()
	r := s.rooms[roomID]
	s.mu.Unlock()
	if r == nil || !r.exclude(receiver, excluded) {
		return ErrNotFound
	}
	r.changed()
	return nil
}

// CloseRoom implements Control.
func (s *SFU) CloseRoom(_ context.Context, roomID, cut snowflake.ID) error {
	s.mu.Lock()
	prev, ok := s.closedRooms[roomID]
	if !ok || cut > prev.id {
		prev.id = cut
	}
	prev.until = s.now().Add(s.lim.ProvisionTimeout)
	s.closedRooms[roomID] = prev
	var all []*conn
	if r := s.rooms[roomID]; r != nil {
		all = r.members()
		// Shut and let go of before its members close, so their closes choose no slots again: one at a
		// time, each would reselect the room and send every member left a slot map, 63 selections and
		// thousands of messages for a full room, all to connections about to close. A provision issued
		// after the cutoff opens a new room under the same id.
		s.dropRoomLocked(r)
	}
	s.mu.Unlock()
	closeAll(all, ErrEvicted, "the room was closed")
	return nil
}

// sweep forgets cutoffs that can no longer refuse anything.
func (s *SFU) sweep() {
	defer s.wg.Done()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		now := s.now()
		s.mu.Lock()
		for k, c := range s.cutoffs {
			if !now.Before(c.until) {
				delete(s.cutoffs, k)
			}
		}
		for k, c := range s.closedRooms {
			if !now.Before(c.until) {
				delete(s.closedRooms, k)
			}
		}
		for k, until := range s.evictedConns {
			if !now.Before(until) {
				delete(s.evictedConns, k)
			}
		}
		s.mu.Unlock()
	}
}

// mediaKind maps a voiceproto kind to Pion's.
func mediaKind(kind string) webrtc.RTPCodecType {
	if kind == voiceproto.KindVideo {
		return webrtc.RTPCodecTypeVideo
	}
	return webrtc.RTPCodecTypeAudio
}
