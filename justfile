set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

go_modules := "backend cli gui daemon"

# List available recipes.
default:
    @just --list

# Run the full local stack (Postgres, Redis, backend). Frontend joins once it exists (Phase O).
dev:
    docker compose -f docker/docker-compose.yml up --build

# Run tests across every Go module. Frontend tests join once frontend/ exists (Phase O).
test:
    #!/usr/bin/env bash
    set -euo pipefail
    for m in {{go_modules}}; do
        echo "--- go test: $m ---"
        (cd "$m" && go test ./...)
    done

# Vet + lint every Go module. Frontend linting joins once frontend/ exists (Phase O).
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    for m in {{go_modules}}; do
        echo "--- lint: $m ---"
        (cd "$m" && go vet ./... && golangci-lint run ./...)
    done

# Apply pending golang-migrate migrations. No-op until backend/migrations/ exists (Milestone M1).
db-migrate:
    @echo "no migrations yet — lands at Milestone M1 (docs/architecture.md §13)"

# govulncheck across every Go module. pnpm audit and Trivy join once frontend/ and a Dockerfile exist.
security-scan:
    #!/usr/bin/env bash
    set -euo pipefail
    for m in {{go_modules}}; do
        echo "--- govulncheck: $m ---"
        (cd "$m" && govulncheck ./...)
    done

# Build every binary via goreleaser, snapshot mode (no publish, no signing — that lands at Milestone M24).
build:
    goreleaser build --snapshot --clean
