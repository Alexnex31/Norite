// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/internal/hotkey"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// fakeBinder is a hotkey.Binder whose bindings the test drives: what became of each key, and the key going
// down and up. stop waits for nothing, since the test makes every report itself.
type fakeBinder struct {
	mu       sync.Mutex
	bindings []*fakeBinding
}

type fakeBinding struct {
	key     string
	ev      hotkey.Events
	stopped chan struct{}
}

func (b *fakeBinder) Bind(key string, ev hotkey.Events) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	fb := &fakeBinding{key: key, ev: ev, stopped: make(chan struct{})}
	b.bindings = append(b.bindings, fb)
	return func() { close(fb.stopped) }
}

// all is every binding made so far, the newest last.
func (b *fakeBinder) all() []*fakeBinding {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.bindings)
}

func (fb *fakeBinding) live() bool {
	select {
	case <-fb.stopped:
		return false
	default:
		return true
	}
}

// inputVoice is a Voice whose worker says each input it is sent into the log, and whose keys are binder's.
func inputVoice(t *testing.T, binder hotkey.Binder) (*Voice, *logBuffer) {
	t.Helper()
	logged := &logBuffer{}
	o := standinOptions(t, "inputs", nil)
	v := NewVoice(VoiceOptions{Program: o.Program, Version: o.Version, Signaler: &selfServer{}, Env: o.Env,
		Account: func() (SignIn, error) { return SignIn{Account: "42"}, nil }, Log: zerolog.New(logged),
		Hotkey: binder})
	t.Cleanup(v.Close)
	return v, logged
}

// inputs are the inputs the worker said it was sent, in order.
func inputs(l *logBuffer) []string {
	var out []string
	for _, line := range l.workerLines() {
		if s, ok := strings.CutPrefix(line, "input "); ok {
			out = append(out, s)
		}
	}
	return out
}

// waitInputs waits for the worker to have said want, in order.
func waitInputs(t *testing.T, l *logBuffer, want ...string) {
	t.Helper()
	require.Eventually(t, func() bool { return slices.Equal(inputs(l), want) }, 5*time.Second, 5*time.Millisecond,
		"the worker was sent %v, want %v", inputs(l), want)
}

func byPushToTalk(key string) ipc.VoiceJoinRequest {
	return ipc.VoiceJoinRequest{VoiceJoin: ipc.VoiceAllOn(),
		VoiceInput: ipc.VoiceInput{Mode: ipc.VoiceInputPushToTalk, Key: key}}
}

// A call by push-to-talk registers its key as it starts and releases it before the leave returns, so another
// program has it back the moment the person is out of the call (ADR 0034 §3); what became of the key is the
// status's, and the key going down and up reaches the worker, which starts with it up.
func TestPushToTalkHoldsTheKeyForTheCallAlone(t *testing.T) {
	fast(t)
	binder := &fakeBinder{}
	v, logged := inputVoice(t, binder)
	st, err := v.JoinWith(context.Background(), "1", byPushToTalk("ctrl+shift+space"))
	require.NoError(t, err)
	bound := binder.all()
	require.Len(t, bound, 1)
	fb := bound[0]
	require.Equal(t, "ctrl+shift+space", fb.key)
	require.Equal(t, &ipc.VoiceInputStatus{Mode: ipc.VoiceInputPushToTalk, Key: "ctrl+shift+space",
		Hotkey: ipc.VoiceHotkeyRegistering}, st.Input)
	waitInputs(t, logged, "push_to_talk false")

	// The desktop's words for the key are a stranger's, and arrive cleaned (rule 19).
	fb.ev.Status(hotkey.Status{State: ipc.VoiceHotkeyRegistered, Trigger: "Ctrl+Shift+Space\x1b[2J"})
	in := v.Status().Input
	require.Equal(t, ipc.VoiceHotkeyRegistered, in.Hotkey)
	require.NotContains(t, in.Trigger, "\x1b")

	fb.ev.Held(true)
	require.True(t, v.Status().Input.Held)
	fb.ev.Held(false)
	waitInputs(t, logged, "push_to_talk false", "push_to_talk true", "push_to_talk false")

	// Asking again for what the call has changes nothing, the key's binding included: there is one daemon
	// per account and one binding per call, whichever clients attach and ask (M35's done-when).
	v.Input(ipc.VoiceInput{Mode: ipc.VoiceInputPushToTalk, Key: "ctrl+shift+space"})
	require.Len(t, binder.all(), 1)
	require.True(t, fb.live())
	require.Equal(t, ipc.VoiceHotkeyRegistered, v.Status().Input.Hotkey)

	st = v.Leave()
	require.False(t, fb.live(), "the key outlived the call")
	require.Nil(t, st.Input)
	// A report from a binding the call has let go of reaches nothing.
	fb.ev.Held(true)
	require.Nil(t, v.Status().Input)
}

