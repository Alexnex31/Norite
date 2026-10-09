// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package logscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
)

// Nothing here may read the log of whoever runs the suite, so before any test the default path is one
// that fails the test reaching it, and times are drawn in UTC whatever the machine's zone.
func TestMain(m *testing.M) {
	defaultPath = func() (string, error) {
		panic("a logscmd test reached the real log; give it a path with logAt")
	}
	zone = time.UTC
	pollEvery = 5 * time.Millisecond
	os.Exit(m.Run())
}

// logAt points the command at a log in a temporary directory, holding body when body is not nil.
func logAt(t *testing.T, body *string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.log")
	if body != nil {
		if err := os.WriteFile(path, []byte(*body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	previous := defaultPath
	defaultPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { defaultPath = previous })
	return path
}

// syncWriter is a buffer a follow can write to while the test reads it.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func root(out, errOut *syncWriter) *cli.Command {
	return &cli.Command{
		Name:           "norite",
		Writer:         out,
		ErrWriter:      errOut,
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
		Commands:       []*cli.Command{Command()},
	}
}

func run(t *testing.T, argv ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut syncWriter
	err = root(&out, &errOut).Run(t.Context(), append([]string{"norite"}, argv...))
	return out.String(), errOut.String(), err
}

const sample = `{"level":"info","component":"daemon","pid":4242,"version":"0.1.0","time":"2026-10-09T16:50:08Z","message":"daemon starting"}
{"level":"warn","component":"daemon","subsystem":"session","attempt":3,"instance":"https://chat.example.com","time":"2026-10-09T16:50:09Z","message":"the instance did not answer"}
{"level":"debug","component":"daemon","subsystem":"attach","time":"2026-10-09T16:50:10Z","message":"closing an attach client","reason":"the daemon is stopping"}
goroutine 7 [running]:
`

func TestTailPrintsTheNewestLinesAsText(t *testing.T) {
	body := sample
	logAt(t, &body)

	out, _, err := run(t, "logs", "tail")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		`2026-10-09 16:50:08 INFO  daemon     daemon starting  pid=4242  version=0.1.0`,
		`2026-10-09 16:50:09 WARN  session    the instance did not answer  attempt=3  instance=https://chat.example.com`,
		`2026-10-09 16:50:10 DEBUG attach     closing an attach client  reason="the daemon is stopping"`,
		`goroutine 7 [running]:`,
		``,
	}, "\n")
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}

	out, _, err = run(t, "logs", "tail", "-n", "2")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out, "\n"); got != 2 || !strings.HasPrefix(out, "2026-10-09 16:50:10 DEBUG") {
		t.Errorf("-n 2 printed:\n%s", out)
	}

	out, _, err = run(t, "logs", "tail", "--lines", "0")
	if err != nil || out != "" {
		t.Errorf("--lines 0 printed %q, %v", out, err)
	}
}

func TestTailPrintsOneSchemaCheckedObjectPerLineAsJSON(t *testing.T) {
	body := sample + `{"level":7,"message":"\u001b[31mred\u001b[0m","time":"not a time"}` + "\n" +
		strings.Repeat("z", 40000) + "\n"
	logAt(t, &body)

	out, _, err := run(t, "--json", "logs", "tail")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("%d lines of output, want 6:\n%s", len(lines), out)
	}
	var views []entryView
	for _, line := range lines {
		daemontest.MatchesCLISchema(t, line, "logs.schema.json", "entry")
		var v entryView
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		views = append(views, v)
	}
	first := views[0]
	if first.Time != "2026-10-09T16:50:08Z" || first.Level != "info" || first.Message != "daemon starting" ||
		string(first.Fields["pid"]) != "4242" || first.Unparsed {
		t.Errorf("first entry: %+v", first)
	}
	if !views[3].Unparsed || views[3].Message != "goroutine 7 [running]:" || views[3].Fields == nil {
		t.Errorf("the line that is not JSON: %+v", views[3])
	}
	// Lossless to a parser: the escape is still in the value, and it reached the terminal as six
	// characters rather than as an escape.
	odd := views[4]
	if odd.Message != "\x1b[31mred\x1b[0m" || odd.Level != "" || odd.Time != "" ||
		string(odd.Fields["level"]) != "7" || string(odd.Fields["time"]) != `"not a time"` {
		t.Errorf("the unusual line: %+v", odd)
	}
	if !views[5].Truncated || !views[5].Unparsed || len(views[5].Message) != 16384 {
		t.Errorf("the long line: truncated=%v, %d bytes", views[5].Truncated, len(views[5].Message))
	}
	assertInert(t, out)
}

// assertInert fails when out holds anything a terminal acts on or reorders by: a control character other
// than the newline that ends a line, or a bidirectional override.
func assertInert(t *testing.T, out string) {
	t.Helper()
	for i, r := range out {
		switch {
		case r == '\n':
		case unicode.IsControl(r), r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			t.Fatalf("output holds U+%04X at byte %d: %q", r, i, out)
		}
	}
}

