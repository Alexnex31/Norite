// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// refusingServer gives a route and refuses the provision, as a voice server whose room is full does,
// remembering the worker that was running when it refused.
type refusingServer struct {
	v      *Voice
	worker atomic.Pointer[Worker]
}

func (s *refusingServer) Route(context.Context, string) (Route, error) { return Route{}, nil }

func (s *refusingServer) Join(context.Context, string, string, string) (string, error) {
	s.v.mu.Lock()
	if s.v.current != nil {
		s.worker.Store(s.v.current.worker)
	}
	s.v.mu.Unlock()
	return "", errors.New("the voice server refused: 409 room full")
}

func standinVoice(t *testing.T, mode string, signaler Signaler) *Voice {
	t.Helper()
	o := standinOptions(t, mode, nil)
	v := NewVoice(VoiceOptions{Program: o.Program, Version: o.Version, Signaler: signaler, Env: o.Env,
		Account: func() (SignIn, error) { return SignIn{Account: "42"}, nil }, Log: zerolog.Nop()})
	t.Cleanup(v.Close)
	return v
}

// A join the voice server refuses answers only once its worker is reaped: a retry must not start a
// second worker beside one still leaving, and a stopping daemon must not go on without it.
func TestAFailedJoinAnswersOnceItsWorkerIsGone(t *testing.T) {
	fast(t)
	srv := &refusingServer{}
	v := standinVoice(t, "good", srv)
	srv.v = v
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.ErrorContains(t, err, "room full")
	// The status the answer carries is the failure's, so the two agree, and a retry finds nothing to leave.
	require.Equal(t, ipc.VoiceIdle, st.State)
	require.Contains(t, st.Reason, "room full")
	w := srv.worker.Load()
	require.NotNil(t, w, "the worker was never started")
	select {
	case <-w.Done():
	default:
		t.Fatal("the join was answered while its worker was still running")
	}
	require.False(t, v.Busy())
	require.Equal(t, ipc.VoiceIdle, v.Status().State)
}

// blockingServer holds every route until its context ends.
type blockingServer struct{ asked atomic.Int32 }

func (s *blockingServer) Route(ctx context.Context, _ string) (Route, error) {
	s.asked.Add(1)
	<-ctx.Done()
	return Route{}, ctx.Err()
}

func (s *blockingServer) Join(context.Context, string, string, string) (string, error) {
	return "", errors.New("unreachable")
}

// A join whose asker went away while it waited behind another is not done: it would end that other call
// for a channel nobody wants any more.
func TestAJoinWhoseAskerLeftWhileWaitingIsNotDone(t *testing.T) {
	srv := &blockingServer{}
	v := standinVoice(t, "good", srv)
	v.joinMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	answered := make(chan error, 1)
	go func() {
		_, err := v.Join(ctx, "2", ipc.VoiceAllOn())
		answered <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	v.joinMu.Unlock()
	select {
	case err := <-answered:
		// Not "still under way", which the daemon answers a join that goes on without its asker.
		var refused *Refusal
		require.True(t, errors.As(err, &refused), "%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the join did not answer")
	}
	require.Zero(t, srv.asked.Load(), "the abandoned join went on to ask the voice server")
	require.Equal(t, ipc.VoiceIdle, v.Status().State)
}

// The sign-in not read yet is not being signed out: the join says to ask again, not to log in.
func TestAJoinBeforeTheSignInIsReadIsPending(t *testing.T) {
	v := NewVoice(VoiceOptions{Signaler: &blockingServer{}, Log: zerolog.Nop(),
		Account: func() (SignIn, error) { return SignIn{}, ErrSignInPending }})
	_, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	var refused *Refusal
	require.True(t, errors.As(err, &refused))
	require.True(t, refused.Pending)
	require.NotContains(t, refused.Msg, "norite login")
}

// recordingServer refuses every route, remembering the account each was asked for.
type recordingServer struct {
	mu       sync.Mutex
	accounts []string
}

func (s *recordingServer) Route(_ context.Context, account string) (Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = append(s.accounts, account)
	return Route{}, errors.New("the voice server refused: 503")
}

func (s *recordingServer) Join(context.Context, string, string, string) (string, error) {
	return "", errors.New("unreachable")
}

func (s *recordingServer) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.accounts...)
}

