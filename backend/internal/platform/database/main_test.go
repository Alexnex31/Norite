package database

import (
	"testing"

	"github.com/Alexnex31/Norite/backend/internal/platform/dbtest"
)

// These tests run against a real Postgres in a container rather than a mock. The behavior under test —
// advisory locks, transaction semantics, golang-migrate's bookkeeping — is entirely Postgres behavior, so
// a mock would only assert that the test author's mental model matches itself.
//
// The container plumbing lives in internal/platform/dbtest so every domain package can share it; this file
// is only the wiring. dbtest deliberately does not import this package, which is what keeps that reusable
// without a dependency cycle.

func TestMain(m *testing.M) { dbtest.Main(m) }

// freshDatabase creates an empty database inside the shared container and returns its DSN.
func freshDatabase(t *testing.T) string {
	t.Helper()
	return dbtest.FreshDatabase(t)
}
