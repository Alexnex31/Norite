// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest"
)

// M32's call state, end to end against M26's SFU: each way a device can fail is played by audiotest's
// device, and what the session reports is what the worker sends the daemon.

// reports reads what s reports of its devices until want holds of the latest, and returns every report
// read, the last one being it.
func reports(t *testing.T, s *Session, within time.Duration, want func(voiceipc.Devices) bool) []voiceipc.Devices {
	t.Helper()
	var seen []voiceipc.Devices
	deadline := time.After(within)
	for {
		select {
		case d := <-s.Devices():
			seen = append(seen, d)
			if want(d) {
				return seen
			}
		case <-deadline:
			t.Fatalf("the devices were reported as %+v, and never as wanted", seen)
			return nil
		}
	}
}

func is(mic, spk string) func(voiceipc.Devices) bool {
	return func(d voiceipc.Devices) bool { return d.Microphone.State == mic && d.Speakers.State == spk }
}

// staysUp fails if s ends within d.
func staysUp(t *testing.T, s *Session, d time.Duration) {
	t.Helper()
	select {
	case reason := <-s.Ended():
		t.Fatalf("the call ended: %s", reason)
	case <-time.After(d):
	}
}

func failure(o audio.Outcome, msg string) error {
	return &audio.Failure{Outcome: o, Err: errors.New(msg)}
}

// One device failing leaves the call up, listening or speaking alone, and says which and why.
func TestOneDeviceFailingLeavesTheCallUp(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	s := h.join(t, "1", "1", &audiotest.Device{CaptureErr: failure(audio.Refused, "access denied")}, Options{})
	got := reports(t, s, 5*time.Second, is(voiceipc.DeviceRefused, voiceipc.DeviceOpen))
	if r := got[len(got)-1].Microphone.Reason; r != "access denied" {
		t.Errorf("the refusal's reason is %q", r)
	}
	staysUp(t, s, time.Second)

	s = h.join(t, "2", "1", &audiotest.Device{PlayErr: failure(audio.NoDevice, "no output")}, Options{})
	reports(t, s, 5*time.Second, is(voiceipc.DeviceOpen, voiceipc.DeviceNoDevice))
	staysUp(t, s, time.Second)
}

// A call whose devices both fail as it starts ends, saying so, and the devices that ended it are reported
// first (ADR 0034 §4). Played twice: by the opens failing, as on Windows with the switch off, and by both
// streams opening onto PulseAudio's stand-in, as on Linux with nobody logged in.
func TestACallThatCanNeitherSendNorHearEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	for _, c := range []struct {
		name, room, why string
		dev             *audiotest.Device
	}{
		{"by its opens", "1", "nobody is logged in", &audiotest.Device{CaptureErr: failure(audio.NoSession, "nobody is logged in"),
			PlayErr: failure(audio.NoSession, "nobody is logged in")}},
		{"by where its streams went", "2", "a stand-in", &audiotest.Device{
			Where: func(bool) error { return failure(audio.NoSession, "a stand-in") }}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := h.join(t, c.room, "1", c.dev, Options{})
			select {
			case reason := <-s.Ended():
				// The microphone's outcome is in the reason (ADR 0034 §4), for a script that reads only it.
				if want := "neither the microphone nor the speakers can be used: " + c.why; reason != want {
					t.Errorf("the call ended saying %q, want %q", reason, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a call with neither device stayed up")
			}
			if d := s.devs.current(); !is(voiceipc.DeviceNoSession, voiceipc.DeviceNoSession)(d) {
				t.Errorf("the devices were left as %+v", d)
			}
			if by := s.EndedBy(); by == nil || !is(voiceipc.DeviceNoSession, voiceipc.DeviceNoSession)(*by) {
				t.Errorf("the call says it was ended by %+v", by)
			}
		})
	}
}