// A join that read the sign-in and then waited behind another is not made once the sign-in has ended: it
// would go on as an account nobody here is signed in as.
func TestAJoinWaitingWhileTheSignInEndsIsNotMade(t *testing.T) {
	srv := &recordingServer{}
	ended := make(chan struct{})
	v := NewVoice(VoiceOptions{Signaler: srv, Log: zerolog.Nop(),
		Account: func() (SignIn, error) { return SignIn{Account: "42", Ended: ended}, nil }})
	t.Cleanup(v.Close)
	v.joinMu.Lock()
	answered := make(chan error, 1)
	go func() {
		_, err := v.Join(context.Background(), "2", ipc.VoiceAllOn())
		answered <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(ended)
	v.joinMu.Unlock()
	select {
	case err := <-answered:
		var refused *Refusal
		require.True(t, errors.As(err, &refused), "%v", err)
		require.True(t, refused.Pending, "%+v", refused)
	case <-time.After(5 * time.Second):
		t.Fatal("the join did not answer")
	}
	require.Empty(t, srv.asked(), "the join went on as the account whose sign-in ended")
	require.Equal(t, ipc.VoiceIdle, v.Status().State)
}

// The end of a sign-in ends the call joined under it, and says so.
func TestTheEndOfTheSignInEndsTheCall(t *testing.T) {
	ended := make(chan struct{})
	v := NewVoice(VoiceOptions{Signaler: &blockingServer{}, Log: zerolog.Nop(),
		Account: func() (SignIn, error) { return SignIn{Account: "42", Ended: ended}, nil }})
	t.Cleanup(v.Close)
	go func() { _, _ = v.Join(context.Background(), "1", ipc.VoiceAllOn()) }()
	require.Eventually(t, v.Busy, 5*time.Second, 5*time.Millisecond)
	close(ended)
	require.Eventually(t, func() bool { return !v.Busy() }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, ipc.VoiceStatus{State: ipc.VoiceIdle, Reason: "the sign-in ended", Available: true}, v.Status())
}

// The join's settings reach the worker in its JOIN, each switch where the request put it (M30).
func TestTheJoinsSettingsReachTheWorker(t *testing.T) {
	fast(t)
	for _, p := range []ipc.VoiceJoin{
		{EchoCancellation: true}, {NoiseSuppression: true}, {AutoGain: true}, ipc.VoiceAllOn(), {},
	} {
		log := &logBuffer{}
		o := standinOptions(t, "processing", nil)
		srv := &refusingServer{}
		v := NewVoice(VoiceOptions{Program: o.Program, Version: o.Version, Signaler: srv, Env: o.Env,
			Account: func() (SignIn, error) { return SignIn{Account: "42"}, nil }, Log: zerolog.New(log)})
		srv.v = v
		_, err := v.Join(context.Background(), "1", p)
		require.ErrorContains(t, err, "room full")
		want := fmt.Sprintf("processing %t %t %t", p.EchoCancellation, p.NoiseSuppression, p.AutoGain)
		require.Eventually(t, func() bool { return strings.Contains(log.String(), want) },
			5*time.Second, 10*time.Millisecond, "%s", log.String())
		v.Close()
	}
}

// A leave while the worker is still starting is a leave: logged as the worker exiting, never as a crash.
func TestALeaveWhileTheWorkerStartsIsNoCrash(t *testing.T) {
	fast(t)
	log := &logBuffer{}
	o := standinOptions(t, "silent", nil)
	v := NewVoice(VoiceOptions{Program: o.Program, Version: o.Version, Signaler: &refusingServer{}, Env: o.Env,
		Account: func() (SignIn, error) { return SignIn{Account: "42"}, nil }, Log: zerolog.New(log)})
	t.Cleanup(v.Close)
	answered := make(chan error, 1)
	go func() {
		_, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
		answered <- err
	}()
	require.Eventually(t, v.Busy, 5*time.Second, 5*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // inside Start, waiting for a HELLO that does not come
	v.Leave()
	select {
	case err := <-answered:
		require.ErrorContains(t, err, "left before it was up")
	case <-time.After(5 * time.Second):
		t.Fatal("the join did not answer")
	}
	require.Eventually(t, func() bool { return strings.Contains(log.String(), "the voice worker exited") },
		5*time.Second, 10*time.Millisecond, "%s", log.String())
	require.NotContains(t, log.String(), "crashed")
	require.NotContains(t, log.String(), `"level":"warn"`)
	require.Equal(t, ipc.VoiceStatus{State: ipc.VoiceIdle, Reason: "left", Available: true}, v.Status())
}

// The leave a sign-in's end starts is that call's: by the time it runs, a join under the next sign-in may
// be the call, and it is left alone.
func TestTheEndOfASignInLeavesOnlyItsOwnCall(t *testing.T) {
	v := standinVoice(t, "good", &refusingServer{})
	ended := &call{channel: "1", cancel: func() {}, finished: make(chan struct{})}
	close(ended.finished)
	next := &call{channel: "2", cancel: func() {}, finished: make(chan struct{})}
	close(next.finished)
	v.mu.Lock()
	v.current = next
	v.status.State, v.status.Channel = ipc.VoiceInCall, "2"
	v.mu.Unlock()
	v.leave(ended, "the sign-in ended")
	require.True(t, v.Busy())
	require.Equal(t, ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "2", Available: true}, v.Status())
}

// routelessServer refuses the route at once, so a join fails before any worker is started.
type routelessServer struct{}

func (routelessServer) Route(context.Context, string) (Route, error) {
	return Route{}, errors.New("the voice server refused: 503")
}

func (routelessServer) Join(context.Context, string, string, string) (string, error) {
	return "", errors.New("unreachable")
}

// A failed join is over when it is answered: the status says why, and a retry that comes at once finds no
// call of the failed one's to leave, which would write "left" over the reason. Staged rather than raced:
// Changed, told the join is idle, waits to see whether the asker is answered meanwhile, which it can be only
// if the answer went first.
func TestAFailedJoinIsOverWhenItIsAnswered(t *testing.T) {
	var answered, early atomic.Bool
	idle := make(chan struct{})
	v := NewVoice(VoiceOptions{Signaler: routelessServer{}, Log: zerolog.Nop(),
		Account: func() (SignIn, error) { return SignIn{Account: "42"}, nil },
		Changed: func(st ipc.VoiceStatus) {
			if st.State != ipc.VoiceIdle {
				return
			}
			defer close(idle)
			deadline := time.Now().Add(100 * time.Millisecond)
			for time.Now().Before(deadline) {
				if answered.Load() {
					early.Store(true)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}})
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	answered.Store(true)
	require.Error(t, err)
	require.NoError(t, within(5*time.Second, idle))
	require.False(t, early.Load(), "the asker was answered before the join was over")
	require.False(t, v.Busy())
	require.Equal(t, ipc.VoiceIdle, st.State)
	require.Contains(t, st.Reason, "503")
}
