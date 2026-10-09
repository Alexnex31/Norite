// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package logfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// The reader's bounds. The log is the daemon's by convention and a file by fact: it can be edited,
// replaced, or pointed at with --file, so nothing read from it is trusted to be the size or the shape the
// daemon writes.
const (
	// MaxLine is how much of one line is kept. The daemon's lines are a few hundred bytes; past this a
	// line is cut and marked, and the rest of it is read past without being held.
	MaxLine = 16 << 10
	// MaxLines is the most entries one Tail returns.
	MaxLines = 10000
	// maxScan is how far back from the end of one file a Tail reads: the daemon rotates at 10 MB, so its
	// own files are read whole, and a file somebody else made is not read without limit.
	maxScan = 64 << 20
	// maxBatch is how much one Follower.Next reads, so a log that grew a great deal between two calls is
	// handed over in pieces.
	maxBatch = 4 << 20
	// readChunk is the buffer both directions read through.
	readChunk = 64 << 10
)

// ErrNotAFile is a log path holding something that is not a regular file: a directory, a pipe, a device.
var ErrNotAFile = errors.New("is not a regular file")

// backupTimeFormat is how lumberjack names a rotated log: <name>-<this>.<ext>, in UTC.
const backupTimeFormat = "2006-01-02T15-04-05.000"

// Entry is one line of the log.
//
// Every string in it is the file's and nobody else's: it is not sanitized here, so that --json can carry it
// whole. Whatever prints one to a terminal passes it through termsafe first (rule 19).
type Entry struct {
	// Time is the line's "time", or zero when it has none that parses.
	Time time.Time
	// Level, Subsystem and Message are the line's fields of those names when they are strings.
	Level     string
	Subsystem string
	Message   string
	// Fields is everything else the line holds, each value as it was written.
	Fields map[string]json.RawMessage
	// Unparsed is a line that is not a JSON object: Message is the line itself.
	Unparsed bool
	// Truncated is a line longer than MaxLine, of which Message holds the beginning.
	Truncated bool
}

// Options says what a Tail returns.
type Options struct {
	// Lines is how many of the newest entries to return, at most MaxLines.
	Lines int
	// Keep chooses entries; nil keeps every one. Lines counts the entries kept.
	Keep func(Entry) bool
	// Follow leaves a last line with no newline yet to the Follower instead of returning it now: the
	// daemon may be in the middle of writing it.
	Follow bool
}

// levels ranks the levels the daemon's logger writes.
var levels = map[string]int{"trace": 0, "debug": 1, "info": 2, "warn": 3, "error": 4, "fatal": 5, "panic": 6}

// Levels lists the names AtLeast accepts, lowest first.
func Levels() []string { return []string{"trace", "debug", "info", "warn", "error", "fatal", "panic"} }

// AtLeast returns a Keep for entries at min or above, and false when min is not a level.
//
// An entry with no level this build knows is kept: a line that is not the daemon's, or a traceback
// somebody pasted in, is more likely to be what a reader is looking for than not.
func AtLeast(min string) (func(Entry) bool, bool) {
	floor, ok := levels[min]
	if !ok {
		return nil, false
	}
	return func(e Entry) bool {
		rank, known := levels[e.Level]
		return !known || rank >= floor
	}, true
}

// open opens what is at path and checks, on the handle, that it is a regular file.
func open(path string) (*os.File, os.FileInfo, error) {
	file, err := openForRead(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, &fs.PathError{Op: "read", Path: path, Err: ErrNotAFile}
	}
	return file, info, nil
}

// Tail returns the newest entries of the log at path, oldest first, and a Follower placed at its end.
//
// When the live file holds fewer than were asked for, the newest rotated copy beside it supplies the rest:
// a daemon that has just rotated has an almost empty log, and the lines somebody wants are in the copy.
// No file at path is an error that is fs.ErrNotExist.
//
// The file is closed before Tail returns, and a Follower opens it afresh at each read. Nothing here holds
// the log open, because the daemon rotates by renaming it and a held file is in the way of that on Windows.
func Tail(path string, o Options) ([]Entry, *Follower, error) {
	if o.Lines < 0 {
		o.Lines = 0
	}
	if o.Lines > MaxLines {
		o.Lines = MaxLines
	}

	file, info, err := open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()

	follower := &Follower{path: path, keep: o.Keep, id: info, offset: info.Size()}
	entries, err := readNewest(file, info.Size(), o.Lines, o.Keep, &follower.split, !o.Follow)
	if err != nil {
		return nil, nil, err
	}

	if missing := o.Lines - len(entries); missing > 0 {
		if older := newestBackup(path); older != "" {
			// A copy that has gone, or cannot be read, costs the lines it held and nothing else.
			if earlier, err := tailOf(older, missing, o.Keep); err == nil {
				entries = append(earlier, entries...)
			}
		}
	}
	return entries, follower, nil
}

// tailOf returns the newest n entries of a file that is not being written to any more.
func tailOf(path string, n int, keep func(Entry) bool) ([]Entry, error) {
	file, info, err := open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var split splitter
	return readNewest(file, info.Size(), n, keep, &split, true)
}

