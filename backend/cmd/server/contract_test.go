package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/Alexnex31/Norite/backend/internal/auth"
)

// CLAUDE.md rule 6 says every REST endpoint added or changed updates contracts/openapi.yaml in the same
// commit. That is a rule someone has to remember, right up until it is a test.
//
// This walks the real router and compares it against the real contract document, in both directions:
//
//   - a route with no entry in openapi.yaml is an undocumented endpoint, which clients codegen against
//     from Milestone M12 and therefore cannot call;
//   - an entry with no route is a documented endpoint that does not exist, which is worse — a client will
//     generate a call for it and get a 404 at runtime.
//
// It is deliberately structural rather than a schema-level check: it verifies the *set* of operations, not
// their payloads. Payload drift is caught by oapi-codegen once it starts generating server types (M12).

// openAPIDoc is the slice of the contract this test needs.
type openAPIDoc struct {
	Servers []serverEntry       `yaml:"servers"`
	Paths   map[string]pathItem `yaml:"paths"`
}

type serverEntry struct {
	URL string `yaml:"url"`
}

// pathItem spells its operations out rather than decoding into a map, because a path item also carries
// non-operation keys — `servers` and `parameters` — and a map would have to guess which is which.
type pathItem struct {
	// Servers overrides the document's base for this path. OpenAPI allows it per path, and the reset
	// pages need it: they are served at the instance root while every JSON endpoint sits under /api/v1.
	Servers []serverEntry `yaml:"servers"`

	Get     *operation `yaml:"get"`
	Head    *operation `yaml:"head"`
	Post    *operation `yaml:"post"`
	Put     *operation `yaml:"put"`
	Patch   *operation `yaml:"patch"`
	Delete  *operation `yaml:"delete"`
	Options *operation `yaml:"options"`
}

type operation struct {
	OperationID string `yaml:"operationId"`
}

// byMethod pairs each defined operation with its HTTP method.
func (p pathItem) byMethod() map[string]*operation {
	return map[string]*operation{
		http.MethodGet:     p.Get,
		http.MethodHead:    p.Head,
		http.MethodPost:    p.Post,
		http.MethodPut:     p.Put,
		http.MethodPatch:   p.Patch,
		http.MethodDelete:  p.Delete,
		http.MethodOptions: p.Options,
	}
}

func loadContract(t *testing.T) openAPIDoc {
	t.Helper()

	path := filepath.Join("..", "..", "..", "contracts", "openapi.yaml")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "the contract document must be readable from the backend module")

	var doc openAPIDoc
	require.NoError(t, yaml.Unmarshal(raw, &doc), "contracts/openapi.yaml must be valid YAML")
	require.NotEmpty(t, doc.Paths, "the contract document has no paths")
	return doc
}

// documentedOperations returns "METHOD /path" for every operation in the contract, with the server prefix
// applied so the strings are comparable with what the router reports.
func documentedOperations(t *testing.T, doc openAPIDoc) map[string]string {
	t.Helper()

	base := ""
	if len(doc.Servers) > 0 {
		base = strings.TrimSuffix(doc.Servers[0].URL, "/")
	}

	out := make(map[string]string)
	for path, item := range doc.Paths {
		// A path may override the document's base. Without honoring that, the reset pages — served at the
		// root while everything else sits under /api/v1 — could only be documented by lying about where
		// they live.
		prefix := base
		if len(item.Servers) > 0 {
			prefix = strings.TrimSuffix(item.Servers[0].URL, "/")
		}

		for method, op := range item.byMethod() {
			if op == nil {
				continue
			}
			out[method+" "+prefix+path] = op.OperationID
		}
	}
	return out
}

// routedOperations returns "METHOD /path" for every route the assembled router actually serves.
func routedOperations(t *testing.T, handler http.Handler) map[string]struct{} {
	t.Helper()

	mux, ok := handler.(*chi.Mux)
	require.True(t, ok, "newRouter must return a *chi.Mux for this test to walk it")

	out := make(map[string]struct{})
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// chi reports a trailing slash on group roots; the contract does not use them.
		route = strings.TrimSuffix(route, "/")
		if route == "" {
			route = "/"
		}
		out[method+" "+route] = struct{}{}
		return nil
	})
	require.NoError(t, err)
	return out
}

func TestEveryRouteIsDocumented(t *testing.T) {
	handler := newTestRouterWithAuth(t)

	documented := documentedOperations(t, loadContract(t))
	routed := routedOperations(t, handler)

	var undocumented []string
	for op := range routed {
		if _, ok := documented[op]; !ok {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(undocumented)

	assert.Empty(t, undocumented,
		"these routes are served but absent from contracts/openapi.yaml — rule 6 requires them in the same commit:\n  %s",
		strings.Join(undocumented, "\n  "))
}

func TestEveryDocumentedEndpointExists(t *testing.T) {
	handler := newTestRouterWithAuth(t)

	documented := documentedOperations(t, loadContract(t))
	routed := routedOperations(t, handler)

	var missing []string
	for op, id := range documented {
		if _, ok := routed[op]; !ok {
			missing = append(missing, fmt.Sprintf("%s (operationId: %s)", op, id))
		}
	}
	sort.Strings(missing)

	// The more dangerous direction: a client generates a call for this and gets a 404 at runtime.
	assert.Empty(t, missing,
		"these endpoints are documented but not served:\n  %s",
		strings.Join(missing, "\n  "))
}

// Every operation needs a distinct operationId, because that is the name oapi-codegen turns into a Go
// method — a duplicate silently collapses two endpoints into one generated call.
func TestOperationIDsAreUniqueAndPresent(t *testing.T) {
	documented := documentedOperations(t, loadContract(t))

	seen := make(map[string]string, len(documented))
	for op, id := range documented {
		require.NotEmpty(t, id, "%s has no operationId", op)
		if previous, duplicate := seen[id]; duplicate {
			t.Errorf("operationId %q is used by both %s and %s", id, previous, op)
		}
		seen[id] = op
	}
}

// newTestRouterWithAuth assembles the real router with the auth routes mounted.
//
// The auth handler is built over a nil service on purpose: this test walks the route table and never
// invokes a handler, and requiring a live database to check that the contract matches the router would make
// a structural test container-dependent for no benefit. The nil service is why AuthSvc is left unset too —
// with it nil the Authenticate middleware is not mounted, which does not affect which routes exist.
func newTestRouterWithAuth(t *testing.T) http.Handler {
	t.Helper()

	router, err := newRouter(routerOptions{
		Config: testConfig(),
		Logger: zerolog.New(io.Discard),
		Health: newHealth(&stubPinger{}),
		Auth:   auth.NewHandler(nil),
	})
	require.NoError(t, err)
	return router
}
