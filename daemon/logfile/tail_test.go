// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// line is one line as the daemon writes them.
func line(level, message string) string {
	return fmt.Sprintf(`{"level":%q,"component":"daemon","time":"2026-10-09T18:50:08+02:00","message":%q}`,
		level, message) + "\n"
}

func writeLog(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func messages(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Message
	}
	return out
}

func numbered(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		b.WriteString(line("info", "line "+strconv.Itoa(i)))
	}
	return b.String()
}

func wantMessages(t *testing.T, got []Entry, want ...string) {
	t.Helper()
	if strings.Join(messages(got), "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", messages(got), want)
	}
}

func TestTailReturnsTheNewestLinesOldestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	writeLog(t, path, numbered(1, 10))

	cases := []struct {
		n    int
		want []string
	}{
		{3, []string{"line 8", "line 9", "line 10"}},
		{1, []string{"line 10"}},
		{10, strings.Split("line 1|line 2|line 3|line 4|line 5|line 6|line 7|line 8|line 9|line 10", "|")},
		// More than there are is all there are.
		{50, strings.Split("line 1|line 2|line 3|line 4|line 5|line 6|line 7|line 8|line 9|line 10", "|")},
		{0, nil},
		{-4, nil},
	}
	for _, tc := range cases {
		got, _, err := Tail(path, Options{Lines: tc.n})
		if err != nil {
			t.Fatalf("n=%d: %v", tc.n, err)
		}
		wantMessages(t, got, tc.want...)
	}
}

func TestTailReadsTheFieldsOfALine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	writeLog(t, path, `{"level":"warn","component":"daemon","subsystem":"session","attempt":3,`+
		`"time":"2026-10-09T18:50:08.25+02:00","message":"renewal failed"}`+"\n")

	got, _, err := Tail(path, Options{Lines: 1})
	if err != nil || len(got) != 1 {
		t.Fatalf("Tail: %v, %d entries", err, len(got))
	}
	e := got[0]
	if e.Level != "warn" || e.Subsystem != "session" || e.Message != "renewal failed" || e.Unparsed || e.Truncated {
		t.Errorf("entry %+v", e)
	}
	if want := time.Date(2026, 10, 9, 16, 50, 8, 250e6, time.UTC); !e.Time.Equal(want) {
		t.Errorf("time %v, want %v", e.Time, want)
	}
	if string(e.Fields["attempt"]) != "3" || string(e.Fields["component"]) != `"daemon"` || len(e.Fields) != 2 {
		t.Errorf("fields %v, want attempt and component and nothing taken twice", e.Fields)
	}
}

// The file is not trusted to be the daemon's: every line that is not what the daemon writes comes back as
// itself, and nothing a line holds is dropped for being unusual.
func TestALineThatIsNotTheDaemonsIsReturnedAsItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	writeLog(t, path, strings.Join([]string{
		"goroutine 1 [running]:",
		`["an","array"]`,
		`"a string"`,
		`null`,
		`{"level":3,"message":["not","a","string"],"time":"yesterday","subsystem":null}`,
		"bad \xff\xfe bytes",
		`{"level":"info","message":"ordinary"}`,
		"",
	}, "\n"))

	got, _, err := Tail(path, Options{Lines: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("%d entries, want 7: %q", len(got), messages(got))
	}
	for i, want := range []string{"goroutine 1 [running]:", `["an","array"]`, `"a string"`, "null"} {
		if !got[i].Unparsed || got[i].Message != want {
			t.Errorf("entry %d: %+v, want unparsed %q", i, got[i], want)
		}
	}
	odd := got[4]
	if odd.Unparsed || odd.Level != "" || odd.Message != "" || !odd.Time.IsZero() || len(odd.Fields) != 4 {
		t.Errorf("a line with fields of the wrong types lost some: %+v", odd)
	}
	if !got[5].Unparsed || got[5].Message != "bad \uFFFD bytes" {
		t.Errorf("invalid UTF-8 came back as %q", got[5].Message)
	}
	if got[6].Unparsed || got[6].Message != "ordinary" {
		t.Errorf("the ordinary line after them: %+v", got[6])
	}
}

