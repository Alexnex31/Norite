// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alexnex31/Norite/backend/voiceproto"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
	"github.com/Alexnex31/Norite/voiceworker/internal/dsp"
)

// What the call says beside its media (M34): who the SFU says is speaking, who fills each slot, whom the
// person has muted for themselves and how many people they hear at once, and what the audio processing is
// doing. The SFU is an instance's, a stranger's: what it says is bounded here, where it is kept, and an id
// that is not one drops the message.

// speakingEvery spaces the speaking lists the worker hands the daemon. The SFU sends one each time the list
// changes, up to ten a second, and the daemon kills a worker past fifty frames a second: a talkative room
// would otherwise be the SFU's way to end the call. A list waiting its turn is replaced by a newer one.
const speakingEvery = 200 * time.Millisecond

// maxSlotMap bounds a slot map taken: the SFU fills at most the slots this worker receives on. maxSources
// bounds the senders remembered past the map that named them: four full rooms' worth.
const (
	maxSlotMap = Slots
	maxSources = 4 * voiceproto.MaxSubscribeList
)

// textSender is the data channel as a subscribe is sent on it: *webrtc.DataChannel.
type textSender interface{ SendText(string) error }

type control struct {
	mu sync.Mutex
	// dc is the data channel, which a subscribe is sent on, and ready the SFU's ready having been read on it.
	// Nothing is sent before ready: Pion calls a channel open as soon as it is dialed and greets on a
	// goroutine of its own afterwards, so a subscribe could precede the hello, and the SFU closes a call whose
	// first message is not hello (M34's /code-review). What is set before is sent at ready.
	dc    textSender
	ready bool
	// sources is who fills each slot by source id, the CSRC on their packets, and drop whom the person
	// muted for themselves. muted is the source ids of the dropped, which the mix reads without the lock.
	//
	// seen remembers every sender a slot map named, in order, past the map: the SFU takes a muted sender
	// off the map within a tick, and their packets already buffered are still theirs, which a muted set
	// built from the current map alone would play.
	sources map[uint32]string
	seen    map[uint32]string
	order   []uint32
	drop    map[string]bool
	cap     *int
	muted   atomic.Pointer[map[uint32]bool]

	// speaking is the SFU's newest list; sentAt is when one was last handed over, and timer, when set, hands
	// over the newest once speakingEvery has passed. speakingOut holds the newest not yet taken.
	speaking    []string
	sentAt      time.Time
	timer       *time.Timer
	speakingOut chan voiceipc.Speaking

	// want is how the person wants the microphone processed now, which send applies; dspOut holds the
	// newest report of what the processing is doing, not yet taken.
	want   atomic.Pointer[dsp.Settings]
	dspOut chan voiceipc.DSP
}

func newControl(processing dsp.Settings) *control {
	c := &control{sources: map[uint32]string{}, seen: map[uint32]string{}, drop: map[string]bool{},
		speakingOut: make(chan voiceipc.Speaking, 1), dspOut: make(chan voiceipc.DSP, 1)}
	c.want.Store(&processing)
	c.muted.Store(&map[uint32]bool{})
	return c
}

// latest puts v in ch, replacing what was not yet taken, so the side reporting never waits on the pipe.
func latest[T any](ch chan T, v T) {
	select {
	case <-ch:
	default:
	}
	ch <- v
}

// onReady is the SFU's ready: a subscription made before it, which the daemon makes with the local mutes it
// keeps from call to call, is sent again, since one sent before the channel opened was lost.
func (c *control) onReady() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = true
	if len(c.drop) > 0 || c.cap != nil {
		c.sendSubscribeLocked()
	}
}

