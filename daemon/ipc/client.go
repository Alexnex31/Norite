// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
)

// ErrNotRunning is a dial that found no daemon listening: no socket, or a socket nobody accepts on.
var ErrNotRunning = errors.New("the daemon is not running")

// ErrClosed is a connection that ended without the daemon saying why: it exited, or the client closed it.
var ErrClosed = errors.New("the connection to the daemon closed")

// VersionError is a daemon this client cannot talk to under ADR 0033's rule.
type VersionError struct {
	Daemon, Client string
}

func (e *VersionError) Error() string {
	// Not gatewayproto.Check's own reason: it names a "server", and here that is the daemon. The two ship
	// together, so the ordinary cause is an upgrade the running daemon has not restarted into.
	return fmt.Sprintf("the running daemon is version %s and this client is %s, which cannot talk to each "+
		"other; restart the daemon so both run the installed version (`norite daemon restart`)",
		e.Daemon, e.Client)
}

// Options configure an attach.
type Options struct {
	// Client names the program attaching, as IDENTIFY's properties.client: "norite", "norite-gui".
	Client string
	// Version is this client's release version, or "dev".
	Version string
	// Events asks for the daemon's DISPATCH stream, delivered on Client.Events.
	Events bool
}

// Event is one dispatch, as the daemon forwarded it.
type Event struct {
	// Type is the dispatch's name: MESSAGE_CREATE, GUILD_UPDATE, and so on.
	Type string
	// Seq numbers this connection's dispatches from 1, READY being 1.
	Seq int64
	// Data is the payload exactly as the instance sent it. Untrusted: text in it reaches a terminal only
	// through termsafe or the escaping JSON writer (rule 19).
	Data json.RawMessage
}

// Result is the instance's answer to a relayed request.
type Result struct {
	Status int
	// Body is the instance's JSON body, or nil when it sent none.
	Body json.RawMessage
}

// eventBuffer is how many dispatches the client holds for a consumer that has not taken them yet. Past it
// the reader stops reading, the daemon's buffer for this client fills, and the daemon drops the client as
// too slow: the consumer's pace is the connection's, which is the point of bounding it.
const eventBuffer = 256

// Client is one attached connection to the daemon. Safe for concurrent use.
type Client struct {
	conn  net.Conn
	hello Hello
	ready Ready

	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[string]chan Response
	nextID  uint64
	err     error

	events    chan Event
	done      chan struct{}
	closing   chan struct{}
	closeOnce sync.Once
}

// Connect dials the daemon and attaches.
func Connect(ctx context.Context, opts Options) (*Client, error) {
	conn, err := Dial(ctx)
	if err != nil {
		return nil, err
	}
	return Attach(ctx, conn, opts)
}

// Attach performs the handshake on conn, which it owns from here on: it is closed on any failure, and by
// Client.Close.
//
// HELLO is read, the version checked here as well as at the daemon so the refusal can say which side to
// restart, IDENTIFY sent, and READY read. A daemon that refuses — too many clients, the version — sends Close
// instead, returned as a *CloseError.
func Attach(ctx context.Context, conn net.Conn, opts Options) (*Client, error) {
	c := &Client{
		conn:    conn,
		pending: make(map[string]chan Response),
		done:    make(chan struct{}),
		closing: make(chan struct{}),
	}

	// The handshake is bounded by ctx. A deadline it carries goes to the connection; a cancellation without
	// one closes the connection, which is the only way to interrupt a blocked read.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	ready, err := c.handshake(opts)
	interrupted := !stop()
	if err == nil && interrupted {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		if ctx.Err() != nil && !isCloseOrVersion(err) {
			return nil, fmt.Errorf("attaching to the daemon: %w", ctx.Err())
		}
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	c.ready = ready

	if opts.Events {
		c.events = make(chan Event, eventBuffer)
	}
	go c.read()
	return c, nil
}

func isCloseOrVersion(err error) bool {
	var ce *CloseError
	var ve *VersionError
	return errors.As(err, &ce) || errors.As(err, &ve)
}

func (c *Client) handshake(opts Options) (Ready, error) {
	f, err := c.readFrame()
	if err != nil {
		return Ready{}, err
	}
	if f.Op != gatewayproto.OpHello {
		return Ready{}, fmt.Errorf("the daemon sent op %d where HELLO belongs", f.Op)
	}
	if err := Decode(f, &c.hello); err != nil {
		return Ready{}, err
	}

	version := opts.Version
	if version == "" {
		version = gatewayproto.DevVersion
	}
	if !gatewayproto.Check(c.hello.Version, version).Compatible {
		return Ready{}, &VersionError{Daemon: c.hello.Version, Client: version}
	}

	identify, err := Encode(gatewayproto.OpIdentify, Identify{
		Properties: gatewayproto.IdentifyProperties{OS: runtime.GOOS, Client: opts.Client, Version: version},
		Events:     opts.Events,
	})
	if err != nil {
		return Ready{}, err
	}
	if err := WriteFrame(c.conn, identify); err != nil {
		return Ready{}, fmt.Errorf("identifying to the daemon: %w", err)
	}

	f, err = c.readFrame()
	if err != nil {
		return Ready{}, err
	}
	if f.Op != gatewayproto.OpDispatch || f.T == nil || *f.T != "READY" {
		return Ready{}, fmt.Errorf("the daemon sent op %d where READY belongs", f.Op)
	}
	var ready Ready
	if err := Decode(f, &ready); err != nil {
		return Ready{}, err
	}
	return ready, nil
}

// readFrame reads one frame, turning the daemon's Close into a *CloseError and a bare EOF into ErrClosed.
func (c *Client) readFrame() (gatewayproto.Frame, error) {
	f, err := ReadFrame(c.conn, MaxDaemonFrame)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return f, ErrClosed
		}
		return f, fmt.Errorf("reading from the daemon: %w", err)
	}
	if f.Op == OpClose {
		var cl Close
		if err := Decode(f, &cl); err != nil {
			return f, err
		}
		return f, &CloseError{Code: cl.Code, Reason: cl.Reason}
	}
	return f, nil
}

