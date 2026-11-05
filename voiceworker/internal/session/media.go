// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/jitter"
	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

// The call's audio, from the moment the SFU says ready (M29):
//
//	microphone → encode → RTP with the audio level → the track
//	each slot → jitter buffer → its decoder, reset when the slot changes sender → mix → speakers
//
// The device's clock drives both directions (internal/audio), so the speakers' requests are what pace
// the jitter buffers and nothing here keeps time of its own.

const (
	// stuckDecode is how long one decode may run before the call ends. libopus's work per packet is
	// bounded by the format, so only a bug reaches it, and a call into WebAssembly cannot be interrupted
	// (ADR 0036): ending the call ends the worker, which is what frees the core.
	stuckDecode = time.Second
	// captureQueue is how many captured frames wait for the encoder before the oldest is dropped.
	captureQueue = 4
	// closeWithin bounds closing the audio, so the connection's goodbye still fits in the worker's 2 s
	// after it: half a second here, then GracefulClose's one.
	closeWithin = 500 * time.Millisecond
)

type media struct {
	s      *Session
	codec  opus.Codec
	enc    opus.Encoder
	track  *webrtc.TrackLocalStaticRTP
	lossy  func() bool
	frames chan []int16
	stop   chan struct{}
	sent   sync.WaitGroup

	// mu is held by every decode and by the mix, so that closing the codec never pulls an instance out
	// from under a call in progress.
	mu       sync.Mutex
	closed   bool
	slots    []*slot
	attachMu sync.Mutex
	attached int

	devMu             sync.Mutex
	capture, playback io.Closer
	stopOnce          sync.Once
	// decoding and encoding are when the call into the codec in progress started, as nanoseconds past
	// base plus one, or zero. Measured from base so the watchdog runs on the monotonic clock: a wall
	// clock stepped by NTP or a resume would end a healthy call, or never end a stuck one.
	base               time.Time
	decoding, encoding atomic.Int64
	mix                [audio.FrameSamples]int32
}

type slot struct {
	buf  *jitter.Buffer
	dec  opus.Decoder
	pcm  []int16 // decoded and not yet played; at most one packet's worth past a frame
	tmp  []int16
	errs int
	// resets counts the decoder's resets, one per sender the slot changed to; levels the packets that
	// arrived with an audio level.
	resets int
	levels atomic.Int64
}

// startMedia starts both directions. A codec that cannot start ends the call, since it is a bug and no
// audio can cross either way. A device that cannot open does not: the call goes on without that direction,
// and the log says why. Turning that into call state the clients explain is M32's (ADR 0034).
func (s *Session) startMedia(ctx context.Context) error {
	codec, err := s.awaitCodec(ctx)
	if err != nil {
		return err
	}
	m := &media{s: s, codec: codec, track: s.track, lossy: s.opts.Lossy,
		frames: make(chan []int16, captureQueue), stop: make(chan struct{}), base: time.Now()}
	if m.enc, err = codec.NewEncoder(ctx, opus.DefaultEncoderOptions()); err != nil {
		_ = codec.Close(ctx)
		return err
	}
	for range s.slotCount() {
		d, err := codec.NewDecoder(ctx)
		if err != nil {
			m.close()
			return err
		}
		m.slots = append(m.slots, &slot{buf: jitter.New(), dec: d,
			pcm: make([]int16, 0, opus.MaxDecodeSamples+audio.FrameSamples), tmp: make([]int16, opus.MaxDecodeSamples)})
	}
	m.sent.Add(2)
	go m.send()
	go m.watch()
	if !s.adopt(m) {
		m.close() // the call closed while the codec compiled
		return nil
	}
	m.openDevices(ctx, s.opts.Device)
	return nil
}

// openDevices opens the speakers and the microphone, and keeps each only if the call has not closed
// meanwhile. close stops the call before it reads them under devMu, and a device opened after that sees
// the stop and closes itself here. So no device is opened for a call already left, and a leave never
// waits on a driver that is slow to open. A device whose Close outruns close's bound is closed by the
// worker's exit, which follows its call, and the daemon reports the call left once the worker is reaped.
func (m *media) openDevices(ctx context.Context, dev audio.Device) {
	keep := func(c io.Closer, at *io.Closer) {
		m.devMu.Lock()
		late := m.stopped()
		if !late {
			*at = c
		}
		m.devMu.Unlock()
		// Closed outside devMu: a device's Close can wait on a callback a stuck decode holds, and close
		// takes devMu before anything it bounds.
		if late {
			_ = c.Close()
		}
	}
	if m.stopped() {
		return
	}
	if p, err := dev.Play(ctx, m.fill); err != nil {
		m.s.logf("the speakers could not be opened, so nothing will be heard: %v", err)
	} else {
		keep(p, &m.playback)
	}
	if m.stopped() {
		return
	}
	if c, err := dev.Capture(ctx, m.captured); err != nil {
		m.s.logf("the microphone could not be opened, so nothing will be sent: %v", err)
	} else {
		keep(c, &m.capture)
	}
}

