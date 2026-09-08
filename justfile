set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

go_modules := "backend cli gui daemon"

# Pinned so every machine and CI generate byte-identical code. Bump deliberately, then re-run
# `just sqlc-generate` and commit the diff.
sqlc_version := "v1.30.0"

# Pinned for the same reason, and it is now the third generator whose version has to move deliberately.
# Must match .github/workflows/ci.yml's OAPI_CODEGEN_VERSION.
oapi_codegen_version := "v2.8.0"

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

# Test every Go module with the race detector (needs a container runtime).
#
# -race because CI runs -race, and a gate that does not run what CI runs is not a gate. That gap cost two
# red runs on the M11 branch: a lock-contention test in daemon/credentials had been sized for an
# uncontended machine, passed here every time, and failed on a loaded runner where the race detector's
# slowdown pushed a queue past its budget. Nothing in the branch had touched that package.
#
# It costs about 1.6x, not the 10x the race detector is reputed to: 63s to 103s on the backend, because
# most of that suite's time is Postgres round trips rather than Go CPU. `just test-short` is still the fast
# inner loop; this is the thing to run before pushing.
test:
    #!/usr/bin/env bash
    set -euo pipefail
    pids=()
    for m in {{go_modules}}; do
        (cd "$m" && go test ./... -race 2>&1 | sed "s/^/[$m] /") &
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

# Regenerate the Go view of contracts/openapi.yaml into backend/internal/apicontract.
#
# Types only — see backend/oapi-codegen.yaml for why this project does not generate a server. Run this and
# commit the diff whenever the contract changes, which rule 6 requires to be the same commit as the
# endpoint it describes.
contract-generate:
    cd backend && go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@{{oapi_codegen_version}} \
        -config oapi-codegen.yaml ../contracts/openapi.yaml

# Fail if the committed contract types are stale.
#
# The sqlc-check arrangement exactly: generated code is committed so a plain `go build` needs no extra
# tooling, and that only stays true if a contract change cannot merge without its regenerated code.
contract-check: contract-generate
    #!/usr/bin/env bash
    set -euo pipefail
    if ! git diff --quiet -- backend/internal/apicontract; then
        echo "backend/internal/apicontract is out of date — run 'just contract-generate' and commit the result:" >&2
        git --no-pager diff --stat -- backend/internal/apicontract >&2
        exit 1
    fi
    echo "contract types are up to date"

# pnpm audit and Trivy join once frontend/ and a Dockerfile exist.

# Run govulncheck and the dependency-license check across every Go module.
security-scan: license-check
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

# Fail if any dependency carries a license outside the allow-list in ADR 0032.
#
# GOWORK=off because go-licenses drives the go tool with flags the workspace refuses, and each module's own
# go.mod is what actually ships anyway — the same reason CI's test job builds each module that way.
#
# --ignore our own module path: go-licenses stops looking for a LICENSE at the module root, and this
# repository's is one level above that, so every first-party package reports Unknown. The project's own
# licensing is ADR 0032's business, not a dependency question.
license-check:
    #!/usr/bin/env bash
    set -uo pipefail
    fail=0
    for m in {{go_modules}}; do
        # The policy splits by module (ADR 0032). backend/ keeps the strict set, because its whole value in
        # that split is being trivially relicensable and every copyleft term is one more thing somebody
        # would have to reason about first. The clients drop `restricted` — which is what admits GPL-3.0,
        # i.e. go.mau.fi/libsignal at M97 — and `reciprocal`, which admits MPL-2.0.
        #
        # `forbidden` stays denied everywhere. It includes AGPL, so an AGPL-licensed *dependency* would fail
        # the clients' check even though it is compatible with Norite's own license. Harmless: there are
        # none, and the failure would be a prompt to think rather than a wrong answer.
        case "$m" in
            backend) types=forbidden,restricted,reciprocal ;;
            *)       types=forbidden ;;
        esac
        # Status captured before the output is filtered: go-licenses logs glog warnings about assembly it
        # cannot inspect on every run, and a pipeline that filtered those would report grep's exit code
        # instead of the check's.
        output=$(cd "$m" && GOWORK=off go-licenses check ./... \
            --ignore github.com/Alexnex31/Norite \
            --disallowed_types="$types" 2>&1)
        status=$?
        if [ "$status" -ne 0 ]; then
            echo "$output" | sed "s/^/[$m] /"
            echo "[$m] license check failed: either a dependency is outside the allow-list" \
                 "(see docs/adr/0032) or go-licenses could not run at all — the output above says which"
            fail=1
        else
            echo "[$m] licenses ok ($types denied)"
        fi
    done
    # The identifier layer. go-licenses classifies by *type*, and GPL-2.0 and GPL-3.0 both land in
    # `restricted` — so allowing `restricted` for the clients above admits both indiscriminately. That is
    # wrong in one specific way that matters: GPL-2.0-only does not combine with AGPL-3.0, while
    # GPL-2.0-or-later does, because it can be taken up to v3. No --disallowed_types value can express the
    # difference, so it is matched on the SPDX identifier itself.
    #
    # -x anchors the match so GPL-2.0-or-later is not caught by a GPL-2.0 prefix. Field 3 of the committed
    # inventory is the identifier; field 2 is a URL, which has contained no comma in any observed row.
    if [ -f {{justfile_directory()}}/contracts/dependency-licenses.txt ]; then
        denied=$(tail -n +4 {{justfile_directory()}}/contracts/dependency-licenses.txt | cut -d, -f3 \
            | grep -Ex 'GPL-2\.0-only|GPL-1\.0.*|SSPL.*|BUSL.*|Elastic-2\.0|CC-BY-SA.*' | sort -u || true)
        if [ -n "$denied" ]; then
            echo "license identifiers outside the allow-list (see docs/adr/0032): $denied"
            fail=1
        else
            echo "[identifiers] no denied SPDX identifier in the inventory"
        fi
    fi
    exit $fail

