// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"errors"
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
		Account: func() (string, error) { return "42", nil }, Log: zerolog.Nop()})
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
	_, err := v.Join(context.Background(), "1")
	require.ErrorContains(t, err, "room full")
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
		_, err := v.Join(ctx, "2")
		answered <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	v.joinMu.Unlock()
	select {
	case err := <-answered:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("the join did not answer")
	}
	require.Zero(t, srv.asked.Load(), "the abandoned join went on to ask the voice server")
	require.Equal(t, ipc.VoiceIdle, v.Status().State)
}

// The sign-in not read yet is not being signed out: the join says to ask again, not to log in.
func TestAJoinBeforeTheSignInIsReadIsPending(t *testing.T) {
	v := NewVoice(VoiceOptions{Signaler: &blockingServer{}, Log: zerolog.Nop(),
		Account: func() (string, error) { return "", ErrSignInPending }})
	_, err := v.Join(context.Background(), "1")
	var refused *Refusal
	require.True(t, errors.As(err, &refused))
	require.True(t, refused.Pending)
	require.NotContains(t, refused.Msg, "norite login")
}
