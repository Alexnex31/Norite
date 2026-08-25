package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-playground/validator/v10"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// Handler serves the auth surface.
type Handler struct {
	svc      *Service
	validate *validator.Validate
}

// NewHandler builds the auth HTTP handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc, validate: validator.New(validator.WithRequiredStructEnabled())}
}

// Routes mounts the auth endpoints, and reports which of them require authentication.
//
// The split is explicit rather than a middleware exemption list: everything under the authenticated group
// is protected by construction, so adding a route there cannot accidentally be public and adding one
// outside it is visibly a public route.
func (h *Handler) Routes(r chi.Router) {
	// Public. These are the only unauthenticated mutating routes in the API, and each is its own
	// brute-force surface — the caller mounts them behind a stricter rate-limit bucket.
	r.Post("/register", h.register)
	r.Post("/login", h.login)
	r.Post("/refresh", h.refresh)
	r.Post("/logout", h.logout)
	r.Post("/password/reset/request", h.requestPasswordReset)
	r.Post("/password/reset", h.confirmPasswordReset)
	r.Post("/verify/request", h.requestEmailVerification)

	// Authenticated.
	r.Group(func(r chi.Router) {
		r.Use(RequireAuth)

		// All three require the person, not a delegated credential. Minting because a token that can
		// mint tokens can grant itself scopes it does not hold; listing because it discloses the
		// account's other tokens to whatever holds this one; revoking for symmetry with both.
		r.With(RequireUserActor).Post("/tokens", h.mintToken)
		r.With(RequireUserActor).Get("/tokens", h.listTokens)
		r.With(RequireUserActor).Delete("/tokens/{tokenId}", h.revokeToken)
	})
}

// UserRoutes mounts the account endpoints that M4 defines.
func (h *Handler) UserRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(RequireAuth)
		r.With(RequireScope(ScopeIdentify)).Get("/@me", h.currentUser)
	})
}

// ---------- request payloads ----------

type registerRequest struct {
	// Bounds only. What a username may *contain* is decided by auth.ValidUsername, after normalization —
	// see username.go. The tag's old `excludesall= ` excluded exactly one character and read as though it
	// were a charset rule.
	//
	// The numbers must match MinUsernameLength and MaxUsernameLength, which they did not: this said 64
	// while ValidUsername enforced 32, so a 40-character name passed the tag and came back rejected for
	// its *characters* — a message that was simply untrue of the input and left no way to fix it. "Bounds
	// only" is what let the two drift, since it reads as though the tag's numbers do not matter.
	Username    string `json:"username" validate:"required,min=2,max=32"`
	Email       string `json:"email" validate:"required,email,max=254"`
	Password    string `json:"password" validate:"required"`
	DisplayName string `json:"display_name" validate:"omitempty,max=64"`
	// InviteCode is required while the instance is gated and ignored while it is open, so it is optional
	// here and the service decides. Bounded generously rather than at the exact code length: what a code
	// may contain is ParseInviteCode's decision, and a tag that disagreed with it would refuse a valid
	// code for the wrong reason — the mistake registerRequest's username bounds already made once.
	InviteCode string `json:"invite_code" validate:"omitempty,max=64"`
}

