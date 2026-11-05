// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/jitter"
	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

// stuck is a decoder whose Decode never returns until released: a libopus bug that loops, which no call
// into WebAssembly can interrupt (ADR 0036).
type stuck struct {
	opus.Decoder
	release chan struct{}
}

func (d stuck) Decode(context.Context, []byte, []int16) (int, error) { <-d.release; return 0, nil }
func (d stuck) Reset(context.Context) error                          { return nil }
func (d stuck) Conceal(context.Context, []int16) (int, error)        { return 0, errors.New("stuck") }

// A decode that never returns ends the call, with the reason, rather than leaving a core spinning and the
// speakers silent until somebody leaves.
func TestADecodeThatNeverReturnsEndsTheCall(t *testing.T) {
	s := &Session{ended: make(chan string, 1)}
	release := make(chan struct{})
	defer close(release)
	buf := jitter.New()
	for i := range jitter.MinDelay {
		buf.Push(7, uint16(i), uint32(i*jitter.FrameSamples), []byte{0xf8, 0xff, 0xfe}, time.Now())
	}
	m := &media{s: s, stop: make(chan struct{}), slots: []*slot{{buf: buf, dec: stuck{release: release},
		pcm: make([]int16, 0, opus.MaxDecodeSamples+audio.FrameSamples), tmp: make([]int16, opus.MaxDecodeSamples)}}}
	m.sent.Add(1)
	go m.watch()
	go m.fill(make([]int16, audio.FrameSamples))
	select {
	case reason := <-s.ended:
		if !strings.Contains(reason, "decoder stopped responding") {
			t.Errorf("the call ended with %q", reason)
		}
	case <-time.After(3 * stuckDecode):
		t.Fatal("the call did not end")
	}
	close(m.stop)
	m.sent.Wait()
}
