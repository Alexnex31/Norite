#!/usr/bin/env bash
#
# Generate a binary's THIRD-PARTY-NOTICES.txt: every module the linker actually retained, with its full
# license text and any NOTICE file.
#
# This answers a different question from contracts/dependency-licenses.txt, and the two are deliberately
# not merged. The inventory asks "is every dependency's license on the allow-list" — a policy question,
# correctly answered over `./...` across all four modules. This asks "what must accompany this binary when
# it is distributed" — an attribution obligation MIT, BSD and Apache-2.0 all impose, and one that must not
# include code that never ships. Measured at the time of writing: `go-licenses report ./...` finds 75
# modules for backend, 14 of them testcontainers/docker/containerd, while the linked binary retains 29 and
# none of those. (The two tools also count slightly differently — go-licenses reports the import path it
# resolved a license at, so its per-binary figure is 30 paths for the same 29 modules.)
#
# The module set comes from `go version -m` on the built binary rather than from the module graph, because
# the linker is the only authority on what was retained. The SPDX identifier comes from `go-licenses
# report`, joined by longest prefix: go-licenses reports the import path it resolved a license at
# (golang.org/x/sys/unix) where `go version -m` reports the module (golang.org/x/sys).
#
# Usage: gen-third-party-notices.sh <module-dir> <main-pkg> <binary> <output>
set -euo pipefail

module_dir=$1; main_pkg=$2; binary=$3; out=$4

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
modcache=$(go env GOMODCACHE)
first_party="github.com/Alexnex31/Norite"

# go-licenses' identifiers, one "importpath license" pair per line.
#
# The version is required rather than defaulted, and go-licenses is run through `go run` rather than taken
# from PATH — the same decision the justfile and CI make, for the same reason: this script is the third
# caller, and a default here would be a fourth place the pin could drift from. It is exported by
# `just notices` and set at the workflow level in CI.
: "${GO_LICENSES_VERSION:?set it, or run this through \`just notices\` which does — it must match the pin in the justfile and ci.yml}"
#
# One file ships inside every OS's build of a program, so it covers what each of them links: the program
# built for each OS a release targets, with cgo off as a release has it, whatever OS this runs on. A
# module only one OS links (purego in the macOS voice-worker, M29; pulse in the Linux one) is otherwise
# missing from the file another OS's build carries, or from the one generated on another machine.
# go-licenses is built once and asked per OS.
targets=(linux darwin windows)
xdir=$(mktemp -d)
GOBIN="$xdir" GOWORK=off go install "github.com/google/go-licenses/v2@$GO_LICENSES_VERSION"
ids=""
for goos in "${targets[@]}"; do
    got=$(cd "$repo/$module_dir" \
            && GOOS=$goos GOWORK=off "$xdir/go-licenses" report "$main_pkg" --ignore "$first_party" 2>/dev/null \
            | awk -F, 'NF>=3 {print $1, $NF}') \
      || { echo "gen-notices: $module_dir: go-licenses report for $goos failed; refusing to write a partial file" >&2; exit 1; }
    ids+="$got"$'\n'
done
ids=$(printf '%s' "$ids" | LC_ALL=C sort -u)
# The binary given is still read: it is the one this machine ships, built by `just build-local`.
binaries=("$repo/$binary")
for goos in "${targets[@]}"; do
    (cd "$repo/$module_dir" && GOOS=$goos GOARCH=amd64 CGO_ENABLED=0 GOWORK=off go build -o "$xdir/$goos" "$main_pkg") \
      || { echo "gen-notices: $module_dir: building for $goos failed" >&2; exit 1; }
    binaries+=("$xdir/$goos")
done

# A module go-licenses cannot classify is reported Unknown, which is no identifier. Those read by hand
# (contracts/license-overrides.txt) are named from that list, first, so the longest-prefix join below finds
# the module's own row before any of its packages'.
by_hand=$("$repo/scripts/license-overrides.sh" ids)
ids=$(printf '%s\n' "$ids" | awk '$2 != "Unknown"')

# The module cache escapes an uppercase letter as '!' + its lowercase form.
escape() { printf '%s' "$1" | sed 's/\([A-Z]\)/!\L\1/g'; }

