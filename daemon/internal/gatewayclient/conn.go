// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// One connection's life: HELLO, IDENTIFY or RESUME, then events until it ends.
//
// Nothing here logs a frame, at any level. IDENTIFY and RESUME carry the access token, a dispatch carries
// message content, and a log is the artifact that gets pasted into a bug report (rule 8). What is logged is
// the op, the event type, the sequence number and the close code.

// converse runs one connection to its end.
func (c *Client) converse(ctx context.Context, ws *websocket.Conn, cred session.Credential) (out outcome) {
	// Reads run on their own context, which stopping the daemon does not cancel. coder/websocket answers a
	// canceled read by tearing the connection down itself; a daemon stopping should instead say so with a
	// 1000, and then read until the server's close comes back.
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRead()
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close(websocket.StatusNormalClosure, "daemon stopping")
		case <-readCtx.Done():
		}
	}()

	// A panic is this connection's, not the daemon's: the session, and from M20 every attached client, go on.
	// The session is not resumed afterwards — resuming would replay the event that caused it.
	defer func() {
		if p := recover(); p != nil {
			c.log.Error().Interface("panic", p).Msg("the gateway connection panicked; identifying afresh")
			_ = ws.Close(websocket.StatusInternalError, "client error")
			out = outcome{ending: endIdentify}
		}
	}()

	hello, ok := c.readHello(readCtx, ws)
	if !ok {
		return outcome{ending: endResume}
	}

	compat := gatewayproto.Check(hello.Version, c.version)
	if !compat.Compatible {
		c.log.Error().Str("server_version", termsafe.Text(hello.Version)).Str("client_version", c.version).
			Str("reason", termsafe.Text(compat.Reason)).
			Msg("this daemon cannot talk to the instance's gateway; upgrade the side the reason names")
		_ = ws.Close(websocket.StatusNormalClosure, "incompatible version")
		return outcome{ending: endVersion}
	}
	if !compat.Checked {
		c.log.Warn().Str("server_version", termsafe.Text(hello.Version)).Str("client_version", c.version).
			Msg("gateway version check skipped: a development build is on one side")
	}

	// The token is asked for now, after HELLO's clock has been sampled, rather than when dialing. A laptop
	// that slept past its token's expiry has a stale estimate of the instance's clock until this sample,
	// and would otherwise IDENTIFY with a token the server considers expired (ADR 0010).
	token, again := c.tokenFor(ctx, cred.Generation)
	if again {
		_ = ws.Close(websocket.StatusNormalClosure, "signing in again")
		return outcome{ending: endAgain}
	}
	out.token = token

	var (
		seq    atomic.Int64
		acked  atomic.Bool
		missed atomic.Bool
	)
	seq.Store(c.seq)
	acked.Store(true)

	if err := c.open(ctx, ws, token); err != nil {
		return outcome{ending: endResume, token: token}
	}

	interval := time.Duration(hello.HeartbeatInterval) * time.Millisecond
	go c.heartbeat(ctx, ws, interval, &seq, &acked, &missed)

	for {
		_, data, err := ws.Read(readCtx)
		if err != nil {
			c.seq = seq.Load()
			if missed.Load() {
				c.log.Warn().Msg("the gateway stopped acknowledging heartbeats; reconnecting")
				out.ending = endResume
				return out
			}
			out.ending = c.closed(err)
			return out
		}

		var f gatewayproto.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			c.log.Error().Msg("the gateway sent a frame that is not JSON; reconnecting")
			_ = ws.Close(websocket.StatusUnsupportedData, "not a gateway frame")
			c.seq = seq.Load()
			out.ending = endResume
			return out
		}

		switch f.Op {
		case gatewayproto.OpDispatch:
			if f.S != nil {
				seq.Store(*f.S)
				c.seq = *f.S
			}
			eventType := ""
			if f.T != nil {
				eventType = *f.T
			}
			switch eventType {
			case "READY":
				var ready struct {
					SessionID string `json:"session_id"`
				}
				if err := json.Unmarshal(f.D, &ready); err == nil {
					c.sessionID = ready.SessionID
				}
				out.established = true
				c.log.Info().Int64("seq", seq.Load()).Msg("connected to the gateway")
			case "RESUMED":
				out.established = true
				c.log.Info().Int64("seq", seq.Load()).Msg("resumed the gateway session")
			default:
				c.log.Debug().Str("type", termsafe.Text(eventType)).Int64("seq", seq.Load()).Msg("gateway event")
			}
			c.sink.Dispatch(eventType, f.D)

		case gatewayproto.OpHeartbeatAck:
			acked.Store(true)

		case gatewayproto.OpReconnect:
			// The server is going away and will close with 1012. Closing first, and not with 1000, keeps the
			// session resumable from this side's point of view as well.
			c.log.Info().Msg("the gateway asked for a reconnect")
			_ = ws.Close(websocket.StatusCode(gatewayproto.CloseUnknownError), "reconnecting")
			c.seq = seq.Load()
			out.ending = endResume
			return out

		case gatewayproto.OpInvalidSess:
			// The RESUME could not be honored. IDENTIFY afresh on this connection, as the contract says, and
			// tell the sink first: the events between the old session and the new one are gone.
			c.log.Info().Msg("the gateway session could not be resumed; identifying afresh")
			c.sessionID, c.seq = "", 0
			seq.Store(0)
			if err := c.open(ctx, ws, token); err != nil {
				out.ending = endIdentify
				return out
			}

		default:
			c.log.Debug().Int("op", int(f.Op)).Msg("ignoring a gateway op this client does not know")
		}
	}
}