# Regenerate the committed dependency-license inventory. Commit the diff.
#
# Checked in rather than merely checkable, for the reason the sqlc output is: a change to the set of
# licenses this project ships under should appear in a diff and be reviewed, not be a thing somebody could
# have run.
#
# LC_ALL=C on the sort, and it is load-bearing rather than tidy: collation is locale-dependent, so
# en_US.UTF-8 and the CI runner's locale order `github.com/go-sql-driver` against `github.com/godbus`
# differently. Without it the committed file and the one CI regenerates hold the same lines in a different
# order, and the staleness check fails on every push from a machine whose locale differs from the runner's.
# Running the recipe twice on one machine cannot catch this.
license-inventory:
    #!/usr/bin/env bash
    set -euo pipefail
    out={{justfile_directory()}}/contracts/dependency-licenses.txt
    tmp=$(mktemp)
    trap 'rm -f "$tmp"' EXIT
    # A module that fails to report must abort the whole run, never be skipped. This was `|| true` with
    # stderr to /dev/null, and it silently dropped every dependency of any module go-licenses choked on —
    # found when a go.mod that needed `go mod tidy` under GOWORK=off took urfave/cli and x/term out of the
    # committed inventory with no output at all. A partial inventory is worse than none: it passes the
    # staleness check, so nothing downstream notices that a dependency stopped being recorded.
    for m in {{go_modules}}; do
        (cd "$m" && GOWORK=off go-licenses report ./... --ignore github.com/Alexnex31/Norite 2>/dev/null) \
            || { echo "license-inventory: $m failed to report; refusing to write a partial inventory" >&2
                 echo "  try: (cd $m && GOWORK=off go mod tidy)" >&2
                 exit 1; }
    done | LC_ALL=C sort -u > "$tmp"
    {
        echo "# Dependency licenses, every Go module, generated by \`just license-inventory\`."
        echo "# The allow-list and the reasoning are in docs/adr/0032-agpl-license.md."
        echo "# module,license-url,license"
        cat "$tmp"
    } > "$out"
    echo "wrote $out"

# Build every binary via goreleaser, snapshot mode (no publish, no signing — that lands at Milestone M24).
build:
    goreleaser build --snapshot --clean

