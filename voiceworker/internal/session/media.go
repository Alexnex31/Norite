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
	"github.com/Alexnex31/Norite/voiceworker/internal/dsp"
	"github.com/Alexnex31/Norite/voiceworker/internal/jitter"
	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

// The call's audio, from the moment the SFU says ready (M29):
//
//	microphone → AEC → RNNoise → AGC → encode → RTP with the audio level → the track
//	each slot → jitter buffer → its decoder, reset when the slot changes sender → mix → speakers
//	                                                                                 ↳ the AEC's reference
//
// The device's clock drives both directions (internal/audio), so the speakers' requests are what pace
// the jitter buffers and nothing here keeps time of its own.
//
// The DSP (M30) runs in send, the one goroutine that enters its instance, and is fed both directions there:
// what the speakers played reaches it through a ring the mix writes, and each captured frame is stamped
// with how much had been played when it was recorded. Before processing a frame, send hands the echo
// canceller what was played up to its stamp. AEC3 queues what it is handed and takes one played frame per
// recorded frame, in order, so what pairs the two directions is the order and the count, not when the
// calls are made.
//
// The stamp is what keeps a sender running behind from handing over more: rendering everything played so
// far put what it was behind by in AEC3's render buffer, and AEC3 resets its delay estimate when that
// stays above 32 ms for a second (DetectExcessRenderBlocks), two frames (M30 /code-review). The stamp was
// in the plan, was removed when no test could see it, and is back with the test that does
// (TestASenderBehindRendersOnlyUpToTheFrame).
//
// What would lose the pairing is a recorded frame dropped on the way, which shifts every later one against
// its reference until AEC3 reconverges: measured, starving the sender with M29's queue left 13 dB of echo
// canceled where this design cancels 57 (TestEchoStaysCanceledWhenTheSenderFallsBehind). So no frame is
// dropped before the DSP. A sender that falls behind runs the DSP on every frame and leaves the encoder to
// skip, which is what keeps what is sent close to the microphone.

const (
	// stuckDecode is how long one decode may run before the call ends. libopus's work per packet is
	// bounded by the format, so only a bug reaches it, and a call into WebAssembly cannot be interrupted
	// (ADR 0036): ending the call ends the worker, which is what frees the core.
	stuckDecode = time.Second
	// captureQueue is how far behind the microphone the encoder may fall: a frame with this many others
	// waiting behind it is processed and not encoded, which bounds what is sent to 80 ms behind once the
	// queue drains. M29 dropped the oldest frame from the queue instead, which the DSP cannot afford
	// (above). Never two frames in a row: a sender too slow for real time keeps the queue full, and would
	// otherwise skip every frame and send nothing.
	captureQueue = 4
	// slowDSP is how many frames in a row the sender may take with captureQueue others waiting before the
	// DSP is judged too slow for this machine and dropped for the rest of the call. Skipping cannot drain a
	// queue the DSP alone keeps full: it filled to captureBacklog, 320 ms behind the microphone, and the far
	// end, sent half the frames, heard nothing (M30 /code-review). Half a second, which a sender that
	// merely stalled has drained long before.
	slowDSP = 25
	// captureBacklog is how many captured frames wait for send before the oldest is dropped after all: as
	// much as the reference ring holds, past which the reference is lost anyway.
	captureBacklog = refFrames
	// refFrames is how much of what the speakers played the ring keeps for the echo canceller: 320 ms, four
	// times the queue's depth. A sender further behind than that renders from the oldest it still holds.
	refFrames = 16
	// voiceProbability is the probability from RNNoise past which a frame is a voice, for RFC 6464's V flag.
	// M35's voice detection reads the same signal, and adds the hysteresis this does not.
	voiceProbability = 0.5
	// closeWithin bounds closing the audio, so the connection's goodbye still fits in the worker's 2 s
	// after it: half a second here, then GracefulClose's one.
	closeWithin = 500 * time.Millisecond
)