// A line past MaxLine is cut and marked, and the lines either side of it are untouched.
func TestALineTooLongIsCutAndTheNextIsWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	long := `{"level":"info","message":"` + strings.Repeat("x", 3*MaxLine) + `"}`
	writeLog(t, path, line("info", "before")+long+"\n"+line("info", "after"))

	got, _, err := Tail(path, Options{Lines: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("%d entries, want 3", len(got))
	}
	if got[0].Message != "before" || got[2].Message != "after" || got[2].Truncated || got[2].Unparsed {
		t.Errorf("the lines around the long one: %+v, %+v", got[0], got[2])
	}
	cut := got[1]
	if !cut.Truncated || !cut.Unparsed || len(cut.Message) != MaxLine || !strings.HasPrefix(cut.Message, `{"level":"info"`) {
		t.Errorf("the long line: truncated=%v unparsed=%v, %d bytes", cut.Truncated, cut.Unparsed, len(cut.Message))
	}
}

// Exactly MaxLine is a whole line.
func TestALineOfExactlyTheBoundIsNotCut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	writeLog(t, path, strings.Repeat("y", MaxLine)+"\n"+strings.Repeat("z", MaxLine+1)+"\n")
	got, _, err := Tail(path, Options{Lines: 2})
	if err != nil || len(got) != 2 {
		t.Fatalf("Tail: %v, %d entries", err, len(got))
	}
	if got[0].Truncated || len(got[0].Message) != MaxLine {
		t.Errorf("a line of MaxLine was cut: truncated=%v, %d bytes", got[0].Truncated, len(got[0].Message))
	}
	if !got[1].Truncated || len(got[1].Message) != MaxLine {
		t.Errorf("a line one past MaxLine was not cut: truncated=%v, %d bytes", got[1].Truncated, len(got[1].Message))
	}
}

func TestTailReachesIntoTheNewestRotatedCopyAndNoFurther(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	writeLog(t, path, numbered(21, 22))
	writeLog(t, filepath.Join(dir, "daemon-2026-10-09T10-00-00.000.log"), numbered(11, 20))
	writeLog(t, filepath.Join(dir, "daemon-2026-10-08T10-00-00.000.log"), numbered(1, 10))
	// Beside the log, and not a copy of it: none of these is read.
	writeLog(t, filepath.Join(dir, "daemon-notes.log"), line("info", "somebody's notes"))
	writeLog(t, filepath.Join(dir, "daemon-2027-01-01T00-00-00.000.txt"), line("info", "another extension"))
	writeLog(t, filepath.Join(dir, "daemon.crash.log"), "panic: not a log line\n")
	writeLog(t, filepath.Join(dir, "other-2027-01-01T00-00-00.000.log"), line("info", "another program's"))

	got, _, err := Tail(path, Options{Lines: 5})
	if err != nil {
		t.Fatal(err)
	}
	wantMessages(t, got, "line 18", "line 19", "line 20", "line 21", "line 22")

	// One copy back and no further: asked for more than the two hold, it returns what the two hold.
	got, _, err = Tail(path, Options{Lines: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 12 || got[0].Message != "line 11" {
		t.Errorf("%d entries starting at %q, want 12 starting at line 11", len(got), got[0].Message)
	}
}

func TestTailKeepsOnlyTheLevelsAskedFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	writeLog(t, path, line("debug", "d1")+line("info", "i1")+line("warn", "w1")+"a traceback line\n"+
		line("error", "e1")+line("info", "i2")+line("loud", "an unknown level")+line("debug", "d2"))

	keep, ok := AtLeast("warn")
	if !ok {
		t.Fatal("warn is a level")
	}
	got, _, err := Tail(path, Options{Lines: 10, Keep: keep})
	if err != nil {
		t.Fatal(err)
	}
	// A line with no level this build knows is kept: it is more likely wanted than not.
	wantMessages(t, got, "w1", "a traceback line", "e1", "an unknown level")

	// The count is of entries kept, not of lines read.
	got, _, err = Tail(path, Options{Lines: 2, Keep: keep})
	if err != nil {
		t.Fatal(err)
	}
	wantMessages(t, got, "e1", "an unknown level")

	if _, ok := AtLeast("verbose"); ok {
		t.Error("a level nobody writes was accepted as a floor")
	}
	for _, name := range Levels() {
		if _, ok := AtLeast(name); !ok {
			t.Errorf("Levels names %q, which AtLeast refuses", name)
		}
	}
}

func TestNoLogIsAnErrorThatSaysSo(t *testing.T) {
	_, _, err := Tail(filepath.Join(t.TempDir(), "daemon.log"), Options{Lines: 5})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Tail on no file: %v, want fs.ErrNotExist", err)
	}
}

func TestADirectoryWhereTheLogGoesIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Tail(path, Options{Lines: 5}); err == nil {
		t.Fatal("a directory was read as a log")
	} else if runtime.GOOS != "windows" && !errors.Is(err, ErrNotAFile) {
		t.Errorf("error %v, want ErrNotAFile", err)
	}
	if _, err := Follow(path, nil).Next(); err == nil {
		t.Error("a directory was followed as a log")
	}
}

// A last line with no newline is one the daemon may still be writing. Read once, it is returned as it
// stands. Followed, it is left until it is finished and then returned whole, not in two pieces.
func TestAnUnfinishedLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	half := `{"level":"info","message":"hal`
	writeLog(t, path, line("info", "whole")+half)

	got, _, err := Tail(path, Options{Lines: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[1].Unparsed || got[1].Message != half {
		t.Fatalf("read once: %q", messages(got))
	}

	for _, n := range []int{5, 1, 0} {
		writeLog(t, path, line("info", "whole")+half)
		got, follower, err := Tail(path, Options{Lines: n, Follow: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range got {
			if e.Unparsed {
				t.Fatalf("n=%d: followed, the unfinished line was returned early: %q", n, messages(got))
			}
		}
		appendTo(t, path, `f"}`+"\n"+line("info", "next"))
		rest, err := follower.Next()
		if err != nil {
			t.Fatal(err)
		}
		wantMessages(t, rest, "half", "next")
	}
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFollowingReturnsWhatIsAppendedAndNothingTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	writeLog(t, path, numbered(1, 3))
	got, follower, err := Tail(path, Options{Lines: 2, Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	wantMessages(t, got, "line 2", "line 3")

	if rest, err := follower.Next(); err != nil || len(rest) != 0 {
		t.Fatalf("nothing was appended, and Next returned %q, %v", messages(rest), err)
	}
	appendTo(t, path, numbered(4, 6))
	rest, err := follower.Next()
	if err != nil {
		t.Fatal(err)
	}
	wantMessages(t, rest, "line 4", "line 5", "line 6")
	if rest, err := follower.Next(); err != nil || len(rest) != 0 {
		t.Fatalf("read twice: %q, %v", messages(rest), err)
	}
}

func TestFollowingKeepsOnlyTheLevelsAskedFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	writeLog(t, path, "")
	keep, _ := AtLeast("error")
	_, follower, err := Tail(path, Options{Keep: keep, Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, line("info", "quiet")+line("error", "loud")+line("debug", "quieter"))
	rest, err := follower.Next()
	if err != nil {
		t.Fatal(err)
	}
	wantMessages(t, rest, "loud")
}

// A log that does not exist yet is waited for, and read from its first line when it appears.
func TestFollowingALogThatDoesNotExistYet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	follower := Follow(path, nil)
	if rest, err := follower.Next(); err != nil || len(rest) != 0 {
		t.Fatalf("no file: %q, %v", messages(rest), err)
	}
	writeLog(t, path, numbered(1, 2))
	rest, err := follower.Next()
	if err != nil {
		t.Fatal(err)
	}
	wantMessages(t, rest, "line 1", "line 2")
}

func TestFollowingAFileEmptiedInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	writeLog(t, path, numbered(1, 5))
	_, follower, err := Tail(path, Options{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, line("info", "after the cut"))
	rest, err := follower.Next()
	if err != nil {
		t.Fatal(err)
	}
	wantMessages(t, rest, "after the cut")
}

// The done-when's clause: a tail held across a rotation keeps printing. The writer is lumberjack itself,
// rotating at one megabyte, and every numbered line it is given must come out of the follower once, in
// order, with the reads landing wherever they land among the rotations.
func TestFollowingAcrossRealRotationsLosesAndRepeatsNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	writer := &lumberjack.Logger{Filename: path, MaxSize: 1, MaxBackups: 3}
	defer func() { _ = writer.Close() }()

	pad := strings.Repeat("p", 900)
	write := func(i int) {
		if _, err := fmt.Fprintf(writer, `{"level":"info","n":%d,"pad":%q,"message":"line %d"}`+"\n", i, pad, i); err != nil {
			t.Fatalf("writing line %d: %v", i, err)
		}
	}
	for i := 1; i <= 3; i++ {
		write(i)
	}
	got, follower, err := Tail(path, Options{Lines: 1, Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	wantMessages(t, got, "line 3")

	next := 4
	check := func() {
		rest, err := follower.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		for _, e := range rest {
			if e.Unparsed || e.Message != "line "+strconv.Itoa(next) {
				t.Fatalf("after line %d came %q (unparsed=%v)", next-1, e.Message, e.Unparsed)
			}
			next++
		}
	}
	// About 3.4 MB: three rotations. Read at uneven intervals, so some reads straddle a rotation and
	// some fall after one with lines still unread in the copy.
	const last = 3500
	for i := 4; i <= last; i++ {
		write(i)
		if i%137 == 0 || i%1000 == 999 {
			check()
		}
	}
	check()
	check()
	if next != last+1 {
		t.Fatalf("the follower returned up to line %d of %d", next-1, last)
	}
	if copies := backups(path); len(copies) < 3 {
		t.Fatalf("only %d rotations happened; the test did not cross what it means to", len(copies))
	}
}

// More than one rotation between two reads: only the newest copy and the live file can be searched for
// the file that was being read, and what was between them is gone. Nothing is repeated, and the follower
// goes on from the live file.
func TestFollowingGoesOnWhenTheFileItWasReadingIsGone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	writeLog(t, path, numbered(1, 3))
	_, follower, err := Tail(path, Options{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeLog(t, path, numbered(10, 11))
	rest, err := follower.Next()
	if err != nil {
		t.Fatal(err)
	}
	wantMessages(t, rest, "line 10", "line 11")
}

// A file far larger than the daemon would write is read back from its end by a bounded distance, and the
// line that distance lands inside is not returned as if it began there.
func TestAHugeFileIsReadFromABoundedDistance(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a sparse file past the read bound")
	}
	path := filepath.Join(t.TempDir(), "daemon.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// One line of zeros, longer than the bound, and sparse so it costs no disk.
	if err := file.Truncate(maxScan + 4<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n" + numbered(1, 3)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	for _, keep := range []func(Entry) bool{nil, func(Entry) bool { return true }} {
		got, _, err := Tail(path, Options{Lines: 10, Keep: keep})
		if err != nil {
			t.Fatal(err)
		}
		wantMessages(t, got, "line 1", "line 2", "line 3")
	}
}

// A follower hands a log that grew a great deal over in pieces, and loses none of it.
func TestFollowingALargeAppendComesInBoundedPieces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	writeLog(t, path, "")
	_, follower, err := Tail(path, Options{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	const count = 12000
	pad := strings.Repeat("q", 1000)
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&body, `{"message":"line %d","pad":%q}`+"\n", i, pad)
	}
	appendTo(t, path, body.String())

	seen, calls := 0, 0
	for {
		rest, err := follower.Next()
		if err != nil {
			t.Fatal(err)
		}
		if len(rest) == 0 {
			break
		}
		calls++
		for _, e := range rest {
			seen++
			if e.Message != "line "+strconv.Itoa(seen) {
				t.Fatalf("entry %d is %q", seen, e.Message)
			}
		}
	}
	if seen != count {
		t.Fatalf("%d of %d lines came through", seen, count)
	}
	if calls < 3 {
		t.Errorf("%d MB came back in %d calls; one call is not bounded", body.Len()>>20, calls)
	}
}

// The rotated copy fills the count only when the live file was read from its first byte and still came up
// short. Short because the last line was left to the Follower, the lines missing are in the live file, and
// one old line from the copy would sit in front of a gap nothing marks.
func TestTheRotatedCopyIsNotReadWhenTheLiveFileHoldsEnough(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	writeLog(t, filepath.Join(dir, "daemon-2026-10-09T10-00-00.000.log"), line("info", "from the copy before"))
	writeLog(t, path, numbered(1, 100)+`{"level":"info","message":"unfin`)

	got, _, err := Tail(path, Options{Lines: 5, Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	// Four, not five: the fifth is the line still being written. And none of them from the copy.
	wantMessages(t, got, "line 97", "line 98", "line 99", "line 100")

	// With the whole live file read and still short, the copy is where the rest are.
	writeLog(t, path, numbered(1, 2)+`{"level":"info","message":"unfin`)
	got, _, err = Tail(path, Options{Lines: 5, Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	wantMessages(t, got, "from the copy before", "line 1", "line 2")
}
