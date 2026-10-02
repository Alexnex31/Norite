// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package attach is the daemon's half of the attach socket (ADR 0010, M20): the listener, the peer check,
// the handshake, the fan-out of the gateway's events to every attached client, and the hand-off of each
// relayed request. What the socket speaks is daemon/ipc, which the clients import; the trust tier is stated
// there (rule 16).
//
// # The fan-out cannot be stalled by a client
//
// Every client has its own writer goroutine and a bounded queue, measured in frames and in bytes. Queuing a
// frame never blocks: a client whose queue is full has stopped reading, and it is dropped with
// ipc.CloseTooSlow rather than allowed to hold up delivery to anybody else. A dropped client attaches again
// and resyncs. That is ADR 0010's rule, and the reason it matters more than it looks: the goroutine that
// delivers events here is the gateway connection's own read loop, so a fan-out that waited on a client would
// stop the daemon reading the gateway.
//
// # A client's view never spans a gap
//
// The Server is the gateway client's Sink, wrapping the state. When the daemon's gateway session starts
// afresh, or its sign-in ends, the state is cleared (M19), and any view an attached client built from earlier
// events would show the gap as history. So every client that asked for events is closed with
// ipc.CloseResync at each of those points, and again when the new session's READY has filled the state, so
// that a client which reattached in between is not left with the empty list it was given. A client that
// asked for no events holds no view and is left alone.
package attach

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/internal/state"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// ErrUnusable is a socket address the daemon cannot listen on and will not be able to on a restart: a path
// too long for the platform, a file that is not a socket where the socket goes, a pipe name another
// process holds. Anything else Listen returns may be transient — out of descriptors, a full disk — and a
// restart can fix it, which is the line daemonproc draws between exit 4 and exit 1 (M20 /code-review).
var ErrUnusable = errors.New("the attach socket's address cannot be used")

// Session is what the server asks of the daemon's session: who is signed in, without waiting.
// *session.Source implements it.
type Session interface {
	Status() (session.Standing, session.Account)
}

// State is the daemon's view of its gateway connection, which the Server wraps. *state.State implements it.
type State interface {
	Begin(generation uint64)
	Dispatch(eventType string, data json.RawMessage)
	End()
	Guilds() []state.Guild
	// Generation is the sign-in the state was built from, or 0 when it holds nothing.
	Generation() uint64
}

// Relay performs a request with the daemon's credential. Its answer's ID is ignored and set by the server.
type Relay interface {
	Do(ctx context.Context, req ipc.Request) ipc.Response
}

// Options configure a Server.
type Options struct {
	Session Session
	State   State
	Relay   Relay
	// Version is the daemon's release version, sent in HELLO and checked against each client's.
	Version string
	Log     zerolog.Logger
}

const (
	// handshakeTimeout bounds the wait for a client's IDENTIFY. A local client that has connected and says
	// nothing is broken, and it holds one of MaxClients places while it does.
	handshakeTimeout = 10 * time.Second
	// queueFrames and queueBytes bound what a client may have waiting to be written. Frames, because the
	// channel needs a capacity; bytes, because a frame may be a relayed body of several megabytes. The bytes
	// of a dispatch are shared by every client it goes to, so this bounds how far behind a client may fall,
	// not a copy per client.
	queueFrames = 256
	queueBytes  = 2 * ipc.MaxResponseBody
	// closeGrace is how long the writer tries to deliver a Close frame before closing anyway. A client that
	// has stopped reading and does not start again within it is cut off without one.
	closeGrace = time.Second
)

// Server serves the attach socket. It is also the gateway client's Sink: see the package comment.
type Server struct {
	session Session
	state   State
	relay   Relay
	version string
	log     zerolog.Logger

	// Overridden by tests.
	maxClients int
	wantUID    int
	closeGrace time.Duration

	mu      sync.Mutex
	clients map[*conn]struct{}
	closing bool
	conns   sync.WaitGroup
}

// New builds a Server. Nothing listens until Serve.
func New(opts Options) *Server {
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	return &Server{
		session:    opts.Session,
		state:      opts.State,
		relay:      opts.Relay,
		version:    version,
		log:        opts.Log,
		maxClients: ipc.MaxClients,
		wantUID:    ownUID(),
		closeGrace: closeGrace,
		clients:    make(map[*conn]struct{}),
	}
}

// Serve accepts clients on l until ctx is done, then closes every client with ipc.CloseGoingAway and waits
// for their goroutines. It owns l and closes it.
func (s *Server) Serve(ctx context.Context, l net.Listener) {
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	defer stop()

	for {
		nc, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			// Most likely out of file descriptors. Waiting a moment is all that helps, and spinning on the
			// error would make it worse.
			s.log.Warn().Err(err).Msg("could not accept an attach client; trying again")
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		s.conns.Go(func() { s.serveConn(ctx, nc) })
	}

	s.mu.Lock()
	s.closing = true
	for c := range s.clients {
		c.kill(ipc.CloseGoingAway, "the daemon is stopping")
	}
	s.mu.Unlock()
	s.conns.Wait()
}

// ---------- the Sink ----------

// Begin clears the state for a fresh gateway session and resyncs every client watching it.
func (s *Server) Begin(generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Begin(generation)
	s.resyncLocked("the daemon's gateway session started afresh")
}

