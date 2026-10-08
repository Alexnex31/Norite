// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package automation is the daemon's port for scripts (M22): a TCP listener on 127.0.0.1 that forwards a
// script's REST calls to the instance with the script's own API token.
//
// # Trust tier (rule 16)
//
// Secret-protected, and lower than the attach socket's. daemon/ipc's automation.go states it in full, with
// the protocol; what this package adds is how the tier is kept.
//
//   - The daemon's own credential is never used here, and cannot be: Options takes the instance's URL and
//     an HTTP client, and nothing that holds a token. This package does not import the session, and a test
//     holds it to that.
//   - Nothing is said to a connection before it presents the port secret, and a wrong one is answered the
//     same whatever was wrong with it, compared in constant time.
//   - Only a value shaped like an API token is forwarded. An access token is a person's sign-in, with
//     every reach the account has; a script that pasted one is refused rather than served.
//   - The paths are the relay's, less all of /auth, and nothing under /@daemon/: no request the daemon
//     answers for a first-party client is reachable with a secret.
//
// # What a script's answer is
//
// The instance's status and body, as they came. A 401 is the script's to deal with: its token was refused,
// and no session here can renew it. That is the difference from the relay, where a 401 is the session's.
//
// # One instance
//
// A token is a credential for the instance that minted it, and the daemon cannot tell which that was. So
// the port serves the instance it was enabled for and no other: Options.EnabledFor is recorded when the
// user turns the port on, and a request made while the daemon is signed in anywhere else is refused before
// the token goes anywhere. Without it, signing in to a second instance would hand the first one's tokens
// to the second's operator, one request at a time (M22 /code-review).
//
// # Bounds
//
// A fixed number of scripts, one request at a time on each, a rate shared by all of them, and the
// attach socket's bounds on a frame and on an answer. Connections that have not yet presented the secret
// are counted apart from those that have, so that filling the port with silence does not use the places
// scripts are served from; one over that count is closed like any other stranger, without a word. The rate exists because a script shares its owner's
// address on the instance: a loop that spent the instance's per-address budget would get the owner's own
// client answered 429.
//
// Neither secret, and no request or response body, reaches the log at any level.
package automation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/atomicfile"
	"github.com/Alexnex31/Norite/daemon/internal/relay"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Instance says where requests go. The daemon answers it from its stored sign-in.
type Instance interface {
	// InstanceURL is the instance the daemon is signed in to, or false when it is signed in to none.
	InstanceURL() (string, bool)
}

// Options configure a Server.
type Options struct {
	// Port is the TCP port on 127.0.0.1. Zero lets the OS choose, which only the tests want.
	Port int
	// StateDir is where the address and secret are written.
	StateDir string
	// Instance says where requests go. It carries no credential, by design.
	Instance Instance
	// EnabledFor is the instance the user turned the port on for. Requests are served while the daemon is
	// signed in to it and refused while it is signed in to any other.
	EnabledFor string
	// HTTP performs the calls. Open takes a copy that follows no redirect, whatever this one does: a
	// redirect would carry the script's token to a path this package has not checked.
	HTTP *http.Client
	// Version goes in Ready and in the User-Agent.
	Version string
	Log     zerolog.Logger

	// Rate and Burst bound requests across every connection. Zero means the defaults.
	Rate  float64
	Burst int
	// now is the clock the rate reads. Nil means time.Now.
	now func() time.Time
}

const (
	// identifyWait bounds how long a connection may take to present its secrets.
	identifyWait = 5 * time.Second
	// idleWait bounds how long an identified connection may ask nothing.
	idleWait = 5 * time.Minute
	// writeWait bounds one write to a script.
	writeWait = 10 * time.Second
	// requestTimeout bounds one call to the instance.
	requestTimeout = 30 * time.Second
	// secretBytes is the port secret's length before encoding.
	secretBytes = 32

	// DefaultRate and DefaultBurst are the shared request rate: requests a second, and how many may come
	// at once after a quiet spell. Well under what an instance allows one address, which is the point.
	DefaultRate  = 5.0
	DefaultBurst = 20
)

// ErrPortTaken is a port something else is listening on. The daemon does not move to another: a script
// would not find it. Only that: a port this account may not bind, or a machine with no loopback address,
// is a different problem with a different remedy, and is reported as itself.
var ErrPortTaken = errors.New("the automation port is taken")

// listen is net.Listen, a variable so a test can make binding fail for a reason other than the port being
// held, which nothing portable provokes.
var listen = net.Listen

// maxPending is how many connections may be waiting to present their secrets. Apart from the scripts being
// served, so strangers holding connections open cost scripts nothing but this.
const maxPending = 32

