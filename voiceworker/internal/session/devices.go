// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"io"
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
// end it, with the microphone's outcome as the reason: a call that can neither send nor hear is no call
// (ADR 0034 §4). Only as it starts: a session that ends mid-call moves both streams to the stand-in, and
// PulseAudio moves them back when somebody logs in again, so ending the call then would end one that
// would have come back.

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
	// judged.
	first, firstFailing [2]bool
	decided             bool
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

// set reports which device's state, if it changed. A report is queued before the call is ended for it, so
// the worker can send the devices that ended the call before it says the call ended.
func (d *devices) set(which int, dev voiceipc.Device) {
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
	if !d.first[which] && dev.State != voiceipc.DeviceOpening && dev.State != voiceipc.DeviceAsking {
		d.first[which], d.firstFailing[which] = true, voiceipc.Failing(dev.State)
	}
	endCall := false
	if !d.decided && d.first[microphone] && d.first[speakers] {
		d.decided = true
		endCall = d.firstFailing[microphone] && d.firstFailing[speakers]
	}
	report := voiceipc.Devices{Microphone: d.now[microphone], Speakers: d.now[speakers]}
	select {
	case <-d.changes:
	default:
	}
	d.changes <- report
	d.mu.Unlock()
	if endCall {
		d.end("neither the microphone nor the speakers can be used")
	}
}

// current is the devices as last reported.
func (d *devices) current() voiceipc.Devices {
	d.mu.Lock()
	defer d.mu.Unlock()
	return voiceipc.Devices{Microphone: d.now[microphone], Speakers: d.now[speakers]}
}

// deviceOf is the state a backend's answer puts a device in: open on nil.
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
	return voiceipc.Device{State: state, Reason: err.Error()}
}

// heard notes that a device called back, as nanoseconds past base plus one. One atomic store, on the
// device's own goroutine.
func (m *media) heard(at *atomic.Int64) { at.Store(int64(time.Since(m.base)) + 1) }

// watchDevice reports which device's state from the moment it opened until the call stops. last is
// where its callback notes it was heard, and c what its open returned, which may be audio.Watched.
func (m *media) watchDevice(which int, c io.Closer, last *atomic.Int64) {
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
		}
		now := time.Since(m.base)
		heard := last.Load()
		var dev voiceipc.Device
		switch {
		case heard == 0 && now-opened < silentFor:
			continue // still opening
		case heard == 0:
			dev = voiceipc.Device{State: voiceipc.DeviceFailed, Reason: "it opened and delivers no audio"}
		case now-time.Duration(heard-1) > silentFor:
			dev = voiceipc.Device{State: voiceipc.DeviceFailed, Reason: "it stopped delivering audio"}
		default:
			if watched && (asked < 0 || now-asked >= watchEvery) {
				where, asked = w.Where(), now
			}
			dev = deviceOf(where)
		}
		if m.stopped() {
			return // the call stopped while the backend was asked, and what it said is of a closed stream
		}
		m.s.devs.set(which, dev)
	}
}

// authorize asks an audio.Authorizer for the microphone as the call starts, so that macOS's question is
// on the screen while ICE connects, and keeps the answer for openDevices.
func (s *Session) authorize(ctx context.Context, a audio.Authorizer) {
	defer close(s.authDone)
	s.authErr = a.Authorize(ctx, func() {
		s.devs.set(microphone, voiceipc.Device{State: voiceipc.DeviceAsking})
	})
}

// Devices delivers the devices' state each time it changes, the latest only. A session with no device,
// a worker built without audio, says so once, so that a join waiting on its microphone is not kept
// waiting for a device that will never be.
func (s *Session) Devices() <-chan voiceipc.Devices {
	if s.devs == nil {
		s.noDevicesOnce.Do(func() {
			none := voiceipc.Device{State: voiceipc.DeviceNoDevice, Reason: "this worker was built without audio"}
			s.noDevices = make(chan voiceipc.Devices, 1)
			s.noDevices <- voiceipc.Devices{Microphone: none, Speakers: none}
		})
		return s.noDevices
	}
	return s.devs.changes
}
