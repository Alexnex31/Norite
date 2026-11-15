// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
)

// How the call's microphone and speakers are doing, which the worker reports to the daemon as call state
// (M32, ADR 0034 §1). Until M32 a device that failed was a log line, and on Linux with nobody logged in
// nothing failed at all: PulseAudio moved the streams to a stand-in and the call sent silence.
//
// What a device is doing is decided by watching it, not by its open returning:
//   - it is open from its first frame, and one that opened and delivers nothing for silentFor has failed;
//   - one that stops delivering for silentFor has failed, and is open again when it resumes;
//   - a backend that can tell where its stream goes (audio.Watched, PulseAudio) is asked once a second, off
//     the audio path, since a stream moved to a stand-in delivers zeros and no error.
//
// One device failing lets the call go on, listening or speaking alone. Both failing as the call starts
// end it, with the microphone's outcome in the reason: a call that can neither send nor hear is no call
// (ADR 0034 §4). Only as it starts: a session that ends mid-call moves both streams to the stand-in, and
// PulseAudio moves them back when somebody logs in again, so ending the call then would end one that
// would have come back. And only on what the backends answered, never on silence: a device can take longer
// than silentFor to deliver its first frame, a Bluetooth headset changing profile as its microphone opens
// for one, and ending the call on it would end one that was about to work. Silence is reported failed all
// the same.

const (
	// silentFor is how long a device may deliver nothing before it has failed.
	silentFor = 2 * time.Second
	// deviceTick is how often each device's watch looks at it, and watchEvery how often it asks a backend
	// where its stream goes.
	deviceTick = 250 * time.Millisecond
	watchEvery = time.Second
)

// The two directions, as devices indexes them.
const (
	microphone = iota
	speakers
)

// devices is what the worker reports of the call's devices, and where D4's end of the call is decided.
type devices struct {
	end func(reason string)

	mu  sync.Mutex
	now [2]voiceipc.Device
	// first is whether each device has had its first outcome past opening and asking, and firstFailing
	// whether that outcome was a failure; decided is whether both have, which is when the call's start is
	// judged. endedCall is whether that judgement ended the call.
	first, firstFailing [2]bool
	decided, endedCall  bool
	// changes holds the latest report not yet taken, at most one: a report replaces one not yet sent, so
	// the device's side never waits on the pipe.
	changes chan voiceipc.Devices
}

func newDevices(end func(string)) *devices {
	d := &devices{end: end, changes: make(chan voiceipc.Devices, 1)}
	d.now[microphone] = voiceipc.Device{State: voiceipc.DeviceOpening}
	d.now[speakers] = voiceipc.Device{State: voiceipc.DeviceOpening}
	return d
}

// set reports which device's state, if it changed. judged is whether the state is the backend's answer or
// the device's first frame, which is what the call's start is judged on, rather than silence. A report is
// queued before the call is ended for it, so the worker can send the devices that ended the call before it
// says the call ended.
func (d *devices) set(which int, dev voiceipc.Device, judged bool) {
	if d == nil {
		return // a test's media, made without a session's devices
	}
	dev.Reason = voiceipc.CutReason(dev.Reason)
	d.mu.Lock()
	if d.now[which] == dev {
		d.mu.Unlock()
		return
	}
	d.now[which] = dev
	if judged && !d.first[which] && dev.State != voiceipc.DeviceOpening && dev.State != voiceipc.DeviceAsking {
		d.first[which], d.firstFailing[which] = true, voiceipc.Failing(dev.State)
	}
	endCall := false
	if !d.decided && d.first[microphone] && d.first[speakers] {
		// Both first outcomes failing, and both failing still: a device whose first answer was a stand-in
		// for a moment, Bluetooth's sink coming back for one, may be working by the time the other answers.
		d.decided = true
		endCall = d.firstFailing[microphone] && d.firstFailing[speakers] &&
			voiceipc.Failing(d.now[microphone].State) && voiceipc.Failing(d.now[speakers].State)
		d.endedCall = endCall
	}
	report := voiceipc.Devices{Microphone: d.now[microphone], Speakers: d.now[speakers]}
	why := "neither the microphone nor the speakers can be used"
	if r := d.now[microphone].Reason; r != "" {
		why += ": " + r
	}
	select {
	case <-d.changes:
	default:
	}
	d.changes <- report
	d.mu.Unlock()
	if endCall {
		d.end(voiceipc.CutReason(why))
	}
}

