// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"fmt"

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

// VoiceSelf words what the person set on themselves (M34): a deafen, which mutes too, or a mute, or
// nothing. Both hold from call to call, so with no call, inCall false, they are said of the next one: "nobody
// hears you" under "Not in a voice call" read as a call going on unheard (M34's Mac pass).
func VoiceSelf(s ipc.VoiceSelf, inCall bool) []string {
	switch {
	case s.Deaf && inCall:
		return []string{"You deafened yourself: you hear nobody, and nobody hears you"}
	case s.Deaf:
		return []string{"You deafened yourself: your next call starts deafened, and `norite voice undeafen` undoes it"}
	case s.Mute && inCall:
		return []string{"You muted yourself: nobody hears you"}
	case s.Mute:
		return []string{"You muted yourself: your next call starts muted, and `norite voice unmute` undoes it"}
	}
	return nil
}

func withReason(line, reason string) string {
	if reason == "" {
		return line
	}
	return line + ": " + reason
}

// VoiceInput words how the call's microphone is let through (M35): by voice, or by push-to-talk and what
// became of its key, and whether it is down. Nothing with no call. The reason and the desktop's trigger are
// a stranger's text, which the caller cleans with the rest, as it does a device's.
func VoiceInput(in *ipc.VoiceInputStatus) []string {
	if in == nil {
		return nil
	}
	if in.Mode != ipc.VoiceInputPushToTalk {
		return []string{"You are heard whenever you speak; `norite voice mode push-to-talk` sends only while a key is held"}
	}
	key := in.Key
	if in.Trigger != "" && in.Trigger != in.Key {
		key = in.Trigger + ", as the desktop bound it"
	}
	var line string
	switch {
	case in.Hotkey == ipc.VoiceHotkeyRegistered && in.Reason != "":
		// A desktop that binds keys only in its own settings, and named none (Hyprland, M25).
		line = "Push-to-talk is registered, and " + in.Reason
	case in.Hotkey == ipc.VoiceHotkeyRegistered:
		line = "Push-to-talk: you are heard while you hold " + key
	case in.Hotkey == ipc.VoiceHotkeyWaiting:
		line = "Push-to-talk on " + key + ": the desktop is asking you to confirm the shortcut, and nobody hears " +
			"you until it is"
	case in.Hotkey == ipc.VoiceHotkeyUnavailable:
		line = withReason("Push-to-talk has no key, so nobody hears you", in.Reason) +
			"; `norite voice mode push-to-talk` tries the key again, and `norite voice mode voice` sends whenever " +
			"you speak instead"
	default:
		line = "Push-to-talk on " + key + ": the key is being registered"
	}
	lines := []string{line}
	if in.Held {
		lines = append(lines, "The key is down: you are talking")
	}
	return lines
}

// VoiceRejoin words a call the daemon joins again by itself (M36), in the state the status gives: waiting or
// trying, rejoined, or given up, why it was lost or given up being the status's reason, which the caller
// cleans. Nothing for a call the person joined.
func VoiceRejoin(r *ipc.VoiceRejoin, state, reason string) []string {
	if r == nil {
		return nil
	}
	after := map[string]string{
		ipc.VoiceRejoinDaemonRestarted: "the daemon restarted",
		ipc.VoiceRejoinWorkerCrashed:   "the voice worker crashed",
		ipc.VoiceRejoinSessionLost:     "the connection to the instance was lost",
	}[r.Cause]
	if after == "" {
		after = "the call was lost"
	}
	switch state {
	case ipc.VoiceJoining:
		lines := []string{withReason("The call was lost", reason)}
		if r.Attempt == 0 {
			return append(lines, "Joining it again after "+after+", once the instance can be reached")
		}
		return append(lines, fmt.Sprintf("Joining it again after %s: attempt %d of %d", after, r.Attempt,
			ipc.MaxVoiceRejoins))
	case ipc.VoiceInCall:
		return []string{"Joined again after " + after}
	}
	return []string{withReason("The call was lost after "+after+", and is not joined again", reason)}
}
