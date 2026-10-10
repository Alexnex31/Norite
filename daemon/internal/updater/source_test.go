// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sourceAt is a Source reading from a test server.
func sourceAt(server *httptest.Server) Source {
	return Source{ListURL: server.URL + "/releases", DownloadBase: server.URL + "/download",
		UserAgent: "norite-daemon/test", Client: newHTTPClient()}
}

func TestReleasesKeepsOnlyWhatIsAVersion(t *testing.T) {
	var agent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.Header.Get("User-Agent")
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("the request carried a credential")
		}
		_, _ = fmt.Fprint(w, `[
			{"tag_name":"v0.2.0","draft":false,"prerelease":false},
			{"tag_name":"v0.2.1","draft":true},
			{"tag_name":"m23"},
			{"tag_name":"v0.1"},
			{"tag_name":"0.1.5"},
			{"tag_name":"v0.1.5+build.1"},
			{"tag_name":"v0.3.0-rc.1","prerelease":false},
			{"tag_name":"v01.2.3"},
			{"tag_name":"v0.1.0/../v9.9.9"},
			{"tag_name":123},
			{"name":"no tag at all"}
		]`)
	}))
	defer server.Close()

	got, err := sourceAt(server).Releases(t.Context())
	if err == nil {
		// A tag that is a number makes the whole listing something other than what was asked for.
		t.Fatalf("a listing with a numeric tag was read: %v", got)
	}

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `[{"tag_name":"v0.2.0"},{"tag_name":"v0.2.1","draft":true},{"tag_name":"m23"},
			{"tag_name":"v0.1"},{"tag_name":"0.1.5"},{"tag_name":"v0.1.5+build.1"},{"tag_name":"v0.3.0-rc.1"},
			{"tag_name":"v01.2.3"},{"tag_name":"v0.1.0/../v9.9.9"},{"name":"no tag at all"}]`)
	})
	got, err = sourceAt(server).Releases(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, r := range got {
		tags = append(tags, r.Tag)
	}
	if strings.Join(tags, " ") != "v0.2.0 v0.3.0-rc.1" {
		t.Errorf("kept %v", tags)
	}
	if agent != "norite-daemon/test" {
		t.Errorf("user agent %q", agent)
	}
}

func TestTheListingIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("shape") {
		case "long":
			_, _ = fmt.Fprint(w, `[{"tag_name":"`+strings.Repeat("a", maxListing)+`"}]`)
		case "many":
			var b strings.Builder
			b.WriteString("[")
			for i := 0; i < maxReleases+50; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"tag_name":"v0.0.%d"}`, i)
			}
			b.WriteString("]")
			_, _ = fmt.Fprint(w, b.String())
		case "refused":
			http.Error(w, "rate limited", http.StatusForbidden)
		}
	}))
	defer server.Close()
	src := sourceAt(server)

	src.ListURL = server.URL + "/releases?shape=long"
	if _, err := src.Releases(t.Context()); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a listing past the bound: %v", err)
	}
	src.ListURL = server.URL + "/releases?shape=many"
	if got, err := src.Releases(t.Context()); err != nil || len(got) != maxReleases {
		t.Errorf("a listing of too many: %d releases, %v", len(got), err)
	}
	src.ListURL = server.URL + "/releases?shape=refused"
	if _, err := src.Releases(t.Context()); err == nil || strings.Contains(err.Error(), "rate limited") {
		t.Errorf("a refusal: %v (the host's own words must not be carried)", err)
	}
}

func TestARedirectNeverLeavesHTTPS(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "[]")
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer secure.Close()

	client := newHTTPClient()
	client.Transport = secure.Client().Transport
	src := Source{ListURL: secure.URL, UserAgent: "t", Client: client}
	if _, err := src.Releases(t.Context()); err == nil || !strings.Contains(err.Error(), "left HTTPS") {
		t.Errorf("a redirect from HTTPS to HTTP was followed: %v", err)
	}
}
