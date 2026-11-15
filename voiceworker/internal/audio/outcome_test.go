// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"errors"
	"strings"
	"testing"
)

// Each OS's answers, and what each means, held here on Linux, the one OS CI runs (outcome.go).

func TestEachHRESULTIsTheOutcomeItMeans(t *testing.T) {
	for _, c := range []struct {
		hr   uint32
		want Outcome
		says string
	}{
		{hrAccessDenied, Refused, "access is denied"},
		{hrNotFound, NoDevice, "there is no such device"},
		{hrDeviceInvalidated, NoDevice, "the device was removed"},
		{hrDeviceInUse, Failed, "another program holds the device"},
		{hrServiceNotRunning, Failed, "audio service is not running"},
		{0x80004005, Failed, "HRESULT 0x80004005"}, // E_FAIL
	} {
		err := hresult("opening the microphone", c.hr)
		if got := OutcomeOf(err); got != c.want {
			t.Errorf("0x%08x is outcome %d, want %d", c.hr, got, c.want)
		}
		if !strings.HasPrefix(err.Error(), "audio: opening the microphone: ") || !strings.Contains(err.Error(), c.says) {
			t.Errorf("0x%08x says %q", c.hr, err)
		}
	}
}

func TestEachAuthorizationStatusIsTheOutcomeItMeans(t *testing.T) {
	if err := authorization(avAuthorized); err != nil {
		t.Errorf("authorized is %v", err)
	}
	if err := authorization(avNotDetermined); !errors.Is(err, errAsk) {
		t.Errorf("not determined is %v, not a question to ask", err)
	}
	// Denied is the person's, which System Settings changes; restricted a policy, which it does not.
	for s, want := range map[int]Outcome{avDenied: Refused, avRestricted: Failed} {
		err := authorization(s)
		if OutcomeOf(err) != want || !strings.Contains(err.Error(), "norite-daemon") {
			t.Errorf("status %d is %v, outcome %d", s, err, OutcomeOf(err))
		}
	}
	if err := authorization(7); err == nil || OutcomeOf(err) != Failed {
		t.Errorf("an unknown status is %v", err)
	}
	if granted(true) != nil || OutcomeOf(granted(false)) != Refused {
		t.Error("the answer to the question is misread")
	}
}

// logind's sessions as M32's spike recorded them (Debian 13, systemd 257): the user's manager and an SSH
// login have no seat; a tty or graphical login has seat0.
func TestOnlyALoginAtASeatIsASession(t *testing.T) {
	manager := loginSession{Class: "manager", Active: true}
	ssh := loginSession{Class: "user", Active: true}
	tty := loginSession{Seat: "seat0", Class: "user", Active: true}
	for _, c := range []struct {
		name     string
		sessions []loginSession
		err      error
		want     Outcome
	}{
		{"none", nil, nil, NoSession},
		{"the manager and SSH alone", []loginSession{manager, ssh}, nil, NoSession},
		{"a login at a seat", []loginSession{manager, ssh, tty}, nil, NoDevice},
		{"a seat's login switched away from", []loginSession{{Seat: "seat0", Class: "user"}}, nil, NoDevice},
		{"a greeter at a seat", []loginSession{{Seat: "seat0", Class: "greeter", Active: true}}, nil, NoSession},
		{"logind not answering", nil, errors.New("no such bus"), NoDevice},
	} {
		err := stoodIn("the microphone", c.sessions, c.err)
		if got := OutcomeOf(err); got != c.want {
			t.Errorf("%s: outcome %d, want %d (%v)", c.name, got, c.want, err)
		}
	}
	if err := stoodIn("the speakers", nil, errors.New("no such bus")); !strings.Contains(err.Error(), "logind did not say") {
		t.Errorf("logind's silence is not said: %v", err)
	}
	if err := stoodIn("the speakers", []loginSession{{Seat: "seat0", Class: "user"}}, nil); !strings.Contains(err.Error(),
		"not the one in front") {
		t.Errorf("a user switch reads as %v", err)
	}
}
