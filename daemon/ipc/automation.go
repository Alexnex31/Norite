// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
)

// The automation port (M22): what a script speaks to the daemon, and the client half that speaks it.
//
// # Trust tier (rule 16)
//
// Secret-protected, for programs that are not first-party clients, and deliberately lower than the attach
// socket's (ADR 0017). It is TCP on 127.0.0.1, which every account on the machine can connect to, so the
// OS decides nothing here. Two secrets answer two questions, and neither stands in for the other:
//
//   - The port secret says this process may use this daemon's port. It is minted for each run of the
//     daemon and kept with the address in AutomationFile, readable by the daemon's own account only. It
//     grants nothing on the instance.
//   - The API token says what the script may do there. The daemon forwards it as the request's credential
//     and never stores or logs it. The daemon's own access token is never used on this path.
//
// Nothing the daemon answers for a first-party client is reachable here: no events, no state, and no
// request of the daemon's own (LocalPathPrefix).
//
// # The protocol
//
// The attach socket's framing, a 4-byte big-endian length and a gatewayproto.Frame as JSON. The script's
// first frame is OpAutomationIdentify, carrying both secrets; the daemon says nothing before it, so a
// stranger learns nothing by connecting. The answer is OpAutomationReady. After that the script sends
// Request frames and reads Response frames, the attach socket's own, one at a time. The daemon's last
// frame on a connection it closes is Close.
//
// A browser cannot speak this: an HTTP request's first four bytes read as a length in the hundreds of
// megabytes, which is refused before anything else is read.
//
// contracts/daemon-automation.schema.json is the contract.

// The automation port's own ops, numbered from 200 so they meet neither the gateway's nor the attach
// socket's. Request, Response and Close keep the attach socket's numbers and shapes.
const (
	// OpAutomationIdentify is a script's first frame. Script to daemon.
	OpAutomationIdentify gatewayproto.Opcode = 200
	// OpAutomationReady answers it. Daemon to script.
	OpAutomationReady gatewayproto.Opcode = 201
)

// AutomationIdentify is op 200's payload: the two secrets.
type AutomationIdentify struct {
	// Secret is the port secret, from AutomationFile.
	Secret string `json:"secret"`
	// Token is an API token, `nat_…`. Never an access token: one of those is a person's sign-in, and the
	// daemon refuses it here rather than forward it.
	Token string `json:"token"`
}

// AutomationReady is op 201's payload.
type AutomationReady struct {
	// Version is the daemon's release version, or "dev".
	Version string `json:"version"`
}

// Close codes the automation port adds to the ones it shares with the attach socket.
const (
	// CloseAutomationRefused: the port secret is wrong, or the token is not an API token. One code and one
	// reason for the secret, whatever was wrong with it.
	CloseAutomationRefused = gatewayproto.CloseAuthenticationFailed
	// CloseAutomationIdle: nothing was asked for longer than the port waits.
	CloseAutomationIdle = gatewayproto.CloseSessionTimedOut
)

// Bounds on the automation port.
const (
	// MaxAutomationIdentify bounds a script's first frame, which holds two short strings. Small, so that
	// whatever connects without the secret costs the daemon almost nothing.
	MaxAutomationIdentify = 4 << 10
	// MaxAutomationConns is how many scripts the daemon serves at once. A connection performs one request
	// at a time, so this is also the most requests in flight.
	MaxAutomationConns = 16
	// maxAutomationSecret and maxAutomationToken bound the two strings as typed.
	maxAutomationSecret = 128
	maxAutomationToken  = 256
)

// DefaultAutomationPort is where the port listens unless the user chose another.
const DefaultAutomationPort = 7717

// automationFileName is the file in the state directory that holds the port's address and secret.
const automationFileName = "automation.json"

// AutomationFilePath is where a running daemon with the port enabled keeps its address and secret.
func AutomationFilePath(stateDir string) string { return filepath.Join(stateDir, automationFileName) }

// AutomationFile is what a script needs to reach the port. Written by the daemon when the listener opens
// and removed when it closes.
type AutomationFile struct {
	// Address is the listener's, always a 127.0.0.1 literal and a port.
	Address string `json:"address"`
	// Secret is the port secret for this run of the daemon.
	Secret string `json:"secret"`
}

// ErrAutomationOff is no automation file: the port is not enabled, or the daemon is not running.
var ErrAutomationOff = errors.New("the automation port is not open")

// maxAutomationFile bounds the file as read. It holds two short strings.
const maxAutomationFile = 4 << 10

// LoadAutomationFile reads the port's address and secret from stateDir.
//
// The file is asked what it is before it is opened, as a config is (M21): a path that names a pipe would
// otherwise wait for ever. And the address must be this machine's loopback literal, so that a file somebody
// altered cannot send a secret and a token to another host.
func LoadAutomationFile(stateDir string) (AutomationFile, error) {
	path := AutomationFilePath(stateDir)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return AutomationFile{}, ErrAutomationOff
	}
	if err != nil {
		return AutomationFile{}, fmt.Errorf("reading the automation port's file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxAutomationFile {
		return AutomationFile{}, fmt.Errorf("%s is not the file the daemon writes", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return AutomationFile{}, fmt.Errorf("reading the automation port's file: %w", err)
	}
	var f AutomationFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return AutomationFile{}, fmt.Errorf("%s is not the file the daemon writes", path)
	}
	if err := CheckAutomationAddress(f.Address); err != nil {
		return AutomationFile{}, fmt.Errorf("%s: %w", path, err)
	}
	if f.Secret == "" || len(f.Secret) > maxAutomationSecret {
		return AutomationFile{}, fmt.Errorf("%s holds no usable secret", path)
	}
	return f, nil
}

