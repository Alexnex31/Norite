// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/jfreymuth/pulse"
)

// latency is what each direction asks the server to buffer: one frame. PipeWire serves the PulseAudio
// protocol through pipewire-pulse and honors it; a PulseAudio server may round it up.
const latency = 0.02

// System is the PulseAudio protocol, which PipeWire speaks on every mainstream desktop through
// pipewire-pulse. The server is found through PULSE_SERVER or $XDG_RUNTIME_DIR/pulse/native, and a
// PulseAudio cookie through PULSE_COOKIE or ~/.config/pulse/cookie, all of which the daemon passes to the
// worker. An ALSA-only system has no server, and capture fails with the reason.
func System() Device { return pulseDevice{} }

type pulseDevice struct{}

// stream is one client connection with one stream on it: each direction has its own, so closing one
// cannot reach the other, and the library's "a closed client may panic" stays inside one of them.
type stream struct {
	once  sync.Once
	c     *pulse.Client
	close func()
}

func (s *stream) Close() error {
	s.once.Do(func() {
		s.close()
		s.c.Close()
	})
	return nil
}

func connect(name string) (*pulse.Client, error) {
	c, err := pulse.NewClient(pulse.ClientApplicationName("Norite"), pulse.ClientApplicationIconName("norite"))
	if err != nil {
		return nil, fmt.Errorf("audio: no PulseAudio or PipeWire server answered for the %s: %w", name, err)
	}
	return c, nil
}

func (pulseDevice) Capture(_ context.Context, frame func([]int16)) (io.Closer, error) {
	c, err := connect("microphone")
	if err != nil {
		return nil, err
	}
	f := &framer{frame: frame}
	r, err := c.NewRecord(pulse.Int16Writer(func(b []int16) (int, error) {
		f.write(b)
		return len(b), nil
	}), pulse.RecordMono, pulse.RecordSampleRate(SampleRate), pulse.RecordLatency(latency),
		pulse.RecordMediaName("Norite voice call"))
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("audio: opening the microphone: %w", err)
	}
	if r.SampleRate() != SampleRate || r.Channels() != 1 {
		r.Close()
		c.Close()
		return nil, fmt.Errorf("audio: the microphone opened at %d Hz and %d channels", r.SampleRate(), r.Channels())
	}
	r.Start()
	return &stream{c: c, close: r.Close}, nil
}

func (pulseDevice) Play(_ context.Context, fill func([]int16)) (io.Closer, error) {
	c, err := connect("speakers")
	if err != nil {
		return nil, err
	}
	f := newFiller(fill)
	p, err := c.NewPlayback(pulse.Int16Reader(func(b []int16) (int, error) {
		f.read(b)
		return len(b), nil
	}), pulse.PlaybackMono, pulse.PlaybackSampleRate(SampleRate), pulse.PlaybackLatency(latency),
		pulse.PlaybackMediaName("Norite voice call"))
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("audio: opening the speakers: %w", err)
	}
	if p.SampleRate() != SampleRate || p.Channels() != 1 {
		p.Close()
		c.Close()
		return nil, errors.New("audio: the speakers opened in another format")
	}
	p.Start()
	return &stream{c: c, close: p.Close}, nil
}
