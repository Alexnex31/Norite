// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// One second's worth of output at the bound, as empty lines and as full lines: what a hostile worker can
// make the daemon do under outputPerSecond.
func benchOutput(b *testing.B, payload string) {
	for b.Loop() {
		r, w, _ := os.Pipe()
		wk := &Worker{log: zerolog.New(io.Discard), lines: &ring{max: lastLines}, outDone: make(chan struct{})}
		go func() { _, _ = w.WriteString(payload); _ = w.Close() }()
		wk.readOutput(r)
	}
}

func BenchmarkOutputEmptyLines(b *testing.B) { benchOutput(b, strings.Repeat("\n", outputPerSecond)) }
func BenchmarkOutputFullLines(b *testing.B) {
	benchOutput(b, strings.Repeat(strings.Repeat("x", maxLine-1)+"\n", outputPerSecond/maxLine))
}

func TestTheRingKeepsTheLastInOrder(t *testing.T) {
	r := &ring{max: 3}
	if got := r.all(); len(got) != 0 {
		t.Fatal(got)
	}
	for _, s := range []string{"a", "b"} {
		r.add(s)
	}
	if got := strings.Join(r.all(), ""); got != "ab" {
		t.Fatal(got)
	}
	for _, s := range []string{"c", "d", "e"} {
		r.add(s)
	}
	if got := strings.Join(r.all(), ""); got != "cde" {
		t.Fatal(got)
	}
}

// An empty line is charged against the bound and nothing else: no log entry, no place among the lines kept
// for a crash.
func TestAnEmptyLineMakesNothing(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	log := &logBuffer{}
	wk := &Worker{log: zerolog.New(log), lines: &ring{max: lastLines}, outDone: make(chan struct{})}
	go func() { _, _ = w.WriteString("\n\r\n\nreal\n\n"); _ = w.Close() }()
	wk.readOutput(r)
	if got := log.workerLines(); len(got) != 1 || got[0] != "real" {
		t.Fatalf("logged %q", got)
	}
	if got := wk.lines.all(); len(got) != 1 || got[0] != "real" {
		t.Fatalf("kept %q", got)
	}
}
