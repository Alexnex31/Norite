#!/usr/bin/env bash
#
# Check the notice for wasi-libc, which scripts/build-opus-wasm.sh and scripts/build-dsp-wasm.sh both link
# statically into the modules the voice-worker embeds: musl's string and maths functions (MIT), cloudlibc's
# (BSD-2-Clause), dlmalloc (CC0) and wasi-libc's own code, taken under MIT of its three licences. Rule 22:
# no Go tool sees C, so the notice is carried by hand, and this keeps it the text of the libc linked.
#
# wasi-sdk's tarball carries no licence text, so the files are fetched from wasi-libc at the commit the
# pinned wasi-sdk names in its VERSION file, each pinned by hash. libc++, libc++abi and compiler-rt's
# builtins are linked too: they are Apache-2.0 WITH LLVM-exception, whose exception waives the notice for
# code embedded in object form, so no notice is owed for them.
#
# Usage: wasi-libc-notice.sh [wasi-sdk directory]
# With the directory, its VERSION must name the commit below: a new wasi-sdk is a reason to look again.
set -euo pipefail

commit=2e6fb9d8ee0cdf9e431fbcabe8af3115de000a13
files=(
	"LICENSE 2711a8b5a5cdfef0e639f96c1aca12ae23d7d64a02d0507f1bdf14d2b27bbc3a"
	"LICENSE-MIT 23f18e03dc49df91622fe2a76176497404e46ced8a715d9d2b67a7446571cca3"
	"libc-top-half/musl/COPYRIGHT b870108ec5e7790e9f9919064f1b9421d62d5f9b0e6c230c6adf7ea2da62e97b"
	"libc-bottom-half/cloudlibc/LICENSE c8b789cf5a746611e6300a0cc7750dbf92b61912a709d04e639245f7290656d0"
)

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cache=${NORITE_BUILD_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/norite-build}/wasi-libc-$commit
mkdir -p "$cache"
notice=$repo/voiceworker/internal/notices/embedded/wasi-libc

if [[ -n ${1:-} ]]; then
	grep -qx "wasi-libc: ${commit:0:12}" "$1/VERSION" ||
		{ echo "wasi-libc-notice: $1 is not built from wasi-libc ${commit:0:12}; check its licences again" >&2; exit 1; }
fi

texts=()
for entry in "${files[@]}"; do
	read -r path sha <<<"$entry"
	f=$cache/$(tr / _ <<<"$path")
	if [[ ! -f $f ]]; then
		curl -fsSL --retry 3 -o "$f.part" "https://raw.githubusercontent.com/WebAssembly/wasi-libc/$commit/$path"
		mv "$f.part" "$f"
	fi
	echo "$sha  $f" | sha256sum -c --quiet - ||
		{ echo "wasi-libc-notice: $path does not match its pinned hash" >&2; exit 1; }
	texts+=("$f")
done
cat "${texts[@]}" | cmp -s - "$notice/COPYING" && [[ $(cat "$notice/name") == "wasi-libc ${commit:0:12}, in wasi-sdk 34" ]] ||
	{ echo "wasi-libc-notice: $notice does not match wasi-libc ${commit:0:12}'s licences" >&2; exit 1; }