// Hello returns what the daemon said on connect.
func (c *Client) Hello() Hello { return c.hello }

// Ready returns the daemon's answer to IDENTIFY: who it is signed in as, and the guilds it holds.
func (c *Client) Ready() Ready { return c.ready }

// Events delivers the daemon's dispatches after READY, in order, and is closed when the connection ends; Err
// then says why. Nil unless Options.Events was set.
func (c *Client) Events() <-chan Event { return c.events }

// Done is closed when the connection has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended: a *CloseError when the daemon said, ErrClosed when it did not or
// this client closed it. Nil while it is open.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection. Requests in flight fail with ErrClosed.
func (c *Client) Close() error {
	c.closeOnce.Do(func() { close(c.closing) })
	err := c.conn.Close()
	<-c.done
	return err
}

// Do asks the daemon to perform a request with its own credential and returns the instance's answer.
//
// body is sent as JSON, nil for none. Whatever status the instance answered with is a Result rather than an
// error: a 404 is an answer, and what it means is the caller's to decide. A *RelayError is the daemon
// declining or failing to ask; a *CloseError or ErrClosed, the connection ending first.
func (c *Client) Do(ctx context.Context, method, path string, body any) (Result, error) {
	var raw json.RawMessage
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return Result{}, fmt.Errorf("encoding the request body: %w", err)
		}
		raw = encoded
	}

	answer := make(chan Response, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return Result{}, err
	}
	c.nextID++
	id := strconv.FormatUint(c.nextID, 10)
	c.pending[id] = answer
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}

	frame, err := Encode(OpRequest, Request{ID: id, Method: method, Path: path, Body: raw})
	if err != nil {
		forget()
		return Result{}, err
	}
	if err := c.write(ctx, frame); err != nil {
		forget()
		return Result{}, err
	}

	select {
	case resp := <-answer:
		return resultOf(id, resp)
	case <-ctx.Done():
		forget()
		return Result{}, ctx.Err()
	case <-c.done:
		// The answer may have arrived just before the connection ended, and an answered request is not a
		// failed one.
		select {
		case resp := <-answer:
			return resultOf(id, resp)
		default:
			return Result{}, c.Err()
		}
	}
}

func resultOf(id string, resp Response) (Result, error) {
	if resp.Error != nil {
		return Result{}, resp.Error
	}
	if resp.Status == nil {
		return Result{}, fmt.Errorf("the daemon answered request %s with neither a status nor an error", id)
	}
	return Result{Status: *resp.Status, Body: resp.Body}, nil
}

// write sends one frame, bounded by ctx's deadline. A local socket blocks a write only when the daemon has
// stopped reading, and that is worth a bounded wait rather than an indefinite one.
func (c *Client) write(ctx context.Context, f gatewayproto.Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetWriteDeadline(deadline)
		defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	}
	if err := WriteFrame(c.conn, f); err != nil {
		if e := c.Err(); e != nil {
			return e
		}
		return fmt.Errorf("writing to the daemon: %w", err)
	}
	return nil
}

// read is the one parser of what the daemon sends after READY: responses to their requests, dispatches to
// Events, and Close to Err. Anything else is a daemon this client does not understand, and ends the
// connection rather than being skipped.
func (c *Client) read() {
	err := c.readLoop()

	c.mu.Lock()
	select {
	case <-c.closing:
		// This client closed it; whatever the read saw afterwards is the close, not the cause.
		err = ErrClosed
	default:
	}
	c.err = err
	// A request still waiting returns through done, with this error.
	c.pending = nil
	c.mu.Unlock()

	_ = c.conn.Close()
	if c.events != nil {
		close(c.events)
	}
	close(c.done)
}

func (c *Client) readLoop() error {
	for {
		f, err := c.readFrame()
		if err != nil {
			return err
		}

		switch f.Op {
		case OpResponse:
			var resp Response
			if err := Decode(f, &resp); err != nil {
				return err
			}
			c.mu.Lock()
			answer, ok := c.pending[resp.ID]
			delete(c.pending, resp.ID)
			c.mu.Unlock()
			if ok {
				answer <- resp
			}
			// A response nobody is waiting for is one whose caller gave up; dropping it is the answer.

		case gatewayproto.OpDispatch:
			if c.events == nil {
				return errors.New("the daemon sent a dispatch to a client that asked for none")
			}
			if f.T == nil || f.S == nil {
				return errors.New("the daemon sent a dispatch without a type or a sequence number")
			}
			select {
			case c.events <- Event{Type: *f.T, Seq: *f.S, Data: f.D}:
			case <-c.closing:
				return ErrClosed
			}

		default:
			return fmt.Errorf("the daemon sent op %d, which this client does not understand", f.Op)
		}
	}
}
