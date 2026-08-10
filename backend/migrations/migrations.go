// Package migrations embeds the golang-migrate SQL migration files into the server binary.
//
// Embedding rather than shipping a directory alongside the binary is what makes the single-binary
// self-hosted deployment story work (docs/architecture.md §1): `norite-server` on a host with a Postgres
// URL is the entire install, with no migration files to copy, version-match, or lose.
//
// Every schema change adds a numbered up/down pair here and is applied by
// internal/platform/database.Migrate — see the `/db-migration` skill for the full workflow, including the
// project rule that a new hot-path query ships with its index in the same migration (CLAUDE.md rule 7).
package migrations

import "embed"

// FS holds every migration file, in golang-migrate's `{version}_{name}.{up|down}.sql` naming scheme.
//
//go:embed *.sql
var FS embed.FS
