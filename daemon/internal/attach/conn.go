// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package attach

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// conn is one attached client.
type conn struct {
	srv *Server
	nc  net.Conn
	log zerolog.Logger

	// Guarded by srv.mu: whether it asked for events, and its last dispatch's sequence number.
	watching bool
	seq      int64

	out    chan net.Buffers
	queued atomic.Int64

	ctx    context.Context // canceled when the client goes, so its relayed requests stop with it
	cancel context.CancelFunc

	killOnce sync.Once
	gone     chan struct{}
	closing  ipc.Close // set once, before gone closes

	inflightMu sync.Mutex
	inflight   map[string]struct{}
	requests   sync.WaitGroup
}

// serveConn runs one client from accept to close.
func (s *Server) serveConn(ctx context.Context, nc net.Conn) {
	// The peer check comes before a single byte is written. The directory's mode is what really keeps other
	// accounts out; this is what still holds if somebody loosens it.
	if err := s.checkPeer(nc); err != nil {
		s.log.Warn().Err(err).Msg("refused an attach client running as another account")
		_ = nc.Close()
		return
	}

	cctx, cancel := context.WithCancel(ctx)
	c := &conn{
		srv: s, nc: nc, log: s.log,
		out:      make(chan net.Buffers, queueFrames),
		ctx:      cctx,
		cancel:   cancel,
		gone:     make(chan struct{}),
		inflight: make(map[string]struct{}),
	}
	defer cancel()

	if refusal := s.add(c); refusal.Code != 0 {
		// Refused before HELLO, so the client's handshake reads the reason as its first frame.
		_ = nc.SetWriteDeadline(time.Now().Add(s.closeGrace))
		if f, err := ipc.Encode(ipc.OpClose, refusal); err == nil {
			_ = ipc.WriteFrame(nc, f)
		}
		_ = nc.Close()
		if refusal.Code == ipc.CloseTooManyClients {
			s.log.Warn().Int("clients", s.maxClients).Msg("refused an attach client: too many attached")
		}
		return
	}
	defer s.remove(c)

	var writer sync.WaitGroup
	writer.Go(c.write)
	// In reverse: the connection is ended, its requests are waited for — canceled by the end — and then the
	// writer, which closes the socket once it has said why.
	defer writer.Wait()
	defer c.requests.Wait()
	defer c.kill(0, "")

	if err := c.handshake(); err != nil {
		c.log.Debug().Err(err).Msg("attach handshake failed")
		return
	}
	c.read()
}

