// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// Signaler is how the daemon reaches the voice server for a call (ADR 0023): whether a call can be signaled
// now, then the worker's offer for the SFU's answer, which since M33 brings the TURN credential with it, and
// the call's end. The gateway's (Gateway) is op 4; M26's harness (Harness) is reached only by a daemon built
// with the norite_voiceharness tag.
type Signaler interface {
	// Prepare is asked before the worker is started: whether a call can be signaled for the account now,
	// and what the worker joins with. A refusal here starts no worker.
	Prepare(ctx context.Context, account string) (Route, error)
	// Join provisions the call for the account in channel with the worker's offer.
	Join(ctx context.Context, account, channel, offer string) (Answer, error)
	// Leave tells the voice server the call is over, once its worker is gone. Best effort: a voice server
	// that is not told ends the call when the connection goes quiet.
	Leave(ctx context.Context, account string)
}

// Route is what a worker joins with, before any answer.
type Route struct {
	// Loopback admits loopback candidates, for a voice server on this machine.
	Loopback bool
}

// Answer is what a provisioned call connects with.
type Answer struct {
	SDP        string
	ICEServers []voiceipc.ICEServer
	// Bitrate is the channel's, zero for the worker's default.
	Bitrate int
	Rights  voiceipc.Rights
	// Changes delivers the account's rights each time the voice server says they changed, the latest
	// only, until the call ends; nil for a voice server that says nothing of them.
	Changes <-chan voiceipc.Rights
	// Lost is closed if the instance drops the session the call was signaled on, which ends the call there;
	// nil for a voice server with no such session.
	Lost <-chan struct{}
}

// Harness signals through M26's harness (backend/internal/voice/voicetest/cmd/voice-sfu): POST
// /credentials, then POST /provision. Its shapes are written here because the harness is internal to the
// backend, and the end-to-end test runs this against the real harness, built from this checkout, which is
// what holds the two together. The room is the channel id and the user the account's.
type Harness struct {
	// URL is the harness's base, http://host:port.
	URL    string
	Client *http.Client
}

// harnessBodies bounds what the harness answers: a credential, or an answer under voiceproto.MaxOffer.
const harnessBodies = 64 << 10

// Prepare says whether the harness is on this machine; the harness has nothing to be connected to.
func (h Harness) Prepare(context.Context, string) (Route, error) {
	return Route{Loopback: h.loopback()}, nil
}

// loopback is a harness on this machine, whose SFU announces loopback addresses: its URL's host is a
// loopback address or exactly localhost. A test of the URL's text took http://localhost.example.com for one,
// which admits the loopback candidates of whatever that host answers.
func (h Harness) loopback() bool {
	u, err := url.Parse(h.URL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

// Join asks the harness for a TURN credential, then provisions the call. The credential comes with the
// answer, as op 4's does, so the worker is driven the same way whichever signaler it has.
func (h Harness) Join(ctx context.Context, account, channel, offer string) (Answer, error) {
	var cred struct {
		Username string   `json:"username"`
		Password string   `json:"password"`
		URLs     []string `json:"urls"`
	}
	if err := h.post(ctx, "/credentials", map[string]string{"user": account}, &cred); err != nil {
		return Answer{}, err
	}
	var out struct {
		Conn   string `json:"conn"`
		Answer string `json:"answer"`
	}
	err := h.post(ctx, "/provision", map[string]string{"room": channel, "user": account, "offer": offer}, &out)
	if err != nil {
		return Answer{}, err
	}
	if out.Answer == "" {
		return Answer{}, errors.New("the voice server answered with no answer")
	}
	a := Answer{SDP: out.Answer, ICEServers: []voiceipc.ICEServer{},
		Rights: voiceipc.Rights{Send: true, Hear: true}}
	if len(cred.URLs) > 0 {
		if len(cred.URLs) > voiceipc.MaxURLs {
			cred.URLs = cred.URLs[:voiceipc.MaxURLs]
		}
		a.ICEServers = []voiceipc.ICEServer{{URLs: cred.URLs, Username: cred.Username, Credential: cred.Password}}
	}
	return a, nil
}

// Leave tells the harness nothing: it has no leave, and the SFU ends the connection when its worker goes.
func (h Harness) Leave(context.Context, string) {}

func (h Harness) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(h.URL, "/")+path,
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("the voice server could not be reached: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, harnessBodies+1))
	if err != nil {
		return err
	}
	if len(raw) > harnessBodies {
		return errors.New("the voice server's answer is too large")
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the voice server refused: %d %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
}