// A press released before the daemon told the worker of it still reaches the worker as a press: the
// worker opens its gate for a press it saw, and one the daemon folded into the release would be a word cut
// to nothing (M25 saw the portal deliver both at one instant). Staged by holding the sends while the key
// goes down and up.
func TestAPressReleasedBeforeItIsSentStillReachesTheWorker(t *testing.T) {
	fast(t)
	binder := &fakeBinder{}
	v, logged := inputVoice(t, binder)
	_, err := v.JoinWith(context.Background(), "1", byPushToTalk("ctrl+f9"))
	require.NoError(t, err)
	waitInputs(t, logged, "push_to_talk false")
	fb := binder.all()[0]

	v.mu.Lock()
	c := v.current
	v.mu.Unlock()
	c.sendMu.Lock()
	var held sync.WaitGroup
	for _, down := range []bool{true, false} {
		held.Add(1)
		go func() {
			defer held.Done()
			fb.ev.Held(down)
		}()
		// Each has changed the state before it waits to send: the release after the press.
		require.Eventually(t, func() bool {
			v.mu.Lock()
			defer v.mu.Unlock()
			return c.held == down && c.presses == 1
		}, 5*time.Second, time.Millisecond)
	}
	c.sendMu.Unlock()
	held.Wait()
	waitInputs(t, logged, "push_to_talk false", "push_to_talk true", "push_to_talk false")
}

// The mode changed during a call takes the key with it: push-to-talk binds it, a new key binds that one
// in its place, and voice lets it go, a key down at the change included, which then reaches the worker as
// up.
func TestAModeChangeDuringACallBindsAndReleasesTheKey(t *testing.T) {
	fast(t)
	binder := &fakeBinder{}
	v, logged := inputVoice(t, binder)
	_, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.Empty(t, binder.all(), "a call by voice took a key")

	v.Input(ipc.VoiceInput{Mode: ipc.VoiceInputPushToTalk, Key: "ctrl+shift+space"})
	first := binder.all()[0]
	first.ev.Status(hotkey.Status{State: ipc.VoiceHotkeyRegistered})
	first.ev.Held(true)

	st := v.Input(ipc.VoiceInput{Mode: ipc.VoiceInputPushToTalk, Key: "alt+f10"})
	require.False(t, first.live(), "the old key stayed bound")
	require.Equal(t, "alt+f10", binder.all()[1].key)
	require.Equal(t, ipc.VoiceHotkeyRegistering, st.Input.Hotkey)
	require.False(t, st.Input.Held, "the key went with its binding, and took being down with it")
	// The old binding's reports reach nothing now.
	first.ev.Status(hotkey.Status{State: ipc.VoiceHotkeyUnavailable, Reason: "late"})
	require.Equal(t, ipc.VoiceHotkeyRegistering, v.Status().Input.Hotkey)

	st = v.Input(ipc.VoiceInput{Mode: ipc.VoiceInputVoice, Key: "alt+f10"})
	require.False(t, binder.all()[1].live())
	require.Equal(t, &ipc.VoiceInputStatus{Mode: ipc.VoiceInputVoice, Key: "alt+f10"}, st.Input)
	waitInputs(t, logged, "voice false", "push_to_talk false", "push_to_talk true", "push_to_talk false", "voice false")
}

// With no key to offer, a call by push-to-talk goes on, and says why nobody hears it.
func TestPushToTalkWithNoKeyToOfferSaysSo(t *testing.T) {
	fast(t)
	v, _ := inputVoice(t, nil)
	_, err := v.JoinWith(context.Background(), "1", byPushToTalk("ctrl+shift+space"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		in := v.Status().Input
		return in != nil && in.Hotkey == ipc.VoiceHotkeyUnavailable && in.Reason != ""
	}, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, ipc.VoiceInCall, v.Status().State)
}

// The input modes and the key's states are written in two packages that cannot import each other's
// tests, and the worker reads the daemon's words as its own.
func TestTheInputWordsAgreeAcrossPackages(t *testing.T) {
	require.Equal(t, voiceipc.InputVoice, ipc.VoiceInputVoice)
	require.Equal(t, voiceipc.InputPushToTalk, ipc.VoiceInputPushToTalk)
}
