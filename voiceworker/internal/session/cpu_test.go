// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package session

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest"
	"github.com/Alexnex31/Norite/voiceworker/internal/dsp"
	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

// native is set by cpu_native_test.go under the norite_opusnative tag.
var nativeCodec func(context.Context) (opus.Codec, error)

// ADR 0036's second measurement, on the finished pipeline: what a call costs this process, both callers'
// sessions together, for ten seconds of both speaking, with the WebAssembly codec and with the native twin.
// It measures rather than checks, so it runs only when asked:
//
//	NORITE_MEASURE=1 taskset -c 2 go test -tags norite_opusnative -run TestWhatACallCosts -v ./internal/session/
func TestWhatACallCosts(t *testing.T) {
	if os.Getenv("NORITE_MEASURE") == "" {
		t.Skip("a measurement, run with NORITE_MEASURE=1")
	}
	codecs := map[string]func(context.Context) (opus.Codec, error){"wasm": opus.New}
	if nativeCodec != nil {
		codecs["native"] = nativeCodec
	}
	h := startHarness(t)
	room := 0
	for name, mk := range codecs {
		room++
		r := string(rune('0' + room))
		a := &audiotest.Device{Source: audiotest.Tone(440, 8000)}
		b := &audiotest.Device{Source: audiotest.Tone(660, 8000)}
		// The DSP on, as every call has it from M30: what a call costs is the codec and the chain.
		sa := h.joinWith(t, r, "1", a, dsp.AllOn(), Options{Codec: mk})
		sb := h.joinWith(t, r, "2", b, dsp.AllOn(), Options{Codec: mk})
		time.Sleep(2 * time.Second) // settled
		before, wall := cpu(), time.Now()
		time.Sleep(10 * time.Second)
		used, elapsed := cpu()-before, time.Since(wall)
		t.Logf("%s: %v of CPU in %v for two callers, %.2f%% of one core per caller",
			name, used.Round(time.Millisecond), elapsed.Round(time.Millisecond),
			100*used.Seconds()/elapsed.Seconds()/2)
		sa.Close()
		sb.Close()
	}
}

func cpu() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