// End clears the state for a sign-in that is over and resyncs every client watching it.
func (s *Server) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.End()
	s.resyncLocked("the daemon's sign-in ended")
}

// Dispatch applies an event to the state and forwards it to every client that asked for events.
//
// Under the server's lock, with the state's update, so a client being handed READY sees either the state
// before this event and then the event, or the state after it and not the event — never the event twice in
// one form and not at all in the other.
func (s *Server) Dispatch(eventType string, data json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Dispatch(eventType, data)

	switch eventType {
	case "READY":
		// The gateway's READY is the daemon's own business, and it has just filled the state: any client
		// that attached since Begin was given an empty guild list and has to come back for this one.
		s.resyncLocked("the daemon's gateway session is ready")
		return
	case "RESUMED":
		// Nothing was missed; the clients' views stand.
		return
	}

	// An event with no payload is not forwarded. The gateway client hands over `d` as it arrived, and a
	// frame with no `d` at all reaches here as nothing, which spliced into the frame below would make it
	// invalid JSON and end every watching client's connection — on every such frame a hostile instance
	// chose to send (M20 /security-sweep). Any payload that is present is valid JSON: it was decoded out
	// of a frame that parsed.
	if len(data) == 0 {
		return
	}

	// Forwarded only while the state belongs to the sign-in the session names. After a login the session
	// names the new account at once, and the gateway connection goes on delivering the old one's events
	// until it notices that sign-in ended; a client that attached in between was told it is the new account,
	// and must not be sent the old one's messages as part of that view (M20 /code-review).
	if _, account := s.session.Status(); account.Generation != s.state.Generation() {
		return
	}

	// Nothing is built for nobody: with no client watching — the ordinary case, a daemon with no TUI open —
	// the payload is not copied at all.
	watching := false
	for c := range s.clients {
		if c.watching {
			watching = true
			break
		}
	}
	if !watching {
		return
	}

	// Encoded once for every client, which differ only in the sequence number. The payload is copied into
	// no client's frame: each writer sends head, its own number and tail as one vectored write.
	t, err := json.Marshal(eventType)
	if err != nil {
		return
	}
	head := make([]byte, 0, len(data)+32)
	head = append(head, `{"op":0,"d":`...)
	head = append(head, data...)
	head = append(head, `,"s":`...)
	tail := make([]byte, 0, len(t)+6)
	tail = append(tail, `,"t":`...)
	tail = append(tail, t...)
	tail = append(tail, '}')

	for c := range s.clients {
		if !c.watching {
			continue
		}
		c.seq++
		c.enqueue(head, []byte(strconv.FormatInt(c.seq, 10)), tail)
	}
}

func (s *Server) resyncLocked(reason string) {
	for c := range s.clients {
		if c.watching {
			c.kill(ipc.CloseResync, reason)
		}
	}
}

// ready builds the answer to a client's IDENTIFY. Called with s.mu held, so it agrees with what Dispatch
// forwards afterwards.
func (s *Server) readyLocked() ipc.Ready {
	ready := ipc.Ready{Guilds: []ipc.GuildSummary{}}

	standing, account := s.session.Status()
	switch standing {
	case session.Live:
		ready.Standing = ipc.StandingSignedIn
	case session.SignedOut:
		ready.Standing = ipc.StandingSignedOut
	default:
		ready.Standing = ipc.StandingStarting
	}
	if standing != session.SignedOut && account.UserID != "" {
		ready.Account = &ipc.Account{
			InstanceURL: account.InstanceURL,
			UserID:      account.UserID,
			// Read back out of a file a person can edit, so foreign again (rule 19).
			Username: termsafe.Text(account.Username),
		}
	}

	// The guilds only when the state was built from the sign-in READY names. A login switches the session
	// to the new account at once, while the state is cleared only once the gateway connection notices the
	// old sign-in ended, so in between the two disagree, and READY would name one account as a member of
	// the other's guilds (M20 /code-review).
	if ready.Account == nil || s.state.Generation() != account.Generation {
		return ready
	}
	guilds := s.state.Guilds()
	// Snowflakes are decimal strings, so by length and then by text is by value.
	slices.SortFunc(guilds, func(a, b state.Guild) int {
		return cmp.Or(cmp.Compare(len(a.ID), len(b.ID)), cmp.Compare(a.ID, b.ID))
	})
	for _, g := range guilds {
		ready.Guilds = append(ready.Guilds, ipc.GuildSummary{ID: g.ID, Name: g.Name, OwnerID: g.OwnerID})
	}
	return ready
}

// add registers c, or says why it cannot be: the daemon is stopping, or full. A zero Close means added.
//
// Two refusals rather than one, because they ask different things of the client: one that arrives as the
// daemon stops should wait for it to come back, not be told it is full (M20 /code-review).
func (s *Server) add(c *conn) ipc.Close {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closing:
		return ipc.Close{Code: ipc.CloseGoingAway, Reason: "the daemon is stopping"}
	case len(s.clients) >= s.maxClients:
		return ipc.Close{Code: ipc.CloseTooManyClients,
			Reason: fmt.Sprintf("the daemon is already serving %d clients", s.maxClients)}
	}
	s.clients[c] = struct{}{}
	return ipc.Close{}
}

func (s *Server) remove(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, c)
}

// Clients reports how many clients are attached, handshaking ones included.
func (s *Server) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}
