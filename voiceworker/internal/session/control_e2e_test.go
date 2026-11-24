// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest"
	"github.com/Alexnex31/Norite/voiceworker/internal/dsp"
)

// heardFor is what dev's speakers play over the next d.
func heardFor(dev *audiotest.Device, d time.Duration) []int16 {
	before, _ := dev.Heard()
	mark := len(before)
	time.Sleep(d)
	heard, _ := dev.Heard()
	return heard[mark:]
}

// speakingSoon waits for the call to say user is speaking.
func speakingSoon(t *testing.T, s *Session, user string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case sp := <-s.Speaking():
			if slices.Contains(sp.Users, user) {
				return
			}
		case <-deadline:
			t.Fatalf("the call never said %s was speaking", user)
		}
	}
}

// A local mute silences one caller for the person who muted them and nobody else, and the active speaker
// still names them: the indicator is how somebody finds whom to mute or report, and muting must not hide
// them from it (M34's done-when, architecture.md §6).
func TestALocalMuteSilencesOneCallerForTheListenerAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	loud := &audiotest.Device{Source: audiotest.Tone(440, 8000)}
	other := &audiotest.Device{Source: audiotest.Tone(660, 8000)}
	listener, bystander := &audiotest.Device{}, &audiotest.Device{}
	h.join(t, "1", "1", loud, Options{})
	h.join(t, "1", "2", other, Options{})
	l := h.join(t, "1", "3", listener, Options{})
	h.join(t, "1", "4", bystander, Options{})
	steady(t, listener, time.Second)

	// At once, from what was already on its way: the worker plays none of it.
	l.Subscribe(voiceipc.Subscribe{Drop: []string{"1"}})
	if p := tonePower(heardFor(listener, 300*time.Millisecond), 440); p > 0.05 {
		t.Errorf("the listener heard the caller they muted for a moment: %.2f of what they heard", p)
	}
	// And the SFU stops forwarding them, which is the bandwidth a local mute saves.
	forwarded(t, l, "1", false)
	heard := heardFor(listener, 2*time.Second)
	if p := tonePower(heard, 440); p > 0.05 {
		t.Errorf("the listener still hears the caller they muted: %.2f of what they heard", p)
	}
	if p := tonePower(heard, 660); p < 0.8 {
		t.Errorf("the listener no longer hears the caller they did not mute: %.2f of what they heard", p)
	}
	others := heardFor(bystander, 2*time.Second)
	if p := tonePower(others, 440); p < 0.2 {
		t.Errorf("somebody else stopped hearing the muted caller: %.2f of what they heard", p)
	}
	speakingSoon(t, l, "1")

	l.Subscribe(voiceipc.Subscribe{Drop: []string{}})
	time.Sleep(500 * time.Millisecond)
	if p := tonePower(heardFor(listener, 2*time.Second), 440); p < 0.2 {
		t.Errorf("the listener does not hear the caller they unmuted: %.2f of what they heard", p)
	}
}

// forwarded waits for the SFU's slot map for s to name user, or not to.
func forwarded(t *testing.T, s *Session, user string, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.ctl.mu.Lock()
		got := false
		for _, u := range s.ctl.sources {
			got = got || u == user
		}
		s.ctl.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the SFU's slot map names %s: %t", user, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Local mutes kept from an earlier call are subscribed to before the SFU says ready, and hold from the
// call's first words: the subscribe waits for the ready, which is when the SFU takes one.
func TestALocalMuteBeforeTheCallIsUpHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	h.join(t, "1", "1", &audiotest.Device{Source: audiotest.Tone(440, 8000)}, Options{})
	h.join(t, "1", "2", &audiotest.Device{Source: audiotest.Tone(660, 8000)}, Options{})
	listener := &audiotest.Device{}
	l := h.join(t, "1", "3", listener, Options{subscribe: &voiceipc.Subscribe{Drop: []string{"1"}}})
	heard := steady(t, listener, 2*time.Second)
	if p := tonePower(heard, 440); p > 0.05 {
		t.Errorf("the caller muted before the call was up is heard: %.2f of what was heard", p)
	}
	forwarded(t, l, "2", true)
	forwarded(t, l, "1", false)
}

