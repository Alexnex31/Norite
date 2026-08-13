package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// A stub provider, because the cases that matter cannot be requested from Google or GitHub: an unverified
// email, a private address, a refused code, a malformed body. The linking rule turns on the first of
// those, so it has to be reachable in a test.

type stubProvider struct {
	server *httptest.Server

	// what the stub reports back
	googleInfo   map[string]any
	githubUser   map[string]any
	githubEmails []map[string]any

	// failure modes
	rejectCode bool
	// lastVerifierSeen is the PKCE verifier the stub received at exchange, so a test can assert it was
	// actually sent and actually matched the challenge.
	lastVerifierSeen  string
	lastChallengeSeen string
}

func newStubProvider(t *testing.T) *stubProvider {
	t.Helper()
	s := &stubProvider{}

	mux := http.NewServeMux()

	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		s.lastChallengeSeen = r.URL.Query().Get("code_challenge")
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.lastVerifierSeen = r.PostFormValue("code_verifier")

		if s.rejectCode {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "stub-access-token",
			"token_type":   "Bearer",
		})
	})

	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.googleInfo)
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.githubUser)
	})

	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.githubEmails)
	})

	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *stubProvider) endpoint() oauth2.Endpoint {
	return oauth2.Endpoint{AuthURL: s.server.URL + "/authorize", TokenURL: s.server.URL + "/token"}
}

// providers builds a provider set pointed at the stub.
func (s *stubProvider) providers() OAuthProviders {
	endpoint := s.endpoint()
	return NewOAuthProviders(OAuthOptions{
		PublicBaseURL:      "https://chat.example.com",
		GoogleClientID:     "google-id",
		GoogleClientSecret: "google-secret",
		GitHubClientID:     "github-id",
		GitHubClientSecret: "github-secret",
		HTTPClient:         s.server.Client(),
		GoogleUserInfoURL:  s.server.URL + "/userinfo",
		GitHubAPIBaseURL:   s.server.URL,
		GoogleEndpoint:     &endpoint,
		GitHubEndpoint:     &endpoint,
	})
}

// ---------- what a provider is trusted for ----------

func TestGoogleIdentityCarriesTheVerifiedFlag(t *testing.T) {
	for name, verified := range map[string]bool{"verified": true, "unverified": false} {
		t.Run(name, func(t *testing.T) {
			stub := newStubProvider(t)
			stub.googleInfo = map[string]any{
				"sub": "google-123", "email": "ada@example.com", "email_verified": verified, "name": "Ada",
			}

			provider, err := stub.providers().Get("google")
			require.NoError(t, err)

			identity, err := provider.Identity(t.Context(), "code", "verifier")
			require.NoError(t, err)

			assert.Equal(t, ProviderGoogle, identity.Provider)
			assert.Equal(t, "google-123", identity.UserID)
			assert.Equal(t, "ada@example.com", identity.Email)
			assert.Equal(t, verified, identity.EmailVerified,
				"the provider's own flag must be carried through, never inferred")
		})
	}
}

// GitHub's /user carries no verification flag and its email is null whenever the address is private, so
// the second call is not optional — this asserts it actually happens.
func TestGitHubReadsTheEmailsEndpoint(t *testing.T) {
	stub := newStubProvider(t)
	stub.githubUser = map[string]any{"id": 4242, "login": "ada", "name": "Ada Lovelace"}
	stub.githubEmails = []map[string]any{
		{"email": "ada@example.com", "primary": true, "verified": true},
	}

	provider, err := stub.providers().Get("github")
	require.NoError(t, err)

	identity, err := provider.Identity(t.Context(), "code", "verifier")
	require.NoError(t, err)

	assert.Equal(t, "4242", identity.UserID, "the numeric id, not the login, is the stable key")
	assert.Equal(t, "ada@example.com", identity.Email)
	assert.True(t, identity.EmailVerified)
	assert.Equal(t, "Ada Lovelace", identity.DisplayName)
}

