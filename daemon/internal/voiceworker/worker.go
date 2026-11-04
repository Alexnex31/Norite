// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package voiceworker starts, supervises and ends norite-voice, the voice-worker (M28, architecture.md §6):
// one process per call, the daemon's direct child, speaking daemon/voiceipc on two pipes it inherits.
//
// The worker is the peer the daemon trusts least (voiceipc's package comment, and ADR 0017 as amended): from
// M29 it decodes a stranger's audio in C. So nothing it sends is believed past its bounds. Every frame is
// limited in size and rate, decoded strictly and checked, and the first that is not right kills it. Its
// stdout and stderr are read into the daemon's log, each line cut short, cleaned (termsafe) and quoted as one
// field so it cannot pose as the daemon's own. Their volume is bounded too.
//
// On macOS the worker must stay the daemon's direct child: TCC charges its microphone use to the daemon, its
// parent, and the grant is the daemon's (ADR 0034). So it is started by its path through os/exec, never
// through a shell, launchctl or open. On Windows it inherits the daemon's console, which conhost --headless
// keeps windowless (M24); it is never given a console of its own, which would show one.
package voiceworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// Program is the worker's file name, beside the daemon's own executable.
const Program = "norite-voice"

// ProgramBeside is the worker's path for a daemon whose resolved executable is daemon.
//
// A daemon that does not know its own path gets "", which Start refuses: a bare name would be looked up on
// PATH and run whatever program of that name came first.
func ProgramBeside(daemon string) string {
	if daemon == "" {
		return ""
	}
	name := Program
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(filepath.Dir(daemon), name)
}

// Bounds on a worker. Variables so the tests can reach them in milliseconds.
var (
	// helloWithin is how long a worker has to say HELLO.
	helloWithin = 5 * time.Second
	// leaveWithin is how long a worker asked to leave has to exit before it is killed, and exitWithin how
	// long one whose pipe closed by itself has.
	leaveWithin = 3 * time.Second
	exitWithin  = 3 * time.Second
	// sendWithin is how long one frame to the worker may take to write. A worker that stops reading its
	// pipe would otherwise hold the write, and every later Send and Leave behind it, for ever.
	sendWithin = 3 * time.Second
	// drainWithin is how long, once the worker has exited, its pipes are read for what it wrote last: its
	// ended state and a panic's trace, which must count before how it ended is decided.
	drainWithin = time.Second
	// readHook runs before each frame after HELLO is handled. Tests only: it stages a reader slower than
	// the reaper, which is the order that once took a clean end for a crash.
	readHook func()
)

const (
	// framesPerSecond bounds what a worker sends. A call's whole life is a handful of frames; M34's
	// speaking updates come a few a second.
	framesPerSecond = 50
	// outputPerSecond bounds what it prints, in bytes. A worker logging a megabyte a second is broken or
	// hostile either way, and reading it costs the daemon what it costs the worker to write it.
	outputPerSecond = 256 << 10
	// maxLine is how much of one printed line is kept, and lastLines how many are kept to explain a crash.
	maxLine   = 512
	lastLines = 20
	// linesPerSecond bounds the log entries a worker's printing makes; outputPerSecond bounds its bytes.
	linesPerSecond = 50
)

// Options configures a worker.
type Options struct {
	// Program is the worker's path: ProgramBeside(the daemon's executable), or a test's.
	Program string
	// Version is the daemon's, which the worker's HELLO is checked against.
	Version string
	Log     zerolog.Logger

	// Args go before the worker's own -pipes, and Env after the allow-list. Tests only: a stand-in worker
	// is this package's test binary, told what to play. Nothing in the daemon sets either.
	Args []string
	Env  []string
}

// Event is a frame from the worker, decoded and checked: an Offer or a State.
type Event struct {
	Offer *voiceipc.Offer
	State *voiceipc.State
}

// Worker is one running worker.
type Worker struct {
	opts Options
	log  zerolog.Logger
	cmd  *exec.Cmd
	// toWorker is the daemon's end of the control pipe, fromWorker of the events pipe, and output of the
	// worker's stdout and stderr. All three are closed once the worker is reaped.
	toWorker   *os.File
	fromWorker *os.File
	output     *os.File
	writeMu    sync.Mutex

	events chan Event
	exited chan struct{}
	done   chan struct{}
	// readDone and outDone are closed when the frame reader and the output reader have returned.
	readDone, outDone chan struct{}

	// The package's bounds as they were at Start.
	leaveWithin, exitWithin, sendWithin, drainWithin time.Duration
	readHook                                         func()

	mu      sync.Mutex
	leaving bool
	ended   *voiceipc.State
	// broke is why the daemon killed the worker, when it did.
	broke string
	exit  Exit
	lines *ring
}

