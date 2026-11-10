// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getJoin(t *testing.T, router http.Handler, target, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if host != "" {
		req.Host = host
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestTheJoinPageSaysWhatToDoWithALink: an invite link opened in a browser lands on a page, not a 404, and
// the page says how to install Norite and to run norite setup with the link. It is a page, so it carries the
// page policy: a nonce-scoped stylesheet, and no script granted.
func TestTheJoinPageSaysWhatToDoWithALink(t *testing.T) {
	router := readyRouter(t, testConfig())
	page := getJoin(t, router, "/join", "")
	require.Equal(t, http.StatusOK, page.Code, page.Body.String())
	assert.Equal(t, "text/html; charset=utf-8", page.Header().Get("Content-Type"))

	body := page.Body.String()
	assert.Contains(t, body, "norite setup")
	assert.Contains(t, body, "curl -fsSL https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.sh | sh")
	assert.Contains(t, body, "irm https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.ps1 | iex")
	assert.NotContains(t, body, "<script")
	assert.NotContains(t, body, "<form")

	csp := page.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "default-src 'none'")
	assert.NotContains(t, csp, "script-src")
	assert.NotContains(t, csp, "unsafe-inline")
	_, after, found := strings.Cut(csp, "style-src 'nonce-")
	require.True(t, found, csp)
	nonce, _, _ := strings.Cut(after, "'")
	assert.Contains(t, body, `<style nonce="`+nonce+`">`)
	assert.Contains(t, page.Header().Get("Cache-Control"), "no-store")
	assert.Equal(t, "no-referrer", page.Header().Get("Referrer-Policy"))
}

// TestTheJoinPageNamesTheConfiguredAddressNeverTheHost: behind a proxy, Host is whatever the proxy sends,
// so the page names public_base_url, and names nothing when none is set.
func TestTheJoinPageNamesTheConfiguredAddressNeverTheHost(t *testing.T) {
	cfg := testConfig()
	cfg.PublicBaseURL = "https://chat.example.com"
	named := getJoin(t, readyRouter(t, cfg), "/join", "evil.example").Body.String()
	assert.Contains(t, named, `the instance at
<code class="code">https://chat.example.com</code>`)
	assert.NotContains(t, named, "evil.example")

	unnamed := getJoin(t, readyRouter(t, testConfig()), "/join", "evil.example").Body.String()
	assert.NotContains(t, unnamed, "the instance at")
	assert.NotContains(t, unnamed, "evil.example")
}

// TestTheJoinPageReadsNothingFromTheRequest: a query, a stranger's headers or a code in the path change not
// one byte of the page but its nonce. A link's code is a fragment, which a browser never sends; a page
// that echoed anything would be one more place a code could be put in front of somebody.
func TestTheJoinPageReadsNothingFromTheRequest(t *testing.T) {
	router := readyRouter(t, testConfig())
	nonce := regexp.MustCompile(`nonce="[^"]*"`)
	plain := nonce.ReplaceAllString(getJoin(t, router, "/join", "").Body.String(), "")

	for _, target := range []string{
		"/join?code=PGTQRBXGTCDFFWBH",
		`/join?x="><script>alert(1)</script>`,
		"/join?instance=https://evil.example",
	} {
		got := getJoin(t, router, target, "evil.example")
		require.Equal(t, http.StatusOK, got.Code, target)
		assert.Equal(t, plain, nonce.ReplaceAllString(got.Body.String(), ""), target)
	}
}

// TestTheJoinPageIsAGetAlone: it has nothing to submit.
func TestTheJoinPageIsAGetAlone(t *testing.T) {
	router := readyRouter(t, testConfig())
	req := httptest.NewRequest(http.MethodPost, "/join", strings.NewReader("code=PGTQRBXGTCDFFWBH"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
