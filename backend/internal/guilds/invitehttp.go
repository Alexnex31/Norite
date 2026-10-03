// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
)

// InviteRoutes mounts the three routes that take an invite code: preview, redeem and revoke (M20a).
//
// Separate from Routes so the caller can put them behind their own rate-limit bucket. Each takes a code,
// so each is somewhere a guesser would aim, and the base bucket is sized for ordinary use rather than for
// that. **The code is in the body of all three, never in the path** — the request logger writes every
// path, and a code there is a capability in a log (ADR 0029).
//
// Creating and listing take no code and are mounted with the rest of the guild and channel routes.
func (h *Handler) InviteRoutes(r chi.Router) {
	read := auth.RequireScope(auth.ScopeGuildsRead)
	write := auth.RequireScope(auth.ScopeGuildsWrite)

	// A POST that writes nothing, because a GET would put the code in the query string, which this
	// server's logger omits and a reverse proxy's does not. Rule 4 forbids writing from a GET; it does not
	// require reading through one.
	r.With(read).Post("/invites/preview", h.previewInvite)
	r.With(write).Post("/invites/redeem", h.redeemInvite)
	r.With(write).Post("/invites/revoke", h.revokeInvite)
}

// createInviteRequest asks for an invite into the route's channel.
//
// Both fields optional, and leaving either out means no limit, exactly as M10's instance-invite request
// does: two invite APIs on one instance spelling "for ever" differently would be one more thing to get
// wrong. A client wanting a safer default sends it; `norite invite create` sends a week.
type createInviteRequest struct {
	MaxUses          *int32 `json:"max_uses" validate:"omitempty,min=1,max=1000"`
	ExpiresInSeconds *int64 `json:"expires_in_seconds" validate:"omitempty,min=60,max=2592000"`
}

func (h *Handler) createInvite(w http.ResponseWriter, r *http.Request) {
	var req createInviteRequest
	if !h.decode(w, r, &req) {
		return
	}
	actor, channelID, ok := h.actorAndID(w, r, "channel_id")
	if !ok {
		return
	}

	in := CreateInviteInput{ChannelID: channelID}
	if req.MaxUses != nil {
		in.MaxUses = *req.MaxUses
	}
	if req.ExpiresInSeconds != nil {
		in.MaxAge = time.Duration(*req.ExpiresInSeconds) * time.Second
	}

	invite, err := h.svc.CreateInvite(r.Context(), actor, in)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, invite)
}

func (h *Handler) listInvites(w http.ResponseWriter, r *http.Request) {
	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}
	invites, err := h.svc.ListInvites(r.Context(), actor, guildID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, invites)
}

// inviteCodeRequest carries a code in the body.
//
// No validator: an empty code or an oversized one is a malformed code, and the contract promises one 404
// for every malformed, unknown or dead code. A validator answered those two with 400 (/code-review). The
// parser refuses anything it could not have issued, and stops reading as soon as the code is too long; the
// body's own size is bounded by the decoder.
type inviteCodeRequest struct {
	Code string `json:"code"`
}

func (h *Handler) previewInvite(w http.ResponseWriter, r *http.Request) {
	var req inviteCodeRequest
	if !h.decode(w, r, &req) {
		return
	}
	if _, ok := h.actor(w, r); !ok {
		return
	}
	preview, err := h.svc.PreviewInvite(r.Context(), req.Code)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, preview)
}

// redeemInvite answers 200 with the guild whether the caller has just joined or already was a member: a
// second click on a link is not an error, and a client branching on the difference has no use for it.
func (h *Handler) redeemInvite(w http.ResponseWriter, r *http.Request) {
	var req inviteCodeRequest
	if !h.decode(w, r, &req) {
		return
	}
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	guild, err := h.svc.RedeemInvite(r.Context(), actor, req.Code)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, guild)
}

func (h *Handler) revokeInvite(w http.ResponseWriter, r *http.Request) {
	var req inviteCodeRequest
	if !h.decode(w, r, &req) {
		return
	}
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	if err := h.svc.RevokeInvite(r.Context(), actor, req.Code); err != nil {
		h.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
