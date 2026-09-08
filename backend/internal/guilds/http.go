// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-playground/validator/v10"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// Handler serves the guild surface.
type Handler struct {
	svc      *Service
	validate *validator.Validate
}

// NewHandler builds the guild HTTP handler.
func NewHandler(svc *Service) *Handler {
	validate := validator.New(validator.WithRequiredStructEnabled())

	// Report the wire name, not the Go field name — the same registration auth.NewHandler makes, for the
	// same reason: a message quoting `DeviceID` names something that appears in no contract.
	validate.RegisterTagNameFunc(func(f reflect.StructField) string {
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "" || name == "-" {
			return f.Name
		}
		return name
	})

	return &Handler{svc: svc, validate: validate}
}

// Routes mounts the guild endpoints.
//
// Every route here is authenticated; the caller mounts this inside the authenticated group. Unlike auth's
// Routes there is no public half to keep separate, and a guild endpoint that did not require a caller
// would be a bug rather than a design.
func (h *Handler) Routes(r chi.Router) {
	r.Post("/guilds", h.createGuild)

	r.Route("/guilds/{guild_id}", func(r chi.Router) {
		r.Get("/", h.getGuild)
		r.Patch("/", h.updateGuild)
		r.Delete("/", h.deleteGuild)

		r.Get("/channels", h.listChannels)
		r.Post("/channels", h.createChannel)

		r.Get("/roles", h.listRoles)
		r.Post("/roles", h.createRole)
		r.Patch("/roles/{role_id}", h.updateRole)
		r.Delete("/roles/{role_id}", h.deleteRole)

		r.Get("/members", h.listMembers)
		r.Patch("/members/{user_id}", h.updateMember)
		r.Delete("/members/{user_id}", h.removeMember)
	})

	// Mounted outside the guild group because these paths carry no guild. That is not a routing
	// convenience: it is why UpdateChannel loads the channel and reads its own guild_id rather than
	// trusting one from the caller (rule 1).
	r.Route("/channels/{channel_id}", func(r chi.Router) {
		r.Patch("/", h.updateChannel)
		r.Delete("/", h.deleteChannel)
	})
}

// --- guilds ---

type createGuildRequest struct {
	Name        string  `json:"name" validate:"required,min=2,max=100"`
	Description *string `json:"description" validate:"omitempty,max=1024"`
}

func (h *Handler) createGuild(w http.ResponseWriter, r *http.Request) {
	var req createGuildRequest
	if !h.decode(w, r, &req) {
		return
	}

	actor, ok := h.actor(w, r)
	if !ok {
		return
	}

	// Field by field rather than a struct conversion, the same choice auth's handlers make: a
	// conversion compiles only while the wire type and the input type keep identical fields in
	// identical order, and starts silently mis-assigning the moment either gains one.
	//nolint:staticcheck // S1016: the coupling a conversion introduces is not wanted here
	guild, err := h.svc.Create(r.Context(), actor, CreateGuildInput{
		Name:        req.Name,
		Description: req.Description,
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusCreated, guild)
}

func (h *Handler) getGuild(w http.ResponseWriter, r *http.Request) {
	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}

	guild, err := h.svc.Get(r.Context(), actor, guildID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, guild)
}

type updateGuildRequest struct {
	Name        *string `json:"name" validate:"omitempty,min=2,max=100"`
	Description *string `json:"description" validate:"omitempty,max=1024"`
	// ClearDescription is how a caller removes a description. A null `description` cannot mean it: the
	// decoder cannot tell an explicit null from an absent field, and absent must mean "leave alone" or
	// every partial update would erase everything it did not mention.
	ClearDescription bool `json:"clear_description"`
}

func (h *Handler) updateGuild(w http.ResponseWriter, r *http.Request) {
	var req updateGuildRequest
	if !h.decode(w, r, &req) {
		return
	}

	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}

	// Field by field rather than a struct conversion, the same choice auth's handlers make: a
	// conversion compiles only while the wire type and the input type keep identical fields in
	// identical order, and starts silently mis-assigning the moment either gains one.
	//nolint:staticcheck // S1016: the coupling a conversion introduces is not wanted here
	guild, err := h.svc.Update(r.Context(), actor, guildID, UpdateGuildInput{
		Name:             req.Name,
		Description:      req.Description,
		ClearDescription: req.ClearDescription,
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, guild)
}

