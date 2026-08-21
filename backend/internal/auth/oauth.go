package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"golang.org/x/oauth2/google"
)

// OAuth provider plumbing: turning "sign in with Google" into a verified identity this package can act on.
//
// # What a provider is trusted for
//
// Exactly one thing: that the person completing the flow controls the account named by ProviderUserID.
// Everything else it reports is a claim. The email in particular is only as good as the provider's own
// verification, which is why EmailVerified is carried separately and why the linking rule turns on it —
// an unverified address at a lax provider is a claim by a stranger, not evidence.
//
// # Why PKCE, for a confidential client
//
// This backend holds a client secret, so an authorization code cannot be redeemed without it and PKCE is
// defense in depth rather than the load-bearing control it is for a public client. It is still worth the
// row it costs: it closes code interception at the redirect, which matters here because the redirect leg
// travels through a browser this server does not control, and later through a loopback listener on the
// user's own machine (Milestone M8) where other local processes can see it.

// oauthProviderPath is where a provider's endpoints are mounted, relative to the instance root.
//
// One definition rather than a string literal at each site: the callback path is registered with Google
// and GitHub, so it is not something to reconstruct by hand in a second place and get subtly wrong.
func oauthProviderPath(provider string) string { return "/api/v1/auth/oauth/" + provider }

// oauthAuthorizePath is where a browser is sent to start a sign-in. Same-origin and relative, because the
// only page that links to it is served by this instance.
func oauthAuthorizePath(provider string) string { return oauthProviderPath(provider) + "/authorize" }

// OAuthProviderName identifies a provider in URLs and in the database.
type OAuthProviderName string

const (
	ProviderGoogle OAuthProviderName = "google"
	ProviderGitHub OAuthProviderName = "github"
)

// ValidOAuthProvider reports whether s names a provider this build understands.
//
// Checked before anything else touches a provider path parameter: the value goes into a database column
// and into an error message, and "whatever the client sent" is not a provider.
func ValidOAuthProvider(s string) bool {
	switch OAuthProviderName(s) {
	case ProviderGoogle, ProviderGitHub:
		return true
	default:
		return false
	}
}

// OAuthStateTTL is how long an authorization request stays completable.
//
// Long enough to sign in to a provider, approve a consent screen, and possibly complete a second factor;
// short enough that an abandoned flow's code verifier is not sitting in the database for an afternoon.
const OAuthStateTTL = 15 * time.Minute

// oauthHTTPTimeout bounds a call to a provider's API. These are third parties on the request path, so an
// unbounded call would hold a request open for as long as GitHub felt like taking.
const oauthHTTPTimeout = 15 * time.Second

// OAuth errors.
var (
	// ErrUnknownProvider is a provider name this build does not implement, or one whose credentials this
	// instance has not configured. The two are deliberately one error: which providers an instance offers
	// is not a secret, but it is also not something a caller needs distinguished.
	ErrUnknownProvider = errors.New("unknown or unconfigured OAuth provider")

	// ErrOAuthState covers every way an authorization request can fail to come back: unknown state,
	// expired, already spent. One error because the client is told one thing.
	ErrOAuthState = errors.New("this sign-in link is no longer valid; start again")

	// ErrOAuthExchange is a provider refusing the code, or failing to answer.
	ErrOAuthExchange = errors.New("the provider did not complete the sign-in")

	// ErrOAuthNoEmail means the provider returned no usable address. Both providers can do this — GitHub
	// when every address is private and unverified, Google essentially never — and without one there is
	// nothing to match an account against.
	ErrOAuthNoEmail = errors.New("the provider did not supply an email address")
)

// OAuthIdentity is what a provider tells us about the person who just signed in.
type OAuthIdentity struct {
	Provider OAuthProviderName
	// UserID is the provider's own stable identifier. This, not the email, is what an identity is keyed
	// by: an address can be changed or reassigned at most providers, and a numeric ID cannot.
	UserID string
	Email  string
	// EmailVerified is the provider's own assertion, carried separately because the linking rule turns on
	// it. Never inferred: a provider that does not say so is treated as not having said so.
	EmailVerified bool
	DisplayName   string
}