// requestID is what a request's id may be: the contract's pattern, checked because the id is echoed back.
var requestID = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)

// Server is the open port.
type Server struct {
	opts      Options
	listener  net.Listener
	file      ipc.AutomationFile
	secretSum [sha256.Size]byte
	userAgent string
	bucket    *bucket

	enabledFor string

	mu sync.Mutex
	// conns is every open connection; identified counts those that have presented the secret.
	conns      map[net.Conn]struct{}
	identified int
	wg         sync.WaitGroup
}

// Clean removes an automation file left in stateDir. The caller holds the daemon's lock, which is what makes
// any file found there stale: left by a daemon that was killed, naming a port it no longer holds and that
// anything may since have bound. Called when the daemon starts, whether or not the port is to open, and by
// Open before it binds. It removes the name and never follows a link.
func Clean(stateDir string) {
	_ = os.Remove(ipc.AutomationFilePath(stateDir))
}

// sameInstance reports whether two instance URLs name one instance: scheme, host and path prefix.
func sameInstance(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil || ua.Host == "" || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) &&
		strings.TrimRight(ua.Path, "/") == strings.TrimRight(ub.Path, "/")
}

// Open binds the port, mints this run's secret and writes both where a script can find them. The caller
// serves with Serve and, when done, calls Close.
func Open(opts Options) (*Server, error) {
	if opts.Instance == nil || opts.HTTP == nil {
		return nil, errors.New("automation: an instance and an HTTP client are required")
	}
	if !sameInstance(opts.EnabledFor, opts.EnabledFor) {
		return nil, errors.New("automation: the instance the port was enabled for is required")
	}
	// A copy, so the caller's client is not changed under it, and so that the policy does not depend on
	// the caller having remembered it.
	client := *opts.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	opts.HTTP = &client
	if opts.Port < 0 || opts.Port > 65535 {
		return nil, fmt.Errorf("automation: %d is not a port", opts.Port)
	}
	if opts.Rate <= 0 {
		opts.Rate = DefaultRate
	}
	if opts.Burst <= 0 {
		opts.Burst = DefaultBurst
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}

	// The address is spelled out. ":7717" binds every interface and looks the same on a developer's
	// machine (M8), and "localhost" is a name something else resolves.
	//
	// Whatever a killed daemon left is removed first, so that failing to bind does not leave a file
	// naming this port with a secret for whoever did bind it.
	Clean(opts.StateDir)
	l, err := listen("tcp4", net.JoinHostPort("127.0.0.1", fmt.Sprint(opts.Port)))
	if err != nil {
		if addressInUse(err) {
			return nil, fmt.Errorf("%w: %s", ErrPortTaken, termsafe.Text(err.Error()))
		}
		return nil, fmt.Errorf("automation: cannot listen on 127.0.0.1 port %d: %s", opts.Port, termsafe.Text(err.Error()))
	}

	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("automation: minting the port secret: %w", err)
	}
	file := ipc.AutomationFile{Address: l.Addr().String(), Secret: base64.RawURLEncoding.EncodeToString(raw)}
	if err := ipc.CheckAutomationAddress(file.Address); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("automation: bound %s: %w", file.Address, err)
	}

	opts.Version = version
	s := &Server{
		opts: opts, listener: l, file: file, secretSum: sha256.Sum256([]byte(file.Secret)),
		userAgent:  "norite-daemon/" + version + " (automation)",
		bucket:     newBucket(opts.Rate, opts.Burst, opts.now),
		conns:      map[net.Conn]struct{}{},
		enabledFor: opts.EnabledFor,
	}

	// Written once the port is bound, so the file never names a port nothing answers on. 0600 and
	// replaced, never followed: a link left where the file goes must not send the secret elsewhere. A
	// write that reached the disk but could not be flushed is still the file (atomicfile.ErrNotDurable).
	body, err := json.Marshal(file)
	if err == nil {
		err = atomicfile.Write(ipc.AutomationFilePath(opts.StateDir), append(body, '\n'), atomicfile.Options{Mode: 0o600})
	}
	if err != nil && !errors.Is(err, atomicfile.ErrNotDurable) {
		_ = l.Close()
		return nil, fmt.Errorf("automation: writing the port's file: %w", err)
	}
	return s, nil
}

// Address is where the port listens.
func (s *Server) Address() string { return s.file.Address }