func (h *Handler) deleteGuild(w http.ResponseWriter, r *http.Request) {
	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}

	if err := h.svc.Delete(r.Context(), actor, guildID); err != nil {
		h.writeErr(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- channels ---

func (h *Handler) listChannels(w http.ResponseWriter, r *http.Request) {
	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}

	channels, err := h.svc.ListChannels(r.Context(), actor, guildID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, channels)
}

type createChannelRequest struct {
	Name      string  `json:"name" validate:"required,min=1,max=100"`
	Type      int16   `json:"type"`
	Topic     *string `json:"topic" validate:"omitempty,max=1024"`
	ParentID  *string `json:"parent_id"`
	Position  int32   `json:"position"`
	NSFW      bool    `json:"nsfw"`
	Bitrate   *int32  `json:"bitrate" validate:"omitempty,min=8000,max=384000"`
	UserLimit *int32  `json:"user_limit" validate:"omitempty,min=0,max=99"`
}

func (h *Handler) createChannel(w http.ResponseWriter, r *http.Request) {
	var req createChannelRequest
	if !h.decode(w, r, &req) {
		return
	}

	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}

	var parentID *snowflake.ID
	if req.ParentID != nil {
		id, err := snowflake.Parse(*req.ParentID)
		if err != nil {
			httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest, "parent_id is not a valid id"))
			return
		}
		parentID = &id
	}

	channel, err := h.svc.CreateChannel(r.Context(), actor, guildID, CreateChannelInput{
		Name:      req.Name,
		Type:      req.Type,
		Topic:     req.Topic,
		ParentID:  parentID,
		Position:  req.Position,
		NSFW:      req.NSFW,
		Bitrate:   req.Bitrate,
		UserLimit: req.UserLimit,
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusCreated, channel)
}

type updateChannelRequest struct {
	Name       *string `json:"name" validate:"omitempty,min=1,max=100"`
	Topic      *string `json:"topic" validate:"omitempty,max=1024"`
	ClearTopic bool    `json:"clear_topic"`
	Position   *int32  `json:"position"`
	NSFW       *bool   `json:"nsfw"`
	Bitrate    *int32  `json:"bitrate" validate:"omitempty,min=8000,max=384000"`
	UserLimit  *int32  `json:"user_limit" validate:"omitempty,min=0,max=99"`
}

func (h *Handler) updateChannel(w http.ResponseWriter, r *http.Request) {
	var req updateChannelRequest
	if !h.decode(w, r, &req) {
		return
	}

	actor, channelID, ok := h.actorAndID(w, r, "channel_id")
	if !ok {
		return
	}

	// Field by field rather than a struct conversion, the same choice auth's handlers make: a
	// conversion compiles only while the wire type and the input type keep identical fields in
	// identical order, and starts silently mis-assigning the moment either gains one.
	//nolint:staticcheck // S1016: the coupling a conversion introduces is not wanted here
	channel, err := h.svc.UpdateChannel(r.Context(), actor, channelID, UpdateChannelInput{
		Name:       req.Name,
		Topic:      req.Topic,
		ClearTopic: req.ClearTopic,
		Position:   req.Position,
		NSFW:       req.NSFW,
		Bitrate:    req.Bitrate,
		UserLimit:  req.UserLimit,
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, channel)
}

func (h *Handler) deleteChannel(w http.ResponseWriter, r *http.Request) {
	actor, channelID, ok := h.actorAndID(w, r, "channel_id")
	if !ok {
		return
	}

	if err := h.svc.DeleteChannel(r.Context(), actor, channelID); err != nil {
		h.writeErr(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- roles ---

func (h *Handler) listRoles(w http.ResponseWriter, r *http.Request) {
	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}

	list, err := h.svc.ListRoles(r.Context(), actor, guildID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, list)
}

type createRoleRequest struct {
	Name        string            `json:"name" validate:"required,min=1,max=100"`
	Color       int32             `json:"color" validate:"min=0,max=16777215"`
	Permissions *roles.Permission `json:"permissions"`
	Hoist       bool              `json:"hoist"`
	Mentionable bool              `json:"mentionable"`
}

func (h *Handler) createRole(w http.ResponseWriter, r *http.Request) {
	var req createRoleRequest
	if !h.decode(w, r, &req) {
		return
	}

	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}

	var permissions roles.Permission
	if req.Permissions != nil {
		permissions = *req.Permissions
	}

	role, err := h.svc.CreateRole(r.Context(), actor, guildID, CreateRoleInput{
		Name:        req.Name,
		Color:       req.Color,
		Permissions: permissions,
		Hoist:       req.Hoist,
		Mentionable: req.Mentionable,
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusCreated, role)
}

type updateRoleRequest struct {
	Name        *string           `json:"name" validate:"omitempty,min=1,max=100"`
	Color       *int32            `json:"color" validate:"omitempty,min=0,max=16777215"`
	Permissions *roles.Permission `json:"permissions"`
	Hoist       *bool             `json:"hoist"`
	Mentionable *bool             `json:"mentionable"`
}

func (h *Handler) updateRole(w http.ResponseWriter, r *http.Request) {
	var req updateRoleRequest
	if !h.decode(w, r, &req) {
		return
	}

	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}
	roleID, ok := h.pathID(w, r, "role_id")
	if !ok {
		return
	}

	// Field by field rather than a struct conversion, the same choice auth's handlers make: a
	// conversion compiles only while the wire type and the input type keep identical fields in
	// identical order, and starts silently mis-assigning the moment either gains one.
	//nolint:staticcheck // S1016: the coupling a conversion introduces is not wanted here
	role, err := h.svc.UpdateRole(r.Context(), actor, guildID, roleID, UpdateRoleInput{
		Name:        req.Name,
		Color:       req.Color,
		Permissions: req.Permissions,
		Hoist:       req.Hoist,
		Mentionable: req.Mentionable,
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, role)
}

func (h *Handler) deleteRole(w http.ResponseWriter, r *http.Request) {
	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}
	roleID, ok := h.pathID(w, r, "role_id")
	if !ok {
		return
	}

	if err := h.svc.DeleteRole(r.Context(), actor, guildID, roleID); err != nil {
		h.writeErr(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- members ---

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}

	var in ListMembersInput

	if raw := r.URL.Query().Get("after"); raw != "" {
		after, err := snowflake.Parse(raw)
		if err != nil {
			httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest, "after is not a valid id"))
			return
		}
		in.After = after
	}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || limit < 1 {
			httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest, "limit must be a positive integer"))
			return
		}
		in.Limit = int32(limit)
	}

	members, err := h.svc.ListMembers(r.Context(), actor, guildID, in)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, members)
}