// OAuthProvider is one configured identity provider.
type OAuthProvider interface {
	Name() OAuthProviderName
	// AuthCodeURL is where the user is sent to approve the request.
	AuthCodeURL(state, verifier string) string
	// Identity exchanges an authorization code and returns who signed in.
	Identity(ctx context.Context, code, verifier string) (OAuthIdentity, error)
}

// OAuthProviders is the set an instance offers, keyed by name.
type OAuthProviders map[OAuthProviderName]OAuthProvider

// Get returns a provider by name, or ErrUnknownProvider.
func (p OAuthProviders) Get(name string) (OAuthProvider, error) {
	provider, ok := p[OAuthProviderName(name)]
	if !ok {
		return nil, ErrUnknownProvider
	}
	return provider, nil
}

// Names lists the configured providers, for a client asking what this instance offers.
func (p OAuthProviders) Names() []string {
	// Fixed order rather than map order: this reaches an API response, and a list that reshuffles between
	// requests is noise in a diff and in a test.
	var out []string
	for _, name := range []OAuthProviderName{ProviderGoogle, ProviderGitHub} {
		if _, ok := p[name]; ok {
			out = append(out, string(name))
		}
	}
	return out
}

// OAuthOptions configures the provider set.
type OAuthOptions struct {
	PublicBaseURL string

	GoogleClientID     string
	GoogleClientSecret string
	GitHubClientID     string
	GitHubClientSecret string

	// HTTPClient calls the providers' APIs. Injected so tests can point at a stub: a real provider cannot
	// be asked to return an unverified email or a malformed body on demand, and those are the cases the
	// linking rule turns on.
	HTTPClient *http.Client

	// GoogleUserInfoURL, GitHubAPIBaseURL and the endpoints override where requests go. Empty means the
	// real provider; tests set them.
	GoogleUserInfoURL string
	GitHubAPIBaseURL  string
	GoogleEndpoint    *oauth2.Endpoint
	GitHubEndpoint    *oauth2.Endpoint
}

// NewOAuthProviders builds the set from configuration. A provider with no credentials is simply absent.
func NewOAuthProviders(opts OAuthOptions) OAuthProviders {
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: oauthHTTPTimeout}
	}

	providers := OAuthProviders{}

	if opts.GoogleClientID != "" && opts.GoogleClientSecret != "" {
		endpoint := google.Endpoint
		if opts.GoogleEndpoint != nil {
			endpoint = *opts.GoogleEndpoint
		}
		providers[ProviderGoogle] = &googleProvider{
			client: client,
			userInfoURL: firstNonEmptyString(opts.GoogleUserInfoURL,
				"https://openidconnect.googleapis.com/v1/userinfo"),
			cfg: &oauth2.Config{
				ClientID:     opts.GoogleClientID,
				ClientSecret: opts.GoogleClientSecret,
				Endpoint:     endpoint,
				RedirectURL:  OAuthRedirectURL(opts.PublicBaseURL, ProviderGoogle),
				// openid and email only. profile would add a display name this milestone does not use, and
				// asking for data before there is a use for it is how a consent screen becomes alarming.
				Scopes: []string{"openid", "email"},
			},
		}
	}

	if opts.GitHubClientID != "" && opts.GitHubClientSecret != "" {
		endpoint := github.Endpoint
		if opts.GitHubEndpoint != nil {
			endpoint = *opts.GitHubEndpoint
		}
		providers[ProviderGitHub] = &githubProvider{
			client:  client,
			apiBase: strings.TrimSuffix(firstNonEmptyString(opts.GitHubAPIBaseURL, "https://api.github.com"), "/"),
			cfg: &oauth2.Config{
				ClientID:     opts.GitHubClientID,
				ClientSecret: opts.GitHubClientSecret,
				Endpoint:     endpoint,
				RedirectURL:  OAuthRedirectURL(opts.PublicBaseURL, ProviderGitHub),
				// user:email rather than read:user: the addresses are the only thing needed, and the
				// narrower scope is what the consent screen shows the person being asked.
				Scopes: []string{"user:email"},
			},
		}
	}

	return providers
}

