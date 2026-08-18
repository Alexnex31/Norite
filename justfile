set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

go_modules := "backend cli gui daemon"

# Pinned so every machine and CI generate byte-identical code. Bump deliberately, then re-run
# `just sqlc-generate` and commit the diff.
sqlc_version := "v1.30.0"

# Must match .github/workflows/ci.yml's GOLANGCI_LINT_VERSION. golangci-lint has to be built with a Go
# version at least as new as the highest `go` directive in the workspace, or it refuses to run at all.
golangci_lint_version := "2.12.2"

# Default connection string for the docker-compose Postgres. Override for any other target:
#   just database_url=postgres://... db-migrate
database_url := env_var_or_default("NORITE_DATABASE_URL", "postgres://norite:norite@localhost:5432/norite?sslmode=disable")

# List available recipes.
default:
    @just --list

# Run the full local stack (Postgres, Redis, backend). Frontend joins once it exists (Phase O).
dev:
    docker compose -f docker/docker-compose.yml up --build

# The modules are independent with no shared state, so they run in parallel. The backend's integration
# tests bring up a real Postgres via testcontainers, so this needs a running container runtime — use
# `just test-short` on a machine without one. Frontend tests join once frontend/ exists (Phase O).

# Test every Go module (needs a container runtime).
test:
    #!/usr/bin/env bash
    set -euo pipefail
    pids=()
    for m in {{go_modules}}; do
        (cd "$m" && go test ./... 2>&1 | sed "s/^/[$m] /") &
        pids+=($!)
    done
    fail=0
    for pid in "${pids[@]}"; do wait "$pid" || fail=1; done
    exit $fail

# Fast inner-loop variant of `test`. Not a substitute for it before pushing.

# Test every Go module, skipping anything that needs a container runtime.
test-short:
    #!/usr/bin/env bash
    set -euo pipefail
    pids=()
    for m in {{go_modules}}; do
        (cd "$m" && go test -short ./... 2>&1 | sed "s/^/[$m] /") &
        pids+=($!)
    done
    fail=0
    for pid in "${pids[@]}"; do wait "$pid" || fail=1; done
    exit $fail

# Warns (but does not fail) when the local golangci-lint differs from the version CI pins, since a
# mismatch means a green run here predicts nothing about CI. Frontend linting joins once frontend/ exists
# (Phase O).

# Vet + lint every Go module, in parallel.
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    local_version="$(golangci-lint version --short 2>/dev/null || echo unknown)"
    if [[ "$local_version" != "{{golangci_lint_version}}" ]]; then
        echo "warning: golangci-lint $local_version locally, CI pins {{golangci_lint_version}} — results may differ" >&2
    fi
    pids=()
    for m in {{go_modules}}; do
        (cd "$m" && { go vet ./... && golangci-lint run ./...; } 2>&1 | sed "s/^/[$m] /") &
        pids+=($!)
    done
    fail=0
    for pid in "${pids[@]}"; do wait "$pid" || fail=1; done
    exit $fail

# Runs the server binary's own -migrate-only mode rather than a separate `migrate` CLI: the migrations are
# go:embed'd into the binary, so this is the same code path — advisory lock included — that a real startup
# and the flagship's Helm pre-upgrade Job take. There is no second implementation to drift.

# Apply pending golang-migrate migrations.
db-migrate:
    cd backend && NORITE_DATABASE_URL="{{database_url}}" go run ./cmd/server -migrate-only

# Inputs are backend/migrations (the schema) and backend/internal/db/queries. Generated code is committed,
# so run this and commit the diff whenever either input changes.

# Regenerate the sqlc query layer in backend/internal/db.
sqlc-generate:
    cd backend && go run github.com/sqlc-dev/sqlc/cmd/sqlc@{{sqlc_version}} generate

# Run in CI so a schema or query change can't merge without its regenerated code.

# Fail if the committed sqlc output is stale.
sqlc-check: sqlc-generate
    #!/usr/bin/env bash
    set -euo pipefail
    if ! git diff --quiet -- backend/internal/db; then
        echo "backend/internal/db is out of date — run 'just sqlc-generate' and commit the result:" >&2
        git --no-pager diff --stat -- backend/internal/db >&2
        exit 1
    fi
    echo "sqlc output is up to date"

# pnpm audit and Trivy join once frontend/ and a Dockerfile exist.

# Run govulncheck across every Go module.
security-scan:
    #!/usr/bin/env bash
    set -euo pipefail
    pids=()
    for m in {{go_modules}}; do
        (cd "$m" && govulncheck ./... 2>&1 | sed "s/^/[$m] /") &
        pids+=($!)
    done
    fail=0
    for pid in "${pids[@]}"; do wait "$pid" || fail=1; done
    exit $fail

# Build every binary via goreleaser, snapshot mode (no publish, no signing — that lands at Milestone M24).
build:
    goreleaser build --snapshot --clean

# Build every module with the workspace switched off.
#
# go.work resolves cross-module imports from the checkout, which since M7 hides a module whose own go.mod
# and go.sum are missing an entry — cli requires daemon now, so a dependency added to daemon is invisible
# from cli until something builds without the workspace. A release build does. So does anyone consuming one
# module on its own.
build-standalone:
    #!/usr/bin/env bash
    set -euo pipefail
    for module in backend cli gui daemon; do
        echo "[$module] building without the workspace"
        (cd "$module" && GOWORK=off go build ./...)
    done
    echo "every module builds standalone"