type loginRequest struct {
	Email    string `json:"email" validate:"required,email,max=254"`
	Password string `json:"password" validate:"required"`
	// DeviceID scopes the refresh-token family. Required, because a login with no device identity would
	// have to share a family with every other such login and rotation would then log them all out.
	DeviceID   string `json:"device_id" validate:"required,max=128"`
	DeviceName string `json:"device_name" validate:"omitempty,max=64"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
}

type passwordResetRequest struct {
	Email string `json:"email" validate:"required,email,max=254"`
}

// emailVerificationRequest asks for a fresh verification link. Its own type rather than reusing
// passwordResetRequest: two endpoints that happen to take one identical field today are not one endpoint,
// and sharing the type would make either one's future field a silent addition to the other.
type emailVerificationRequest struct {
	Email string `json:"email" validate:"required,email,max=254"`
}

type passwordResetConfirmRequest struct {
	Token       string `json:"token" validate:"required"`
	NewPassword string `json:"new_password" validate:"required"`
}

type mintTokenRequest struct {
	Name   string   `json:"name" validate:"required,max=100"`
	Scopes []string `json:"scopes" validate:"required,min=1,dive,required"`
}

// ---------- response payloads ----------

// userResponse is the public shape of an account.
//
// Built field-by-field from the database row rather than by marshaling it: password_hash lives on that
// struct, and a response type that starts as a copy of a table row is exactly how a hash ends up on the
// wire the first time someone adds a column.
type userResponse struct {
	ID              snowflake.ID `json:"id"`
	Username        string       `json:"username"`
	Email           string       `json:"email"`
	DisplayName     string       `json:"display_name"`
	AvatarHash      *string      `json:"avatar_hash"`
	EmailVerifiedAt *time.Time   `json:"email_verified_at"`
	CreatedAt       time.Time    `json:"created_at"`
}

func newUserResponse(u db.User) userResponse {
	out := userResponse{
		ID:          snowflake.ID(u.ID),
		Username:    u.Username,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		AvatarHash:  u.AvatarHash,
	}
	if u.CreatedAt.Valid {
		out.CreatedAt = u.CreatedAt.Time
	}
	if u.EmailVerifiedAt.Valid {
		t := u.EmailVerifiedAt.Time
		out.EmailVerifiedAt = &t
	}
	return out
}

type tokenPairResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	// ExpiresAt is when the *access* token expires, not the refresh token. Clients refresh on this, and
	// naming it ambiguously would have them refresh 30 days late.
	ExpiresAt time.Time `json:"expires_at"`
}

// newTokenPairResponse converts explicitly rather than by struct conversion, which would silently depend on
// the two types keeping identical field order.
func newTokenPairResponse(p TokenPair) tokenPairResponse {
	return tokenPairResponse{
		AccessToken:  p.AccessToken,
		RefreshToken: p.RefreshToken,
		TokenType:    p.TokenType,
		ExpiresAt:    p.ExpiresAt,
	}
}

type apiTokenResponse struct {
	ID         snowflake.ID `json:"id"`
	Name       string       `json:"name"`
	Scopes     []string     `json:"scopes"`
	CreatedAt  time.Time    `json:"created_at"`
	LastUsedAt *time.Time   `json:"last_used_at"`
}

func newAPITokenResponse(t db.ApiToken) apiTokenResponse {
	out := apiTokenResponse{
		ID:     snowflake.ID(t.ID),
		Name:   t.Name,
		Scopes: t.Scopes,
	}
	if t.CreatedAt.Valid {
		out.CreatedAt = t.CreatedAt.Time
	}
	if t.LastUsedAt.Valid {
		u := t.LastUsedAt.Time
		out.LastUsedAt = &u
	}
	return out
}

// mintedTokenResponse carries the raw credential, which is returned exactly once.
type mintedTokenResponse struct {
	apiTokenResponse
	// Value is the only time this string exists outside the client. It is stored as a SHA-256 hash and
	// cannot be recovered, which the field's documentation in openapi.yaml states explicitly.
	Value string `json:"value"`
}

// ---------- handlers ----------

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !h.decode(w, r, &req) {
		return
	}

	// Field-by-field rather than a struct conversion, for the same reason as newTokenPairResponse: a
	// conversion compiles only while the two types keep identical fields in identical order, and would
	// start silently mis-assigning the moment either gained one.
	//
	//nolint:staticcheck // S1016: the coupling a conversion introduces is not wanted here
	_, err := h.svc.Register(r.Context(), RegisterInput{
		Username:    req.Username,
		Email:       req.Email,
		Password:    req.Password,
		DisplayName: req.DisplayName,
		InviteCode:  req.InviteCode,
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	// 202 with a fixed body, and deliberately not the account.
	//
	// Until M10 this answered 201 with the new user, which cannot survive the anti-enumeration rule: an
	// address that already has an account creates nothing, so there is no user to return, and any response
	// shaped around one would differ between the two cases. What the caller is told is that the address
	// will hear about it — which is true either way, and is the only thing this endpoint can honestly say
	// without disclosing whether the address was already registered.
	//
	// The account is not signed in either, unchanged from M4: registration and login are separate
	// operations with separate inputs, and login needs a device_id registration has no business requiring.
	httpx.WriteJSON(w, r, http.StatusAccepted, registrationAcceptedResponse{
		Message: "Check your email to finish creating your account.",
	})
}

// registrationAcceptedResponse is the one body POST /auth/register returns, identical in every case it
// does not reject outright.
type registrationAcceptedResponse struct {
	Message string `json:"message"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !h.decode(w, r, &req) {
		return
	}

	pair, err := h.svc.Login(r.Context(), LoginInput{
		Email:      req.Email,
		Password:   req.Password,
		DeviceID:   req.DeviceID,
		DeviceName: req.DeviceName,
		IP:         clientAddr(r),
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, newTokenPairResponse(pair))
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if !h.decode(w, r, &req) {
		return
	}

	pair, err := h.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, newTokenPairResponse(pair))
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var req logoutRequest
	if !h.decode(w, r, &req) {
		return
	}
	if err := h.svc.Logout(r.Context(), req.RefreshToken); err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusNoContent, nil)
}