// The done-when's clause. The log is a file anybody can write to, so a line can hold whatever its writer
// liked: escape sequences that recolor or retitle a terminal, a carriage return that overwrites the line
// before it, an override that reverses what follows. None of it reaches the terminal, in a message, a
// level, a subsystem, a field's name or a field's value, as text or as JSON.
func TestNothingInTheLogActsOnTheTerminal(t *testing.T) {
	hostile := "\x1b]0;owned\x07\x1b[2J\rSTOPPED \u202egnp.exe\u2066 \u009b31m"
	encoded, err := json.Marshal(hostile)
	if err != nil {
		t.Fatal(err)
	}
	quoted := string(encoded)
	body := strings.Join([]string{
		`{"level":"info","message":` + quoted + `}`,
		`{"level":` + quoted + `,"subsystem":` + quoted + `,"message":"m"}`,
		`{"level":"info","message":"m",` + quoted + `:` + quoted + `}`,
		`{"level":"info","message":"m","nested":{"k":[` + quoted + `]}}`,
		// A value with no space in it is drawn without quotes, so nothing but its own cleaning covers it.
		`{"level":"info","message":"m","k":"\u001b[31mred\u202e"}`,
		// Not JSON at all, so the bytes are raw in the file.
		"raw " + hostile,
		"raw \xff\x1b[31m and invalid UTF-8",
		`{"level":"info","message":"an over-long line ` + strings.Repeat("\x1b[31m", 5000) + `"}`,
		"",
	}, "\n")
	logAt(t, &body)

	for _, argv := range [][]string{{"logs", "tail"}, {"--json", "logs", "tail"}} {
		out, _, err := run(t, argv...)
		if err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
		if got := strings.Count(out, "\n"); got != 8 {
			t.Errorf("%v: %d lines of output for 8 lines of log; a line was forged or lost:\n%q", argv, got, out)
		}
		assertInert(t, out)
		for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
			// The longest thing a line can carry is one line of the file, cut, with its escapes spelled out.
			if len(line) > 8*16384 {
				t.Errorf("%v: a line of output is %d bytes", argv, len(line))
			}
		}
	}

	// And as text the over-long line says it was cut, rather than ending mid-word without a reason.
	out, _, _ := run(t, "logs", "tail", "-n", "1")
	if !strings.Contains(out, "[cut at 16384 bytes]") {
		t.Errorf("a cut line does not say so: %q", out[max(0, len(out)-80):])
	}
}

func TestTailKeepsTheLevelAskedForAndAbove(t *testing.T) {
	body := sample
	logAt(t, &body)
	out, _, err := run(t, "logs", "tail", "--level", "WARN")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "INFO") || strings.Contains(out, "DEBUG") || !strings.Contains(out, "WARN") ||
		!strings.Contains(out, "goroutine 7") {
		t.Errorf("--level WARN printed:\n%s", out)
	}
}

func TestTailReadsAFileNamedOnTheCommandLine(t *testing.T) {
	// The default is somewhere with no log at all, so only --file can have supplied these lines.
	logAt(t, nil)
	elsewhere := filepath.Join(t.TempDir(), "custom.log")
	if err := os.WriteFile(elsewhere, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, "logs", "tail", "--file", elsewhere, "-n", "1")
	if err != nil || !strings.Contains(out, "goroutine 7") {
		t.Errorf("--file: %q, %v", out, err)
	}
}

// With no daemon ever started there is no log, and saying nothing with exit 0 would read as an empty one.
func TestTailWithNoLogSaysSoAndLeavesNothingBehind(t *testing.T) {
	path := logAt(t, nil)
	out, _, err := run(t, "logs", "tail")
	if err == nil || out != "" {
		t.Fatalf("no log: output %q, error %v", out, err)
	}
	if !strings.Contains(err.Error(), "there is no log at") || !strings.Contains(err.Error(), "norite daemon status") {
		t.Errorf("the error does not say what to look at: %v", err)
	}
	var usage *clierr.UsageError
	if errors.As(err, &usage) {
		t.Errorf("a missing log is not a mistake on the command line: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 0 {
		t.Errorf("reading a log that is not there created %d entries", len(entries))
	}
}

func TestTailRefusesWhatItCannotMean(t *testing.T) {
	body := sample
	logAt(t, &body)
	cases := [][]string{
		{"logs", "tail", "extra"},
		{"logs", "tail", "-n", "-1"},
		{"logs", "tail", "-n", "10001"},
		{"logs", "tail", "--level", "loud"},
		{"logs", "tail", "--level", ""},
		{"logs", "tail", "--file", ""},
	}
	for _, argv := range cases {
		out, _, err := run(t, argv...)
		var usage *clierr.UsageError
		if !errors.As(err, &usage) {
			t.Errorf("%v: %v, want a usage error", argv, err)
		}
		if out != "" {
			t.Errorf("%v printed %q before refusing", argv, out)
		}
	}

	// A level somebody typed is printed back, and is theirs to have put anything in.
	_, _, err := run(t, "logs", "tail", "--level", "x\x1b[31m")
	if err == nil || strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("the refusal carries the escape it was given: %q", err)
	}
}