# Fail if any hand-written Go file is missing its two-line SPDX header (rule 24, ADR 0032).
#
# Nothing else enforces this. The pass that added the headers was mechanical and one-off; what this catches
# is the *next* file, written months from now by somebody who has not read rule 24 — which is the only way
# the rule was ever going to decay.
#
# Generated files are exempt, matched on Go's own convention rather than on the loose phrase: a line
# `// Code generated ... DO NOT EDIT.` in the first five lines. `grep -q "Code generated"` over the whole
# file would also skip any hand-written file that merely *mentions* the marker in a comment, and skipping a
# file is how this check would fail silently rather than loudly. The two agree on today's tree (14 files
# either way); the strict form is the one that keeps agreeing.
#
# `--others --exclude-standard` alongside `--cached`, so a file that exists but has not been `git add`ed
# yet is checked too. Without it the recipe passes on exactly the case it is for — somebody has just
# written a new .go file, runs this, sees green, commits, and CI fails on the file they were told was
# fine. Untracked-and-ignored paths (bin/, dist/) stay excluded, which is what --exclude-standard buys.
spdx-check:
    #!/usr/bin/env bash
    set -uo pipefail
    fail=0
    for f in $(git -C {{justfile_directory()}} ls-files --cached --others --exclude-standard '*.go'); do
        p={{justfile_directory()}}/$f
        if head -5 "$p" | grep -qE '^// Code generated .* DO NOT EDIT\.$'; then
            # A header here would be stripped by the next regeneration and the staleness check would then
            # fail on a file nobody edited — so its presence is a mistake worth naming, not a harmless extra.
            if head -2 "$p" | grep -q 'SPDX-'; then
                echo "$f: generated file carries an SPDX header; it will be lost on regeneration"
                fail=1
            fi
            continue
        fi
        head -2 "$p" | grep -q '^// SPDX-FileCopyrightText: ' \
            || { echo "$f: missing '// SPDX-FileCopyrightText: <year> <holder>' on line 1"; fail=1; }
        head -2 "$p" | grep -q '^// SPDX-License-Identifier: AGPL-3.0-or-later$' \
            || { echo "$f: missing '// SPDX-License-Identifier: AGPL-3.0-or-later' on line 2"; fail=1; }
        # The blank line is not cosmetic: without it a package comment is rendered by godoc as the license
        # text, and a //go:build constraint sits inside the header's comment block.
        [ -z "$(sed -n '3p' "$p")" ] \
            || { echo "$f: line 3 must be blank, separating the header from what follows"; fail=1; }
    done
    # SPDX deprecated the bare identifier because it was ambiguous about the "or later" clause.
    if git -C {{justfile_directory()}} grep --untracked -n 'SPDX-License-Identifier: AGPL-3.0$' -- '*.go'; then
        echo "the bare AGPL-3.0 identifier is deprecated — use AGPL-3.0-or-later"
        fail=1
    fi
    # Rule 24 is also a statement about what does *not* carry a header. Scoped to the first two lines
    # because CLAUDE.md and ADR 0032 both quote the identifier while specifying the rule.
    for f in $(git -C {{justfile_directory()}} ls-files --cached --others --exclude-standard \
                   '*.sql' '*.md' '*.yml' '*.yaml' '*.toml'); do
        if head -2 {{justfile_directory()}}/$f | grep -q '^\(#\|--\|//\) *SPDX-'; then
            echo "$f: only .go files carry SPDX headers; LICENSE covers the rest (ADR 0032)"
            fail=1
        fi
    done
    [ "$fail" -eq 0 ] && echo "every hand-written Go file carries its SPDX header"
    exit $fail

# Regenerate every binary's THIRD-PARTY-NOTICES.txt. Commit the diff.
#
# Distinct from `license-inventory`, and deliberately not merged with it. The inventory answers a policy
# question ("is every dependency allowed") over `./...`; this answers an attribution obligation ("what must
# accompany this binary") over what the linker actually kept. Backend measures 75 modules the first way and
# 29 the second, the difference being testcontainers and its Docker/containerd set — test-only code that is
# never distributed and must not appear in a notice claiming to describe a shipped binary.
#
# The files live under each module's internal/notices/ because `//go:embed` cannot reach outside the package
# directory, and cli and daemon both embed theirs. Depends on build-local: the generator reads the binaries.
notices: build-local
    #!/usr/bin/env bash
    set -euo pipefail
    gen={{justfile_directory()}}/scripts/gen-third-party-notices.sh
    "$gen" backend ./cmd/server  bin/norite-server backend/internal/notices/THIRD-PARTY-NOTICES.txt
    "$gen" cli     ./cmd/app     bin/norite        cli/internal/notices/THIRD-PARTY-NOTICES.txt
    "$gen" gui     ./cmd/gui     bin/norite-gui    gui/internal/notices/THIRD-PARTY-NOTICES.txt
    "$gen" daemon  ./cmd/daemond bin/norite-daemon daemon/internal/notices/THIRD-PARTY-NOTICES.txt

# Build every binary to ./bin/ with plain `go build`, at paths that do not vary by platform.
#
# `just build` goes through goreleaser, which writes to dist/<id>_<os>_<arch>/ — correct for a release and
# unusable anywhere a path has to be written down, which is what a CI assertion and a verification checklist
# both need. The names match .goreleaser.yaml's `binary:` fields exactly, because a second set of names for
# the same four programs is a thing that drifts.
#
# GOWORK=off for build-standalone's reason: it is what a release build and a single-module consumer both do.
build-local:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p {{justfile_directory()}}/bin
    # Stamped the same way a release is, so `curl /api/v1/meta` locally reports a revision that actually
    # exists rather than "unknown" — the AGPL section 13 offer is not verifiable otherwise.
    rev=$(git -C {{justfile_directory()}} rev-parse HEAD 2>/dev/null || echo unknown)
    meta=github.com/Alexnex31/Norite/backend/internal/meta.Revision
    build() { (cd "$1" && GOWORK=off go build ${4:+-ldflags "$4"} -o "{{justfile_directory()}}/bin/$2" "$3"); echo "  bin/$2"; }
    build backend norite-server ./cmd/server "-X $meta=$rev"
    build cli     norite        ./cmd/app
    build gui     norite-gui    ./cmd/gui
    build daemon  norite-daemon ./cmd/daemond

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
