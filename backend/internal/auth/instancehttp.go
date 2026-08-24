package auth

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// The instance-administration HTTP surface, mounted at /instance.
//
// Separate from Routes for the reason operator.go's header gives: this group runs
// AuthenticateInstanceAdmin rather than Authenticate, so an operator token is never something the ordinary
// Bearer path can land on. Keeping the two in different files makes it visibly wrong to mount a handler
// from one group into the other.

// bootstrapRequest is the first administrator's account.
//
// The same bounds registerRequest carries, and they must stay the same: the two create rows in one table
// with one set of constraints, so a field one accepts and the other rejects is a difference with no
// meaning behind it.
type bootstrapRequest struct {
	Username    string `json:"username" validate:"required,min=2,max=32"`
	Email       string `json:"email" validate:"required,email,max=254"`
	Password    string `json:"password" validate:"required"`
	DisplayName string `json:"display_name" validate:"omitempty,max=64"`
}

// InstanceRoutes mounts the instance-administration endpoints.
//
// Every route here is behind AuthenticateInstanceAdmin, applied by the router at the group rather than
// per-route. There is no public endpoint under /instance and there is not going to be one — an instance's
// administration surface with an exemption in it is a contradiction.
func (h *Handler) InstanceRoutes(r chi.Router) {
	r.Post("/bootstrap", h.bootstrap)

	r.Post("/invites", h.createInvite)
	r.Get("/invites", h.listInvites)
	r.Delete("/invites/{code}", h.revokeInvite)
}

// bootstrap creates the instance's first administrator.
func (h *Handler) bootstrap(w http.ResponseWriter, r *http.Request) {
	var req bootstrapRequest
	if !h.decode(w, r, &req) {
		return
	}

	//nolint:staticcheck // S1016: see register's comment — a struct conversion couples the two shapes
	user, err := h.svc.Bootstrap(r.Context(), BootstrapInput{
		Username:    req.Username,
		Email:       req.Email,
		Password:    req.Password,
		DisplayName: req.DisplayName,
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	// The account, and no credential. Bootstrap does not sign anybody in: a session is scoped to a
	// device_id, and the operator's shell is not the device that will hold it — the same reasoning that
	// keeps Register from returning a token pair. `norite login` is the next step, and it is one the
	// operator was going to run anyway.
	httpx.WriteJSON(w, r, http.StatusCreated, newUserResponse(user))
}

// createInviteRequest describes an invite to mint. Both fields are optional and both mean "no limit" when
// omitted, which is the shape an operator opening an instance to a group chat wants.
type createInviteRequest struct {
	// MaxUses of zero, or absent, means unlimited.
	MaxUses int32 `json:"max_uses" validate:"omitempty,min=1,max=10000"`
	// ExpiresInSeconds of zero, or absent, means it never expires. Capped at a year: a longer-lived
	// invite is almost always a mistake, and an unlimited one is spelled by omitting the field rather
	// than by a number nobody meant to type.
	ExpiresInSeconds int64 `json:"expires_in_seconds" validate:"omitempty,min=60,max=31536000"`
}

// inviteResponse is one invite, as an administrator sees it.
type inviteResponse struct {
	// Code in full. The caller is an administrator or the operator, and an invite exists to be handed to
	// somebody — a list that cannot show its own contents is not a list. See migration 000009 for why
	// this value is stored in plaintext and why its blast radius makes that the right call.
	Code string `json:"code"`
	// CreatedBy is absent when the instance operator issued it, who is not an account.
	CreatedBy *string    `json:"created_by"`
	MaxUses   *int32     `json:"max_uses"`
	Uses      int32      `json:"uses"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
}

func newInviteResponse(in db.InstanceInvite) inviteResponse {
	out := inviteResponse{
		Code:      in.Code,
		MaxUses:   in.MaxUses,
		Uses:      in.Uses,
		CreatedAt: in.CreatedAt.Time,
	}
	if in.CreatedBy != nil {
		id := snowflake.ID(*in.CreatedBy).String()
		out.CreatedBy = &id
	}
	if in.ExpiresAt.Valid {
		t := in.ExpiresAt.Time
		out.ExpiresAt = &t
	}
	return out
}

// createInvite mints an invite code.
func (h *Handler) createInvite(w http.ResponseWriter, r *http.Request) {
	var req createInviteRequest
	if !h.decode(w, r, &req) {
		return
	}

	in := CreateInviteInput{
		MaxUses:   req.MaxUses,
		ExpiresIn: time.Duration(req.ExpiresInSeconds) * time.Second,
	}
	// Zero when the operator called, who is not an account. IsOperator is not consulted: an actor is
	// present for an administrator and absent for an operator, which is the same distinction without a
	// second thing to keep in step.
	if actor, ok := ActorFrom(r.Context()); ok {
		in.CreatedBy = actor.UserID
	}

	invite, err := h.svc.CreateInstanceInvite(r.Context(), in)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	// Logged because an invite is how somebody gets onto a gated instance, and "who opened the door" is
	// the question an operator asks afterwards. The code itself is never logged — it is a credential, and
	// rule 8 does not carve out an exception for the audit trail. The row records the same fact durably;
	// M72's instance_audit_log is where this becomes a queryable record rather than a log line.
	logging.FromContext(r.Context()).Info().
		Str("created_by", inviteActorForLog(r)).
		Bool("unlimited_uses", invite.MaxUses == nil).
		Msg("instance invite created")

	httpx.WriteJSON(w, r, http.StatusCreated, newInviteResponse(invite))
}

// listInvites returns every outstanding invite.
func (h *Handler) listInvites(w http.ResponseWriter, r *http.Request) {
	invites, err := h.svc.ListInstanceInvites(r.Context())
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	out := make([]inviteResponse, 0, len(invites))
	for _, invite := range invites {
		out = append(out, newInviteResponse(invite))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// revokeInvite deletes an invite so it can no longer be redeemed.
func (h *Handler) revokeInvite(w http.ResponseWriter, r *http.Request) {
	// From the path, which is the one place in this file a credential appears in a URL — and the reason
	// it is acceptable here where it was not for the device flow's poll: this code is not spent by the
	// request, the caller already holds administrator authority, and the alternative is a DELETE with a
	// body, which several proxies drop.
	if err := h.svc.DeleteInstanceInvite(r.Context(), chi.URLParam(r, "code")); err != nil {
		h.writeErr(w, r, err)
		return
	}

	logging.FromContext(r.Context()).Info().
		Str("revoked_by", inviteActorForLog(r)).
		Msg("instance invite revoked")

	w.WriteHeader(http.StatusNoContent)
}

// inviteActorForLog names who acted, without inventing an account id for the operator.
func inviteActorForLog(r *http.Request) string {
	if actor, ok := ActorFrom(r.Context()); ok {
		return actor.UserID.String()
	}
	return "operator"
}
