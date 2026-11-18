// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// The settings that give a program the microphone back, as each OS names them (ADR 0034 §1). Windows'
// is a switch for every desktop program at once, macOS's an entry for the daemon, which the worker's use
// is charged to.
const (
	WindowsMicrophoneSetting = "Settings → Privacy & security → Microphone → Let desktop apps access your microphone"
	MacMicrophoneSetting     = "System Settings → Privacy & Security → Microphone → norite-daemon"
)

// VoiceDevices words what a call's microphone and speakers are doing, for a person on goos (M32), one line
// each, or one for both when they say the same thing. Lines end without a full stop, and carry the
// worker's reason where it says more than the state: the reason is a stranger's text, and the caller
// cleans it with the rest. A device not reported yet says nothing.
func VoiceDevices(mic, spk *ipc.VoiceDevice, goos string) []string {
	if mic != nil && spk != nil && mic.State == ipc.VoiceDeviceNoSession && spk.State == mic.State {
		return []string{"Nothing can use the microphone or speakers until you log in at this machine"}
	}
	var lines []string
	if mic != nil {
		lines = append(lines, microphoneLine(*mic, goos))
	}
	if spk != nil {
		lines = append(lines, speakersLine(*spk))
	}
	return lines
}

func microphoneLine(d ipc.VoiceDevice, goos string) string {
	switch d.State {
	case ipc.VoiceDeviceOpening:
		return "The microphone is opening"
	case ipc.VoiceDeviceOpen:
		return "The microphone is on"
	case ipc.VoiceDeviceAsking:
		return "macOS is asking whether norite-daemon may use the microphone; nothing is sent until it is answered"
	case ipc.VoiceDeviceRefused:
		switch goos {
		case "windows":
			return "Windows refuses the microphone; turn on " + WindowsMicrophoneSetting
		case "darwin":
			return "macOS refuses the microphone; allow it in " + MacMicrophoneSetting
		}
		return withReason("The microphone was refused", d.Reason)
	case ipc.VoiceDeviceNoDevice:
		return withReason("There is no microphone to use", d.Reason)
	case ipc.VoiceDeviceNoSession:
		return "Nothing can use the microphone until you log in at this machine"
	}
	return withReason("The microphone failed", d.Reason)
}

func speakersLine(d ipc.VoiceDevice) string {
	switch d.State {
	case ipc.VoiceDeviceOpening:
		return "The speakers are opening"
	case ipc.VoiceDeviceOpen:
		return "The speakers are on"
	case ipc.VoiceDeviceRefused:
		return withReason("The speakers were refused", d.Reason)
	case ipc.VoiceDeviceNoDevice:
		return withReason("There are no speakers to play to", d.Reason)
	case ipc.VoiceDeviceNoSession:
		return "Nothing can use the speakers until you log in at this machine"
	}
	return withReason("The speakers failed", d.Reason)
}

// VoiceRights words what the account may not do in its call (M33), for the TUI and the GUI as for the
// command: nothing when it may send and hear, or when there is no call answered.
func VoiceRights(r *ipc.VoiceRights) []string {
	if r == nil {
		return nil
	}
	var out []string
	if !r.Send {
		out = append(out, "You are not heard: a moderator muted you, or this channel does not let you speak")
	}
	if !r.Hear {
		out = append(out, "You hear nothing: a moderator deafened you")
	}
	return out
}

func withReason(line, reason string) string {
	if reason == "" {
		return line
	}
	return line + ": " + reason
}
