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

// Provision posts an offer to a cmd/voice-sfu at base and returns its answer.
func Provision(ctx context.Context, base string, req ProvisionRequest) (ProvisionResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return ProvisionResponse{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/provision", bytes.NewReader(body))
	if err != nil {
		return ProvisionResponse{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(hr)
	if err != nil {
		return ProvisionResponse{}, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return ProvisionResponse{}, err
	}
	if res.StatusCode != http.StatusOK {
		return ProvisionResponse{}, fmt.Errorf("voicetest: provisioning answered %d: %s", res.StatusCode, bytes.TrimSpace(raw))
	}
	var out ProvisionResponse
	return out, json.Unmarshal(raw, &out)
}