// A microphone that opens and delivers nothing has failed after silentFor, and was never open: open is
// said at the first frame, not when the open returns.
func TestAMicrophoneThatDeliversNothingIsNeverOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	start := time.Now()
	s := h.join(t, "1", "1", &audiotest.Device{Silent: true}, Options{})
	got := reports(t, s, silentFor+3*time.Second, func(d voiceipc.Devices) bool {
		return d.Microphone.State == voiceipc.DeviceFailed
	})
	if took := time.Since(start); took < silentFor {
		t.Errorf("the microphone failed after %v, before the %v it is given", took, silentFor)
	}
	for _, d := range got {
		if d.Microphone.State == voiceipc.DeviceOpen {
			t.Fatalf("a microphone that delivered nothing was reported open: %+v", got)
		}
	}
	if r := got[len(got)-1].Microphone.Reason; !strings.Contains(r, "delivers no audio") {
		t.Errorf("the reason is %q", r)
	}
	staysUp(t, s, 500*time.Millisecond) // the speakers work
}

// A microphone that stops delivering mid-call has failed after silentFor, and is open again once it
// resumes: a device unplugged, or a driver that stopped calling back.
func TestAMicrophoneThatStopsHasFailedUntilItResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	var paused atomic.Bool
	s := h.join(t, "1", "1", &audiotest.Device{Paused: paused.Load}, Options{})
	reports(t, s, 5*time.Second, is(voiceipc.DeviceOpen, voiceipc.DeviceOpen))
	paused.Store(true)
	got := reports(t, s, silentFor+2*time.Second, is(voiceipc.DeviceFailed, voiceipc.DeviceOpen))
	if r := got[len(got)-1].Microphone.Reason; !strings.Contains(r, "stopped delivering") {
		t.Errorf("the reason is %q", r)
	}
	staysUp(t, s, 300*time.Millisecond)
	paused.Store(false)
	reports(t, s, 2*time.Second, is(voiceipc.DeviceOpen, voiceipc.DeviceOpen))
}

// The OS's question is reported while it is open, and the microphone opens on a yes and is refused on a
// no, the call going on either way.
func TestTheQuestionIsReportedAndItsAnswerFollowed(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	for _, c := range []struct {
		name, room string
		answer     error
		// silent is a microphone that has not delivered its first frame yet, so that only the answer
		// itself can move it past asking.
		silent bool
		want   string
	}{
		{"yes", "1", nil, false, voiceipc.DeviceOpen},
		{"no", "2", failure(audio.Refused, "the person said no"), false, voiceipc.DeviceRefused},
		{"yes, before the first frame", "3", nil, true, voiceipc.DeviceOpening},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer := make(chan error, 1)
			dev := &audiotest.Asking{Device: &audiotest.Device{Silent: c.silent}, Ask: func(ctx context.Context) error {
				select {
				case err := <-answer:
					return err
				case <-ctx.Done():
					return ctx.Err()
				}
			}}
			s := h.join(t, c.room, "1", dev, Options{})
			reports(t, s, 5*time.Second, is(voiceipc.DeviceAsking, voiceipc.DeviceOpen))
			answer <- c.answer
			// Within silentFor, after which a silent microphone has failed whatever the answer did.
			reports(t, s, silentFor, is(c.want, voiceipc.DeviceOpen))
			staysUp(t, s, 500*time.Millisecond)
		})
	}
}

// A stream PulseAudio moves to its stand-in mid-call is reported, and so is its return, and neither ends
// the call: the stream comes back by itself when somebody logs in again (M32's spike).
func TestAStreamMovedMidCallIsReportedBothWays(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	var gone atomic.Bool
	dev := &audiotest.Device{Where: func(bool) error {
		if gone.Load() {
			return failure(audio.NoSession, "a stand-in")
		}
		return nil
	}}
	s := h.join(t, "1", "1", dev, Options{})
	reports(t, s, 5*time.Second, is(voiceipc.DeviceOpen, voiceipc.DeviceOpen))
	gone.Store(true)
	reports(t, s, 3*watchEvery, is(voiceipc.DeviceNoSession, voiceipc.DeviceNoSession))
	staysUp(t, s, 500*time.Millisecond)
	gone.Store(false)
	reports(t, s, 3*watchEvery, is(voiceipc.DeviceOpen, voiceipc.DeviceOpen))
}

