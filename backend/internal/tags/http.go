// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tags

import (
	"net/http"
	"reflect"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-playground/validator/v10"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// Handler serves the tag endpoints.
type Handler struct {
	svc      *Service
	validate *validator.Validate
}

// NewHandler builds the validator once rather than per request.
func NewHandler(svc *Service) *Handler {
	validate := validator.New(validator.WithRequiredStructEnabled())

	// Report the wire name rather than the Go field name, as every other handler here does: a message
	// quoting `IsShared` names something that appears in no contract.
	validate.RegisterTagNameFunc(func(f reflect.StructField) string {
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "" || name == "-" {
			return f.Name
		}
		return name
	})

	return &Handler{svc: svc, validate: validate}
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	return httpx.DecodeAndValidate(w, r, h.validate, dst)
}

// Routes mounts the tag endpoints under two prefixes.
//
// The vocabulary is the guild's, so creating, listing and deleting a tag sit under `/guilds` — the guild
// is what is being authorized, and putting it in a query parameter would make the thing being checked an
// argument (M16's placement argument for the report queue).
//
// Applying sits under the *message*, because that is the object being changed and the permission is the
// one that reads the channel. A tag id in the path and a message in the route is the pairing the
// statement then has to agree with, which is what ApplyMessageTag's join predicate enforces.
//
// Both prefixes are already matched by `guildSurfaceRoutes` in the route-surface tests, so neither can
// hide from them — the gap M16's `POST /reports` fell through.
func (h *Handler) Routes(r chi.Router) {
	read := auth.RequireScope(auth.ScopeTagsRead)
	write := auth.RequireScope(auth.ScopeTagsWrite)

	r.Route("/guilds/{guild_id}/tags", func(r chi.Router) {
		r.With(read).Get("/", h.list)
		r.With(write).Post("/", h.create)
		r.With(write).Delete("/{tag_id}", h.delete)
	})

	r.Route("/channels/{channel_id}/messages/{message_id}/tags", func(r chi.Router) {
		r.With(read).Get("/", h.forMessage)
		r.With(write).Put("/{tag_id}", h.apply)
		r.With(write).Delete("/{tag_id}", h.unapply)
	})
}

type createRequest struct {
	Name string `json:"name" validate:"required,min=1,max=50"`
	// A plain bool rather than a pointer: absent means false, which is the safe default — a request that
	// forgets the field creates a *private* tag, needing no permission and visible to nobody else, rather
	// than quietly adding to the guild's shared vocabulary.
	IsShared bool `json:"is_shared"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
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

	var req createRequest
	if !h.decode(w, r, &req) {
		return
	}

	tag, err := h.svc.Create(r.Context(), actor, CreateInput{
		GuildID: guildID, Name: req.Name, IsShared: req.IsShared,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusCreated, tag)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
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

	list, err := h.svc.List(r.Context(), actor, guildID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, list)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
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
	tagID, err := pathID(r, "tag_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	if err := h.svc.Delete(r.Context(), actor, guildID, tagID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) apply(w http.ResponseWriter, r *http.Request) {
	actor, in, ok := h.applyTarget(w, r)
	if !ok {
		return
	}

	if err := h.svc.Apply(r.Context(), actor, in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// 204 rather than 201, and it is the idempotence showing: a repeat application is the same answer as
	// the first, so there is no "created" to report and nothing a caller could distinguish.
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) unapply(w http.ResponseWriter, r *http.Request) {
	actor, in, ok := h.applyTarget(w, r)
	if !ok {
		return
	}

	if err := h.svc.Unapply(r.Context(), actor, in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// applyTarget parses the three ids the apply pair share, writing the error itself and reporting whether
// to continue — the shape `decode` has, for the same reason: three repetitions of the same six lines is
// three chances to answer one of them with the wrong status.
func (h *Handler) applyTarget(w http.ResponseWriter, r *http.Request) (auth.Actor, ApplyInput, bool) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, httpx.ErrUnauthorized)
		return auth.Actor{}, ApplyInput{}, false
	}

	channelID, err := pathID(r, "channel_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return auth.Actor{}, ApplyInput{}, false
	}
	messageID, err := pathID(r, "message_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return auth.Actor{}, ApplyInput{}, false
	}
	tagID, err := pathID(r, "tag_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return auth.Actor{}, ApplyInput{}, false
	}

	return actor, ApplyInput{ChannelID: channelID, MessageID: messageID, TagID: tagID}, true
}

func (h *Handler) forMessage(w http.ResponseWriter, r *http.Request) {
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

	applied, err := h.svc.ForMessage(r.Context(), actor, channelID, messageID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, applied)
}

// pathID parses a snowflake from the path, answering 404 rather than 400 on a malformed one.
//
// M12's decision and it is anti-enumeration rather than tidiness: a 400 for an unparseable id and a 404
// for a well-formed one that does not exist tells a caller which of their guesses were the right *shape*,
// which is half of knowing whether the object is there.
func pathID(r *http.Request, key string) (snowflake.ID, error) {
	id, err := snowflake.Parse(chi.URLParam(r, key))
	if err != nil || id == 0 {
		return 0, httpx.ErrNotFound
	}
	return id, nil
}
