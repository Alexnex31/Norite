package main

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
)

// The invite endpoints over the real router, where the authority checks live.

type inviteBody struct {
	Code      string  `json:"code"`
	CreatedBy *string `json:"created_by"`
	MaxUses   *int32  `json:"max_uses"`
	Uses      int32   `json:"uses"`
}

// mintInvite creates one as the operator and returns it.
func mintInvite(t *testing.T, a *api, body map[string]any) inviteBody {
	t.Helper()
	resp := a.call(http.MethodPost, "/api/v1/instance/invites", body, withToken(operatorToken(t)))
	require.Equal(t, http.StatusCreated, resp.Code, resp)

	var out inviteBody
	resp.decode(&out)
	return out
}

// The milestone's other done-when: a gated instance admits somebody holding a code and nobody else.
func TestAGatedInstanceAdmitsAnInviteHolder(t *testing.T) {
	a := newAPI(t, auth.RegistrationInvite)

	refused := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "nobody", "email": "nobody@example.com", "password": testPassword,
	})
	require.Equal(t, http.StatusForbidden, refused.Code, refused)
	assert.Equal(t, "invite_required", refused.errorBody().Code)

	invite := mintInvite(t, a, map[string]any{"max_uses": 1})

	admitted := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
		"invite_code": invite.Code,
	})
	assert.Equal(t, http.StatusAccepted, admitted.Code, admitted)
}

// A one-use code is spent, and the second attempt is refused with the code that says so — distinct from
// "you need an invite", because a client that has one and finds it used needs different advice.
func TestASpentInviteIsRefusedAsInvalid(t *testing.T) {
	a := newAPI(t, auth.RegistrationInvite)
	invite := mintInvite(t, a, map[string]any{"max_uses": 1})

	first := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
		"invite_code": invite.Code,
	})
	require.Equal(t, http.StatusAccepted, first.Code, first)

	second := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "grace", "email": "grace@example.com", "password": testPassword,
		"invite_code": invite.Code,
	})
	assert.Equal(t, http.StatusForbidden, second.Code, second)
	assert.Equal(t, "invite_invalid", second.errorBody().Code)
}

// Unknown, exhausted and expired are one answer. Distinguishing them would let somebody holding no valid
// code learn which codes exist by watching the response change.
func TestAnUnknownInviteIsIndistinguishableFromASpentOne(t *testing.T) {
	a := newAPI(t, auth.RegistrationInvite)
	invite := mintInvite(t, a, map[string]any{"max_uses": 1})

	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
		"invite_code": invite.Code,
	}).Code)

	spent := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "b", "email": "b@example.com", "password": testPassword,
		"invite_code": invite.Code,
	})
	unknown := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "c", "email": "c@example.com", "password": testPassword,
		"invite_code": "BCDFGHJKMNPQRSTV",
	})

	assert.Equal(t, spent.Code, unknown.Code)
	assert.Equal(t, spent.errorBody().Code, unknown.errorBody().Code)
	assert.Equal(t, spent.errorBody().Message, unknown.errorBody().Message)
}

// Invite management is administration, so an ordinary account must not reach it. Without this, anybody who
// registered could mint themselves the means to invite others onto a private instance.
func TestAnOrdinaryAccountCannotManageInvites(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	mallory := a.newAccount("mallory", "mallory@example.com", "device-1")

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/instance/invites"},
		{http.MethodGet, "/api/v1/instance/invites"},
		{http.MethodDelete, "/api/v1/instance/invites/BCDFGHJKMNPQRSTV"},
	} {
		resp := a.call(tc.method, tc.path, nil, withToken(mallory.Tokens.AccessToken))
		assert.Equal(t, http.StatusForbidden, resp.Code, "%s %s: %s", tc.method, tc.path, resp)
	}
}

// And an instance administrator must. This is the other half of the check — a middleware that refused
// everybody would pass the test above while making the feature useless.
func TestAnInstanceAdministratorCanManageInvites(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	created := a.call(http.MethodPost, "/api/v1/instance/bootstrap", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
	}, withToken(operatorToken(t)))
	require.Equal(t, http.StatusCreated, created.Code, created)

	admin := a.login("ada@example.com", "device-1")

	resp := a.call(http.MethodPost, "/api/v1/instance/invites", map[string]any{"max_uses": 2},
		withToken(admin.AccessToken))
	require.Equal(t, http.StatusCreated, resp.Code, resp)

	var invite inviteBody
	resp.decode(&invite)
	require.NotNil(t, invite.CreatedBy, "an invite made by an administrator records who made it")
	assert.NotEmpty(t, *invite.CreatedBy)
}

// An invite made by the operator records nobody, because the operator is not an account. The doc's DDL has
// created_by NOT NULL, which cannot hold — see migration 000009.
func TestAnOperatorMadeInviteRecordsNoAccount(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	invite := mintInvite(t, a, map[string]any{})

	assert.Nil(t, invite.CreatedBy, "the operator has no account id to record")
	assert.Nil(t, invite.MaxUses, "an omitted max_uses means unlimited")
}

// Listing shows codes in full: the caller is an administrator or the operator, and an invite exists to be
// handed to somebody, so a list that cannot show its own contents is not a list.
func TestListingShowsTheCodesInFull(t *testing.T) {
	a := newAPI(t, auth.RegistrationInvite)
	invite := mintInvite(t, a, map[string]any{"max_uses": 3})

	resp := a.call(http.MethodGet, "/api/v1/instance/invites", nil, withToken(operatorToken(t)))
	require.Equal(t, http.StatusOK, resp.Code, resp)

	var list []inviteBody
	resp.decode(&list)
	require.Len(t, list, 1)
	assert.Equal(t, invite.Code, list[0].Code)
}

// Revocation, and that it distinguishes "done" from "there was nothing there" — the difference between a
// successful revocation and a mistyped code, which an administrator needs to be able to tell apart.
func TestRevokingAnInviteReportsWhetherThereWasOne(t *testing.T) {
	a := newAPI(t, auth.RegistrationInvite)
	invite := mintInvite(t, a, map[string]any{})

	gone := a.call(http.MethodDelete, "/api/v1/instance/invites/"+invite.Code, nil, withToken(operatorToken(t)))
	assert.Equal(t, http.StatusNoContent, gone.Code, gone)

	again := a.call(http.MethodDelete, "/api/v1/instance/invites/"+invite.Code, nil, withToken(operatorToken(t)))
	assert.Equal(t, http.StatusNotFound, again.Code, again)

	refused := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "toolate", "email": "toolate@example.com", "password": testPassword,
		"invite_code": invite.Code,
	})
	assert.Equal(t, http.StatusForbidden, refused.Code, refused)
}

// An open instance ignores a code rather than rejecting it, so a client that always sends one works
// against either mode without knowing which it is talking to.
func TestAnOpenInstanceAcceptsRegistrationWithAStrayCode(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	resp := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
		"invite_code": "BCDFGHJKMNPQRSTV",
	})
	assert.Equal(t, http.StatusAccepted, resp.Code, resp)
}