find_file() {
    local dir=$1; shift
    for name in "$@"; do
        if [ -f "$dir/$name" ]; then printf '%s' "$dir/$name"; return 0; fi
    done
    return 1
}

tmp=$(mktemp) && trap 'rm -rf "$tmp" "$xdir"' EXIT

count=0
while read -r mod ver; do
    case "$mod" in "$first_party"*) continue ;; esac

    dir="$modcache/$(escape "$mod")@$ver"
    if [ ! -d "$dir" ]; then
        echo "gen-notices: $mod@$ver not in the module cache — run 'go mod download'" >&2
        exit 1
    fi

    lic=$(find_file "$dir" LICENSE LICENSE.txt LICENSE.md LICENCE COPYING COPYING.txt) \
      || { echo "gen-notices: no license file found for $mod@$ver in $dir" >&2; exit 1; }

    # Longest-prefix join: the identifier is reported against the import path, not the module path.
    #
    # Since go-licenses v2 a path can report more than one identifier, because a LICENSE file can hold more
    # than one license — pgerrcode's MIT is followed by the PostgreSQL license its error data is under. The
    # comparison is strictly greater, so the first wins, and that is deterministic only because the version
    # is pinned. It is also the lesser half of what this file does: the identifier is a summary, while the
    # obligation is discharged by the full LICENSE text below it, which carries every license in the file.
    id=$(awk -v m="$mod" 'index($1, m) == 1 && length($1) >= length(m) {
             if (length($1) > best) { best = length($1); v = $2 }
         } END { print (v == "" ? "UNKNOWN" : v) }' <<<"$ids"$'\n'"$by_hand")
    if [ "$id" = "UNKNOWN" ]; then
        echo "gen-notices: no license identifier for $mod — refusing to write a partial file" >&2
        exit 1
    fi

    {
        printf '================================================================================\n'
        printf '%s %s\n' "$mod" "$ver"
        printf 'SPDX-License-Identifier: %s\n' "$id"
        printf '================================================================================\n\n'
        cat "$lic"
        if notice=$(find_file "$dir" NOTICE NOTICE.txt NOTICE.md); then
            printf '\n---- NOTICE (propagated as Apache-2.0 section 4(d) requires) ----\n\n'
            cat "$notice"
        fi
        printf '\n\n'
    } >> "$tmp"
    count=$((count + 1))
done < <(for b in "${binaries[@]}"; do go version -m "$b"; done | awk '$1 == "dep" { print $2, $3 }' | LC_ALL=C sort -u)

# What the linker cannot report: C compiled into a Go module's own files, libopus in the voice-worker's
# WebAssembly module since M29 (ADR 0036). Each is a directory under the module's internal/notices/embedded/
# holding its name and version, its SPDX identifier, and its licence text verbatim, which the component's
# own build checks against the source it compiled (scripts/build-opus-wasm.sh).
embedded=0
for dir in "$repo/$module_dir"/internal/notices/embedded/*/; do
    [ -d "$dir" ] || continue
    {
        printf '================================================================================\n'
        printf '%s (compiled into this binary)\n' "$(cat "$dir/name")"
        printf 'SPDX-License-Identifier: %s\n' "$(cat "$dir/license")"
        printf '================================================================================\n\n'
        cat "$dir/COPYING"
        printf '\n\n'
    } >> "$tmp"
    embedded=$((embedded + 1))
done

{
    printf 'THIRD-PARTY NOTICES for %s\n' "$(basename "$binary")"
    printf '\n'
    printf 'Norite itself is licensed AGPL-3.0-or-later; see the LICENSE file at the repository root.\n'
    printf 'This file covers the third-party code in this binary, and nothing else. It is generated by\n'
    printf '`just notices` from `go version -m` on the built binary — the linker is the only authority on\n'
    printf 'what actually ships — and from what the binary compiles from C, which no Go tool can see. Do\n'
    printf 'not edit it by hand; CI regenerates it and fails on a diff.\n'
    printf '\n'
    if [ "$embedded" -gt 0 ]; then
        printf '%d third-party modules, and C libraries compiled in from source: %d.\n' "$count" "$embedded"
    else
        printf '%d third-party modules.\n' "$count"
    fi
    printf '\n\n'
    cat "$tmp"
} > "$repo/$out"

echo "  $out ($count modules)"
