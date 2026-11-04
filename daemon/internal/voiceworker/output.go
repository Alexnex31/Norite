// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"bufio"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// readOutput reads what the worker prints, stdout and stderr together, into the daemon's log: Pion's
// warnings, a panic's trace, and from M29 whatever a C library writes. Each line is cut to maxLine, cleaned
// and logged as one quoted field, and the last lastLines are kept to go with a crash.
//
// Two bounds, because bytes and log entries cost differently. Bytes are counted as they are read, a line
// with no end included, and a worker past outputPerSecond is killed: reading it costs the daemon what
// writing it costs the worker. Log entries are bounded apart, at linesPerSecond: an empty line is one byte
// read and a whole entry written, so a worker printing newlines would otherwise rotate the daemon's log
// away within the byte bound. The lines past it are kept for a crash and counted, and the count is logged.
func (w *Worker) readOutput(f *os.File) {
	defer close(w.outDone)
	defer func() { _ = f.Close() }()
	br := bufio.NewReaderSize(f, maxLine)
	var (
		window  time.Time
		spent   int
		logged  int
		dropped int
	)
	tick := func() {
		if now := time.Now(); now.Sub(window) >= time.Second {
			if dropped > 0 {
				w.log.Warn().Int("lines", dropped).Msg("voice worker lines past the rate, not logged")
			}
			window, spent, logged, dropped = now, 0, 0, 0
		}
	}
	charge := func(n int) bool {
		tick()
		spent += n
		return spent <= outputPerSecond
	}
	for {
		l, err := readLine(br, charge)
		if errors.Is(err, errOverBudget) {
			w.Kill("the voice worker printed more than the daemon reads")
			_, _ = io.Copy(io.Discard, br)
			return
		}
		if l.size > 0 || err == nil {
			text := termsafe.Text(string(l.kept))
			w.lines.add(text)
			tick()
			if logged < linesPerSecond {
				logged++
				w.log.Info().Str("line", text).Msg("voice worker")
			} else {
				dropped++
			}
		}
		if err != nil {
			if dropped > 0 {
				w.log.Warn().Int("lines", dropped).Msg("voice worker lines past the rate, not logged")
			}
			return
		}
	}
}

// errOverBudget is output past outputPerSecond.
var errOverBudget = errors.New("past the output bound")

type line struct {
	kept []byte
	size int
}

// readLine reads one line, keeping its first maxLine bytes, and charges every chunk it reads as it reads
// it: a line that never ends is stopped by the bound rather than read for ever.
func readLine(br *bufio.Reader, charge func(int) bool) (line, error) {
	var l line
	for {
		chunk, err := br.ReadSlice('\n')
		l.size += len(chunk)
		if !charge(len(chunk)) {
			return l, errOverBudget
		}
		if room := maxLine - len(l.kept); room > 0 {
			l.kept = append(l.kept, chunk[:min(room, len(chunk))]...)
		}
		switch {
		case err == nil:
			l.kept = trimNewline(l.kept)
			return l, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return l, err
		}
	}
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// ring keeps the last max lines.
type ring struct {
	mu    sync.Mutex
	max   int
	lines []string
}

func (r *ring) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) == r.max {
		r.lines = r.lines[1:]
	}
	r.lines = append(r.lines, s)
}

func (r *ring) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}