// Serve accepts scripts until ctx ends or the listener closes, then closes every connection and waits for
// them.
func (s *Server) Serve(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() { _ = s.listener.Close() })
	defer stop()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			// Most likely out of file descriptors, as on the attach socket. Leaving the loop would end the
			// port for the rest of the daemon's run with its file still naming it.
			s.opts.Log.Warn().Str("error", termsafe.Text(err.Error())).Msg("could not accept a script; trying again")
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		// Set here and not by the connection's own goroutine: once a connection is among those Serve wakes
		// below, its deadline is already this one, so the wake cannot be overwritten by a goroutine that
		// had not started yet and leave a stopping daemon waiting out the five seconds.
		_ = conn.SetReadDeadline(time.Now().Add(identifyWait))
		if !s.admit(conn) {
			// A stranger like any other: it has presented nothing, so it is told nothing.
			_ = conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.release(conn)
			s.serve(ctx, conn)
		}()
	}

	// Each connection is woken rather than closed under it, so it can say why it is ending. One in the
	// middle of a request has had that request's context canceled already.
	s.mu.Lock()
	for c := range s.conns {
		_ = c.SetReadDeadline(time.Now())
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Close stops listening and removes the file, so nothing is left naming a port and a secret that are gone.
// It removes the file only while it still holds this run's secret: a daemon that replaced this one has
// written its own.
func (s *Server) Close() {
	_ = s.listener.Close()
	path := ipc.AutomationFilePath(s.opts.StateDir)
	if f, err := ipc.LoadAutomationFile(s.opts.StateDir); err == nil && f.Secret == s.file.Secret {
		_ = os.Remove(path)
	}
}

// admit takes a place among the connections that have yet to present their secrets.
func (s *Server) admit(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns)-s.identified >= maxPending {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

// promote moves a connection that presented the secret to a place among the scripts served, if there is one.
func (s *Server) promote() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.identified >= ipc.MaxAutomationConns {
		return false
	}
	s.identified++
	return true
}

func (s *Server) demote() {
	s.mu.Lock()
	s.identified--
	s.mu.Unlock()
}

func (s *Server) release(conn net.Conn) {
	_ = conn.Close()
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

// closeWith sends a Close frame and closes. Best effort: the peer may be gone, or may never read.
func (s *Server) closeWith(conn net.Conn, code int, reason string) {
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	if f, err := ipc.Encode(ipc.OpClose, ipc.Close{Code: code, Reason: reason}); err == nil {
		_ = ipc.WriteFrame(conn, f)
	}
	_ = conn.Close()
}

// refusal is the one answer to a first frame that does not open the port, whatever was wrong with it.
const refusal = "the automation port did not accept that secret"

// serve runs one connection: the two secrets, then requests, one at a time.
func (s *Server) serve(ctx context.Context, conn net.Conn) {
	// Serve set the deadline for this read before it admitted the connection.
	first, err := ipc.ReadFrame(conn, ipc.MaxAutomationIdentify)
	if err != nil {
		// Nothing is said to something that has not presented the secret: not a browser, not a port
		// scanner, not a script that sent a megabyte. It is closed.
		return
	}
	var id ipc.AutomationIdentify
	if first.Op != ipc.OpAutomationIdentify || ipc.Decode(first, &id) != nil {
		return
	}
	// Hashed first, so the comparison's time says nothing about the length of what was sent either.
	sum := sha256.Sum256([]byte(id.Secret))
	if subtle.ConstantTimeCompare(sum[:], s.secretSum[:]) != 1 {
		s.closeWith(conn, ipc.CloseAutomationRefused, refusal)
		return
	}
	// The secret is right, so this is a script of the user's and is told what is wrong.
	if !ipc.LooksLikeAPIToken(id.Token) {
		s.closeWith(conn, ipc.CloseAutomationRefused, "the token is not an API token: one begins nat_ and "+
			"comes from `norite token create`. An access token is a person's sign-in and is never forwarded")
		return
	}

	// Said only now, to something that has proved it is the user's.
	if !s.promote() {
		s.closeWith(conn, ipc.CloseTooManyClients, "the automation port is serving as many scripts as it will")
		return
	}
	defer s.demote()

	if !s.write(conn, ipc.OpAutomationReady, ipc.AutomationReady{Version: s.opts.Version}) {
		return
	}

	for {
		// The deadline first, then the question. Serve wakes a waiting connection by moving its deadline
		// to now, after the context has ended. Asked in the other order, a connection could see a live
		// context, lose the processor, and then set its deadline back over Serve's: five minutes of a
		// stopping daemon waiting on it (M22 /code-review). This way either the context is seen ended
		// here, or Serve's deadline lands after this one and ends the read.
		_ = conn.SetReadDeadline(time.Now().Add(idleWait))
		if ctx.Err() != nil {
			s.closeWith(conn, ipc.CloseGoingAway, "the daemon is stopping")
			return
		}
		f, err := ipc.ReadFrame(conn, ipc.MaxClientFrame)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			s.closeWith(conn, ipc.CloseGoingAway, "the daemon is stopping")
			return
		case errors.Is(err, io.EOF):
			return
		case errors.Is(err, os.ErrDeadlineExceeded):
			s.closeWith(conn, ipc.CloseAutomationIdle, "nothing was asked for five minutes")
			return
		default:
			s.closeWith(conn, ipc.CloseDecodeError, "a frame that is not valid, or is too large")
			return
		}

		switch f.Op {
		case ipc.OpRequest:
		case ipc.OpAutomationIdentify:
			s.closeWith(conn, ipc.CloseAlreadyIdentified, "the secrets were already presented")
			return
		default:
			s.closeWith(conn, ipc.CloseUnknownOpcode, "an op a script may not send")
			return
		}
		var req ipc.Request
		if err := ipc.Decode(f, &req); err != nil || !requestID.MatchString(req.ID) {
			// Before anything is asked of the instance: the id is echoed, and a request whose answer
			// could not be a valid frame is not performed.
			s.closeWith(conn, ipc.CloseDecodeError, "a request that is not valid")
			return
		}
		resp := s.do(ctx, req, id.Token)
		resp.ID = req.ID
		if !s.write(conn, ipc.OpResponse, resp) {
			return
		}
	}
}

// write sends one frame, bounded so a script that stopped reading cannot hold its connection for ever.
func (s *Server) write(conn net.Conn, op gatewayproto.Opcode, payload any) bool {
	f, err := ipc.Encode(op, payload)
	if err != nil {
		return false
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	return ipc.WriteFrame(conn, f) == nil
}

func failure(code, msg string) ipc.Response {
	return ipc.Response{Error: &ipc.RelayError{Code: code, Message: msg}}
}

var methods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
}

// do performs one request with the script's token.
func (s *Server) do(ctx context.Context, req ipc.Request, token string) ipc.Response {
	if !methods[req.Method] {
		return failure(ipc.RelayBadRequest, "the method must be GET, POST, PUT, PATCH or DELETE")
	}
	target, err := relay.ScriptTarget(req.Path)
	if err != nil {
		return failure(ipc.RelayRefused, err.Error())
	}
	// "No body" is JSON null on the wire, as on the attach socket.
	body := req.Body
	if len(bytes.TrimSpace(body)) == 0 || bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		body = nil
	}

	// After the checks that cost nothing, so a refused request does not spend the budget, and before the
	// one that reaches the instance.
	if !s.bucket.take() {
		return failure(ipc.RelayTooManyRequests, fmt.Sprintf("the automation port takes %g requests a second "+
			"across every script; slow down and send it again", s.opts.Rate))
	}

	instanceURL, ok := s.opts.Instance.InstanceURL()
	if !ok {
		return failure(ipc.RelayNotSignedIn, "the daemon is not signed in to an instance; run `norite login`")
	}
	// Asked on every request, not once per connection: a sign-in can change under an open one.
	if !sameInstance(instanceURL, s.enabledFor) {
		return failure(ipc.RelayRefused, "the automation port was turned on for another instance than the one "+
			"the daemon is signed in to now, and a token is not sent to an instance it was not made on; "+
			"run `norite automation enable` to use the port with this one")
	}
	u, err := relay.Build(instanceURL, target)
	if err != nil {
		return failure(ipc.RelayUnreachable, err.Error())
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(ctx, req.Method, u.String(), reader)
	if err != nil {
		return failure(ipc.RelayBadRequest, "the request could not be built")
	}
	// The script's token, and only ever the script's: there is no other credential in reach of this
	// function.
	hreq.Header.Set("Authorization", "Bearer "+token)
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("User-Agent", s.userAgent)
	if body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.opts.HTTP.Do(hreq)
	if err != nil {
		// The error names the URL, which is the instance's and the path's; no secret is in it. Logged
		// sanitized, and the script is told only that the instance did not answer.
		s.opts.Log.Debug().Str("method", req.Method).Str("error", termsafe.Text(err.Error())).
			Msg("an automation request did not reach the instance")
		return failure(ipc.RelayUnreachable, "the instance did not answer")
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, ipc.MaxResponseBody+1))
	if err != nil {
		return failure(ipc.RelayUnreachable, "the instance's answer was cut short")
	}
	if len(raw) > ipc.MaxResponseBody {
		return failure(ipc.RelayTooLarge, "the instance's answer is larger than the daemon will carry")
	}
	status := resp.StatusCode
	out := ipc.Response{Status: &status}
	if len(bytes.TrimSpace(raw)) != 0 && json.Valid(raw) {
		out.Body = raw
	}
	return out
}