// OAuthRedirectURL is the callback this instance registers with a provider.
//
// Derived from the configured public base URL, never from a request: the provider matches it against what
// was registered, and building it from a Host header would make that match depend on whatever a caller
// sent.
func OAuthRedirectURL(baseURL string, provider OAuthProviderName) string {
	return strings.TrimSuffix(baseURL, "/") + oauthProviderPath(string(provider)) + "/callback"
}

// GenerateOAuthVerifier returns a PKCE code verifier.
func GenerateOAuthVerifier() string { return oauth2.GenerateVerifier() }

// ---------- Google ----------

type googleProvider struct {
	cfg         *oauth2.Config
	client      *http.Client
	userInfoURL string
}

func (p *googleProvider) Name() OAuthProviderName { return ProviderGoogle }

func (p *googleProvider) AuthCodeURL(state, verifier string) string {
	return p.cfg.AuthCodeURL(state,
		oauth2.AccessTypeOnline,
		oauth2.S256ChallengeOption(verifier),
	)
}

func (p *googleProvider) Identity(ctx context.Context, code, verifier string) (OAuthIdentity, error) {
	token, err := exchange(ctx, p.cfg, p.client, code, verifier)
	if err != nil {
		return OAuthIdentity{}, err
	}

	// The userinfo endpoint rather than the ID token: both carry the same claims, and taking them from a
	// direct TLS call to Google avoids having to fetch and cache JWKS to verify a signature whose only
	// purpose here would be to authenticate a response we already made ourselves.
	var info struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := p.getJSON(ctx, token, p.userInfoURL, &info); err != nil {
		return OAuthIdentity{}, err
	}
	if info.Sub == "" {
		return OAuthIdentity{}, fmt.Errorf("%w: no subject in the userinfo response", ErrOAuthExchange)
	}
	if info.Email == "" {
		return OAuthIdentity{}, ErrOAuthNoEmail
	}

	return OAuthIdentity{
		Provider:      ProviderGoogle,
		UserID:        info.Sub,
		Email:         info.Email,
		EmailVerified: info.EmailVerified,
		DisplayName:   info.Name,
	}, nil
}

func (p *googleProvider) getJSON(ctx context.Context, token *oauth2.Token, url string, dst any) error {
	return getProviderJSON(ctx, p.client, token, url, nil, dst)
}

// ---------- GitHub ----------

type githubProvider struct {
	cfg     *oauth2.Config
	client  *http.Client
	apiBase string
}

func (p *githubProvider) Name() OAuthProviderName { return ProviderGitHub }

func (p *githubProvider) AuthCodeURL(state, verifier string) string {
	return p.cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

func (p *githubProvider) Identity(ctx context.Context, code, verifier string) (OAuthIdentity, error) {
	token, err := exchange(ctx, p.cfg, p.client, code, verifier)
	if err != nil {
		return OAuthIdentity{}, err
	}

	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := getProviderJSON(ctx, p.client, token, p.apiBase+"/user", githubHeaders, &user); err != nil {
		return OAuthIdentity{}, err
	}
	if user.ID == 0 {
		return OAuthIdentity{}, fmt.Errorf("%w: no user id in the profile response", ErrOAuthExchange)
	}

	// A second call, and not an optional one. /user's email field is null whenever the address is private,
	// which is the default for a lot of accounts — and even when present it carries no verification flag.
	// /user/emails is the only place GitHub says whether an address is verified, and that is the fact the
	// linking rule turns on.
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := getProviderJSON(ctx, p.client, token, p.apiBase+"/user/emails", githubHeaders, &emails); err != nil {
		return OAuthIdentity{}, err
	}

	address, verified, ok := pickGitHubEmail(emails)
	if !ok {
		return OAuthIdentity{}, ErrOAuthNoEmail
	}

	return OAuthIdentity{
		Provider:      ProviderGitHub,
		UserID:        fmt.Sprintf("%d", user.ID),
		Email:         address,
		EmailVerified: verified,
		DisplayName:   firstNonEmptyString(user.Name, user.Login),
	}, nil
}

// pickGitHubEmail chooses which of an account's addresses to link by.
//
// A verified address always wins over the primary one. Preferring "primary" would mean an account whose
// primary address is unverified gets refused even though a verified address sits right beside it — and
// preferring it *without* checking verification would hand the linking rule an address GitHub never
// confirmed.
func pickGitHubEmail(emails []struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}) (address string, verified, ok bool) {
	var firstVerified, primary, first string

	for _, e := range emails {
		if e.Email == "" {
			continue
		}
		if first == "" {
			first = e.Email
		}
		if e.Verified {
			// A verified primary address is the best answer available; nothing later can improve on it.
			// Checked before the firstVerified guard rather than inside it: a verified *non*-primary
			// address appearing earlier in the list would otherwise latch firstVerified and skip this
			// branch entirely, linking the account by the wrong address.
			if e.Primary {
				return e.Email, true, true
			}
			if firstVerified == "" {
				firstVerified = e.Email
			}
		}
		if e.Primary && primary == "" {
			primary = e.Email
		}
	}

	switch {
	case firstVerified != "":
		return firstVerified, true, true
	case primary != "":
		return primary, false, true
	case first != "":
		return first, false, true
	default:
		return "", false, false
	}
}

