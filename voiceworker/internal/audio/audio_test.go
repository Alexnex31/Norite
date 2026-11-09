// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"slices"
	"testing"
)

// A device hands over whatever lengths it likes; the session sees whole frames, in order, nothing lost or
// repeated.
func TestCaptureIsCutIntoWholeFrames(t *testing.T) {
	var got []int16
	frames := 0
	f := &framer{frame: func(b []int16) {
		if len(b) != FrameSamples {
			t.Fatalf("a frame of %d samples", len(b))
		}
		frames++
		got = append(got, b...)
	}}
	var sent []int16
	next := int16(0)
	for _, n := range []int{1, 959, 960, 961, 4000, 0, 7, 2000, 313} {
		chunk := make([]int16, n)
		for i := range chunk {
			chunk[i] = next
			next++
		}
		sent = append(sent, chunk...)
		f.write(chunk)
	}
	if want := len(sent) / FrameSamples; frames != want {
		t.Fatalf("%d frames from %d samples, want %d", frames, len(sent), want)
	}
	if !slices.Equal(got, sent[:len(got)]) {
		t.Fatal("the frames are not the samples in order")
	}
}

func TestPlaybackIsFilledFromWholeFrames(t *testing.T) {
	next := int16(0)
	asked := 0
	f := newFiller(func(b []int16) {
		if len(b) != FrameSamples {
			t.Fatalf("asked for a frame of %d samples", len(b))
		}
		asked++
		for i := range b {
			b[i] = next
			next++
		}
	})
	var got []int16
	for _, n := range []int{1, 959, 960, 961, 4000, 0, 7, 2000, 313} {
		out := make([]int16, n)
		f.read(out)
		got = append(got, out...)
	}
	for i, s := range got {
		if s != int16(i) {
			t.Fatalf("sample %d is %d", i, s)
		}
	}
	if want := (len(got) + FrameSamples - 1) / FrameSamples; asked != want {
		t.Errorf("asked for %d frames to play %d samples, want %d", asked, len(got), want)
	}
}
