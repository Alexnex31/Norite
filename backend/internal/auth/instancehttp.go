package auth

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
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
