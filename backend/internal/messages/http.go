// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package messages

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-playground/validator/v10"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// Handler serves the message endpoints.
type Handler struct {
	svc      *Service
	validate *validator.Validate
}

// NewHandler builds the validator once rather than per request.
func NewHandler(svc *Service) *Handler {
	validate := validator.New(validator.WithRequiredStructEnabled())

	// Report the wire name rather than the Go field name, as every other handler here does: a message
	// quoting `ReplyToID` names something that appears in no contract.
	validate.RegisterTagNameFunc(func(f reflect.StructField) string {
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "" || name == "-" {
			return f.Name
		}
		return name
	})

	return &Handler{svc: svc, validate: validate}
}

// decode reads and validates a request body, writing the error itself and reporting whether to continue.
//
// The same wrapper guilds carries, and the shape matters: httpx.DecodeAndValidate already enforces the
// body size cap, DisallowUnknownFields and the single-value body, and writes the 400 — so a handler that
// called WriteError on top would write two responses.
func (h *Handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	return httpx.DecodeAndValidate(w, r, h.validate, dst)
}

// Routes mounts the message endpoints under /channels/{channel_id}.
//
// Read and write are separate scopes for M12's reason, and write does not imply read: a bot that posts
// need not be able to read a channel's history, and holding one scope is not an argument for being handed
// the other.
func (h *Handler) Routes(r chi.Router) {
	read := auth.RequireScope(auth.ScopeMessagesRead)
	write := auth.RequireScope(auth.ScopeMessagesWrite)
	// M16a. A third scope rather than messages.read, because the permission layer already separates the
	// backlog from what was edited out of it — see auth.ScopeMessagesModerate.
	moderate := auth.RequireScope(auth.ScopeMessagesModerate)
	// M16b. A fourth, and the widest: messages.moderate reads one message's prior versions, this reads
	// every message in the guild. See auth.ScopeMessagesAudit.
	audit := auth.RequireScope(auth.ScopeMessagesAudit)

	r.Route("/channels/{channel_id}/messages", func(r chi.Router) {
		r.With(read).Get("/", h.list)
		r.With(write).Post("/", h.send)
		r.With(write).Patch("/{message_id}", h.update)
		r.With(write).Delete("/{message_id}", h.delete)
		r.With(moderate).Get("/{message_id}/history", h.history)
	})

	// Mounted under /guilds rather than /channels, because the guild *is* the authorization scope here —
	// the log spans every channel in it, and there is no channel in the path to resolve. Same placement
	// and same reasoning as `reports`' triage queue, which is the other guild-scoped surface a
	// non-`guilds` package serves.
	//
	// Note this is the second prefix this handler mounts under, which matters for the route-surface
	// tests: `guildSurfaceRoutes` matches both /guilds and /channels, so unlike M16's `POST /reports`
	// this route cannot hide from them.
	r.With(audit).Get("/guilds/{guild_id}/message-audit", h.guildMessageAudit)
}

type sendRequest struct {
	Content   string  `json:"content" validate:"required,min=1,max=4000"`
	ReplyToID *string `json:"reply_to_id" validate:"omitempty"`
}

type updateRequest struct {
	Content string `json:"content" validate:"required,min=1,max=4000"`
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, httpx.ErrUnauthorized)
		return
	}

	channelID, err := pathID(r, "channel_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	var req sendRequest
	if !h.decode(w, r, &req) {
		return
	}

	replyTo, err := optionalID(req.ReplyToID, "reply_to_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	msg, err := h.svc.Send(r.Context(), actor, SendInput{
		ChannelID: channelID, Content: req.Content, ReplyToID: replyTo,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusCreated, msg)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, httpx.ErrUnauthorized)
		return
	}

	channelID, err := pathID(r, "channel_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	before, err := queryID(r, "before")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	after, err := queryID(r, "after")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	var limit int32
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, convErr := strconv.ParseInt(raw, 10, 32)
		if convErr != nil || n < 1 || n > maxPageSize {
			httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest,
				"limit must be between 1 and %d", maxPageSize))
			return
		}
		limit = int32(n)
	}

	msgs, err := h.svc.List(r.Context(), actor, ListInput{
		ChannelID: channelID, Before: before, After: after, Limit: limit,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, msgs)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, httpx.ErrUnauthorized)
		return
	}

	channelID, err := pathID(r, "channel_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	messageID, err := pathID(r, "message_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	var req updateRequest
	if !h.decode(w, r, &req) {
		return
	}

	msg, err := h.svc.Update(r.Context(), actor, UpdateInput{
		ChannelID: channelID, MessageID: messageID, Content: req.Content,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, msg)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, httpx.ErrUnauthorized)
		return
	}

	channelID, err := pathID(r, "channel_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	messageID, err := pathID(r, "message_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	if err := h.svc.Delete(r.Context(), actor, channelID, messageID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, httpx.ErrUnauthorized)
		return
	}

	channelID, err := pathID(r, "channel_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	messageID, err := pathID(r, "message_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	before, err := queryID(r, "before")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	var limit int32
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, convErr := strconv.ParseInt(raw, 10, 32)
		if convErr != nil || n < 1 || n > maxPageSize {
			httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest,
				"limit must be between 1 and %d", maxPageSize))
			return
		}
		limit = int32(n)
	}

	history, err := h.svc.History(r.Context(), actor, HistoryInput{
		ChannelID: channelID, MessageID: messageID, Before: before, Limit: limit,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, history)
}

func (h *Handler) guildMessageAudit(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, httpx.ErrUnauthorized)
		return
	}

	guildID, err := pathID(r, "guild_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	before, err := queryID(r, "before")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	var limit int32
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, convErr := strconv.ParseInt(raw, 10, 32)
		if convErr != nil || n < 1 || n > maxPageSize {
			httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest,
				"limit must be between 1 and %d", maxPageSize))
			return
		}
		limit = int32(n)
	}

	entries, err := h.svc.GuildMessageAudit(r.Context(), actor, GuildAuditInput{
		GuildID: guildID, Before: before, Limit: limit,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, entries)
}

// pathID parses a snowflake from the path, answering 404 rather than 400 on a malformed one.
//
// M12's decision, and it is anti-enumeration rather than tidiness: a 400 for an unparseable id and a 404
// for a well-formed one that does not exist tells a caller which of their guesses were the right *shape*,
// which is half of knowing whether the object is there.
func pathID(r *http.Request, key string) (snowflake.ID, error) {
	id, err := snowflake.Parse(chi.URLParam(r, key))
	if err != nil || id == 0 {
		return 0, httpx.ErrNotFound
	}
	return id, nil
}

// queryID parses an optional snowflake cursor.
//
// Absence travels as nil the whole way to sqlc.narg rather than as a zero, because snowflake.Parse accepts
// "0" and a zero cursor would silently mean "no filter" — M14's correction on the audit log's cursor.
func queryID(r *http.Request, key string) (*snowflake.ID, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return nil, nil
	}
	return optionalID(&raw, key)
}

func optionalID(raw *string, key string) (*snowflake.ID, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	id, err := snowflake.Parse(*raw)
	if err != nil || id == 0 {
		return nil, httpx.Errorf(httpx.ErrBadRequest, "%s must be a snowflake id", key)
	}
	return &id, nil
}
