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
// and logged as one quoted field, and the last lastLines are kept to go with a crash. A worker printing past
// outputPerSecond is killed: reading it costs the daemon what writing it costs the worker.
func (w *Worker) readOutput(f *os.File) {
	defer func() { _ = f.Close() }()
	br := bufio.NewReaderSize(f, maxLine)
	var (
		window time.Time
		spent  int
	)
	for {
		line, err := readLine(br)
		if line.size > 0 || err == nil {
			if now := time.Now(); now.Sub(window) >= time.Second {
				window, spent = now, 0
			}
			spent += line.size
			if spent > outputPerSecond {
				w.Kill("the voice worker printed more than the daemon reads")
				_, _ = io.Copy(io.Discard, br)
				return
			}
			text := termsafe.Text(string(line.kept))
			w.lines.add(text)
			w.log.Info().Str("line", text).Msg("voice worker")
		}
		if err != nil {
			return
		}
	}
}

type line struct {
	kept []byte
	size int
}

// readLine reads one line, keeping its first maxLine bytes and counting the rest. A line that never ends
// is read to the end of the pipe, which is bounded by the rate above.
func readLine(br *bufio.Reader) (line, error) {
	var l line
	for {
		chunk, err := br.ReadSlice('\n')
		l.size += len(chunk)
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