// Exit is how a worker ended.
type Exit struct {
	// Crashed is a worker that ended without saying so and without being asked: its pipe closed with no
	// ended state, it exited non-zero, or the daemon killed it for breaking the protocol.
	Crashed bool
	// Reason says why, for a person: the worker's own, or the daemon's account of a crash.
	Reason string
}

// ErrStart is a worker that could not be started or did not say who it is.
var ErrStart = errors.New("the voice worker could not be started")

// Start starts a worker and waits for its HELLO, whose version must be compatible with the daemon's.
func Start(ctx context.Context, opts Options) (*Worker, error) {
	// Only a path: a bare name would be looked up on PATH, and run whichever program of that name came
	// first rather than the one shipped beside this daemon.
	if !filepath.IsAbs(opts.Program) {
		return nil, fmt.Errorf("%w: the daemon does not know where it is installed, so where its voice "+
			"worker is", ErrStart)
	}
	ctlR, ctlW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStart, err)
	}
	evR, evW, err := os.Pipe()
	if err != nil {
		_ = ctlR.Close()
		_ = ctlW.Close()
		return nil, fmt.Errorf("%w: %w", ErrStart, err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		closeAll(ctlR, ctlW, evR, evW)
		return nil, fmt.Errorf("%w: %w", ErrStart, err)
	}

	cmd := exec.Command(opts.Program) //nolint:gosec // the worker beside this daemon, or a test's stand-in
	cmd.Env = append(environment(os.Environ()), opts.Env...)
	cmd.Dir = filepath.Dir(opts.Program)
	// Stdin is the null device. Stdout and stderr share one pipe: both are the worker's log, and one reader
	// keeps their order.
	cmd.Stdout, cmd.Stderr = outW, outW
	arg, err := inherit(cmd, ctlR, evW)
	if err == nil {
		cmd.Args = append(append([]string{opts.Program}, opts.Args...), "-"+voiceipc.PipesFlag, arg)
		err = cmd.Start()
	}
	// The child's ends are the child's now, or nobody's: the daemon keeps only its own, so the child
	// closing or dying is EOF here.
	closeAll(ctlR, evW, outW)
	if err != nil {
		closeAll(ctlW, evR, outR)
		return nil, fmt.Errorf("%w: %w", ErrStart, err)
	}

	w := &Worker{
		opts: opts, log: opts.Log, cmd: cmd, toWorker: ctlW, fromWorker: evR, output: outR,
		events: make(chan Event, 8), exited: make(chan struct{}), done: make(chan struct{}),
		lines: &ring{max: lastLines}, readDone: make(chan struct{}), outDone: make(chan struct{}),
		leaveWithin: leaveWithin, exitWithin: exitWithin, sendWithin: sendWithin, drainWithin: drainWithin,
		readHook: readHook,
	}
	go w.readOutput(outR)
	hello := make(chan error, 1)
	go w.read(hello)
	go w.wait()
	timer := time.NewTimer(helloWithin)
	defer timer.Stop()
	select {
	case err := <-hello:
		if err != nil {
			w.Kill(err.Error())
			<-w.done
			return nil, fmt.Errorf("%w: %w", ErrStart, err)
		}
	case <-timer.C:
		w.Kill("the voice worker did not say who it is in time")
		<-w.done
		return nil, fmt.Errorf("%w: it did not say who it is within %s", ErrStart, helloWithin)
	case <-ctx.Done():
		// Left before it said who it is, by a leave or the sign-in's end: ended as asked, not crashed.
		w.mu.Lock()
		w.leaving = true
		w.mu.Unlock()
		_ = w.cmd.Process.Kill()
		<-w.done
		return nil, ctx.Err()
	}
	return w, nil
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// Send writes one frame to the worker. A write that has not finished within sendWithin kills the worker,
// which ends the write: a worker that stops reading must not hold the daemon's side, nor the Leave behind
// it, and a pipe's buffer is a few kilobytes on Windows, under one answer.
func (w *Worker) Send(op gatewayproto.Opcode, v any) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	stuck := time.AfterFunc(w.sendWithin, func() { w.Kill("the voice worker stopped reading its pipe") })
	defer stuck.Stop()
	return voiceipc.Write(w.toWorker, op, v)
}