// requestPasswordReset always answers 202, whether or not the address belongs to an account.
//
// 202 rather than 200 because it is literally accurate — the mail is queued, not sent — and because a
// status that says "accepted for processing" is the honest way to describe an endpoint that deliberately
// declines to tell you what it found.
func (h *Handler) requestPasswordReset(w http.ResponseWriter, r *http.Request) {
	var req passwordResetRequest
	if !h.decode(w, r, &req) {
		return
	}

	if err := h.svc.RequestPasswordReset(r.Context(), req.Email); err != nil {
		// ErrResetUnavailable is the one failure worth reporting: it does not depend on the address, so it
		// discloses nothing, and an operator with no relay configured has not chosen for reset to fail
		// silently. Everything else is a server fault and must not change the answer either way.
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusAccepted, nil)
}

// requestEmailVerification re-sends a verification link.
//
// Always 202, exactly as the reset request is and for the same reason: this endpoint has no business
// telling a caller whether an address is registered, or whether the account behind it is already verified.
// Both would be usable to enumerate, and the second would be usable to find accounts mid-signup.
func (h *Handler) requestEmailVerification(w http.ResponseWriter, r *http.Request) {
	var req emailVerificationRequest
	if !h.decode(w, r, &req) {
		return
	}

	if err := h.svc.RequestEmailVerification(r.Context(), req.Email); err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusAccepted, registrationAcceptedResponse{
		Message: "If that address needs verifying, a link is on its way.",
	})
}

func (h *Handler) confirmPasswordReset(w http.ResponseWriter, r *http.Request) {
	var req passwordResetConfirmRequest
	if !h.decode(w, r, &req) {
		return
	}

	if err := h.svc.ConfirmPasswordReset(r.Context(), req.Token, req.NewPassword); err != nil {
		h.writeErr(w, r, err)
		return
	}
	// 204: the caller now signs in with the new password, and there is nothing to hand back — every
	// credential the account had was just revoked.
	httpx.WriteJSON(w, r, http.StatusNoContent, nil)
}

func (h *Handler) currentUser(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())

	user, err := h.svc.GetUser(r.Context(), actor.UserID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, newUserResponse(user))
}

func (h *Handler) mintToken(w http.ResponseWriter, r *http.Request) {
	var req mintTokenRequest
	if !h.decode(w, r, &req) {
		return
	}
	actor, _ := ActorFrom(r.Context())

	scopes := make([]Scope, 0, len(req.Scopes))
	for _, s := range req.Scopes {
		scopes = append(scopes, Scope(s))
	}

	minted, err := h.svc.MintAPIToken(r.Context(), actor.UserID, MintAPITokenInput{Name: req.Name, Scopes: scopes})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	httpx.WriteJSON(w, r, http.StatusCreated, mintedTokenResponse{
		apiTokenResponse: newAPITokenResponse(minted.Token),
		Value:            minted.Raw,
	})
}

func (h *Handler) listTokens(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())

	tokens, err := h.svc.ListAPITokens(r.Context(), actor.UserID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	out := make([]apiTokenResponse, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, newAPITokenResponse(t))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())

	tokenID, err := snowflake.Parse(chi.URLParam(r, "tokenId"))
	if err != nil {
		// Same answer as a token that does not exist. A malformed ID, an unowned one and an absent one must
		// be indistinguishable, or the endpoint becomes a way to probe which IDs are real.
		//
		// Routed through writeErr rather than writing a 404 here, so all three answers are produced by one
		// line of code instead of two that have to be kept identical by hand — they had already drifted to
		// different messages once.
		h.writeErr(w, r, ErrNotFound)
		return
	}

	if err := h.svc.RevokeAPIToken(r.Context(), actor.UserID, tokenID); err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusNoContent, nil)
}

