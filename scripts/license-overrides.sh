#!/usr/bin/env bash
#
# The dependencies whose license was read by hand (contracts/license-overrides.txt), for the three things
# that run go-licenses: the allow-list check, the committed inventory, and each binary's notices. One
# script, so the justfile, ci.yml and gen-third-party-notices.sh cannot treat the list differently.
#
# Usage:
#   license-overrides.sh flags <module-dir>   the --ignore flags for `go-licenses check` in that module,
#                                             after checking each listed module's LICENSE still has the
#                                             recorded hash
#   license-overrides.sh inventory            a filter: go-licenses report rows in, the same rows out with
#                                             the listed modules' Unknown rows replaced by what was read;
#                                             fails if any Unknown is left
#   license-overrides.sh ids                  "module identifier" lines, for the notices
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
list="$repo/contracts/license-overrides.txt"

rows() { grep -v '^#' "$list" | grep -v '^[[:space:]]*$'; }

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
    else shasum -a 256 "$1" | awk '{print $1}'
    fi
}

case "${1:-}" in
flags)
    module_dir=${2:?usage: license-overrides.sh flags <module-dir>}
    cd "$repo/$module_dir"
    while IFS=, read -r mod _url _id want; do
        # A module this one does not depend on needs no flag, and has nothing here to check.
        dir=$(GOWORK=off go list -m -f '{{.Dir}}' "$mod" 2>/dev/null) || continue
        if [ -z "$dir" ]; then
            GOWORK=off go mod download "$mod" >&2
            dir=$(GOWORK=off go list -m -f '{{.Dir}}' "$mod")
        fi
        got=$(sha256_of "$dir/LICENSE")
        if [ "$got" != "$want" ]; then
            echo "license-overrides: $mod's LICENSE is not the file that was read ($got, recorded $want)." >&2
            echo "  Read $dir/LICENSE, and if its terms are on ADR 0032's list, record the new hash in" >&2
            echo "  contracts/license-overrides.txt." >&2
            exit 1
        fi
        printf -- '--ignore %s ' "$mod"
    done < <(rows)
    echo
    ;;
inventory)
    tmp=$(mktemp) && trap 'rm -f "$tmp"' EXIT
    cat > "$tmp"
    while IFS=, read -r mod url id _sha; do
        # Only what go-licenses could not name is replaced, and only for a module actually reported: an
        # override nothing depends on adds no row.
        if grep -q "^$mod\(/[^,]*\)\?,Unknown,Unknown$" "$tmp"; then
            grep -v "^$mod\(/[^,]*\)\?,Unknown,Unknown$" "$tmp" > "$tmp.next" || true
            printf '%s,%s,%s\n' "$mod" "$url" "$id" >> "$tmp.next"
            mv "$tmp.next" "$tmp"
        fi
    done < <(rows)
    if grep ',Unknown' "$tmp" >&2; then
        echo "license-overrides: the rows above have no license go-licenses can name. Read the module's" >&2
        echo "  LICENSE; contracts/license-overrides.txt says when a module may be listed there." >&2
        exit 1
    fi
    cat "$tmp"
    ;;
ids)
    rows | awk -F, '{print $1, $3}'
    ;;
*)
    echo "usage: license-overrides.sh flags <module-dir> | inventory | ids" >&2
    exit 2
    ;;
esac
