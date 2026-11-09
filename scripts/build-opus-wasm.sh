#!/usr/bin/env bash
#
# Build voiceworker/internal/opus/opus.wasm: libopus compiled to WebAssembly, with the worker's shim
# (ADR 0036). The result is committed and embedded, and CI runs this again and fails on any difference,
# which is why both inputs are pinned by hash and nothing in the build depends on where it runs.
#
# The build is this script's, not libopus's: its configure, CMake and Meson files are not run. The source
# lists come from libopus's own *_sources.mk, read as text, and the configuration is the defines below:
# floating point, no DNN (no DRED, deep PLC or OSCE), C99 variable-length arrays for scratch space.
#
# The module imports nothing. Linked with --gc-sections, libopus and the shim reach no WASI call, so the
# worker instantiates it with no host module, and a test in internal/opus holds that.
#
# Usage: build-opus-wasm.sh [output]
#
# OPUS_WASM_CLANG and OPUS_WASM_SYSROOT build with another clang and wasi-libc (a distro's, for instance)
# instead of fetching wasi-sdk. That is for development only: the bytes differ from the pinned toolchain's,
# so CI's check fails on what it produces until the pinned build replaces it.
set -euo pipefail

opus_version=1.6.1
opus_sha256=6ffcb593207be92584df15b32466ed64bbec99109f007c82205f0194572411a1
opus_url=https://downloads.xiph.org/releases/opus/opus-$opus_version.tar.gz
# wasi-sdk 34 is LLVM 23.1.0. The digest is the one GitHub publishes for the release asset.
wasi_sdk=wasi-sdk-34.0-x86_64-linux
wasi_sdk_sha256=b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4
wasi_sdk_url=https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-34/$wasi_sdk.tar.gz

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=${1:-$repo/voiceworker/internal/opus/opus.wasm}
cache=${NORITE_BUILD_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/norite-build}
mkdir -p "$cache"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# fetch <url> <sha256> <file in the cache>: downloads once, and checks the hash every time.
fetch() {
	if [[ ! -f $cache/$3 ]]; then
		curl -fsSL --retry 3 -o "$cache/$3.part" "$1"
		mv "$cache/$3.part" "$cache/$3"
	fi
	echo "$2  $cache/$3" | sha256sum -c --quiet - ||
		{ echo "build-opus-wasm: $3 does not match its pinned hash" >&2; exit 1; }
}

fetch "$opus_url" "$opus_sha256" "opus-$opus_version.tar.gz"
tar -xzf "$cache/opus-$opus_version.tar.gz" -C "$work"
src=$work/opus-$opus_version

# The notices carry libopus's licence by hand, since no Go tool sees it; it must be this source's.
notice=$repo/voiceworker/internal/notices/embedded/libopus
cmp -s "$src/COPYING" "$notice/COPYING" && [[ $(cat "$notice/name") == "libopus $opus_version" ]] ||
	{ echo "build-opus-wasm: $notice does not match libopus $opus_version's COPYING and version" >&2; exit 1; }

if [[ -n ${OPUS_WASM_CLANG:-} ]]; then
	clang=$OPUS_WASM_CLANG
	sysroot=${OPUS_WASM_SYSROOT:?set it beside OPUS_WASM_CLANG}
	echo "build-opus-wasm: building with $clang, which is not the pinned toolchain" >&2
else
	[[ $(uname -sm) == "Linux x86_64" ]] ||
		{ echo "build-opus-wasm: the pinned wasi-sdk is for Linux on x86-64" >&2; exit 1; }
	fetch "$wasi_sdk_url" "$wasi_sdk_sha256" "$wasi_sdk.tar.gz"
	tar -xzf "$cache/$wasi_sdk.tar.gz" -C "$work"
	clang=$work/$wasi_sdk/bin/clang
	sysroot=$work/$wasi_sdk/share/wasi-sysroot
fi

# list <mk file> <variable>: the files a make variable names, one per line.
list() {
	sed -n "/^$2 = *\\\\\$/,/^\$/p" "$src/$1" | sed '1d;s/\\$//;s/^[[:space:]]*//;/^$/d'
}
files=$( { list opus_sources.mk OPUS_SOURCES; list opus_sources.mk OPUS_SOURCES_FLOAT
	list celt_sources.mk CELT_SOURCES; list silk_sources.mk SILK_SOURCES
	list silk_sources.mk SILK_SOURCES_FLOAT; } | LC_ALL=C sort)
[[ $(wc -l <<<"$files") -gt 100 ]] ||
	{ echo "build-opus-wasm: read too few sources from libopus's lists" >&2; exit 1; }

# -ffile-prefix-map keeps the temporary directory's name out of the output; -msimd128 lets the compiler
# vectorize, which measured 6% faster per frame than without (ADR 0036).
cflags=(--target=wasm32-wasip1 --sysroot="$sysroot" -O2 -g0 -msimd128
	-ffile-prefix-map="$work"=. -ffile-prefix-map="$repo"=.
	-DOPUS_BUILD -DVAR_ARRAYS -DHAVE_LRINTF -DHAVE_LRINT
	-I"$src/include" -I"$src/celt" -I"$src/silk" -I"$src/silk/float" -I"$src/src" -I"$src/dnn"
	-Wno-everything)
mkdir "$work/obj"
objs=()
# silk/NSQ_del_dec.c is built for size. At -O2, wazero v1.12.0's arm64 compiler mistranslates its
# silk_noise_shape_quantizer_del_dec: from the second frame on, every encode at complexity 5 or more traps
# with an out-of-bounds access. Its interpreter, and its amd64 compiler, run the same module correctly.
# Found by M29's macOS pass and reproduced under qemu-aarch64. -O0, -O1 and -Os all avoid it, and -O3
# does not; the encoder's other files are unaffected. internal/opus's differential test runs every path
# of the codec through the compiler and the interpreter and requires the same bytes, on each architecture
# CI runs, which is what makes this a workaround rather than a hope.
for f in $files; do
	o=$work/obj/$(tr / _ <<<"$f").o
	extra=()
	[[ $f == silk/NSQ_del_dec.c ]] && extra=(-Os)
	"$clang" "${cflags[@]}" "${extra[@]}" -c "$src/$f" -o "$o" &
	objs+=("$o")
	while (( $(jobs -r | wc -l) >= $(nproc) )); do wait -n; done
done
wait
"$clang" "${cflags[@]}" -c "$repo/voiceworker/internal/opus/shim.c" -o "$work/obj/shim.o"

# A reactor: no main, and _initialize runs the constructors. Memory starts at 1 MiB, of which the stack is
# 256 KiB, and the module declares 4 MiB at most; the worker caps it there too (internal/opus).
"$clang" "${cflags[@]}" -mexec-model=reactor -Wl,--strip-all -Wl,--gc-sections \
	-Wl,-z,stack-size=262144 -Wl,--initial-memory=1048576 -Wl,--max-memory=4194304 \
	"${objs[@]}" "$work/obj/shim.o" -o "$work/opus.wasm"
mv "$work/opus.wasm" "$out"
echo "build-opus-wasm: $(sha256sum "$out" | cut -c1-64)  $out" >&2