// PID is the worker's process id.
func (w *Worker) PID() int { return w.cmd.Process.Pid }

// Events delivers the worker's frames after HELLO, decoded and checked. It is closed when the worker's pipe
// ends.
func (w *Worker) Events() <-chan Event { return w.events }

// Done is closed once the worker has exited and been reaped; Exit then says how.
func (w *Worker) Done() <-chan struct{} { return w.done }

// Exit is how the worker ended. Read it after Done.
func (w *Worker) Exit() Exit {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exit
}

// Leave asks the worker to end its call and exit: LEAVE, then its control pipe closed. One that has not
// exited within leaveWithin is killed. It returns once the worker is reaped.
func (w *Worker) Leave() {
	w.mu.Lock()
	w.leaving = true
	w.mu.Unlock()
	// The bound is armed before anything is written, so a write held by a worker that stopped reading is
	// inside it rather than ahead of it.
	late := time.AfterFunc(w.leaveWithin, func() { w.Kill("the voice worker did not leave in time") })
	defer late.Stop()
	_ = w.Send(voiceipc.OpLeave, voiceipc.Leave{})
	// Closed outside writeMu: a close beside a write in flight ends that write, which is the point.
	_ = w.toWorker.Close()
	<-w.done
}

// Kill ends the worker now, recording why. Killing one that has exited does nothing.
//
// The reason is cut to what a STATE carries where it is recorded: it may quote what the worker sent, and
// what a worker sends is believed past no bound, the daemon's log included.
func (w *Worker) Kill(reason string) {
	w.mu.Lock()
	if w.broke == "" {
		w.broke = voiceipc.CutReason(reason)
	}
	w.mu.Unlock()
	_ = w.cmd.Process.Kill()
}

// wait reaps the worker and decides how it ended.
func (w *Worker) wait() {
	err := w.cmd.Wait()
	close(w.exited)
	_ = w.toWorker.Close()
	// What the worker wrote last is read before it is judged: its ended state, which makes an exit clean,
	// and a panic's trace, which goes with a crash. The pipes end once it has gone, unless something it
	// started still holds them, which the bound is for.
	// One bound for both: a timer's channel fires once, so the reader waited for second was not bounded at
	// all, and a pipe an heir of the worker held kept Done, and every Leave, waiting until the heir exited.
	drained := make(chan struct{})
	bound := time.AfterFunc(w.drainWithin, func() { close(drained) })
	for _, ch := range []chan struct{}{w.readDone, w.outDone} {
		select {
		case <-ch:
		case <-drained:
		}
	}
	bound.Stop()

	w.mu.Lock()
	defer func() {
		w.mu.Unlock()
		// The daemon's ends of the worker's pipes, once nothing is judged from them. A reader something the
		// worker started still holds open is ended by this, and the descriptors go now rather than when the
		// collector finds them, one call's worth at a time.
		_ = w.fromWorker.Close()
		_ = w.output.Close()
		close(w.done)
	}()
	code := -1
	if w.cmd.ProcessState != nil {
		code = w.cmd.ProcessState.ExitCode()
	}
	switch {
	case w.broke != "":
		w.exit = Exit{Crashed: true, Reason: w.broke}
	case err != nil && !w.leaving:
		w.exit = Exit{Crashed: true, Reason: fmt.Sprintf("the voice worker stopped unexpectedly (%s)", status(err, code))}
	case w.ended != nil:
		w.exit = Exit{Reason: termsafe.Text(w.ended.Reason)}
	case w.leaving:
		w.exit = Exit{Reason: "left"}
	default:
		w.exit = Exit{Crashed: true, Reason: "the voice worker stopped unexpectedly, without saying why"}
	}
	if w.exit.Crashed {
		ev := w.log.Error().Str("reason", w.exit.Reason)
		if last := w.lines.all(); len(last) > 0 {
			ev = ev.Strs("last_lines", last)
		}
		ev.Msg("the voice worker crashed")
	} else {
		w.log.Info().Str("reason", w.exit.Reason).Msg("the voice worker exited")
	}
}

func status(err error, code int) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) && code >= 0 {
		return exitStatus(runtime.GOOS, code)
	}
	return termsafe.Text(err.Error())
}

// exitStatus words an exit code. On Windows a code past 255 is an NTSTATUS or a -1, read as unsigned, and
// both are written in hexadecimal by everyone who looks them up: a killed worker was "exit status
// 4294967295" in M28's pass on Windows, where it is 0xFFFFFFFF, and an access violation is 0xC0000005.
func exitStatus(goos string, code int) string {
	if goos == "windows" && code > 0xFF {
		return fmt.Sprintf("exit status 0x%08X", uint32(code))
	}
	return fmt.Sprintf("exit status %d", code)
}

