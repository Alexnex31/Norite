// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"testing"
	"time"

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
			d := newDevices(func(why string, _ voiceipc.Devices) { ended = append(ended, why) })
			c.steps(d)
			if got := len(ended) == 1; got != c.endsFor {
				t.Errorf("ended %q, want an end: %v", ended, c.endsFor)
			}
		})
	}
}

// The devices' end is the call's only when it is the end delivered: one that comes after another, the
// voice server closing the call as the second device fails, is no end, and names no devices.
func TestOnlyTheEndDeliveredSaysWhoseItWas(t *testing.T) {
	none := voiceipc.Devices{Microphone: voiceipc.Device{State: voiceipc.DeviceNoSession},
		Speakers: voiceipc.Device{State: voiceipc.DeviceNoSession}}
	s := &Session{ended: make(chan string, 1)}
	s.end("the voice server closed the call")
	s.endFor("neither the microphone nor the speakers can be used", &none)
	if why := <-s.ended; why != "the voice server closed the call" || s.EndedBy() != nil {
		t.Errorf("the call ended with %q, by %+v", why, s.EndedBy())
	}
	s = &Session{ended: make(chan string, 1)}
	s.endFor("neither the microphone nor the speakers can be used", &none)
	s.end("the voice server closed the call")
	if <-s.ended; s.EndedBy() == nil || *s.EndedBy() != none {
		t.Errorf("the devices' end names %+v", s.EndedBy())
	}
}

// Every report says whether the start has been judged with the call going on, which a join waiting on a
// failing microphone answers on; the report of a start judged to end the call does not. Past judgeWithin
// the start is no longer judged: the call goes on whatever its devices do.
func TestTheReportsSayWhenTheStartIsJudged(t *testing.T) {
	none := voiceipc.Device{State: voiceipc.DeviceNoDevice, Reason: "a stand-in"}
	open := voiceipc.Device{State: voiceipc.DeviceOpen}
	last := func(d *devices) voiceipc.Devices { return <-d.changes }

	var by []voiceipc.Devices
	d := newDevices(func(_ string, r voiceipc.Devices) { by = append(by, r) })
	d.set(microphone, none, true)
	if last(d).Decided {
		t.Error("decided with the speakers not yet heard of")
	}
	d.set(speakers, open, true)
	if !last(d).Decided {
		t.Error("not decided once both have answered, the speakers working")
	}

	d = newDevices(func(_ string, r voiceipc.Devices) { by = append(by, r) })
	d.set(microphone, none, true)
	d.set(speakers, none, true)
	if r := last(d); r.Decided || len(by) != 1 || by[0] != r {
		t.Errorf("a start that ends the call reported %+v and ended by %+v", r, by)
	}

	by = nil
	d = newDevices(func(_ string, r voiceipc.Devices) { by = append(by, r) })
	d.began = time.Now().Add(-judgeWithin - time.Second)
	d.set(microphone, none, true)
	d.set(speakers, none, true)
	if r := last(d); !r.Decided || len(by) != 0 {
		t.Errorf("a start past its window reported %+v and ended by %+v", r, by)
	}
}
