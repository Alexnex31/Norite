// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package reports

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

// Handler serves the report endpoints.
type Handler struct {
	svc      *Service
	validate *validator.Validate
}

// NewHandler builds the validator once rather than per request.
func NewHandler(svc *Service) *Handler {
	validate := validator.New(validator.WithRequiredStructEnabled())

	// Report the wire name rather than the Go field name, as every other handler here does: a message
	// quoting `ReasonCategory` names something that appears in no contract.
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

// Routes mounts the report endpoints.
//
// # Two scopes, split by audience rather than by verb
//
// Every other pair in this codebase is read/write, and this one is not, deliberately. `reports.write`
// files; `reports.moderate` reads the queue and closes a report. The blast radii are what decide it — the
// same argument that split `guilds.audit` out of `guilds.read` at M14: a token minted so a bot can file a
// report on its owner's behalf must not also be able to dismiss every report in a guild its owner
// moderates. A read/write split would put exactly those two in the same bucket.
//
// Permission resolution still runs underneath (rule 1): a token holding reports.moderate can do what its
// owner could and no more, which is nothing at all in a guild where they lack PermManageMessages.
//
// # Where they are mounted
//
// Filing is top-level because the target vocabulary already spans objects with no guild — a whisper and a
// plain DM are M74's and will file through this same endpoint. Triage is under the guild because the guild
// *is* the authorization scope, and putting it in a query parameter would make the thing being authorized
// an argument rather than a path.
func (h *Handler) Routes(r chi.Router) {
	write := auth.RequireScope(auth.ScopeReportsWrite)
	moderate := auth.RequireScope(auth.ScopeReportsModerate)

	r.With(write).Post("/reports", h.file)

	r.Route("/guilds/{guild_id}/reports", func(r chi.Router) {
		r.With(moderate).Get("/", h.list)
		r.With(moderate).Get("/{report_id}", h.get)
		r.With(moderate).Post("/{report_id}/resolve", h.resolve)
	})
}

// fileRequest is what a reporter sends.
//
// **There is no `routed_to` field, and that is the enforcement rather than an omission.**
// httpx.DecodeAndValidate sets DisallowUnknownFields, so a client sending one gets a 400 instead of having
// it silently ignored — which is the stronger of the two and is worth knowing before somebody "helpfully"
// adds the field to make the error friendlier. M12 learned the same mechanism from the other direction,
// where a contract field reserved for a later milestone had to exist in the struct to avoid a hard 400.
type fileRequest struct {
	TargetType     string  `json:"target_type" validate:"required"`
	TargetID       string  `json:"target_id" validate:"required"`
	ReasonCategory string  `json:"reason_category" validate:"required"`
	Detail         *string `json:"detail" validate:"omitempty,max=2000"`
}

type resolveRequest struct {
	Status string `json:"status" validate:"required"`
}

func (h *Handler) file(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, httpx.ErrUnauthorized)
		return
	}

	var req fileRequest
	if !h.decode(w, r, &req) {
		return
	}

	targetID, err := requiredID(req.TargetID, "target_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	report, err := h.svc.File(r.Context(), actor, FileInput{
		TargetType:     req.TargetType,
		TargetID:       targetID,
		ReasonCategory: req.ReasonCategory,
		Detail:         req.Detail,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusCreated, report)
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

	entries, err := h.svc.List(r.Context(), actor, guildID, ListInput{
		Status: r.URL.Query().Get("status"),
		Before: before,
		Limit:  limit,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, entries)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
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
	reportID, err := pathID(r, "report_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	detail, err := h.svc.Get(r.Context(), actor, guildID, reportID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, detail)
}

func (h *Handler) resolve(w http.ResponseWriter, r *http.Request) {
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
	reportID, err := pathID(r, "report_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	var req resolveRequest
	if !h.decode(w, r, &req) {
		return
	}

	report, err := h.svc.Resolve(r.Context(), actor, guildID, reportID, req.Status)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, report)
}

// pathID parses a snowflake from the path, answering 404 rather than 400 on a malformed one.
//
// M12's decision and messages' copy of it: a 400 for an unparseable id against a 404 for a well-formed one
// that does not exist tells a caller which of their guesses were the right *shape*, which is half of
// knowing whether the object is there.
//
// The third copy of this helper in the backend, which is a deliberate stop rather than an oversight.
// Moving it to httpx would touch `guilds` and `messages` in a milestone that is about neither, and three
// short functions is the threshold this project puts ahead of an abstraction. A fourth is the point to
// extract one.
func pathID(r *http.Request, key string) (snowflake.ID, error) {
	id, err := snowflake.Parse(chi.URLParam(r, key))
	if err != nil || id == 0 {
		return 0, httpx.ErrNotFound
	}
	return id, nil
}

// queryID parses an optional snowflake cursor, carrying absence as nil the whole way to sqlc.narg.
func queryID(r *http.Request, key string) (*snowflake.ID, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return nil, nil
	}
	id, err := snowflake.Parse(raw)
	if err != nil || id == 0 {
		return nil, httpx.Errorf(httpx.ErrBadRequest, "%s must be a snowflake id", key)
	}
	return &id, nil
}

// requiredID parses an id sent in a body, where a bad value is a 400 rather than a 404.
//
// Different from pathID on purpose: a malformed id in a *path* is answered 404 so the shape of a guess
// discloses nothing, while a malformed id in a body the caller composed is an input error and saying so
// reveals nothing they did not just type.
func requiredID(raw, field string) (snowflake.ID, error) {
	id, err := snowflake.Parse(raw)
	if err != nil || id == 0 {
		return 0, httpx.Errorf(httpx.ErrBadRequest, "%s must be a snowflake id", field)
	}
	return id, nil
}