type updateMemberRequest struct {
	Nickname      *string `json:"nickname" validate:"omitempty,min=1,max=32"`
	ClearNickname bool    `json:"clear_nickname"`
	Deaf          *bool   `json:"deaf"`
	Mute          *bool   `json:"mute"`
}

func (h *Handler) updateMember(w http.ResponseWriter, r *http.Request) {
	var req updateMemberRequest
	if !h.decode(w, r, &req) {
		return
	}

	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}
	userID, ok := h.pathID(w, r, "user_id")
	if !ok {
		return
	}

	// Field by field rather than a struct conversion, the same choice auth's handlers make: a
	// conversion compiles only while the wire type and the input type keep identical fields in
	// identical order, and starts silently mis-assigning the moment either gains one.
	//nolint:staticcheck // S1016: the coupling a conversion introduces is not wanted here
	member, err := h.svc.UpdateMember(r.Context(), actor, guildID, userID, UpdateMemberInput{
		Nickname:      req.Nickname,
		ClearNickname: req.ClearNickname,
		Deaf:          req.Deaf,
		Mute:          req.Mute,
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, member)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}
	userID, ok := h.pathID(w, r, "user_id")
	if !ok {
		return
	}

	if err := h.svc.RemoveMember(r.Context(), actor, guildID, userID); err != nil {
		h.writeErr(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- shared ---

// actor pulls the authenticated identity the middleware resolved.
func (h *Handler) actor(w http.ResponseWriter, r *http.Request) (auth.Actor, bool) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		// Unreachable behind the authenticated group, and a 401 rather than a 500 if the group is ever
		// mis-wired: a route that lost its middleware must refuse, not serve.
		httpx.WriteError(w, r, httpx.ErrUnauthorized)
		return auth.Actor{}, false
	}
	return actor, true
}

// pathID parses a snowflake out of a URL parameter.
//
// A malformed id answers 404, never 400. It is the same answer an id that parses but names nothing gets,
// which is what stops the pair distinguishing "no such guild" from "not yours" — the oracle authorize
// exists to close would otherwise reopen through the parser.
func (h *Handler) pathID(w http.ResponseWriter, r *http.Request, name string) (snowflake.ID, bool) {
	id, err := snowflake.Parse(chi.URLParam(r, name))
	if err != nil {
		httpx.WriteError(w, r, httpx.ErrNotFound)
		return 0, false
	}
	return id, true
}

func (h *Handler) actorAndID(
	w http.ResponseWriter, r *http.Request, name string,
) (auth.Actor, snowflake.ID, bool) {
	actor, ok := h.actor(w, r)
	if !ok {
		return auth.Actor{}, 0, false
	}
	id, ok := h.pathID(w, r, name)
	if !ok {
		return auth.Actor{}, 0, false
	}
	return actor, id, true
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := httpx.DecodeJSON(w, r, dst); err != nil {
		httpx.WriteError(w, r, err)
		return false
	}
	if err := h.validate.Struct(dst); err != nil {
		var verrs validator.ValidationErrors
		if errors.As(err, &verrs) && len(verrs) > 0 {
			fe := verrs[0]
			httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest,
				"field %q failed the %q requirement", fe.Field(), fe.Tag()))
			return false
		}
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest, "invalid request body"))
		return false
	}
	return true
}

// writeErr maps a service error to its response.
//
// Most refusals arrive as httpx sentinels already — authorize returns them directly, so the anti-
// enumeration split between 404 and 403 is decided in one place and cannot be softened here.
func (h *Handler) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrDefaultRoleImmutable), errors.Is(err, ErrCannotRemoveOwner):
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrConflict, "%s", messageOf(err)))

	case errors.Is(err, ErrUnsupportedChannelType), errors.Is(err, ErrAlreadyAMember):
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest, "%s", messageOf(err)))

	default:
		httpx.WriteError(w, r, err)
	}
}

// messageOf prefers the contextual message a StatusError carries over the bare sentinel's text.
func messageOf(err error) string {
	var se *httpx.StatusError
	if errors.As(err, &se) && se.Error() != "" {
		return se.Error()
	}
	return err.Error()
}
