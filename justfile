set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

go_modules := "backend cli gui daemon"

# List available recipes.
default:
    @just --list

# Run the full local stack (Postgres, Redis, backend). Frontend joins once it exists (Phase O).
dev:
    docker compose -f docker/docker-compose.yml up --build

# Run tests across every Go module, in parallel (they're independent modules with no shared state).
# Frontend tests join once frontend/ exists (Phase O).
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

# Vet + lint every Go module, in parallel. Frontend linting joins once frontend/ exists (Phase O).
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    pids=()
    for m in {{go_modules}}; do
        (cd "$m" && { go vet ./... && golangci-lint run ./...; } 2>&1 | sed "s/^/[$m] /") &
        pids+=($!)
    done
    fail=0
    for pid in "${pids[@]}"; do wait "$pid" || fail=1; done
    exit $fail

# Apply pending golang-migrate migrations. No-op until backend/migrations/ exists (Milestone M1).
db-migrate:
    @echo "no migrations yet — lands at Milestone M1 (docs/architecture.md §13)"

# govulncheck across every Go module, in parallel. pnpm audit and Trivy join once frontend/ and a
# Dockerfile exist.
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