func TestTailRefusesADirectoryWhereTheLogGoes(t *testing.T) {
	path := logAt(t, nil)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := run(t, "logs", "tail")
	if err == nil || !strings.Contains(err.Error(), "cannot read the daemon's log") {
		t.Fatalf("a directory: %v", err)
	}
}

// follow starts `logs tail --follow` and returns what it has printed so far, and how to stop it.
func follow(t *testing.T, argv ...string) (out, errOut *syncWriter, stop func() error) {
	t.Helper()
	out, errOut = &syncWriter{}, &syncWriter{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- root(out, errOut).Run(ctx, append([]string{"norite"}, argv...)) }()
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				result = errors.New("the follow did not stop when interrupted")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return out, errOut, stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func appendLog(t *testing.T, path, text string) {
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

func entry(message string) string {
	return fmt.Sprintf(`{"level":"info","time":"2026-10-09T16:50:08Z","message":%q}`, message) + "\n"
}

func TestFollowPrintsNewLinesUntilInterrupted(t *testing.T) {
	body := entry("one") + entry("two")
	path := logAt(t, &body)

	out, _, stop := follow(t, "logs", "tail", "-n", "1", "--follow")
	waitFor(t, "the last line", func() bool { return strings.Contains(out.String(), "two") })
	if strings.Contains(out.String(), "one") {
		t.Fatalf("-n 1 printed more than one line of what was there: %q", out.String())
	}

	appendLog(t, path, entry("three"))
	waitFor(t, "a line appended later", func() bool { return strings.Contains(out.String(), "three") })

	// A rotation, as lumberjack does one: the log renamed aside, and a new file at its path.
	appendLog(t, path, entry("four, written just before the rename"))
	if err := os.Rename(path, filepath.Join(filepath.Dir(path), "daemon-2026-10-09T16-50-09.000.log")); err != nil {
		t.Fatal(err)
	}
	appendLog(t, path, entry("five, in the new file"))
	waitFor(t, "the lines either side of a rotation", func() bool {
		return strings.Contains(out.String(), "four, written") && strings.Contains(out.String(), "five, in the new")
	})

	// Interrupted is how a follow ends, and it is not a failure.
	if err := stop(); err != nil {
		t.Errorf("an interrupted follow returned %v", err)
	}
	if got := strings.Count(out.String(), "\n"); got != 4 {
		t.Errorf("%d lines printed, want 4 with none repeated:\n%s", got, out.String())
	}
}

// Followed, a log that does not exist yet is waited for: start this, then start the daemon.
func TestFollowWaitsForALogThatIsNotThereYet(t *testing.T) {
	path := logAt(t, nil)
	out, errOut, stop := follow(t, "logs", "tail", "-f")
	waitFor(t, "the note that it is waiting", func() bool { return strings.Contains(errOut.String(), "waiting for the daemon") })
	if out.String() != "" {
		t.Fatalf("the note went to standard output, where a script reads log lines: %q", out.String())
	}

	appendLog(t, path, entry("the daemon's first line"))
	waitFor(t, "the first line of a new log", func() bool { return strings.Contains(out.String(), "first line") })
	if err := stop(); err != nil {
		t.Errorf("stop: %v", err)
	}
}

// brokenPipe refuses every write after the first, as a reader that has gone does.
type brokenPipe struct {
	mu     sync.Mutex
	writes int
}

func (b *brokenPipe) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writes++
	if b.writes > 1 {
		return 0, errors.New("broken pipe")
	}
	return len(p), nil
}

// `norite logs tail -f | head -1`: when whoever was reading stops, so does the follow, without an error
// about a pipe that is nobody's problem. Both places a line is written from: the lines already there, and
// the ones that arrive while following.
func TestAReaderThatStopsEndsTheCommandQuietly(t *testing.T) {
	t.Run("among the lines already there", func(t *testing.T) {
		body := entry("one") + entry("two") + entry("three")
		logAt(t, &body)
		endsQuietly(t, func() {})
	})
	t.Run("while following", func(t *testing.T) {
		body := entry("one")
		path := logAt(t, &body)
		endsQuietly(t, func() { appendLog(t, path, entry("two")+entry("three")) })
	})
}

// endsQuietly runs a follow into a reader that takes one line and goes, calling later once that line has
// been taken.
func endsQuietly(t *testing.T, later func()) {
	t.Helper()
	pipe := &brokenPipe{}
	cmd := &cli.Command{
		Name: "norite", Writer: pipe, ErrWriter: &syncWriter{},
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
		Commands:       []*cli.Command{Command()},
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Run(t.Context(), []string{"norite", "logs", "tail", "--follow"}) }()
	waitFor(t, "the first line to be taken", func() bool {
		pipe.mu.Lock()
		defer pipe.mu.Unlock()
		return pipe.writes >= 1
	})
	later()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a reader that stopped produced %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follow went on after its reader had gone")
	}
}