// A verified address beats the primary one. Preferring "primary" would refuse an account whose primary
// address happens to be unverified while a verified one sits right beside it.
func TestGitHubPrefersAVerifiedAddress(t *testing.T) {
	cases := map[string]struct {
		emails       []map[string]any
		wantEmail    string
		wantVerified bool
		wantErr      error
	}{
		"verified primary": {
			emails: []map[string]any{
				{"email": "other@example.com", "primary": false, "verified": true},
				{"email": "ada@example.com", "primary": true, "verified": true},
			},
			wantEmail: "ada@example.com", wantVerified: true,
		},
		"unverified primary, verified secondary": {
			emails: []map[string]any{
				{"email": "primary@example.com", "primary": true, "verified": false},
				{"email": "verified@example.com", "primary": false, "verified": true},
			},
			wantEmail: "verified@example.com", wantVerified: true,
		},
		"nothing verified": {
			emails: []map[string]any{
				{"email": "primary@example.com", "primary": true, "verified": false},
			},
			// Still returned, but flagged unverified — refusing here would hide the reason from the caller,
			// which is the one place able to explain it.
			wantEmail: "primary@example.com", wantVerified: false,
		},
		"no addresses at all": {
			emails:  []map[string]any{},
			wantErr: ErrOAuthNoEmail,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newStubProvider(t)
			stub.githubUser = map[string]any{"id": 1, "login": "ada"}
			stub.githubEmails = tc.emails

			provider, _ := stub.providers().Get("github")
			identity, err := provider.Identity(t.Context(), "code", "verifier")

			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantEmail, identity.Email)
			assert.Equal(t, tc.wantVerified, identity.EmailVerified)
		})
	}
}

// ---------- PKCE ----------

// The verifier must actually reach the token endpoint, and the challenge on the authorize URL must be its
// S256 hash. A flow that sends neither still works against a provider that does not enforce PKCE, so this
// is the only thing that would notice it had been dropped.
func TestPKCEChallengeAndVerifierAreSentAndMatch(t *testing.T) {
	stub := newStubProvider(t)
	stub.googleInfo = map[string]any{"sub": "s", "email": "a@b.co", "email_verified": true}

	provider, _ := stub.providers().Get("google")
	verifier := GenerateOAuthVerifier()

	authURL, err := url.Parse(provider.AuthCodeURL("state-value", verifier))
	require.NoError(t, err)

	challenge := authURL.Query().Get("code_challenge")
	require.NotEmpty(t, challenge, "the authorize URL must carry a PKCE challenge")
	assert.Equal(t, "S256", authURL.Query().Get("code_challenge_method"),
		"plain would let anyone who saw the challenge derive the verifier")

	sum := sha256.Sum256([]byte(verifier))
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), challenge,
		"the challenge must be the S256 hash of the verifier this flow will present")

	// ...and the verifier itself must reach the token endpoint.
	_, err = provider.Identity(t.Context(), "code", verifier)
	require.NoError(t, err)
	assert.Equal(t, verifier, stub.lastVerifierSeen, "the exchange must present the code verifier")
}

func TestAuthCodeURLCarriesTheState(t *testing.T) {
	stub := newStubProvider(t)
	provider, _ := stub.providers().Get("google")

	authURL, err := url.Parse(provider.AuthCodeURL("the-state", "verifier"))
	require.NoError(t, err)
	assert.Equal(t, "the-state", authURL.Query().Get("state"))
	assert.Contains(t, authURL.Query().Get("redirect_uri"), "/api/v1/auth/oauth/google/callback")
}

// ---------- failure modes ----------

func TestARefusedCodeIsReportedWithoutTheRequest(t *testing.T) {
	stub := newStubProvider(t)
	stub.rejectCode = true

	provider, _ := stub.providers().Get("google")
	_, err := provider.Identity(t.Context(), "bad-code", "verifier")

	require.ErrorIs(t, err, ErrOAuthExchange)
	// The library's error can render the whole token request, which carries the client secret. Only the
	// provider's error code may survive into a string that reaches a log (CLAUDE.md rule 8).
	assert.NotContains(t, err.Error(), "google-secret", "the client secret must never reach an error string")
	assert.Contains(t, err.Error(), "invalid_grant")
}