func (m *media) stopped() bool {
	select {
	case <-m.stop:
		return true
	default:
		return false
	}
}

// attach gives a slot's track the next slot of the mix, and drains one past them, which an SFU offering
// more slots than were asked for would send.
func (m *media) attach(tr remote) {
	// Not under mu, which a stuck decode holds: a track arriving then would wait for ever, unread.
	m.attachMu.Lock()
	var sl *slot
	if m.attached < len(m.slots) {
		sl = m.slots[m.attached]
		m.attached++
	}
	m.attachMu.Unlock()
	if sl == nil {
		drain(tr.tr)
		return
	}
	m.receive(tr.tr, audioLevelID(tr.rcv.GetParameters().HeaderExtensions), sl)
}

// captured hands a frame to the encoder without ever waiting on it: the device's goroutine must not
// stall, so a frame the encoder has not kept up with is dropped, oldest first.
func (m *media) captured(pcm []int16) {
	f := make([]int16, len(pcm))
	copy(f, pcm)
	for {
		select {
		case <-m.stop:
			return
		case m.frames <- f:
			return
		default:
			select {
			case <-m.frames:
			default:
			}
		}
	}
}

// send encodes each captured frame and writes it to the track. A frame DTX reduced to a packet of two
// bytes or fewer is not sent, as WebRTC's own Opus sender does not: the timestamp moves on, the sequence
// number does not, and the far end's jitter buffer reads the gap as silence rather than loss.
func (m *media) send() {
	defer m.sent.Done()
	var seq uint16
	var ts uint32
	talking := false
	out := make([]byte, opus.MaxPacket)
	for {
		var pcm []int16
		select {
		case <-m.stop:
			return
		case pcm = <-m.frames:
		}
		var n int
		err := m.clocked(&m.encoding, func() (e error) { n, e = m.enc.Encode(pcm, out); return })
		if err != nil {
			m.s.end(voiceipc.CutReason("the microphone's audio could not be encoded: " + err.Error()))
			return
		}
		frameTS := ts
		ts += audio.FrameSamples
		if n <= 2 {
			talking = false
			continue
		}
		p := rtp.Packet{Header: rtp.Header{Version: 2, Marker: !talking, SequenceNumber: seq, Timestamp: frameTS},
			Payload: out[:n]}
		seq++
		talking = true
		if id := m.s.levelID(); id != 0 {
			if b, err := (rtp.AudioLevelExtension{Level: Level(pcm), Voice: true}).Marshal(); err == nil {
				_ = p.SetExtension(id, b)
			}
		}
		if m.lossy != nil && m.lossy() {
			continue
		}
		// An error is a track not yet bound or already closed; the call's own state says which.
		_ = m.track.WriteRTP(&p)
	}
}

// Level is RFC 6464's audio level of a frame: its power in -dBov, 0 the loudest and 127 silence.
func Level(pcm []int16) uint8 {
	var sum float64
	for _, v := range pcm {
		x := float64(v) / 32768
		sum += x * x
	}
	if sum == 0 {
		return 127
	}
	db := -10 * math.Log10(sum/float64(len(pcm)))
	return uint8(min(127, max(0, math.Round(db))))
}

// receive reads one slot's packets into its jitter buffer until the track ends. level is the audio
// level's extension id, so the slot can count the packets that carry one.
func (m *media) receive(tr *webrtc.TrackRemote, level uint8, sl *slot) {
	buf := make([]byte, 1500)
	var p rtp.Packet
	for {
		n, _, err := tr.Read(buf)
		if err != nil {
			return
		}
		if p.Unmarshal(buf[:n]) != nil || len(p.CSRC) == 0 {
			// The SFU writes every packet's sender as its CSRC; one without is not the SFU's.
			continue
		}
		if level != 0 && p.GetExtension(level) != nil {
			sl.levels.Add(1)
		}
		sl.buf.Push(p.CSRC[0], p.SequenceNumber, p.Timestamp, p.Payload, time.Now())
	}
}

