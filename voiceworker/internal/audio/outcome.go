// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"errors"
	"fmt"
)

// What each OS's answer means, as pure functions over the answer (M32). They are here, in a file every OS
// compiles, rather than beside the call that gets the answer: CI runs Linux alone, so a decision written
// in a _windows or _darwin file is compiled there and never tested anywhere. The calls stay thin layers
// above these, and the tests hold each to every answer its OS documents.

// HRESULTs the WASAPI backend tells apart.
const (
	hrAccessDenied      = 0x80070005 // E_ACCESSDENIED: the microphone switch is off
	hrNotFound          = 0x80070490 // HRESULT_FROM_WIN32(ERROR_NOT_FOUND): no default endpoint
	hrDeviceInvalidated = 0x88890004 // AUDCLNT_E_DEVICE_INVALIDATED: unplugged, or disabled
	hrDeviceInUse       = 0x8889000A // AUDCLNT_E_DEVICE_IN_USE: held in exclusive mode by another program
	hrServiceNotRunning = 0x88890010 // AUDCLNT_E_SERVICE_NOT_RUNNING: the audio service is stopped
)

// hresult is the error a failed WASAPI call makes, naming what was being done and saying which outcome it
// is. Windows' access denied is the "Let desktop apps access your microphone" switch, or the per-app one
// being tested in 2026 (ADR 0034), and the client names it: the error says only that access was denied.
func hresult(what string, hr uint32) error {
	var why string
	switch hr {
	case hrAccessDenied:
		why = "access is denied"
	case hrNotFound:
		why = "there is no such device"
	case hrDeviceInvalidated:
		why = "the device was removed"
	case hrDeviceInUse:
		why = "another program holds the device for itself"
	case hrServiceNotRunning:
		why = "the Windows audio service is not running"
	default:
		why = fmt.Sprintf("HRESULT 0x%08x", hr)
	}
	return &Failure{Outcome: outcomeOfHRESULT(hr), Err: fmt.Errorf("audio: %s: %s", what, why)}
}

// outcomeOfHRESULT is what a WASAPI failure means for the call.
func outcomeOfHRESULT(hr uint32) Outcome {
	switch hr {
	case hrAccessDenied:
		return Refused
	case hrNotFound, hrDeviceInvalidated:
		return NoDevice
	}
	return Failed
}

// AVAuthorizationStatus, as AVFoundation numbers it.
const (
	avNotDetermined = 0
	avRestricted    = 1
	avDenied        = 2
	avAuthorized    = 3
)

// errAsk is an AVAuthorizationStatus that leaves the question to ask.
var errAsk = errors.New("audio: macOS has not asked yet")

// authorization is what macOS's status for the microphone means: nil to capture, errAsk to ask the
// person, or why not. The prompt and the grant are the daemon's, which the worker's use is charged to
// (ADR 0034), so the refusals name it. Restricted is a policy, parental controls or a device manager's,
// which System Settings does not change, so it is not Refused, whose wording sends the person there: it
// fails, and says why.
func authorization(status int) error {
	switch status {
	case avAuthorized:
		return nil
	case avNotDetermined:
		return errAsk
	case avDenied:
		return &Failure{Outcome: Refused, Err: errors.New("macOS refuses norite-daemon the microphone")}
	case avRestricted:
		return &Failure{Outcome: Failed,
			Err: errors.New("a policy on this Mac keeps norite-daemon from the microphone")}
	}
	return fmt.Errorf("audio: macOS answered the microphone's authorization with %d", status)
}

// granted is what the answer to macOS's question means.
func granted(yes bool) error {
	if yes {
		return nil
	}
	return authorization(avDenied)
}

// loginSession is one of logind's sessions for the account, as far as the question needs.
type loginSession struct {
	Seat   string // the seat it is on, "" for none: SSH, and the user's own manager
	Active bool
	Class  string // "user" for a person's login, "greeter", "lock-screen", "manager", "background"…
}

// stoodIn is why a PulseAudio stream on a stand-in for no device is there, from the account's logind
// sessions (D5, M32's spike), which are this account's alone: logind grants the sound devices to the
// session in front, so with none of the account's at a seat its PipeWire lists none, whoever else is
// logged in, and no device is NoSession; with the account's in front, the machine has none to give. A
// session over SSH has no seat, and the user's own State is active with SSH alone, so what decides is a
// session with a seat, active, of class user. One of the account's at a seat that is not active is
// somebody else's session in front, a user switch: no device, saying so, since logging in is not what
// brings the devices back. err is logind's failure to answer, which is a system without it: then all that
// is known is that there is no device.
func stoodIn(what string, sessions []loginSession, err error) error {
	if err != nil {
		return &Failure{Outcome: NoDevice,
			Err: fmt.Errorf("audio: the stream for %s goes to no device, and logind did not say whether anybody "+
				"is logged in: %w", what, err)}
	}
	switched := false
	for _, s := range sessions {
		if s.Seat != "" && s.Class == "user" {
			if s.Active {
				return &Failure{Outcome: NoDevice, Err: fmt.Errorf("audio: the stream for %s goes to no device", what)}
			}
			switched = true
		}
	}
	if switched {
		return &Failure{Outcome: NoDevice, Err: fmt.Errorf("audio: the stream for %s goes to no device, since "+
			"this account's session at this machine is not the one in front", what)}
	}
	return &Failure{Outcome: NoSession,
		Err: fmt.Errorf("audio: the stream for %s goes to no device, since this account is not logged in at this machine",
			what)}
}
