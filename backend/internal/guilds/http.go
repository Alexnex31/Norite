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
	// Every route carries a scope, and the split between read and write is the point.
	//
	// A scope bounds a *delegated* credential below its owner's reach; a user actor passes every check by
	// design, so this changes nothing for a person at the CLI. What it changes is what an API token can
	// do — and M12 is the first milestone to put a mutating surface within reach of one. Shipped without
	// this, an `identify`-only token deleted a guild, which was reproduced before it was fixed.
	//
	// Permission resolution still runs underneath (rule 1). A token holding guilds.write can do exactly
	// what its owner could and no more.
	read := auth.RequireScope(auth.ScopeGuildsRead)
	write := auth.RequireScope(auth.ScopeGuildsWrite)

	r.With(write).Post("/guilds", h.createGuild)

	r.Route("/guilds/{guild_id}", func(r chi.Router) {
		r.With(read).Get("/", h.getGuild)
		r.With(write).Patch("/", h.updateGuild)
		r.With(write).Delete("/", h.deleteGuild)

		r.With(read).Get("/channels", h.listChannels)
		r.With(write).Post("/channels", h.createChannel)

		r.With(read).Get("/roles", h.listRoles)
		r.With(write).Post("/roles", h.createRole)
		r.With(write).Patch("/roles", h.reorderRoles)
		r.With(write).Patch("/roles/{role_id}", h.updateRole)
		r.With(write).Delete("/roles/{role_id}", h.deleteRole)

		r.With(read).Get("/members", h.listMembers)
		r.With(write).Patch("/members/{user_id}", h.updateMember)
		r.With(write).Delete("/members/{user_id}", h.removeMember)
		r.With(write).Put("/members/{user_id}/roles/{role_id}", h.assignRole)
		r.With(write).Delete("/members/{user_id}/roles/{role_id}", h.unassignRole)
	})

	// Mounted outside the guild group because these paths carry no guild. That is not a routing
	// convenience: it is why UpdateChannel loads the channel and reads its own guild_id rather than
	// trusting one from the caller (rule 1).
	r.Route("/channels/{channel_id}", func(r chi.Router) {
		r.With(write).Patch("/", h.updateChannel)
		r.With(write).Delete("/", h.deleteChannel)

		// The overwrite pair. Scoped like everything else — M12 shipped fifteen guild routes with no
		// scope at all and a review found an identify-only API token deleting a guild, so a new route
		// without one is the mistake this package has already made once. This group is where it is
		// easiest to make again: it is small, it sits outside the guild tree, and it does not look like
		// the guild surface.
		r.With(write).Put("/permissions/{overwrite_id}", h.setOverwrite)
		r.With(write).Delete("/permissions/{overwrite_id}", h.deleteOverwrite)
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
	return httpx.DecodeAndValidate(w, r, h.validate, dst)
}

type reorderRolesRequest struct {
	// An object wrapping the array rather than a bare array, so the request can carry validate tags and
	// so a later field has somewhere to go without changing the shape a client sends.
	//
	// A fixed 250, which is the *default* role ceiling and not the configured one — a validator tag is
	// evaluated at construction and cannot read config. So this is a transport-layer sanity cap on how
	// long a list may be, and the service's own bound on each position (config.MaxRolesPerGuild) is the
	// one that tracks the instance. They coincide on a default instance and diverge on one configured
	// higher, where a guild holding more than 250 roles has to reorder them in batches. Stated because
	// the two numbers look like the same number.
	Roles []rolePositionRequest `json:"roles" validate:"required,min=1,max=250,dive"`
}

type rolePositionRequest struct {
	ID snowflake.ID `json:"id" validate:"required"`
	// A pointer so an omitted position is a validation error rather than a silent zero — and zero is
	// exactly the value that would collide with @everyone.
	Position *int32 `json:"position" validate:"required"`
}

func (h *Handler) reorderRoles(w http.ResponseWriter, r *http.Request) {
	var req reorderRolesRequest
	if !h.decode(w, r, &req) {
		return
	}

	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}

	in := make([]RolePosition, 0, len(req.Roles))
	for _, want := range req.Roles {
		in = append(in, RolePosition{ID: want.ID, Position: *want.Position})
	}

	out, err := h.svc.ReorderRoles(r.Context(), actor, guildID, in)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func (h *Handler) assignRole(w http.ResponseWriter, r *http.Request) { h.changeMemberRole(w, r, true) }
func (h *Handler) unassignRole(w http.ResponseWriter, r *http.Request) {
	h.changeMemberRole(w, r, false)
}

// changeMemberRole serves both verbs. Neither carries a body: the path names everything the operation
// needs, and both are idempotent, so there is nothing for a body to add.
func (h *Handler) changeMemberRole(w http.ResponseWriter, r *http.Request, assigning bool) {
	actor, guildID, ok := h.actorAndID(w, r, "guild_id")
	if !ok {
		return
	}
	userID, ok := h.pathID(w, r, "user_id")
	if !ok {
		return
	}
	roleID, ok := h.pathID(w, r, "role_id")
	if !ok {
		return
	}

	change := h.svc.UnassignRole
	if assigning {
		change = h.svc.AssignRole
	}

	member, err := change(r.Context(), actor, guildID, userID, roleID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	// The member, with the roles they now hold. UpdateMember returns the same shape for the same reason:
	// a client refreshing its cache from this response is entitled to read `roles` as the whole truth,
	// and M12 shipped it returning nil until a review caught that.
	httpx.WriteJSON(w, r, http.StatusOK, member)
}

// --- permission overwrites ---

type setOverwriteRequest struct {
	// Type is the target kind: 0 role, 1 member. Required and validated against the two the resolver
	// understands, rather than stored and silently ignored.
	//
	// A pointer because 0 is a meaningful value — `required` on an int16 rejects the role case, which is
	// the common one.
	Type *int16 `json:"type" validate:"required,oneof=0 1"`
	// Allow and Deny arrive as quoted decimal strings, like every other permission field on this surface.
	Allow *roles.Permission `json:"allow"`
	Deny  *roles.Permission `json:"deny"`
}

func (h *Handler) setOverwrite(w http.ResponseWriter, r *http.Request) {
	var req setOverwriteRequest
	if !h.decode(w, r, &req) {
		return
	}

	actor, channelID, ok := h.actorAndID(w, r, "channel_id")
	if !ok {
		return
	}
	targetID, ok := h.pathID(w, r, "overwrite_id")
	if !ok {
		return
	}

	// Absent means "nothing", not "leave alone": a PUT replaces the row it names, so an omitted allow is
	// an empty allow. That is the whole difference between this verb and the PATCHes above it, and it is
	// why neither field carries a clear-flag the way a partial update would.
	var allow, deny roles.Permission
	if req.Allow != nil {
		allow = *req.Allow
	}
	if req.Deny != nil {
		deny = *req.Deny
	}

	out, err := h.svc.SetOverwrite(r.Context(), actor, SetOverwriteInput{
		ChannelID:  channelID,
		TargetType: *req.Type,
		TargetID:   targetID,
		Allow:      allow,
		Deny:       deny,
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, out)
}

type deleteOverwriteRequest struct {
	Type *int16 `json:"type" validate:"required,oneof=0 1"`
}

func (h *Handler) deleteOverwrite(w http.ResponseWriter, r *http.Request) {
	// A body on a DELETE, which is unusual and is the price of a polymorphic target: the path carries an
	// id that names either a role or a member, and nothing about the id says which. Guessing by looking
	// the id up in both tables would make the answer depend on which one happened to hold it.
	//
	// An earlier version of this comment justified the choice by saying a query parameter would land in
	// the request log. It would not: the logger records r.URL.Path and never RawQuery, deliberately, and
	// M9's reasoning was about the *path* rather than the query string. The design stands on the
	// polymorphism alone.
	var req deleteOverwriteRequest
	if !h.decode(w, r, &req) {
		return
	}

	actor, channelID, ok := h.actorAndID(w, r, "channel_id")
	if !ok {
		return
	}
	targetID, ok := h.pathID(w, r, "overwrite_id")
	if !ok {
		return
	}

	if err := h.svc.DeleteOverwrite(r.Context(), actor, channelID, *req.Type, targetID); err != nil {
		h.writeErr(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// writeErr maps a service error to its response.
//
// Most refusals arrive as httpx sentinels already — authorize returns them directly, so the anti-
// enumeration split between 404 and 403 is decided in one place and cannot be softened here.
func (h *Handler) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrOutranked):
		// 403 rather than 404. The caller is a member holding PermManageRoles who can already list the
		// guild's roles and members, so refusing them by name discloses nothing they cannot read — and
		// answering 404 for something they can see in their own client would be a bug rather than a
		// defense. The message names neither the target's position nor their own.
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrForbidden, "%s", messageOf(err)))

	case errors.Is(err, ErrDefaultRoleImmutable), errors.Is(err, ErrCannotRemoveOwner),
		errors.Is(err, ErrGuildFull), errors.Is(err, ErrChannelFull):
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
