// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"testing"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// The call's start is judged on both devices' first outcomes and on what they are doing when the second
// arrives: a device whose first answer was a stand-in for a moment, and which works by then, keeps the
// call up. Both failing then end it, and the call says its devices did.
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
			s := &Session{devs: d}
			if s.EndedByDevices() != c.endsFor {
				t.Errorf("EndedByDevices is %v, want %v", s.EndedByDevices(), c.endsFor)
			}
		})
	}
}