// read reads the worker's frames: HELLO first, reported on hello, then the rest onto events. A frame that
// breaks anything kills the worker. When the pipe ends without that, the worker is given exitWithin to
// exit by itself, and is killed after it: a closed pipe is a worker that is going, or one to make go.
func (w *Worker) read(hello chan<- error) {
	defer close(w.readDone)
	defer close(w.events)
	r := &reader{w: w}
	f, err := r.next()
	if err == nil {
		err = w.checkHello(f)
	}
	hello <- err
	if err != nil {
		return
	}
	// Once the worker has exited nobody may be reading events any more, and the frames still in the pipe
	// are read anyway, so an ended state written last is recorded rather than lost behind a full channel.
	unheard := false
	for {
		f, err := r.next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				w.Kill(err.Error())
				return
			}
			select {
			case <-w.exited:
			case <-time.After(w.exitWithin):
				w.Kill("the voice worker closed its pipe and did not exit")
			}
			return
		}
		if w.readHook != nil {
			w.readHook()
		}
		ev, err := w.decode(f)
		if err != nil {
			w.Kill(err.Error())
			return
		}
		if ev.State != nil && ev.State.State == voiceipc.StateEnded {
			w.mu.Lock()
			w.ended = ev.State
			w.mu.Unlock()
		}
		if unheard {
			continue
		}
		select {
		case w.events <- ev:
		case <-w.exited:
			unheard = true
		}
	}
}

// reader reads frames within the size bound and the rate.
type reader struct {
	w      *Worker
	window time.Time
	count  int
}

func (r *reader) next() (gatewayproto.Frame, error) {
	f, err := voiceipc.Read(r.w.fromWorker, voiceipc.MaxWorkerFrame)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return f, io.EOF
		}
		return f, broke(err.Error())
	}
	if now := time.Now(); now.Sub(r.window) >= time.Second {
		r.window, r.count = now, 0
	}
	r.count++
	if r.count > framesPerSecond {
		return f, broke(fmt.Sprintf("more than %d frames in a second", framesPerSecond))
	}
	return f, nil
}

func broke(what string) error {
	return fmt.Errorf("the voice worker broke the protocol: %s", termsafe.Text(what))
}

func (w *Worker) checkHello(f gatewayproto.Frame) error {
	if f.Op != voiceipc.OpHello {
		return broke(fmt.Sprintf("op %d where HELLO was owed", f.Op))
	}
	// The version is read leniently first and the rest strictly after, as on the attach socket: a worker
	// of a newer MINOR may say more in its HELLO, and must be told it is of another release, not that it
	// broke the protocol.
	var lenient struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(f.D, &lenient); err != nil || lenient.Version == "" {
		return broke("a HELLO naming no version")
	}
	// The attach socket's rule, the daemon as the server. Its reason is not quoted: it speaks of a server and
	// a client and says which to upgrade, and here the two are one install, replaced together.
	if c := gatewayproto.Check(w.opts.Version, lenient.Version); !c.Compatible {
		return fmt.Errorf("the voice worker beside this daemon is version %s and the daemon %s; reinstall "+
			"Norite so both are of one release", termsafe.Text(truncate(lenient.Version, 64)), w.opts.Version)
	}
	var h voiceipc.Hello
	if err := ipc.Decode(f, &h); err != nil {
		return broke(err.Error())
	}
	return nil
}

func (w *Worker) decode(f gatewayproto.Frame) (Event, error) {
	switch f.Op {
	case voiceipc.OpOffer:
		var o voiceipc.Offer
		if err := ipc.Decode(f, &o); err != nil {
			return Event{}, broke(err.Error())
		}
		if err := o.Check(); err != nil {
			return Event{}, broke(err.Error())
		}
		return Event{Offer: &o}, nil
	case voiceipc.OpState:
		var s voiceipc.State
		if err := ipc.Decode(f, &s); err != nil {
			return Event{}, broke(err.Error())
		}
		if err := s.Check(); err != nil {
			return Event{}, broke(err.Error())
		}
		return Event{State: &s}, nil
	}
	return Event{}, broke(fmt.Sprintf("op %d, which a worker does not send after HELLO", f.Op))
}

// truncate cuts s to at most n bytes, on a character's boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