// CheckAutomationAddress holds an address to 127.0.0.1 and a port. Not `localhost`, which resolves through
// files and servers a literal does not (M8), and not another loopback address: one rule, exactly.
func CheckAutomationAddress(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		return errors.New("the automation port's address must be 127.0.0.1 and a port")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("the automation port's address must be 127.0.0.1 and a port")
	}
	return nil
}

// LooksLikeAPIToken reports whether s has the shape of an API token: the prefix, a sane length, and the
// alphabet a token is minted in. It decides nothing about validity, which is the instance's. It is what
// keeps an access token, a refresh token or a line of garbage from being forwarded, and what makes the
// value safe to place in a header.
func LooksLikeAPIToken(s string) bool {
	const prefix = "nat_"
	if len(s) <= len(prefix) || len(s) > maxAutomationToken || s[:len(prefix)] != prefix {
		return false
	}
	for _, r := range s[len(prefix):] {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// ---------- the client half ----------

// automationDialTimeout bounds connecting and being answered Ready.
const automationDialTimeout = 5 * time.Second

// AutomationClient is one script's connection to the port. One request at a time; safe for concurrent use.
type AutomationClient struct {
	mu   sync.Mutex
	conn net.Conn
	next int
	// Version is the daemon's, from Ready.
	Version string
}

// DialAutomation connects to the port file names, presents its secret and token, and waits for Ready.
//
// The address is checked again here, whoever built file: the secret and the token are sent to 127.0.0.1 or
// to nobody. A token that is not shaped like an API token is refused before anything is sent.
func DialAutomation(ctx context.Context, file AutomationFile, token string) (*AutomationClient, error) {
	if err := CheckAutomationAddress(file.Address); err != nil {
		return nil, err
	}
	if !LooksLikeAPIToken(token) {
		return nil, errors.New("that is not an API token: one begins nat_ and comes from `norite token create`")
	}

	dctx, cancel := context.WithTimeout(ctx, automationDialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp4", file.Address)
	if err != nil {
		return nil, fmt.Errorf("%w: nothing answers on %s", ErrAutomationOff, file.Address)
	}
	if deadline, ok := dctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	frame, err := Encode(OpAutomationIdentify, AutomationIdentify{Secret: file.Secret, Token: token})
	if err == nil {
		err = WriteFrame(conn, frame)
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("presenting to the automation port: %w", err)
	}
	c := &AutomationClient{conn: conn}
	answer, err := c.read()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	var ready AutomationReady
	if answer.Op != OpAutomationReady || Decode(answer, &ready) != nil {
		_ = conn.Close()
		return nil, errors.New("the automation port answered with something other than ready")
	}
	c.Version = ready.Version
	_ = conn.SetDeadline(time.Time{})
	return c, nil
}

// read returns the next frame, turning a Close into a CloseError and an ended stream into a plain sentence.
func (c *AutomationClient) read() (gatewayproto.Frame, error) {
	f, err := ReadFrame(c.conn, MaxDaemonFrame)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return f, errors.New("the automation port closed the connection")
		}
		return f, fmt.Errorf("reading from the automation port: %w", err)
	}
	if f.Op == OpClose {
		var cl Close
		if err := Decode(f, &cl); err != nil {
			return f, errors.New("the automation port closed the connection")
		}
		return f, &CloseError{Code: cl.Code, Reason: cl.Reason}
	}
	return f, nil
}

// Do performs one request and returns the daemon's answer: the instance's status and body, or the reason
// the daemon did not ask it. body is JSON, or nil for none.
func (c *AutomationClient) Do(ctx context.Context, method, path string, body json.RawMessage) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.next++
	id := strconv.Itoa(c.next)
	if body == nil {
		body = json.RawMessage("null")
	}
	frame, err := Encode(OpRequest, Request{ID: id, Method: method, Path: path, Body: body})
	if err != nil {
		return Response{}, err
	}

	// The context ends the wait by ending the read. The connection is not usable afterwards, which is the
	// honest state: an answer may still be on its way for a request nobody is waiting on.
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Now()) })
	defer stop()

	if err := WriteFrame(c.conn, frame); err != nil {
		return Response{}, fmt.Errorf("writing to the automation port: %w", err)
	}
	answer, err := c.read()
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, err
	}
	var resp Response
	if answer.Op != OpResponse || Decode(answer, &resp) != nil || resp.ID != id {
		return Response{}, errors.New("the automation port answered with something other than this request's response")
	}
	return resp, nil
}

// Close ends the connection.
func (c *AutomationClient) Close() error { return c.conn.Close() }