// readHello waits for the server's first frame and samples its clock.
func (c *Client) readHello(ctx context.Context, ws *websocket.Conn) (gatewayproto.Hello, bool) {
	helloCtx, cancel := context.WithTimeout(ctx, c.helloTimeout)
	defer cancel()

	_, data, err := ws.Read(helloCtx)
	if err != nil {
		c.log.Warn().Msg("the gateway did not say hello; reconnecting")
		return gatewayproto.Hello{}, false
	}
	// Sampled at receipt, before anything else is decoded or decided.
	received := time.Now()

	var f gatewayproto.Frame
	var hello gatewayproto.Hello
	if err := json.Unmarshal(data, &f); err != nil || f.Op != gatewayproto.OpHello ||
		json.Unmarshal(f.D, &hello) != nil || hello.HeartbeatInterval <= 0 {
		c.log.Error().Msg("the gateway's first frame was not a HELLO; reconnecting")
		_ = ws.Close(websocket.StatusProtocolError, "expected HELLO")
		return gatewayproto.Hello{}, false
	}
	// Dated from when the frame arrived, so the time spent decoding it does not count against the estimate.
	c.creds.ObserveServerTime(hello.ServerTime.Add(time.Since(received)))
	return hello, true
}

// tokenFor gets an access token for IDENTIFY or RESUME. again is true when the sign-in changed meanwhile —
// a login, a logout — and the connection should be started over for the new one.
func (c *Client) tokenFor(ctx context.Context, generation uint64) (token string, again bool) {
	tokenCtx, cancel := context.WithTimeout(ctx, c.helloTimeout)
	defer cancel()
	cred, err := c.creds.Current(tokenCtx)
	if err != nil || cred.Generation != generation {
		return "", true
	}
	return cred.AccessToken, false
}

// open sends RESUME when there is a session to resume, and IDENTIFY when there is not — telling the sink a
// fresh session is beginning before its READY can arrive.
func (c *Client) open(ctx context.Context, ws *websocket.Conn, token string) error {
	if c.sessionID != "" {
		return c.send(ctx, ws, gatewayproto.OpResume, gatewayproto.Resume{
			Token: token, SessionID: c.sessionID, Seq: c.seq,
		})
	}
	c.sink.Begin(c.generation)
	return c.send(ctx, ws, gatewayproto.OpIdentify, gatewayproto.Identify{
		Token: token,
		Properties: gatewayproto.IdentifyProperties{
			OS: runtime.GOOS, Client: "daemon", Version: c.version,
		},
	})
}

// heartbeat keeps the connection alive, and ends it when the server stops answering.
//
// The first beat comes after a random fraction of the interval, so daemons reconnecting together after a
// rollout do not heartbeat in step for the rest of their lives. A beat whose predecessor was never
// acknowledged means the connection is dead in a way TCP has not noticed yet: it is closed, and resumed.
func (c *Client) heartbeat(ctx context.Context, ws *websocket.Conn, interval time.Duration,
	seq *atomic.Int64, acked, missed *atomic.Bool,
) {
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(interval)) + 1)) //nolint:gosec // jitter
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !acked.Swap(false) {
			missed.Store(true)
			_ = ws.Close(websocket.StatusCode(gatewayproto.CloseUnknownError), errHeartbeatMissed.Error())
			return
		}
		var last *int64
		if n := seq.Load(); n > 0 {
			last = &n
		}
		if err := c.send(ctx, ws, gatewayproto.OpHeartbeat, last); err != nil {
			return
		}
		timer.Reset(interval)
	}
}

// send writes one client frame.
func (c *Client) send(ctx context.Context, ws *websocket.Conn, op gatewayproto.Opcode, payload any) error {
	d, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	frame, err := json.Marshal(gatewayproto.Frame{Op: op, D: d})
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageText, frame)
}

// closed reads how a connection ended and logs it.
func (c *Client) closed(err error) ending {
	code := websocket.CloseStatus(err)
	if code == -1 {
		// No close frame: a dropped network, a timeout, a process that died.
		c.log.Warn().Msg("the gateway connection dropped; reconnecting")
		return endResume
	}
	reason := ""
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		reason = ce.Reason
	}
	end := closeEnding(code)
	ev := c.log.Info()
	if end == endProtocol || end == endVersion {
		ev = c.log.Error()
	}
	// The reason is the server's text, and this log is read in a terminal (rule 19).
	ev.Int("code", int(code)).Str("reason", termsafe.Text(reason)).Msg("the gateway closed the connection")
	return end
}
