// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ops_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// Each state of a call's devices is worded for the OS it is on, and a refusal names the setting that gives
// the microphone back where the OS has one (ADR 0034 §1).
func TestEachDeviceStateIsWordedForItsOS(t *testing.T) {
	dev := func(state, reason string) *ipc.VoiceDevice { return &ipc.VoiceDevice{State: state, Reason: reason} }
	open := dev(ipc.VoiceDeviceOpen, "")
	cases := []struct {
		name     string
		mic, spk *ipc.VoiceDevice
		goos     string
		want     []string
	}{
		{"nothing said yet", nil, nil, "linux", nil},
		{"both on", open, open, "linux", []string{"The microphone is on", "The speakers are on"}},
		{"opening", dev(ipc.VoiceDeviceOpening, ""), nil, "linux", []string{"The microphone is opening"}},
		{"macOS asking", dev(ipc.VoiceDeviceAsking, ""), open, "darwin", []string{
			"macOS is asking whether norite-daemon may use the microphone; nothing is sent until it is answered",
			"The speakers are on"}},
		{"refused on Windows", dev(ipc.VoiceDeviceRefused, "audio: opening the microphone: access is denied"), open,
			"windows", []string{"Windows refuses the microphone; turn on Settings → Privacy & security → Microphone → " +
				"Let desktop apps access your microphone", "The speakers are on"}},
		{"refused on macOS", dev(ipc.VoiceDeviceRefused, "macOS refuses norite-daemon the microphone"), open, "darwin",
			[]string{"macOS refuses the microphone; allow it in System Settings → Privacy & Security → Microphone → " +
				"norite-daemon", "The speakers are on"}},
		{"refused elsewhere", dev(ipc.VoiceDeviceRefused, "a sandbox said no"), nil, "linux",
			[]string{"The microphone was refused: a sandbox said no"}},
		{"no session for either", dev(ipc.VoiceDeviceNoSession, "x"), dev(ipc.VoiceDeviceNoSession, "y"), "linux",
			[]string{"Nothing can use the microphone or speakers until you log in at this machine"}},
		{"no session for one", dev(ipc.VoiceDeviceNoSession, "x"), open, "linux",
			[]string{"Nothing can use the microphone until you log in at this machine", "The speakers are on"}},
		{"no device", dev(ipc.VoiceDeviceNoDevice, ""), dev(ipc.VoiceDeviceNoDevice, "audio: the stream for the "+
			"speakers goes to no device"), "linux", []string{"There is no microphone to use",
			"There are no speakers to play to: audio: the stream for the speakers goes to no device"}},
		{"failed", dev(ipc.VoiceDeviceFailed, "it stopped delivering audio"), dev(ipc.VoiceDeviceFailed, ""), "windows",
			[]string{"The microphone failed: it stopped delivering audio", "The speakers failed"}},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, ops.VoiceDevices(c.mic, c.spk, c.goos), c.name)
	}
}
