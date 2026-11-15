// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"testing"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// The call's start is judged on both devices' first outcomes and on what they are doing when the second
// arrives: a device whose first answer was a stand-in for a moment, and which works or is merely silent by
// then, keeps the call up. Both failing on their backends' answers end it.
func TestTheStartIsJudgedOnBothDevicesAsTheSecondAnswers(t *testing.T) {
	none := voiceipc.Device{State: voiceipc.DeviceNoDevice, Reason: "a stand-in"}
	open := voiceipc.Device{State: voiceipc.DeviceOpen}
	for _, c := range []struct {
		name    string
		steps   func(d *devices)
		endsFor bool
	}{
		{"the speakers back by the time the microphone answers", func(d *devices) {
			d.set(speakers, none, true)
			d.set(speakers, open, true)
			d.set(microphone, none, true)
		}, false},
		{"both failing still", func(d *devices) {
			d.set(speakers, none, true)
			d.set(microphone, none, true)
		}, true},
		{"the speakers silent by the time the microphone answers", func(d *devices) {
			d.set(speakers, none, true)
			d.set(speakers, voiceipc.Device{State: voiceipc.DeviceFailed, Reason: "silent"}, false)
			d.set(microphone, none, true)
		}, false},
		{"both failing only by their silence", func(d *devices) {
			d.set(speakers, voiceipc.Device{State: voiceipc.DeviceFailed, Reason: "silent"}, false)
			d.set(microphone, voiceipc.Device{State: voiceipc.DeviceFailed, Reason: "silent"}, false)
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var ended []string
			d := newDevices(func(why string) { ended = append(ended, why) })
			c.steps(d)
			if got := len(ended) == 1; got != c.endsFor {
				t.Errorf("ended %q, want an end: %v", ended, c.endsFor)
			}
		})
	}
}

// The devices' end is the call's only when it is the end delivered: one that comes after another, the
// voice server closing the call as the second device fails, is no end, and does not say it was the devices'.
func TestOnlyTheEndDeliveredSaysWhoseItWas(t *testing.T) {
	s := &Session{ended: make(chan string, 1)}
	s.end("the voice server closed the call")
	s.endFor("neither the microphone nor the speakers can be used", true)
	if why := <-s.ended; why != "the voice server closed the call" || s.EndedByDevices() {
		t.Errorf("the call ended with %q, by its devices: %v", why, s.EndedByDevices())
	}
	s = &Session{ended: make(chan string, 1)}
	s.endFor("neither the microphone nor the speakers can be used", true)
	s.end("the voice server closed the call")
	if <-s.ended; !s.EndedByDevices() {
		t.Error("the devices' end does not say it was theirs")
	}
}
