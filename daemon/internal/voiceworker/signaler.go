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

// Signaler is how the daemon reaches the voice server for a call: a TURN credential before the offer, then
// the offer for the answer (ADR 0023, M27's ordering). M33's is op 4 over the gateway; until then the one
// implementation is M26's harness (Harness), which a daemon reaches only when built with the
// norite_voiceharness tag.
type Signaler interface {
	// Route is what the worker gathers with: the TURN servers with the account's credential.
	Route(ctx context.Context, account string) (Route, error)
	// Join provisions the call for the account in channel with the worker's offer, and returns the answer.
	Join(ctx context.Context, account, channel, offer string) (string, error)
}

// Route is what a worker gathers with.
type Route struct {
	ICEServers []voiceipc.ICEServer
	// Loopback admits loopback candidates, for a voice server on this machine.
	Loopback bool
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

// Route asks the harness for a TURN credential.
func (h Harness) Route(ctx context.Context, account string) (Route, error) {
	var out struct {
		Username string   `json:"username"`
		Password string   `json:"password"`
		URLs     []string `json:"urls"`
	}
	if err := h.post(ctx, "/credentials", map[string]string{"user": account}, &out); err != nil {
		return Route{}, err
	}
	route := Route{Loopback: h.loopback()}
	if len(out.URLs) > 0 {
		if len(out.URLs) > voiceipc.MaxURLs {
			out.URLs = out.URLs[:voiceipc.MaxURLs]
		}
		route.ICEServers = []voiceipc.ICEServer{{URLs: out.URLs, Username: out.Username, Credential: out.Password}}
	}
	return route, nil
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

// Join provisions the call.
func (h Harness) Join(ctx context.Context, account, channel, offer string) (string, error) {
	var out struct {
		Conn   string `json:"conn"`
		Answer string `json:"answer"`
	}
	err := h.post(ctx, "/provision", map[string]string{"room": channel, "user": account, "offer": offer}, &out)
	if err != nil {
		return "", err
	}
	if out.Answer == "" {
		return "", errors.New("the voice server answered with no answer")
	}
	return out.Answer, nil
}

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
