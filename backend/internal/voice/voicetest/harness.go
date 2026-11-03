// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicetest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ProvisionRequest is what a client posts to cmd/voice-sfu: the harness's stand-in for M33's op 4.
type ProvisionRequest struct {
	Room  string `json:"room"`
	User  string `json:"user"`
	Offer string `json:"offer"`
}

// ProvisionResponse is the harness's answer: the connection it minted, and the SFU's answer to the offer.
type ProvisionResponse struct {
	Conn   string `json:"conn"`
	Answer string `json:"answer"`
}

// CredentialsRequest asks cmd/voice-sfu for a TURN credential, before the offer: the client gathers its
// relayed candidate with it, and the offer carries that candidate. M33 delivers the same thing, in the
// same order, over the gateway.
type CredentialsRequest struct {
	User string `json:"user"`
}

// CredentialsResponse is a credential for the account, and TURN's URLs, one per transport offered.
type CredentialsResponse struct {
	Username string   `json:"username"`
	Password string   `json:"password"`
	URLs     []string `json:"urls"`
}

// Provision posts an offer to a cmd/voice-sfu at base and returns its answer.
func Provision(ctx context.Context, base string, req ProvisionRequest) (ProvisionResponse, error) {
	var out ProvisionResponse
	// Not "return out, post(...)": the spec does not order reading out against the call that fills it.
	err := post(ctx, base+"/provision", req, &out)
	return out, err
}

// Credentials asks a cmd/voice-sfu at base for a TURN credential.
func Credentials(ctx context.Context, base string, req CredentialsRequest) (CredentialsResponse, error) {
	var out CredentialsResponse
	// Not "return out, post(...)": the spec does not order reading out against the call that fills it.
	err := post(ctx, base+"/credentials", req, &out)
	return out, err
}

func post(ctx context.Context, url string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(hr)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("voicetest: %s answered %d: %s", url, res.StatusCode, bytes.TrimSpace(raw))
	}
	return json.Unmarshal(raw, out)
}
