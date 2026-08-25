package apiclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/cli/internal/apiclient"
)

// The properties this package exists to hold, tested here rather than only through the commands that use
// it. Before M10 they were reachable only through `norite login`, which is how a shared security boundary
// ends up with its coverage attached to one of its callers — and then the second caller arrives and
// nothing says which of these still holds.

// The credential-leak defense, and the one with the most to lose. An instance URL that 302s to another
// host would otherwise have Go's default client re-issue the request there, carrying the Authorization
// header — or the password in the body — to whatever the redirect named.
//
// A redirecting instance URL is a misconfiguration worth reporting rather than chasing, so the redirect is
// surfaced as the error response it is.
func TestARedirectIsNotFollowed(t *testing.T) {
	var elsewhereHits int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHits++
		_, _ = w.Write([]byte(`{"stolen":"` + r.Header.Get("Authorization") + `"}`))
	}))
	defer elsewhere.Close()

	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/api/v1/auth/login", http.StatusFound)
	}))
	defer instance.Close()

	var out map[string]string
	err := apiclient.New(instance.URL).Do(context.Background(),
		http.MethodPost, "/api/v1/auth/login", "a-secret-bearer-token",
		map[string]string{"password": "a-test-password"}, &out)

	require.Error(t, err, "a redirect must not be quietly followed to a success")
	assert.Zero(t, elsewhereHits, "the redirect target must never receive the request, let alone the credential")

	var apiErr *apiclient.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusFound, apiErr.Status)
}

// A hostile or broken instance must not decide how much of a terminal it gets. The message is printed
// verbatim by the command that called, so an unbounded body is an unbounded write to somebody's screen.
func TestAnEnormousErrorBodyIsCapped(t *testing.T) {
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("A", 1<<20)))
	}))
	defer instance.Close()

	err := apiclient.New(instance.URL).Do(context.Background(), http.MethodGet, "/x", "", nil, nil)
	require.Error(t, err)
	assert.Less(t, len(err.Error()), 64<<10, "an error message must stay bounded regardless of the response")
}

// The route from a stranger's server to a user's terminal, which is CLAUDE.md rule 19's whole subject. An
// erase-line sequence in a server-chosen message would blank the line it was written on and replace a
// refusal with whatever the server preferred it to say.
func TestAnErrorMessageIsSanitized(t *testing.T) {
	// The escape is written as JSON's own \u001b rather than as a raw byte, which matters: a raw control
	// character inside a JSON string is invalid JSON, so the body would fail to parse and never reach the
	// sanitizer at all. A hostile instance has no reason to send malformed JSON — it sends the escape the
	// legal way, and that is the input worth defending against.
	hostile := `{"error":{"code":"nope",` +
		`"message":"denied\u001b[2K\rlogin succeeded","request_id":"r1"}}`

	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(hostile))
	}))
	defer instance.Close()

	err := apiclient.New(instance.URL).Do(context.Background(), http.MethodGet, "/x", "", nil, nil)
	require.Error(t, err)

	var apiErr *apiclient.Error
	require.ErrorAs(t, err, &apiErr)
	assert.NotContains(t, apiErr.Message, "\x1b", "the escape must not survive into a printed message")
	assert.NotContains(t, apiErr.Message, "\r", "a carriage return rewrites the line just as effectively")
	assert.Contains(t, apiErr.Message, "denied")
}

// A URL that is not a Norite instance at all — a proxy, a captive portal, a plain web server. The status
// is reported without pretending to have understood the body, and the reason phrase is the server's text
// too, so it gets the same treatment the body would.
func TestANonAPIResponseIsReportedAsSuch(t *testing.T) {
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html><body>nginx</body></html>"))
	}))
	defer instance.Close()

	err := apiclient.New(instance.URL).Do(context.Background(), http.MethodGet, "/x", "", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a Norite API response")
}

// ForDisplay is applied where a foreign value enters the program, so it has to do both jobs at once: make
// the value safe, and keep it from burying what is printed around it.
func TestForDisplaySanitizesAndBounds(t *testing.T) {
	assert.NotContains(t, apiclient.ForDisplay("ada"+"\x1b[2K"+"root"), "\x1b")

	long := apiclient.ForDisplay(strings.Repeat("n", 500))
	assert.Less(t, len([]rune(long)), 200)
	assert.True(t, strings.HasSuffix(long, "…"),
		"a cut must be marked: a silently shortened name reads as somebody else's")

	// A name nobody would notice being processed passes through untouched, which is what keeps the
	// sanitizer from being something people work around.
	assert.Equal(t, "ada.lovelace", apiclient.ForDisplay("ada.lovelace"))
}

// The Authorization header is sent when there is one and omitted when there is not — the second half
// mattering because an empty bearer must not become a literal "Bearer " that an instance might log.
func TestTheBearerHeaderIsSentOnlyWhenPresent(t *testing.T) {
	var seen []string
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer instance.Close()

	c := apiclient.New(instance.URL)
	require.NoError(t, c.Do(context.Background(), http.MethodGet, "/a", "tok", nil, nil))
	require.NoError(t, c.Do(context.Background(), http.MethodGet, "/b", "", nil, nil))

	assert.Equal(t, []string{"Bearer tok", ""}, seen)
}