// ---------- plumbing ----------

// decode reads and validates a JSON request body, writing the error response itself on failure.
func (h *Handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := httpx.DecodeJSON(w, r, dst); err != nil {
		httpx.WriteError(w, r, err)
		return false
	}
	if err := h.validate.Struct(dst); err != nil {
		var verrs validator.ValidationErrors
		if errors.As(err, &verrs) && len(verrs) > 0 {
			fe := verrs[0]
			// Names the offending field and the rule it broke, and nothing else. A validation message must
			// never echo the submitted value back — that value is a password on two of these routes.
			httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest,
				"field %q failed the %q requirement", fe.Field(), fe.Tag()))
			return false
		}
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest, "invalid request body"))
		return false
	}
	return true
}

// writeErr maps a service error to its HTTP response.
//
// Credential failures are collapsed into one indistinguishable answer on purpose. "No such account",
// "wrong password" and "this account uses OAuth" are all 401 with the same message, so a login form cannot
// be used to discover which addresses are registered.
func (h *Handler) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrInvalidCredentials), errors.Is(err, ErrPasswordNotSet):
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrUnauthorized, "invalid email or password"))

	case errors.Is(err, ErrInvalidRefreshToken), errors.Is(err, ErrSessionReuse):
		// Reuse is deliberately reported the same as an ordinary invalid token. Telling the caller "that
		// token was already used" confirms to a thief that they hold something real and tells them the
		// legitimate client got there first.
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrUnauthorized, "invalid or expired refresh token"))

	case errors.Is(err, ErrEmailNotVerified):
		// 403 rather than 401: the credentials were right, so re-sending them changes nothing and a 401
		// would put a client into a retry loop. Its own code so a client can offer to resend the link.
		httpx.WriteError(w, r, &httpx.StatusError{
			Status:  http.StatusForbidden,
			Code:    "email_not_verified",
			Message: ErrEmailNotVerified.Error(),
			Err:     err,
		})

	case errors.Is(err, ErrInviteRequired):
		// `invite_required` rather than M4's `registration_closed`, which was accurate only while there
		// was no way to redeem anything: registration is not closed on a gated instance, it has a
		// precondition. A client that can tell the two apart can prompt for a code instead of giving up.
		httpx.WriteError(w, r, &httpx.StatusError{
			Status:  http.StatusForbidden,
			Code:    "invite_required",
			Message: ErrInviteRequired.Error(),
			Err:     err,
		})

	case errors.Is(err, ErrInviteInvalid):
		// Deliberately one code for unknown, exhausted and expired. Distinguishing them would let
		// somebody holding no valid code learn which codes exist by watching the message change.
		httpx.WriteError(w, r, &httpx.StatusError{
			Status:  http.StatusForbidden,
			Code:    "invite_invalid",
			Message: ErrInviteInvalid.Error(),
			Err:     err,
		})

	case errors.Is(err, ErrInviteExpiry), errors.Is(err, ErrInviteMaxUses):
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest, "%s", err.Error()))

	case errors.Is(err, ErrAlreadyBootstrapped):
		// 409 rather than 403: the credential was accepted and the request was well formed, but the
		// instance is in a state where this operation no longer applies. A 403 would read as "your
		// operator token is wrong" and send somebody hunting for a config problem that is not there.
		httpx.WriteError(w, r, &httpx.StatusError{
			Status:  http.StatusConflict,
			Code:    "already_bootstrapped",
			Message: ErrAlreadyBootstrapped.Error(),
			Err:     err,
		})

	// ErrEmailTaken no longer arrives from registration — that path answers 202 either way (M10) — but
	// bootstrap still reports it, where the caller is the operator setting the instance up and there is
	// nobody to enumerate.
	case errors.Is(err, ErrEmailTaken), errors.Is(err, ErrUsernameTaken):
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrConflict, "%s", err.Error()))

	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The client gave up — most often while queued for an argon2id slot, which is the gate working as
		// designed rather than a fault. Logging these at ERROR and answering 500 turned an ordinary login
		// burst into a stream of alarming lines about a server that was fine. Nobody is left to read the
		// response, so its only job is not to lie about whose problem this was.
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrUnavailable, "the server is busy; retry shortly"))

	case errors.Is(err, ErrPasswordTooShort), errors.Is(err, ErrPasswordTooLong),
		errors.Is(err, ErrUnknownScope), errors.Is(err, ErrInvalidUsername),
		errors.Is(err, ErrInvalidTokenName), errors.Is(err, ErrOAuthFlowChallenge),
		errors.Is(err, ErrOAuthClientRedirect), errors.Is(err, ErrDeviceContinuation),
		errors.Is(err, ErrOAuthSignupForDevice):
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrBadRequest, "%s", err.Error()))

	case errors.Is(err, ErrUnknownProvider):
		// 404, not 400: a provider this instance has not configured is indistinguishable from one that
		// does not exist, and neither is a malformed request.
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrNotFound, "no such sign-in provider"))

	case errors.Is(err, ErrOAuthExchangeCode), errors.Is(err, ErrOAuthSignupToken),
		errors.Is(err, ErrOAuthState):
		// One answer for unknown, expired and already-spent, exactly as for a reset token: distinguishing
		// them tells whoever holds a captured code which of those it is.
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrUnauthorized, "%s", err.Error()))

	case errors.Is(err, ErrOAuthEmailUnverified):
		// One code and one message whether or not an account owns the address: the pair of codes this
		// replaced was an account-existence oracle for anyone able to present an address unverified.
		httpx.WriteError(w, r, &httpx.StatusError{
			Status:  http.StatusForbidden,
			Code:    "oauth_email_unverified",
			Message: ErrOAuthEmailUnverified.Error(),
			Err:     err,
		})

	case errors.Is(err, ErrOAuthIdentityLinkedElsewhere), errors.Is(err, ErrOAuthAccountAlreadyLinked):
		// 409 rather than 403: nothing about this caller is unauthorized, and the request would be fine
		// against a different account on either side. It is a collision, which is what 409 is for.
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrConflict, "%s", err.Error()))

	case errors.Is(err, ErrOAuthRegistrationClosed):
		httpx.WriteError(w, r, &httpx.StatusError{
			Status:  http.StatusForbidden,
			Code:    "registration_closed",
			Message: "registration on this instance requires an invite code",
			Err:     err,
		})

	case errors.Is(err, ErrOAuthExchange), errors.Is(err, ErrOAuthNoEmail):
		// The provider failed us, not the other way round. 502 rather than 500 because the fault is
		// upstream, and saying so is what stops someone debugging this instance for an hour.
		httpx.WriteError(w, r, &httpx.StatusError{
			Status:  http.StatusBadGateway,
			Code:    "oauth_provider_error",
			Message: err.Error(),
			Err:     err,
		})

	case errors.Is(err, ErrInvalidResetToken):
		// One answer for expired, spent, unknown, and issued-to-a-since-changed-address. Distinguishing
		// them tells whoever holds a stolen link which of those it is.
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrUnauthorized, "invalid or expired password reset token"))

	case errors.Is(err, ErrDeviceFlowUnavailable):
		httpx.WriteError(w, r, &httpx.StatusError{
			Status:  http.StatusServiceUnavailable,
			Code:    "device_flow_unavailable",
			Message: "the device sign-in flow is unavailable: this instance has no public base URL configured",
			Err:     err,
		})

	case errors.Is(err, ErrResetUnavailable):
		httpx.WriteError(w, r, &httpx.StatusError{
			Status:  http.StatusServiceUnavailable,
			Code:    "reset_unavailable",
			Message: "password reset is unavailable: this instance has no email relay configured",
			Err:     err,
		})

	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, httpx.Errorf(httpx.ErrNotFound, "not found"))

	default:
		if isAuthFailure(err) {
			httpx.WriteError(w, r, httpx.Errorf(httpx.ErrUnauthorized, "invalid credentials"))
			return
		}
		// Genuinely unexpected: logged in full here, reported to the client as a bare 500 by WriteError.
		logging.FromContext(r.Context()).Error().Err(err).Msg("auth request failed")
		httpx.WriteError(w, r, err)
	}
}

// clientAddr returns the client's IP for the session record.
//
// Best-effort: it is diagnostic detail on a session listing, not an access-control input, so an
// unparseable RemoteAddr yields the zero Addr and a NULL column rather than an error.
func clientAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr
}