// onSlots takes the SFU's slot map, whole.
func (c *control) onSlots(b []byte) {
	if c == nil {
		return
	}
	var m voiceproto.Slots
	if voiceproto.Decode(b, &m) != nil || len(m.Map) > maxSlotMap {
		return
	}
	sources := make(map[uint32]string, len(m.Map))
	for _, e := range m.Map {
		if !voiceipcID(e.User) {
			return
		}
		sources[e.Source] = e.User
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sources = sources
	for src, user := range sources {
		if _, known := c.seen[src]; !known {
			c.order = append(c.order, src)
		}
		c.seen[src] = user
	}
	for len(c.order) > maxSources {
		delete(c.seen, c.order[0])
		c.order = c.order[1:]
	}
	c.remuteLocked()
}

// onSpeaking takes the SFU's speaking list, and hands it over now or once speakingEvery has passed.
func (c *control) onSpeaking(b []byte) {
	if c == nil {
		return
	}
	var m voiceproto.Speaking
	if voiceproto.Decode(b, &m) != nil || (voiceipc.Speaking{Users: m.Users}).Check() != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.speaking = slices.Clone(m.Users)
	if c.timer != nil {
		return // the timer hands over the newest
	}
	if wait := speakingEvery - time.Since(c.sentAt); wait > 0 {
		// Assigned under the lock its callback takes (M27), so the callback always sees it set.
		c.timer = time.AfterFunc(wait, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.timer = nil
			c.handSpeakingLocked()
		})
		return
	}
	c.handSpeakingLocked()
}

func (c *control) handSpeakingLocked() {
	c.sentAt = time.Now()
	latest(c.speakingOut, voiceipc.Speaking{Users: slices.Clone(c.speaking)})
}

// subscribe is the daemon's local mutes and cap, whole.
func (c *control) subscribe(s voiceipc.Subscribe) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop = make(map[string]bool, len(s.Drop))
	for _, u := range s.Drop {
		c.drop[u] = true
	}
	c.cap = s.Cap
	c.remuteLocked()
	c.sendSubscribeLocked()
}

// remuteLocked works out which source ids are muted from the slot map and the drop list.
func (c *control) remuteLocked() {
	muted := map[uint32]bool{}
	for src, user := range c.seen {
		if c.drop[user] {
			muted[src] = true
		}
	}
	c.muted.Store(&muted)
}

// sendSubscribeLocked asks the SFU for the subscription. A send that fails is a channel closing, which ends
// the call by itself.
func (c *control) sendSubscribeLocked() {
	if c.dc == nil || !c.ready {
		return
	}
	m := voiceproto.Subscribe{Op: voiceproto.OpSubscribe, Pin: []string{}, Drop: make([]string, 0, len(c.drop)),
		Cap: c.cap}
	for u := range c.drop {
		m.Drop = append(m.Drop, u)
	}
	slices.Sort(m.Drop)
	if b, err := voiceproto.Encode(m); err == nil {
		_ = c.dc.SendText(string(b))
	}
}

// The media's reads, each safe on a test's media made without a session's control, as devices' are.

// isMuted reports whether packets from source are the person's to not hear.
func (c *control) isMuted(source uint32) bool { return c != nil && (*c.muted.Load())[source] }

// reportDSP hands over what the processing is doing.
func (c *control) reportDSP(d voiceipc.DSP) {
	if c == nil {
		return
	}
	d.Reason = voiceipc.CutReason(d.Reason)
	latest(c.dspOut, d)
}

// wanted is how the person wants the microphone processed now, or def with no control.
func (c *control) wanted(def dsp.Settings) dsp.Settings {
	if c == nil {
		return def
	}
	return *c.want.Load()
}

// running is the report of a chain processing with settings.
func running(settings dsp.Settings) voiceipc.DSP {
	return voiceipc.DSP{State: voiceipc.DSPRunning, EchoCancellation: settings.EchoCancellation,
		NoiseSuppression: settings.NoiseSuppression, AutoGain: settings.AutoGain}
}

func (c *control) stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

// voiceipcID is whether s is a user id, as voiceipc checks one.
func voiceipcID(s string) bool { return (voiceipc.Speaking{Users: []string{s}}).Check() == nil }
