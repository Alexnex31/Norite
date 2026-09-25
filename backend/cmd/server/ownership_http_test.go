// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTransferringOwnershipNeedsAPersonWithALiveSession pins the two guards M13a puts on the route rather
// than in the service, where no service test can see them.
//
// A token holding every guild scope is still refused, because the refusal is about the kind of credential
// and not its reach: a delegated credential able to hand a guild's layer 2 to somebody can hand it to its
// attacker's account. And an access token whose session was signed out is refused although it has not
// expired, because §17.10's fifteen-minute window buys reading, not giving a guild away.
func TestTransferringOwnershipNeedsAPersonWithALiveSession(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	path := fmt.Sprintf("/api/v1/guilds/%s/owner", f.guildID)
	toMember := map[string]any{"user_id": f.memberID}

	ownerNow := func() string {
		t.Helper()
		res := f.api.call(http.MethodGet, "/api/v1/guilds/"+f.guildID, nil, withToken(f.memberToken))
		require.Equal(t, http.StatusOK, res.Code, res)
		return res.field(t, "owner_id")
	}

	minted := f.api.call(http.MethodPost, "/api/v1/auth/tokens", map[string]any{
		"name": "bot", "scopes": []string{"guilds.read", "guilds.write", "guilds.audit"},
	}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, minted.Code, minted)

	res := f.api.call(http.MethodPost, path, toMember, withToken(minted.field(t, "value")))
	require.Equal(t, http.StatusForbidden, res.Code, "an API token, whatever its scopes: %s", res)
	require.Equal(t, f.ownerID, ownerNow())

	res = f.api.call(http.MethodPost, path, map[string]any{"user_id": "not-an-id"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusBadRequest, res.Code, "an id in the body is input: %s", res)

	// Signed out from a second device: logout/all spares the device that calls it (M11), so the owner's
	// first session has to be ended from somewhere else — which is also the realistic case, somebody
	// signing a lost laptop out from their phone.
	phone := f.api.login("owner@example.com", "owner-phone")
	signedOut := f.api.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(phone.AccessToken))
	require.Equal(t, http.StatusOK, signedOut.Code, signedOut)

	res = f.api.call(http.MethodPost, path, toMember, withToken(f.ownerToken))
	require.Equal(t, http.StatusUnauthorized, res.Code,
		"the access token has not expired, and its session has been signed out: %s", res)
	require.Equal(t, f.ownerID, ownerNow(), "and nothing moved")
}

// TestTransferringOwnershipOverHTTP is the happy path through the real router: the response is the guild
// under its new owner, and the audit log's action filter accepts the new verb and finds one entry.
func TestTransferringOwnershipOverHTTP(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)

	res := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/owner", f.guildID),
		map[string]any{"user_id": f.memberID}, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, res.Code, res)
	require.Equal(t, f.memberID, res.field(t, "owner_id"))

	log := f.api.call(http.MethodGet,
		fmt.Sprintf("/api/v1/guilds/%s/audit-log?action=guild.owner_transfer", f.guildID), nil,
		withToken(f.memberToken))
	require.Equal(t, http.StatusOK, log.Code, "the new owner reads the log, and the filter knows the verb: %s", log)
	require.Contains(t, string(log.Body), `"guild.owner_transfer"`)
}