func TestAProviderWithNoEmailIsRefused(t *testing.T) {
	stub := newStubProvider(t)
	stub.googleInfo = map[string]any{"sub": "google-123", "email_verified": true}

	provider, _ := stub.providers().Get("google")
	_, err := provider.Identity(t.Context(), "code", "verifier")

	assert.ErrorIs(t, err, ErrOAuthNoEmail)
}

func TestAProviderWithNoSubjectIsRefused(t *testing.T) {
	stub := newStubProvider(t)
	stub.googleInfo = map[string]any{"email": "ada@example.com", "email_verified": true}

	provider, _ := stub.providers().Get("google")
	_, err := provider.Identity(t.Context(), "code", "verifier")

	assert.ErrorIs(t, err, ErrOAuthExchange)
}

// ---------- the provider set ----------

func TestOnlyConfiguredProvidersAreOffered(t *testing.T) {
	providers := NewOAuthProviders(OAuthOptions{
		PublicBaseURL:      "https://chat.example.com",
		GoogleClientID:     "id",
		GoogleClientSecret: "secret",
	})

	assert.Equal(t, []string{"google"}, providers.Names())

	_, err := providers.Get("github")
	assert.ErrorIs(t, err, ErrUnknownProvider, "an unconfigured provider must not be offered")

	_, err = providers.Get("myspace")
	assert.ErrorIs(t, err, ErrUnknownProvider)
}

// Half a provider is rejected at config validation, but NewOAuthProviders must not offer one either — the
// two guards are independent, and a Config built by hand in a test would otherwise slip past both.
func TestHalfAProviderIsNotOffered(t *testing.T) {
	assert.Empty(t, NewOAuthProviders(OAuthOptions{GoogleClientID: "id"}).Names())
	assert.Empty(t, NewOAuthProviders(OAuthOptions{GoogleClientSecret: "secret"}).Names())
	assert.Empty(t, NewOAuthProviders(OAuthOptions{}).Names())
}

func TestValidOAuthProvider(t *testing.T) {
	assert.True(t, ValidOAuthProvider("google"))
	assert.True(t, ValidOAuthProvider("github"))

	for _, bad := range []string{"", "Google", "myspace", "../etc/passwd", "google;drop"} {
		assert.False(t, ValidOAuthProvider(bad), "%q must not be accepted as a provider name", bad)
	}
}

func TestRedirectURLIsBuiltFromTheConfiguredOrigin(t *testing.T) {
	assert.Equal(t, "https://chat.example.com/api/v1/auth/oauth/google/callback",
		OAuthRedirectURL("https://chat.example.com", ProviderGoogle))
	// A trailing slash must not produce a double slash: the provider matches this string exactly against
	// what was registered.
	assert.Equal(t, "https://chat.example.com/api/v1/auth/oauth/github/callback",
		OAuthRedirectURL("https://chat.example.com/", ProviderGitHub))
}

// Scopes reach a consent screen the person being asked actually reads, so asking for more than is used is
// a thing to notice in review rather than discover in a screenshot.
func TestScopesAreNarrow(t *testing.T) {
	stub := newStubProvider(t)

	google, _ := stub.providers().Get("google")
	googleURL, _ := url.Parse(google.AuthCodeURL("s", "v"))
	assert.ElementsMatch(t, []string{"openid", "email"},
		strings.Fields(googleURL.Query().Get("scope")))

	github, _ := stub.providers().Get("github")
	githubURL, _ := url.Parse(github.AuthCodeURL("s", "v"))
	assert.Equal(t, "user:email", githubURL.Query().Get("scope"),
		"read:user would grant profile access this milestone does not use")
}