// readNewest returns the newest n kept entries among the first size bytes of file. What it leaves in split
// is a last line with no newline yet, unless flush says to return that as an entry too.
func readNewest(file *os.File, size int64, n int, keep func(Entry) bool, split *splitter, flush bool) ([]Entry, error) {
	if n == 0 {
		// Nothing is wanted of what is already there. The start of an unfinished last line is still
		// found, so that a Follower prints the whole of it and not its end.
		start, cut, err := lineStart(file, size, 1)
		if err != nil {
			return nil, err
		}
		split.skip = cut
		if size > 0 {
			var last [1]byte
			if _, err := file.ReadAt(last[:], size-1); err != nil {
				return nil, err
			}
			if last[0] == '\n' {
				return nil, nil
			}
		}
		_, err = feed(file, start, size, split, func([]byte, bool) {})
		return nil, err
	}

	var (
		start int64
		cut   bool
		err   error
	)
	if keep == nil {
		// Exactly the lines wanted, found by counting newlines from the end.
		start, cut, err = lineStart(file, size, n)
		if err != nil {
			return nil, err
		}
	} else if size > maxScan {
		// Which lines pass is not known without reading them, so everything within the bound is read.
		start, cut = size-maxScan, true
	}
	// A start that is not known to be a line's is in the middle of one, whose end is not an entry.
	split.skip = cut

	var entries []Entry
	collect := func(line []byte, truncated bool) {
		entry := parse(line, truncated)
		if keep != nil && !keep(entry) {
			return
		}
		entries = append(entries, entry)
		// Kept to twice what is wanted and no more, however many pass.
		if len(entries) >= 2*n && len(entries) > n {
			entries = append(entries[:0], entries[len(entries)-n:]...)
		}
	}
	if _, err := feed(file, start, size, split, collect); err != nil {
		return nil, err
	}
	if flush {
		split.flush(collect)
	}
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	return entries, nil
}

// lineStart finds where the last n lines of the first size bytes begin. cut is true when the search
// stopped at maxScan and the offset is in the middle of a line.
func lineStart(file *os.File, size int64, n int) (start int64, cut bool, err error) {
	floor := int64(0)
	if size > maxScan {
		floor = size - maxScan
	}
	buf := make([]byte, readChunk)
	seen := 0
	for end := size; end > floor; {
		begin := end - int64(len(buf))
		if begin < floor {
			begin = floor
		}
		chunk := buf[:end-begin]
		if _, err := file.ReadAt(chunk, begin); err != nil {
			return 0, false, err
		}
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] != '\n' {
				continue
			}
			// The newline that ends the file ends its last line, and begins none.
			if begin+int64(i) == size-1 {
				continue
			}
			seen++
			if seen == n {
				return begin + int64(i) + 1, false, nil
			}
		}
		end = begin
	}
	return floor, floor > 0, nil
}

// feed reads file from start to end through split, and returns how many bytes it read.
func feed(file *os.File, start, end int64, split *splitter, emit func(line []byte, truncated bool)) (int64, error) {
	buf := make([]byte, readChunk)
	var read int64
	for at := start; at < end; {
		chunk := buf
		if left := end - at; left < int64(len(chunk)) {
			chunk = chunk[:left]
		}
		n, err := file.ReadAt(chunk, at)
		split.feed(chunk[:n], emit)
		at += int64(n)
		read += int64(n)
		if err != nil {
			// A file that ends before the size it had is one somebody cut short. What was read stands.
			if errors.Is(err, io.EOF) {
				return read, nil
			}
			return read, err
		}
	}
	return read, nil
}

// splitter cuts a stream of bytes into lines of at most MaxLine, holding at most one of them.
type splitter struct {
	line []byte
	// over is a line that has passed MaxLine: the rest of it is dropped until its newline.
	over bool
	// skip drops everything up to and including the next newline: the tail of a line whose beginning was
	// not read.
	skip bool
}

func (s *splitter) feed(chunk []byte, emit func(line []byte, truncated bool)) {
	for len(chunk) > 0 {
		end := bytes.IndexByte(chunk, '\n')
		part := chunk
		if end >= 0 {
			part = chunk[:end]
		}
		if !s.skip {
			if room := MaxLine - len(s.line); len(part) > room {
				s.line = append(s.line, part[:room]...)
				s.over = true
			} else {
				s.line = append(s.line, part...)
			}
		}
		if end < 0 {
			return
		}
		if !s.skip {
			emit(bytes.TrimSuffix(s.line, []byte{'\r'}), s.over)
		}
		s.reset()
		chunk = chunk[end+1:]
	}
}

// flush emits a last line that never got its newline.
func (s *splitter) flush(emit func(line []byte, truncated bool)) {
	if !s.skip && len(s.line) > 0 {
		emit(bytes.TrimSuffix(s.line, []byte{'\r'}), s.over)
	}
	s.reset()
}

func (s *splitter) reset() {
	s.line, s.over, s.skip = s.line[:0], false, false
}