// A cap is how many slots the SFU fills for this call (M34, ADR 0023): with one, the listener hears one of
// the two callers speaking at once, and the SFU's slot map names one.
func TestACapLimitsTheSlotsTheSFUFills(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	h.join(t, "1", "1", &audiotest.Device{Source: audiotest.Tone(440, 8000)}, Options{})
	h.join(t, "1", "2", &audiotest.Device{Source: audiotest.Tone(660, 8000)}, Options{})
	listener := &audiotest.Device{}
	l := h.join(t, "1", "3", listener, Options{})
	steady(t, listener, time.Second)

	one := 1
	l.Subscribe(voiceipc.Subscribe{Drop: []string{}, Cap: &one})
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.ctl.mu.Lock()
		n := len(l.ctl.sources)
		l.ctl.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the SFU fills %d slots under a cap of one", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	heard := heardFor(listener, 2*time.Second)
	a, b := tonePower(heard, 440), tonePower(heard, 660)
	if min(a, b) > 0.05 || max(a, b) < 0.8 {
		t.Errorf("under a cap of one the listener hears %.2f of one caller and %.2f of the other", a, b)
	}
}

// dspSoon waits for the call to report the processing as want.
func dspSoon(t *testing.T, s *Session, want voiceipc.DSP) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var last voiceipc.DSP
	for {
		select {
		case d := <-s.DSP():
			if last = d; d == want {
				return
			}
		case <-deadline:
			t.Fatalf("the processing was last reported %+v, never %+v", last, want)
		}
	}
}

// The call reports what the processing is doing, not what was asked (M30's carried item), and a change
// during the call makes the chain again with the stages asked for, the call going on throughout (M34's D5).
func TestTheProcessingIsReportedAndChangedDuringTheCall(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	speaker := &audiotest.Device{Source: audiotest.Tone(440, 8000)}
	listener := &audiotest.Device{}
	s := h.joinWith(t, "1", "1", speaker, dsp.AllOn(), Options{})
	h.join(t, "1", "2", listener, Options{})
	dspSoon(t, s, running(dsp.AllOn()))

	only := dsp.Settings{NoiseSuppression: true}
	s.Processing(voiceipc.Processing{Processing: ipc.VoiceJoin(only)})
	dspSoon(t, s, running(only))
	if mediaOf(s).echo.Load() {
		t.Error("the speakers still feed an echo canceller that was turned off")
	}
	checkHeard(t, "the listener, after the change", steady(t, listener, 2*time.Second), 440, 0.8)
}

// A voice server that stops ends the call saying nothing, which is the connection lost: nobody chose it, and
// the daemon joins such a call again (M36's manual pass, where an instance's restart ended every call as
// though it had been chosen).
func TestACallWhoseVoiceServerStopsIsLost(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	if runtime.GOOS == "windows" {
		t.Skip("stops the harness with SIGTERM, which Windows cannot send")
	}
	h := startHarness(t)
	s := h.join(t, "1", "1", &audiotest.Device{Source: audiotest.Tone(440, 8000)}, Options{})
	select {
	case <-s.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("the call never came up")
	}
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case why := <-s.Ended():
		if !s.EndedLost() {
			t.Errorf("the call ended with %q, not as lost", why)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("the call did not end when its voice server stopped")
	}
}

// A voice server that goes away without a word, as a network cut leaves it, is a connection ICE gives up on:
// lost as well, with nothing closed first (M36's manual pass, where a 40 s cut ended the call for good).
func TestACallWhoseVoiceServerVanishesIsLost(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls, and waits out ICE")
	}
	if runtime.GOOS == "windows" {
		t.Skip("shares the harness's stop with the test above")
	}
	h := startHarness(t)
	s := h.join(t, "1", "1", &audiotest.Device{Source: audiotest.Tone(440, 8000)}, Options{})
	select {
	case <-s.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("the call never came up")
	}
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case why := <-s.Ended():
		if why != "the connection to the voice server failed" || !s.EndedLost() {
			t.Errorf("the call ended with %q, lost %v", why, s.EndedLost())
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the call did not end when its voice server vanished")
	}
}