type media struct {
	s      *Session
	codec  opus.Codec
	enc    opus.Encoder
	track  interface{ WriteRTP(*rtp.Packet) error } // the session's track; a test's recorder
	lossy  func() bool
	frames chan captureFrame
	// captures counts the frames the microphone delivered, which numbers each; captured alone writes it.
	captures uint64
	stop     chan struct{}
	sent     sync.WaitGroup

	// The DSP, all of it send's but ref. send takes the module up once it has compiled (taken), and makes
	// the chain then; until it has, and once the chain has failed or if it could not start, the call goes on
	// with its audio unprocessed. ref is what the mix played; rendered is how much of it send has handed the
	// chain; raw is the frame as captured, put back when the chain fails partway through it.
	module   dsp.Module
	chain    dsp.Chain
	taken    bool
	ref      refRing
	rendered uint64
	raw      [audio.FrameSamples]int16
	overruns atomic.Int64
	// skipped counts the frames send processed and did not encode for being behind, dropped those the
	// queue lost before send took them.
	skipped, dropped atomic.Int64

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
	base                           time.Time
	decoding, encoding, processing atomic.Int64
	mix                            [audio.FrameSamples]int32
}

type slot struct {
	buf  *jitter.Buffer
	dec  opus.Decoder
	pcm  []int16 // decoded and not yet played; at most one packet's worth past a frame
	tmp  []int16
	errs int
	// resets counts the decoder's resets, one per sender the slot changed to; levels the packets that
	// arrived with an audio level, voiced those of them whose V flag was set, and loudness the sum of their
	// levels, in -dBov.
	resets           int
	levels           atomic.Int64
	voiced, loudness atomic.Int64
}