// handshake sends HELLO, reads IDENTIFY, checks the version and answers READY.
func (c *conn) handshake() error {
	hello, err := ipc.Encode(gatewayproto.OpHello, ipc.Hello{Version: c.srv.version})
	if err != nil {
		c.kill(0, "")
		return err
	}
	c.enqueueFrame(hello)

	_ = c.nc.SetReadDeadline(time.Now().Add(handshakeTimeout))
	f, err := ipc.ReadFrame(c.nc, ipc.MaxClientFrame)
	if err != nil {
		c.killForRead(err)
		return err
	}
	_ = c.nc.SetReadDeadline(time.Time{})

	if f.Op != gatewayproto.OpIdentify {
		c.kill(ipc.CloseNotIdentified, "IDENTIFY must come first")
		return fmt.Errorf("op %d before IDENTIFY", f.Op)
	}
	// The version first, read leniently, and only then the whole payload strictly: a newer client may have
	// added a field, and it should hear "restart the daemon" rather than "IDENTIFY is malformed".
	var announced struct {
		Properties struct {
			Version string `json:"version"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(f.D, &announced); err != nil {
		c.kill(ipc.CloseDecodeError, "IDENTIFY is malformed")
		return err
	}
	if v := announced.Properties.Version; !gatewayproto.Check(c.srv.version, v).Compatible {
		c.kill(ipc.CloseVersionMismatch, fmt.Sprintf("this daemon is version %s and cannot serve a %s client; "+
			"restart the daemon so both run the installed version", c.srv.version, termsafe.Text(v)))
		return errors.New("incompatible client version")
	}
	var id ipc.Identify
	if err := ipc.Decode(f, &id); err != nil {
		c.kill(ipc.CloseDecodeError, "IDENTIFY is malformed")
		return err
	}

	c.srv.mu.Lock()
	ready := c.srv.readyLocked()
	c.watching = id.Events
	c.seq = 1
	frame, err := ipc.Encode(gatewayproto.OpDispatch, ready)
	if err == nil {
		one, readyType := int64(1), "READY"
		frame.S, frame.T = &one, &readyType
		c.enqueueFrame(frame)
	}
	c.srv.mu.Unlock()
	if err != nil {
		c.kill(0, "")
		return err
	}

	// A logger of its own rather than c.log reassigned: c.log is read by kill, which the server calls under
	// its lock from other goroutines, and replacing it here raced with them (M20 /code-review).
	c.log.Debug().Str("client", termsafe.Text(id.Properties.Client)).
		Str("client_version", termsafe.Text(id.Properties.Version)).Bool("events", id.Events).
		Msg("attach client identified")
	return nil
}

// requestID is what an id may be: the contract's pattern, checked here because the id is echoed back.
var requestID = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)

// read handles everything after IDENTIFY: requests, and the end of the connection.
func (c *conn) read() {
	for {
		f, err := ipc.ReadFrame(c.nc, ipc.MaxClientFrame)
		if err != nil {
			c.killForRead(err)
			return
		}

		switch f.Op {
		case ipc.OpRequest:
			var req ipc.Request
			if err := ipc.Decode(f, &req); err != nil || !requestID.MatchString(req.ID) {
				c.kill(ipc.CloseDecodeError, "a request is malformed")
				return
			}
			c.handle(req)
		case gatewayproto.OpIdentify:
			c.kill(ipc.CloseAlreadyIdentified, "IDENTIFY was already sent")
			return
		default:
			c.kill(ipc.CloseUnknownOpcode, fmt.Sprintf("op %d is not one a client sends", f.Op))
			return
		}
	}
}

// handle starts one request, or answers at once when it cannot be started.
func (c *conn) handle(req ipc.Request) {
	refuse := func(code, msg string) {
		c.respond(ipc.Response{ID: req.ID, Error: &ipc.RelayError{Code: code, Message: msg}})
	}
	if !ipc.ValidMethod(req.Method) {
		refuse(ipc.RelayBadRequest, "the method must be GET, POST, PUT, PATCH or DELETE")
		return
	}

	c.inflightMu.Lock()
	_, duplicate := c.inflight[req.ID]
	full := len(c.inflight) >= ipc.MaxInFlight
	if !duplicate && !full {
		c.inflight[req.ID] = struct{}{}
	}
	c.inflightMu.Unlock()
	switch {
	case duplicate:
		refuse(ipc.RelayBadRequest, "a request with this id is already in flight")
		return
	case full:
		refuse(ipc.RelayTooManyRequests,
			fmt.Sprintf("this connection already has %d requests in flight", ipc.MaxInFlight))
		return
	}

	c.requests.Go(func() {
		var resp ipc.Response
		switch {
		case !strings.HasPrefix(req.Path, ipc.LocalPathPrefix):
			resp = c.srv.relay.Do(c.ctx, req)
		case c.srv.local != nil:
			// The daemon's own, and never the relay's: it is not sent to the instance, and does not wait on a
			// sign-in.
			resp = c.srv.local.Do(c.ctx, req)
		default:
			resp = ipc.Response{Error: &ipc.RelayError{Code: ipc.RelayBadRequest,
				Message: "this daemon answers no request of its own at that path"}}
		}
		resp.ID = req.ID
		c.inflightMu.Lock()
		delete(c.inflight, req.ID)
		c.inflightMu.Unlock()
		c.respond(resp)
	})
}

func (c *conn) respond(resp ipc.Response) {
	f, err := ipc.Encode(ipc.OpResponse, resp)
	if err != nil {
		c.log.Error().Err(err).Msg("could not encode a response")
		c.kill(0, "")
		return
	}
	c.enqueueFrame(f)
}

// killForRead ends the connection after a failed read, with a close code when the read failed because of
// what the client sent.
func (c *conn) killForRead(err error) {
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed):
		c.kill(0, "")
	case errors.Is(err, ipc.ErrFrameTooLarge):
		c.kill(ipc.CloseDecodeError, fmt.Sprintf("a frame exceeds %d bytes", ipc.MaxClientFrame))
	case isTimeout(err):
		c.kill(ipc.CloseNotIdentified, "no IDENTIFY in time")
	default:
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) || errors.Is(err, io.ErrUnexpectedEOF) {
			c.kill(ipc.CloseDecodeError, "a frame is not valid")
			return
		}
		c.kill(0, "")
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// enqueueFrame queues an encoded frame for the writer.
func (c *conn) enqueueFrame(f gatewayproto.Frame) {
	payload, err := ipc.Marshal(f)
	if err != nil {
		c.kill(0, "")
		return
	}
	c.enqueue(payload)
}

// enqueue queues one frame, given as the pieces of its JSON, behind its length. It never blocks: a client
// with no room left has stopped reading, and is dropped.
func (c *conn) enqueue(parts ...[]byte) {
	select {
	case <-c.gone:
		return
	default:
	}

	n := 0
	for _, p := range parts {
		n += len(p)
	}
	prefix := make([]byte, 4)
	binary.BigEndian.PutUint32(prefix, uint32(n))
	bufs := make(net.Buffers, 0, len(parts)+1)
	bufs = append(bufs, prefix)
	bufs = append(bufs, parts...)

	if c.queued.Add(int64(n+4)) > queueBytes {
		c.dropSlow()
		return
	}
	select {
	case c.out <- bufs:
	default:
		c.dropSlow()
	}
}

func (c *conn) dropSlow() {
	c.log.Warn().Msg("dropped an attach client that stopped reading")
	c.kill(ipc.CloseTooSlow, "this client stopped reading and fell too far behind; attach again to resync")
}

// write drains the queue onto the socket until the client goes, then tries to say why and closes.
func (c *conn) write() {
	for {
		select {
		case bufs := <-c.out:
			n := 0
			for _, b := range bufs {
				n += len(b)
			}
			if _, err := bufs.WriteTo(c.nc); err != nil {
				// The stream may hold half a frame now, so no Close frame can follow it.
				c.kill(0, "")
				_ = c.nc.Close()
				return
			}
			c.queued.Add(-int64(n))
		case <-c.gone:
			if c.closing.Code != 0 {
				_ = c.nc.SetWriteDeadline(time.Now().Add(c.srv.closeGrace))
				if f, err := ipc.Encode(ipc.OpClose, c.closing); err == nil {
					_ = ipc.WriteFrame(c.nc, f)
				}
			}
			_ = c.nc.Close()
			return
		}
	}
}

// kill ends the connection, with a Close frame carrying code and reason when code is not 0. It never
// blocks, so it is safe under the server's lock and from the fan-out. A writer blocked on a client that has
// stopped reading is unblocked by the deadline; the reader, by its own.
func (c *conn) kill(code int, reason string) {
	c.killOnce.Do(func() {
		c.closing = ipc.Close{Code: code, Reason: reason}
		close(c.gone)
		c.cancel()
		_ = c.nc.SetWriteDeadline(time.Now().Add(c.srv.closeGrace))
		_ = c.nc.SetReadDeadline(time.Now())
		if code != 0 {
			c.log.Debug().Int("code", code).Str("reason", reason).Msg("closing an attach client")
		}
	})
}
