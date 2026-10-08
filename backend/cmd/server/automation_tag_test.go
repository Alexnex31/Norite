// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTheInstanceTagsWhatATokenSendsAndNoRequestCanAsk is M22's tag over the real router.
//
// The service test hands the service an actor; this is the half that shows a real `nat_` credential
// arrives as the actor kind the tag keys on, and a real access token does not. The token goes straight to
// the instance, with no daemon and no automation port anywhere, because that is the route a mark added by
// the daemon would have left unmarked.
//
// The last leg is "no request sets it": `type` is not a field of the send body, so naming it is refused
// rather than obeyed or ignored.
func TestTheInstanceTagsWhatATokenSendsAndNoRequestCanAsk(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	channel := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "general", "type": 0}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, channel.Code, channel)
	messages := "/api/v1/channels/" + channel.field(t, "id") + "/messages"

	minted := f.api.call(http.MethodPost, "/api/v1/auth/tokens",
		map[string]any{"name": "bot", "scopes": []string{"messages.write"}}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, minted.Code, minted)
	bot := minted.field(t, "value")

	typeOf := func(res *response) float64 {
		t.Helper()
		var body struct {
			Type *float64 `json:"type"`
		}
		res.decode(&body)
		require.NotNil(t, body.Type, "the message carries no type: %s", res)
		return *body.Type
	}

	typed := f.api.call(http.MethodPost, messages, map[string]any{"content": "typed"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, typed.Code, typed)
	require.EqualValues(t, 0, typeOf(typed))

	scripted := f.api.call(http.MethodPost, messages, map[string]any{"content": "scripted"}, withToken(bot))
	require.Equal(t, http.StatusCreated, scripted.Code, scripted)
	require.EqualValues(t, 1, typeOf(scripted))

	// The same account's token editing the message the person typed.
	edited := f.api.call(http.MethodPatch, messages+"/"+typed.field(t, "id"),
		map[string]any{"content": "rewritten"}, withToken(bot))
	require.Equal(t, http.StatusOK, edited.Code, edited)
	require.EqualValues(t, 1, typeOf(edited))

	for _, asked := range []int{0, 1} {
		res := f.api.call(http.MethodPost, messages,
			map[string]any{"content": "asking", "type": asked}, withToken(bot))
		require.Equal(t, http.StatusBadRequest, res.Code, "a request naming type %d: %s", asked, res)
	}
}