// parse reads one line as the daemon writes them: a JSON object with a time, a level and a message. A
// line that is anything else is returned as itself.
func parse(line []byte, truncated bool) Entry {
	asText := func() Entry {
		return Entry{Message: strings.ToValidUTF8(string(line), "\uFFFD"), Unparsed: true, Truncated: truncated}
	}
	// A cut line is not valid JSON any more, and its beginning is still worth reading.
	if truncated || !utf8.Valid(line) {
		return asText()
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil || fields == nil {
		return asText()
	}

	entry := Entry{Fields: fields}
	// A field of a known name is taken only when it is a string. One that is not stays among the others,
	// so nothing a line holds is lost by its being unusual.
	take := func(key string, into *string) {
		var value string
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &value) == nil && len(raw) > 0 && raw[0] == '"' {
			*into = value
			delete(fields, key)
		}
	}
	take("level", &entry.Level)
	take("subsystem", &entry.Subsystem)
	take("message", &entry.Message)
	var stamp string
	if raw, ok := fields["time"]; ok && json.Unmarshal(raw, &stamp) == nil && len(raw) > 0 && raw[0] == '"' {
		if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			entry.Time = at
			delete(fields, "time")
		}
	}
	return entry
}

// newestBackup returns the most recent rotated copy of the log at path, or "".
//
// Only names lumberjack would have written count: the log's own name, a timestamp, the log's extension.
// A file somebody put beside the log is not read as part of it.
func newestBackup(path string) string {
	copies := backups(path)
	if len(copies) == 0 {
		return ""
	}
	return copies[0]
}

// backups lists the rotated copies of the log at path, newest first.
func backups(path string) []string {
	dir, base := filepath.Split(path)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext) + "-"
	listing, err := os.ReadDir(filepath.Clean(dir + "."))
	if err != nil {
		return nil
	}
	type copyOf struct {
		name  string
		stamp string
	}
	var found []copyOf
	for _, item := range listing {
		name := item.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) || len(name) <= len(prefix)+len(ext) {
			continue
		}
		stamp := name[len(prefix) : len(name)-len(ext)]
		if _, err := time.Parse(backupTimeFormat, stamp); err != nil {
			continue
		}
		found = append(found, copyOf{name: name, stamp: stamp})
	}
	// The format sorts as text in the order it sorts as time.
	sort.Slice(found, func(i, j int) bool { return found[i].stamp > found[j].stamp })
	names := make([]string, len(found))
	for i, c := range found {
		names[i] = filepath.Join(dir, c.name)
	}
	return names
}

// Follower reads what is appended to a log after a Tail, across the daemon's rotations.
//
// It holds no file between calls. Each Next opens the log, reads what is new and closes it.
type Follower struct {
	path string
	keep func(Entry) bool
	// id is the file last read, from its open handle, or nil before one has been seen. A handle's
	// identity is the file's; one taken from a path would follow the path to whatever is there now.
	id os.FileInfo
	// offset is how far into that file has been read.
	offset int64
	split  splitter
}

// Follow returns a Follower that starts at the beginning of whatever appears at path: for a log that does
// not exist yet.
func Follow(path string, keep func(Entry) bool) *Follower {
	return &Follower{path: path, keep: keep}
}

// Next returns the entries written since the last call, oldest first, and none when nothing was.
//
// No file at the path is not an error: it is the moment between a rotation's rename and the daemon's next
// line, or a daemon that has not started. Anything else that stops the log being read is.
func (f *Follower) Next() ([]Entry, error) {
	file, info, err := open(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	var entries []Entry
	emit := func(line []byte, truncated bool) {
		entry := parse(line, truncated)
		if f.keep == nil || f.keep(entry) {
			entries = append(entries, entry)
		}
	}

	switch {
	case f.id != nil && !os.SameFile(f.id, info):
		// Rotated: the file that was being read has another name now, and may have been written to
		// between the last read and its rename.
		f.finish(emit)
		f.offset = 0
	case info.Size() < f.offset:
		// The same file, shorter: emptied in place. What is in it now is new.
		f.split.reset()
		f.offset = 0
	}
	f.id = info

	end := info.Size()
	if end-f.offset > maxBatch {
		end = f.offset + maxBatch
	}
	read, err := feed(file, f.offset, end, &f.split, emit)
	f.offset += read
	if err != nil {
		return entries, fmt.Errorf("reading %s: %w", f.path, err)
	}
	return entries, nil
}

// finish reads the rest of the file that was being followed, which a rotation has renamed, and ends its
// last line whether or not a newline did.
func (f *Follower) finish(emit func(line []byte, truncated bool)) {
	for _, name := range backups(f.path) {
		file, info, err := open(name)
		if err != nil {
			continue
		}
		if !os.SameFile(f.id, info) {
			_ = file.Close()
			continue
		}
		end := info.Size()
		if end-f.offset > maxScan {
			end = f.offset + maxScan
		}
		_, _ = feed(file, f.offset, end, &f.split, emit)
		_ = file.Close()
		break
	}
	f.split.flush(emit)
}