// githubHeaders asks for the documented media type, which is what pins the response shape across API
// versions rather than accepting whatever the default happens to become.
func githubHeaders(h http.Header) {
	h.Set("Accept", "application/vnd.github+json")
	h.Set("X-GitHub-Api-Version", "2022-11-28")
}

// ---------- shared plumbing ----------

// exchange redeems an authorization code, presenting the PKCE verifier.
func exchange(ctx context.Context, cfg *oauth2.Config, client *http.Client, code, verifier string) (*oauth2.Token, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)

	token, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		// Wrapped, never surfaced: the library's error can quote the request, and that request carries the
		// client secret (CLAUDE.md rule 8). The caller gets a sentinel; the detail goes to the log.
		return nil, fmt.Errorf("%w: %s", ErrOAuthExchange, redactOAuthError(err))
	}
	if !token.Valid() {
		return nil, fmt.Errorf("%w: the provider returned no usable access token", ErrOAuthExchange)
	}
	return token, nil
}

// maxProviderResponse caps how much of a provider's response is read. Generous for a profile document,
// small enough that a misbehaving or impersonated endpoint cannot stream until memory runs out.
const maxProviderResponse = 1 << 20 // 1 MiB

// getProviderJSON performs an authenticated GET against a provider API and decodes the result.
func getProviderJSON(ctx context.Context, client *http.Client, token *oauth2.Token,
	url string, setHeaders func(http.Header), dst any,
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		// Wrapped, unlike the provider-facing errors below: this one comes from our own configured URL
		// being malformed, so it carries nothing a provider sent and nothing credential-shaped.
		return fmt.Errorf("%w: building the profile request: %w", ErrOAuthExchange, err)
	}
	token.SetAuthHeader(req)
	req.Header.Set("Accept", "application/json")
	if setHeaders != nil {
		setHeaders(req.Header)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrOAuthExchange, redactOAuthError(err))
	}
	// Drained before closing, so the connection returns to the idle pool. The decoder can stop before the
	// end of the body — a LimitReader cut, or trailing bytes after the JSON — and net/http will not reuse a
	// connection whose body was left unread. GitHub costs two calls per sign-in, so without this the second
	// pays a fresh TLS handshake every time.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProviderResponse))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		// The body is deliberately not included: it is a third party's text, and this string reaches a log.
		return fmt.Errorf("%w: the provider answered %s", ErrOAuthExchange, resp.Status)
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProviderResponse)).Decode(dst); err != nil {
		return fmt.Errorf("%w: the provider's response could not be read", ErrOAuthExchange)
	}
	return nil
}

// redactOAuthError strips anything credential-shaped out of a provider or transport error.
//
// x/oauth2's *RetrieveError renders the response body, and a provider that echoes the request — or an
// operator who has misconfigured the endpoint to something that does — would put the client secret into
// that string, which then reaches a log line (CLAUDE.md rule 8). Only the status survives.
func redactOAuthError(err error) string {
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		if retrieve.ErrorCode != "" {
			return "the provider rejected the request (" + retrieve.ErrorCode + ")"
		}
		return "the provider rejected the request (" + retrieve.Response.Status + ")"
	}
	return "could not reach the provider"
}

// firstNonEmptyString returns the first non-empty argument, or "".
func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