// A device slow to deliver its first frame is not an outcome the call's start is judged on: a Bluetooth
// headset changing profile as its microphone opens can take longer than silentFor. It is reported failed,
// and the call stays up, so that it works once the frames come.
func TestSilenceAtTheStartEndsNoCall(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	s := h.join(t, "1", "1", &audiotest.Device{Silent: true, PlayErr: failure(audio.NoSession, "nobody")}, Options{})
	reports(t, s, silentFor+3*time.Second, is(voiceipc.DeviceFailed, voiceipc.DeviceNoSession))
	staysUp(t, s, time.Second)
}

// A device is open at its first frame, not at the watch's next tick: a join waits on the microphone.
func TestADeviceIsOpenAtItsFirstFrame(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	s := h.join(t, "1", "1", &audiotest.Device{}, Options{})
	ready := time.Now()
	reports(t, s, 5*time.Second, func(d voiceipc.Devices) bool { return d.Microphone.State == voiceipc.DeviceOpen })
	if took := time.Since(ready); took >= deviceTick {
		t.Errorf("the microphone was reported open %v after the call was ready, at a tick of %v", took, deviceTick)
	}
}

// An answer that comes once the call has given its question up is no refusal, whatever it says: the
// microphone is never reported refused, and the call is not ended as its devices'.
func TestAnAnswerAfterTheCallLeftIsNoRefusal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	answered := make(chan struct{})
	dev := &audiotest.Asking{Device: &audiotest.Device{PlayErr: failure(audio.Refused, "no output for you")},
		Ask: func(ctx context.Context) error {
			<-ctx.Done()
			defer close(answered)
			return failure(audio.Refused, "the person said no as the call closed")
		}}
	s := h.join(t, "1", "1", dev, Options{})
	reports(t, s, 5*time.Second, is(voiceipc.DeviceAsking, voiceipc.DeviceRefused))
	s.Close()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("the question was never given up")
	}
	<-s.authDone
	if m := s.devs.current().Microphone; m.State != voiceipc.DeviceAsking {
		t.Errorf("the microphone was reported %+v after the call left", m)
	}
	if s.EndedBy() != nil {
		t.Error("the call says its devices ended it")
	}
}

// An answer that is the question given up is no refusal, whenever it arrives: it never reports the
// microphone refused, nor ends the call as its devices'.
func TestAGivenUpQuestionIsNoRefusal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	answer := make(chan struct{})
	dev := &audiotest.Asking{Device: &audiotest.Device{PlayErr: failure(audio.Refused, "no output for you")},
		Ask: func(context.Context) error { <-answer; return context.Canceled }}
	s := h.join(t, "1", "1", dev, Options{})
	reports(t, s, 5*time.Second, is(voiceipc.DeviceAsking, voiceipc.DeviceRefused))
	close(answer)
	staysUp(t, s, 500*time.Millisecond)
	if m := s.devs.current().Microphone; m.State != voiceipc.DeviceAsking {
		t.Errorf("the microphone was left %+v", m)
	}
}

// Leaving gives the question up before it waits on anything, so it leaves the screen with the call and an
// answer given while the call closes is no refusal: here the speakers' Close holds the media's close to
// its bound, and the question is given up all the same.
func TestTheQuestionIsGivenUpBeforeTheCloseWaits(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	given := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	dev := slowSpeakers{Asking: &audiotest.Asking{Device: &audiotest.Device{}, Ask: func(ctx context.Context) error {
		<-ctx.Done()
		close(given)
		return context.Canceled
	}}, release: release}
	s := h.join(t, "1", "1", dev, Options{})
	reports(t, s, 5*time.Second, is(voiceipc.DeviceAsking, voiceipc.DeviceOpen))
	go s.Close()
	select {
	case <-given:
	case <-time.After(closeWithin / 2):
		t.Error("the question was given up only once the close had waited")
	}
}

// slowSpeakers is a device whose speakers' Close waits until released, as a driver slow to close does.
type slowSpeakers struct {
	*audiotest.Asking
	release chan struct{}
}

func (d slowSpeakers) Play(ctx context.Context, fill func([]int16)) (io.Closer, error) {
	c, err := d.Asking.Play(ctx, fill)
	if err != nil {
		return nil, err
	}
	return heldClose{c, d.release}, nil
}

type heldClose struct {
	io.Closer
	release chan struct{}
}

func (c heldClose) Close() error { <-c.release; return c.Closer.Close() }