// fill is the speakers asking for the next frame: each slot's, mixed.
func (m *media) fill(out []int16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		clear(out)
		return
	}
	clear(m.mix[:])
	for _, sl := range m.slots {
		m.next(sl)
		for i := range audio.FrameSamples {
			m.mix[i] += int32(sl.pcm[i])
		}
		sl.pcm = append(sl.pcm[:0], sl.pcm[audio.FrameSamples:]...)
	}
	for i, v := range m.mix {
		out[i] = int16(max(math.MinInt16, min(math.MaxInt16, v)))
	}
}

// next decodes until the slot holds a frame.
func (m *media) next(sl *slot) {
	// What the previous sender's last packet decoded to and has not played yet is theirs, not the new
	// sender's: a 120 ms packet leaves 100 ms behind. It is dropped when a change is pending, and again on
	// the step that carries the reset, which the jitter buffer sets under the same lock as the change: a
	// packet of the new sender's arriving between the two is decoded after the drop, never before it.
	if sl.buf.ResetPending() {
		sl.pcm = sl.pcm[:0]
	}
	for len(sl.pcm) < audio.FrameSamples {
		st := sl.buf.Next()
		ctx := context.Background()
		if st.Reset {
			sl.pcm = sl.pcm[:0]
			sl.resets++
			_ = m.timed(func() error { return sl.dec.Reset(ctx) })
		}
		var n int
		var err error
		switch st.Kind {
		case jitter.Silence:
			sl.pcm = append(sl.pcm, silence[:]...)
			sl.buf.Done(0)
			continue
		case jitter.Play:
			err = m.timed(func() (e error) { n, e = sl.dec.Decode(ctx, st.Payload, sl.tmp); return })
		case jitter.Recover:
			err = m.timed(func() (e error) { n, e = sl.dec.Recover(ctx, st.Payload, sl.tmp); return })
		case jitter.Conceal:
			err = m.timed(func() (e error) { n, e = sl.dec.Conceal(ctx, sl.tmp); return })
		}
		if err != nil || n <= 0 {
			// A packet that does not decode costs that packet: it is concealed, and a decoder that trapped
			// was replaced already (internal/opus).
			sl.errs++
			if errors.Is(err, opus.ErrReplaced) ||
				m.timed(func() (e error) { n, e = sl.dec.Conceal(ctx, sl.tmp); return }) != nil || n <= 0 {
				n = audio.FrameSamples
				clear(sl.tmp[:n])
			}
		}
		sl.pcm = append(sl.pcm, sl.tmp[:n]...)
		sl.buf.Done(n)
	}
}

// timed runs one call into a decoder under the watchdog's eye.
func (m *media) timed(f func() error) error { return m.clocked(&m.decoding, f) }

// clocked runs one call into the codec, noting when it started in at for the watchdog.
func (m *media) clocked(at *atomic.Int64, f func() error) error {
	at.Store(int64(time.Since(m.base)) + 1)
	defer at.Store(0)
	return f()
}

// stuck is whether the call noted in at has run past stuckDecode.
func (m *media) stuck(at *atomic.Int64) bool {
	since := at.Load()
	return since != 0 && time.Since(m.base)-time.Duration(since-1) > stuckDecode
}

// watch ends the call when a decode has run past stuckDecode.
func (m *media) watch() {
	defer m.sent.Done()
	t := time.NewTicker(stuckDecode / 4)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			if m.stuck(&m.decoding) {
				m.s.end("an audio decoder stopped responding, so the call was ended")
				return
			}
			if m.stuck(&m.encoding) {
				m.s.end("the audio encoder stopped responding, so the call was ended")
				return
			}
		}
	}
}

// close stops both directions and closes the codec. A decode that never returns holds mu; the codec is
// then left to the worker's exit, which follows the call's end.
func (m *media) close() {
	first := false
	m.stopOnce.Do(func() { close(m.stop); first = true })
	if !first {
		return
	}
	m.devMu.Lock()
	devices := []io.Closer{m.capture, m.playback}
	m.capture, m.playback = nil, nil
	m.devMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// WASAPI's and AudioQueue's Close wait for a callback in progress, and a mix stuck in a decode is
		// one. So is a sender stuck in an encode. Everything here waits on something a stuck call could
		// hold, which is why the whole of it is bounded below rather than each step.
		for _, c := range devices {
			if c != nil {
				_ = c.Close()
			}
		}
		m.sent.Wait()
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		_ = m.codec.Close(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(closeWithin):
		// Left to the worker's exit, which follows the call's end. The bound leaves the connection's own
		// close room for its goodbye inside the worker's two seconds (cmd/norite-voice).
	}
}

var silence [audio.FrameSamples]int16