// current is the devices as last reported.
func (d *devices) current() voiceipc.Devices {
	d.mu.Lock()
	defer d.mu.Unlock()
	return voiceipc.Devices{Microphone: d.now[microphone], Speakers: d.now[speakers]}
}

// deviceOf is the state a backend's answer puts a device in: open on nil. The reason is the error's, less
// the package's prefix, which says nothing to a person reading it in a call's state.
func deviceOf(err error) voiceipc.Device {
	if err == nil {
		return voiceipc.Device{State: voiceipc.DeviceOpen}
	}
	state := voiceipc.DeviceFailed
	switch audio.OutcomeOf(err) {
	case audio.Refused:
		state = voiceipc.DeviceRefused
	case audio.NoDevice:
		state = voiceipc.DeviceNoDevice
	case audio.NoSession:
		state = voiceipc.DeviceNoSession
	case audio.Failed:
	}
	return voiceipc.Device{State: state, Reason: strings.TrimPrefix(err.Error(), "audio: ")}
}

// heard notes that a device called back, as nanoseconds past base plus one, and closes first at its first
// frame, so its watch says open at once rather than at its next tick. One atomic swap, on the device's own
// goroutine.
func (m *media) heard(at *atomic.Int64, first chan struct{}) {
	if at.Swap(int64(time.Since(m.base))+1) == 0 && first != nil {
		close(first)
	}
}

// watchDevice reports which device's state from the moment it opened until the call stops. last is
// where its callback notes it was heard, first closed at its first frame, and c what its open returned,
// which may be audio.Watched.
func (m *media) watchDevice(which int, c io.Closer, last *atomic.Int64, first <-chan struct{}) {
	opened := time.Since(m.base)
	w, watched := c.(audio.Watched)
	var where error
	asked := time.Duration(-1)
	t := time.NewTicker(deviceTick)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
		case <-first:
			first = nil
		}
		now := time.Since(m.base)
		heard := last.Load()
		var dev voiceipc.Device
		judged := true
		switch {
		case heard == 0 && now-opened < silentFor:
			continue // still opening
		case heard == 0:
			dev = voiceipc.Device{State: voiceipc.DeviceFailed, Reason: "it opened and delivers no audio"}
			judged = false
		case now-time.Duration(heard-1) > silentFor:
			dev = voiceipc.Device{State: voiceipc.DeviceFailed, Reason: "it stopped delivering audio"}
			judged = false
		default:
			if watched && (asked < 0 || now-asked >= watchEvery) {
				where, asked = w.Where(), now
			}
			dev = deviceOf(where)
		}
		if m.stopped() {
			return // the call stopped while the backend was asked, and what it said is of a closed stream
		}
		m.s.devs.set(which, dev, judged)
	}
}

// authorize asks an audio.Authorizer for the microphone as the call starts, so that macOS's question is
// on the screen while ICE connects, and keeps the answer for openDevices. The answer is reported as soon
// as it comes, so a join that waits on the microphone never says it is being asked about once somebody has
// answered: a yes is opening again until its first frame, a no is the refusal. A question given up because
// the call closed is no refusal, whatever the backend returned for it, and reports nothing.
func (s *Session) authorize(ctx context.Context, a audio.Authorizer) {
	defer close(s.authDone)
	s.authErr = a.Authorize(ctx, func() {
		s.devs.set(microphone, voiceipc.Device{State: voiceipc.DeviceAsking}, true)
	})
	switch err := s.authErr; {
	case err != nil && (ctx.Err() != nil || errors.Is(err, context.Canceled)):
	case err != nil:
		s.logf("the microphone may not be used, so nothing will be sent: %v", err)
		s.devs.set(microphone, deviceOf(err), true)
	default:
		s.devs.set(microphone, voiceipc.Device{State: voiceipc.DeviceOpening}, true)
	}
}

// EndedByDevices is whether the call ended because both its devices failed as it started, asked once
// Ended has delivered: the daemon keeps the devices in its status after that end alone.
func (s *Session) EndedByDevices() bool {
	if s.devs == nil {
		return false
	}
	s.devs.mu.Lock()
	defer s.devs.mu.Unlock()
	return s.devs.endedCall
}

// Devices delivers the devices' state each time it changes, the latest only. A session with no device, a
// worker built without audio, says nothing: no state would be true of it, and both failing would read as
// a call its devices are about to end.
func (s *Session) Devices() <-chan voiceipc.Devices {
	if s.devs == nil {
		return nil
	}
	return s.devs.changes
}