// startMedia starts both directions. A codec that cannot start ends the call, since it is a bug and no
// audio can cross either way. A device that cannot open does not: the call goes on without that direction,
// and the log says why. Turning that into call state the clients explain is M32's (ADR 0034).
func (s *Session) startMedia(ctx context.Context) error {
	codec, err := s.awaitCodec(ctx)
	if err != nil {
		go func() { // the call ends, and the DSP compiled for it goes with it
			if d, _ := s.awaitDSP(context.Background()); d != nil {
				_ = d.Close(context.Background())
			}
		}()
		return err
	}
	// The DSP is not awaited: it compiles in about 200 ms, twice the codec, and send takes it up when it is
	// ready, so it never holds back a call's first words.
	m := &media{s: s, codec: codec, track: s.track, lossy: s.opts.Lossy,
		frames: make(chan captureFrame, captureBacklog), stop: make(chan struct{}), base: time.Now()}
	if m.enc, err = codec.NewEncoder(ctx, opus.DefaultEncoderOptions()); err != nil {
		m.closeDSP()
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

// captureFrame is a frame as the microphone delivered it, which one it was, and how many frames the
// speakers had played when it was.
type captureFrame struct {
	pcm    []int16
	n      uint64
	played uint64
}

// captured hands a frame to send without ever waiting on it: the device's goroutine must not stall, so
// past captureBacklog the oldest frame waiting is dropped, and the echo canceller reconverges across it.
func (m *media) captured(pcm []int16) {
	f := captureFrame{pcm: make([]int16, len(pcm)), n: m.captures, played: m.ref.count()}
	m.captures++
	copy(f.pcm, pcm)
	for {
		select {
		case <-m.stop:
			return
		case m.frames <- f:
			return
		default:
			select {
			case <-m.frames:
				m.dropped.Add(1)
			default:
			}
		}
	}
}

// send encodes each captured frame and writes it to the track. A frame DTX reduced to a packet of two
// bytes or fewer is not sent, as WebRTC's own Opus sender does not: the timestamp moves on, the sequence
// number does not, and the far end's jitter buffer reads the gap as silence rather than loss.
//
// The timestamp is the frame's place in what the microphone delivered, so a frame skipped or dropped
// leaves the same gap. Counting only the frames sent made a sender slower than real time send a stream
// whose timestamps ran at half the clock's speed: the far end's buffer, playing it at the clock's speed,
// found every packet late and played silence (M30 /code-review).
func (m *media) send() {
	defer m.sent.Done()
	var seq uint16
	talking, skipped, behind := false, false, 0
	out := make([]byte, opus.MaxPacket)
	for {
		var f captureFrame
		select {
		case <-m.stop:
			return
		case f = <-m.frames:
		}
		pcm := f.pcm
		if m.s.opts.stall != nil {
			m.s.opts.stall()
		}
		voice := m.process(pcm, f.played)
		if len(m.frames) < captureQueue {
			behind = 0
		} else if behind++; behind >= slowDSP && m.chain != nil {
			m.s.logf("the audio processing is slower than real time on this machine, so the rest of the call " +
				"goes unprocessed")
			m.dropChain()
			behind = 0
		}
		if len(m.frames) >= captureQueue && !skipped {
			// Behind: this frame has been through the DSP, which keeps it in step, and is not sent: no
			// packet, and a gap in the timestamps the far end hears as silence.
			skipped = true
			m.skipped.Add(1)
			continue
		}
		skipped = false
		var n int
		err := m.clocked(&m.encoding, func() (e error) { n, e = m.enc.Encode(pcm, out); return })
		if err != nil {
			m.s.end(voiceipc.CutReason("the microphone's audio could not be encoded: " + err.Error()))
			return
		}
		frameTS := uint32(f.n * audio.FrameSamples) //nolint:gosec // RTP's timestamp wraps, as it is meant to
		if n <= 2 {
			talking = false
			continue
		}
		p := rtp.Packet{Header: rtp.Header{Version: 2, Marker: !talking, SequenceNumber: seq, Timestamp: frameTS},
			Payload: out[:n]}
		seq++
		talking = true
		if id := m.s.levelID(); id != 0 {
			// Both read the frame as processed, which is what is sent: the level after AGC2.
			if b, err := (rtp.AudioLevelExtension{Level: Level(pcm), Voice: voice}).Marshal(); err == nil {
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

// process runs the DSP on a captured frame in place, after rendering what the speakers had played by the
// time it was recorded, its stamp, and reports whether the frame holds a voice. With no chain, the frame goes as it was and
// is reported a voice, as M29 sent every frame.
//
// A failed call into the chain drops it for the rest of the call (M30's plan, D1). The reference is other
// people's decoded audio, so an input that makes the DSP fail is one a stranger can send: ending the call
// on it would let anybody end everybody's, where this lets them switch off one caller's processing.
// Starting it again would hand them the same lever on every frame. A call that never returns is the
// watchdog's.
func (m *media) process(pcm []int16, stamp uint64) bool {
	m.takeDSP(stamp)
	if m.chain == nil {
		return true
	}
	copy(m.raw[:], pcm)
	var voice float32
	err := m.clocked(&m.processing, func() error {
		played := stamp
		if !m.s.processing.EchoCancellation {
			m.rendered = played // nothing reads the reference
		} else if behind := played - m.rendered; behind > refFrames {
			m.overruns.Add(int64(behind - refFrames)) //nolint:gosec // at most the frames played
			m.rendered = played - refFrames
		}
		var ref [audio.FrameSamples]int16
		for ; m.rendered < played; m.rendered++ {
			if !m.ref.get(m.rendered, ref[:]) {
				m.overruns.Add(1) // written over between the count and the copy
				continue
			}
			for h := 0; h < audio.FrameSamples; h += dsp.ChunkSamples {
				if err := m.chain.Render(ref[h : h+dsp.ChunkSamples]); err != nil {
					return err
				}
			}
		}
		for h := 0; h < audio.FrameSamples; h += dsp.ChunkSamples {
			p, err := m.chain.Capture(pcm[h : h+dsp.ChunkSamples])
			if err != nil {
				return err
			}
			voice = max(voice, p)
		}
		return nil
	})
	if err != nil {
		m.s.logf("%s", voiceipc.CutReason("the audio processing failed, so the rest of the call goes unprocessed: "+
			err.Error()))
		m.dropChain()
		// A failure on the frame's second half leaves its first processed: half delayed and gained, half
		// not, which would click. The frame goes as it was captured.
		copy(pcm, m.raw[:])
		return true
	}
	return voice >= voiceProbability
}

// dropChain ends the chain for the rest of the call.
func (m *media) dropChain() {
	_ = m.chain.Close()
	m.chain = nil
}

// takeDSP makes the chain once the module has compiled, without waiting for it. The reference starts at
// the stamp of the frame it first processes: AEC3 pairs the two directions by count, so starting further
// back would make it estimate a delay that is not there, on top of the devices' own.
func (m *media) takeDSP(stamp uint64) {
	if m.taken || m.s.dspDone == nil {
		return
	}
	select {
	case <-m.s.dspDone:
	default:
		return
	}
	m.taken = true
	m.module = m.s.dsp
	if m.s.dspErr != nil {
		m.s.logf("the audio processing could not start, so the microphone goes unprocessed: %v", m.s.dspErr)
		return
	}
	// Making the chain runs the module's own C++ constructors and dsp_new in WebAssembly, so the watchdog
	// bounds it as it bounds every call into the instance (M30 /code-review).
	var c dsp.Chain
	err := m.clocked(&m.processing, func() (e error) {
		c, e = m.module.NewChain(context.Background(), m.s.processing)
		return
	})
	if err != nil {
		m.s.logf("the audio processing could not start, so the microphone goes unprocessed: %v", err)
		return
	}
	m.chain, m.rendered = c, stamp
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
		var lvl rtp.AudioLevelExtension
		if level != 0 && lvl.Unmarshal(p.GetExtension(level)) == nil {
			sl.levels.Add(1)
			sl.loudness.Add(int64(lvl.Level))
			if lvl.Voice {
				sl.voiced.Add(1)
			}
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
	// Nothing reads the reference with echo cancellation off, so the speakers' callback is spared the copy.
	if m.s.processing.EchoCancellation {
		m.ref.put(out)
	}
}

// refRing is the last refFrames frames the speakers played, for the echo canceller. The mix writes it under
// m.mu, and send reads it; its own lock is held to copy a frame and for nothing else, never across a call
// into WebAssembly, so a stuck DSP cannot hold up the speakers. A send more than refFrames behind skips
// what was written over, and counts it.
type refRing struct {
	mu     sync.Mutex
	frames [refFrames][audio.FrameSamples]int16
	played uint64 // frames written since the call began
}

func (r *refRing) put(f []int16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy(r.frames[r.played%refFrames][:], f)
	r.played++
}

func (r *refRing) count() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.played
}

// get copies frame n, and reports false when the ring no longer holds it.
func (r *refRing) get(n uint64, out []int16) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n >= r.played || r.played-n > refFrames {
		return false
	}
	copy(out, r.frames[n%refFrames][:])
	return true
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
			if m.stuck(&m.processing) {
				m.s.end("the audio processing stopped responding, so the call was ended")
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
	// What each slot's jitter buffer refused or covered for, once per call: the daemon's log is where a
	// call that sounded late or broken is looked into. Counts and a target only, nothing of the audio.
	for i, sl := range m.slots {
		if st := sl.buf.Stats(); st.Senders > 0 {
			m.s.logf("slot %d: %d senders, delay %d frames, %d late, %d concealed, %d recovered, %d oversize, "+
				"%d resyncs", i, st.Senders, sl.buf.Target(), st.Late, st.Concealed, st.Recovered, st.Oversize,
				st.Resyncs)
		}
	}
	if n := m.overruns.Load(); n > 0 {
		m.s.logf("the audio processing fell %d frames of the speakers' audio behind, and skipped them", n)
	}
	if skipped, dropped := m.skipped.Load(), m.dropped.Load(); skipped+dropped > 0 {
		m.s.logf("the sender fell behind the microphone: %d frames processed and not sent, %d lost before "+
			"processing", skipped, dropped)
	}
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
		m.closeDSP()
	}()
	select {
	case <-done:
	case <-time.After(closeWithin):
		// Left to the worker's exit, which follows the call's end. The bound leaves the connection's own
		// close room for its goodbye inside the worker's two seconds (cmd/norite-voice).
	}
}

// closeDSP closes the chain and the module, once send has stopped or never started. A module send never
// took up is closed when it has compiled, which the call no longer waits for.
func (m *media) closeDSP() {
	if m.chain != nil {
		_ = m.chain.Close()
		m.chain = nil
	}
	if m.module != nil {
		_ = m.module.Close(context.Background())
		m.module = nil
	}
	if !m.taken && m.s.dspDone != nil {
		m.taken = true
		go func() {
			<-m.s.dspDone
			if m.s.dsp != nil {
				_ = m.s.dsp.Close(context.Background())
			}
		}()
	}
}

var silence [audio.FrameSamples]int16
