package main

import (
	"testing"

	"github.com/Alexnex31/Norite/backend/internal/platform/dbtest"
)

// The HTTP tests in auth_http_test.go drive the real router against a real database, so this package needs
// the shared container harness every domain package uses. Under `-short` no container is started and those
// tests skip themselves, which is what keeps `just test-short` usable with no container runtime.
//
// The subprocess tests in e2e_test.go deliberately keep starting their own containers: one of them holds
// the migration advisory lock for the whole database to prove a second instance blocks on it, which is not
// something to do to a container shared with every other test in the package.
func TestMain(m *testing.M) { dbtest.Main(m) }
