// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package dsp

import (
	"context"
	"slices"
	"testing"
	"time"
)

// BenchmarkChain is M30's measurement against the threshold the author set before measuring: the chain's
// work for one 20 ms frame of the worker's (two chunks played, two recorded), unpaced, as internal/opus's
// BenchmarkFrame measures the codec. The threshold is 10% of one core for the whole chain, per caller;
// share-% is the p50 against the frame's 20 ms. Run it pinned to one core:
//
//	taskset -c 2 go test -run '^$' -bench Chain -benchtime 6000x ./internal/dsp/
func BenchmarkChain(b *testing.B) {
	c := newChain(b, newModule(b), AllOn())
	s := newScene(30, 1)
	pcm := make([]int16, ChunkSamples)
	ts := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := range b.N {
		k := 2 * i % (len(s.mic) - 1)
		t0 := time.Now()
		for h := range 2 {
			if err := c.Render(s.far[k+h]); err != nil {
				b.Fatal(err)
			}
			copy(pcm, s.mic[k+h])
			if _, err := c.Capture(pcm); err != nil {
				b.Fatal(err)
			}
		}
		ts = append(ts, time.Since(t0))
	}
	b.StopTimer()
	p50 := percentile(ts, 0.5)
	b.ReportMetric(p50, "frame-p50-µs")
	b.ReportMetric(percentile(ts, 0.99), "frame-p99-µs")
	b.ReportMetric(100*p50/20000, "share-%")
}

// BenchmarkStart is what a call pays before its first frame: compiling the module, then one chain.
func BenchmarkStart(b *testing.B) {
	ctx := context.Background()
	for range b.N {
		m, err := New(ctx)
		if err != nil {
			b.Fatal(err)
		}
		c, err := m.NewChain(ctx, AllOn())
		if err != nil {
			b.Fatal(err)
		}
		_ = c.Close()
		_ = m.Close(ctx)
	}
}

func percentile(d []time.Duration, p float64) float64 {
	s := slices.Clone(d)
	slices.Sort(s)
	return float64(s[int(p*float64(len(s)-1))].Nanoseconds()) / 1000
}
