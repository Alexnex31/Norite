#!/usr/bin/env bash
#
# Build voiceworker/internal/dsp/dsp.wasm: RNNoise and WebRTC's audio processing module (AEC3 and AGC2)
# compiled to WebAssembly, with the worker's shim (M30, ADR 0036). The result is committed and embedded,
# and CI runs this again and fails on any difference, which is why every input is pinned by hash and
# nothing in the build depends on where it runs.
#
# The build is this script's, not the libraries': no meson, no configure. RNNoise's sources are named
# below. The APM's and abseil's are the files in scripts/dsp-wasm/*-sources.txt: exactly the translation
# units the module links, taken once from the meson build's compile_commands.json and the linker's
# --why-extract. A version bump that needs a file the lists lack fails the link with an undefined symbol,
# and a listed file the tarball lacks fails here.
#
# scripts/dsp-wasm/*.patch are applied first: two lines that let the APM's sources recognise wasm32 and
# WASI as they recognise Emscripten without threads, and two that select voiceworker/internal/dsp/csrc's
# SIMD kernels for RNNoise. wasi_stubs.c answers libc's WASI calls inside the module, which therefore
# imports nothing, and a test in internal/dsp holds that.
#
# Usage: build-dsp-wasm.sh [--scalar] [output]
#
# --scalar builds RNNoise's plain-C kernels instead of its SIMD ones, and nothing else differs. That module
# is never committed: internal/dsp's tests compare it with dsp.wasm byte for byte when
# NORITE_DSP_SCALAR_WASM names it, which is how the SIMD port is held to being the same function.
#
# DSP_WASM_CLANG and DSP_WASM_SYSROOT build with another clang and wasi-libc instead of fetching wasi-sdk,
# for development only: the bytes differ from the pinned toolchain's, so CI's check fails on what they
# produce.
set -euo pipefail

rnnoise_version=0.2
# Neither xiph nor GitHub publishes a digest for this asset; this is the one taken when M30 first built it.
rnnoise_sha256=90fce4b00b9ff24c08dbfe31b82ffd43bae383d85c5535676d28b0a2b11c0d37
rnnoise_url=https://github.com/xiph/rnnoise/releases/download/v$rnnoise_version/rnnoise-$rnnoise_version.tar.gz
# The digest freedesktop publishes beside the tarball.
apm_version=2.1
apm_sha256=ae9302824b2038d394f10213cab05312c564a038434269f11dbf68f511f9f9fe
apm_url=https://freedesktop.org/software/pulseaudio/webrtc-audio-processing/webrtc-audio-processing-$apm_version.tar.xz
# The version webrtc-audio-processing 2.1's meson wrap pins, and the digest it names.
abseil_version=20240722.0
abseil_sha256=f50e5ac311a81382da7fa75b97310e4b9006474f9560ac46f54a9967f07d4ae3
abseil_url=https://github.com/abseil/abseil-cpp/releases/download/$abseil_version/abseil-cpp-$abseil_version.tar.gz
# wasi-sdk 34 is LLVM 23.1.0, the same as scripts/build-opus-wasm.sh's.
wasi_sdk=wasi-sdk-34.0-x86_64-linux
wasi_sdk_sha256=b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4
wasi_sdk_url=https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-34/$wasi_sdk.tar.gz

scalar=0
if [[ ${1:-} == --scalar ]]; then
	scalar=1
	shift
fi
# Anything else is refused, rather than read as a path: "output --scalar" would otherwise build the SIMD
# module where the scalar one was asked for, and the test comparing the two would pass on a module
# compared with itself.
if (( $# > 1 )) || [[ ${1:-} == -* ]]; then
	echo "usage: build-dsp-wasm.sh [--scalar] [output]" >&2
	exit 2
fi
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=${1:-$repo/voiceworker/internal/dsp/dsp.wasm}
csrc=$repo/voiceworker/internal/dsp/csrc
lists=$repo/scripts/dsp-wasm
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
		{ echo "build-dsp-wasm: $3 does not match its pinned hash" >&2; exit 1; }
}

fetch "$rnnoise_url" "$rnnoise_sha256" "rnnoise-$rnnoise_version.tar.gz"
fetch "$apm_url" "$apm_sha256" "webrtc-audio-processing-$apm_version.tar.xz"
fetch "$abseil_url" "$abseil_sha256" "abseil-cpp-$abseil_version.tar.gz"
tar -xzf "$cache/rnnoise-$rnnoise_version.tar.gz" -C "$work"
tar -xJf "$cache/webrtc-audio-processing-$apm_version.tar.xz" -C "$work"
tar -xzf "$cache/abseil-cpp-$abseil_version.tar.gz" -C "$work"
rnnoise=$work/rnnoise-$rnnoise_version
apm=$work/webrtc-audio-processing-$apm_version
abseil=$work/abseil-cpp-$abseil_version
patch -s -p1 -d "$rnnoise" <"$lists/rnnoise-vec-wasm.patch"
patch -s -p1 -d "$apm" <"$lists/webrtc-audio-processing-wasi.patch"

# The notices carry each licence by hand, since no Go tool sees C (rule 22), and each must be the text of
# the source compiled here. The APM's tree carries four more components' code, each with its own terms.
notices=$repo/voiceworker/internal/notices/embedded
notice() { # notice <directory> <name> <licence file>...
	cat "${@:3}" | cmp -s - "$notices/$1/COPYING" && [[ $(cat "$notices/$1/name") == "$2" ]] ||
		{ echo "build-dsp-wasm: $notices/$1 does not match $2's licence and version" >&2; exit 1; }
}
notice rnnoise "RNNoise $rnnoise_version" "$rnnoise/COPYING"
notice webrtc-audio-processing "webrtc-audio-processing $apm_version" "$apm/webrtc/LICENSE" "$apm/webrtc/PATENTS"
notice pffft "PFFFT, in webrtc-audio-processing $apm_version" "$apm/webrtc/third_party/pffft/LICENSE"
notice webrtc-rnnoise "RNNoise's voice activity weights, in webrtc-audio-processing $apm_version" \
	"$apm/webrtc/third_party/rnnoise/COPYING"
notice ooura-fft "Ooura's FFT, in webrtc-audio-processing $apm_version" \
	"$apm/webrtc/common_audio/third_party/ooura/LICENSE"
notice spl-sqrt-floor "spl_sqrt_floor, in webrtc-audio-processing $apm_version" \
	"$apm/webrtc/common_audio/third_party/spl_sqrt_floor/LICENSE"
notice abseil "Abseil $abseil_version" "$abseil/LICENSE"

if [[ -n ${DSP_WASM_CLANG:-} ]]; then
	clang=$DSP_WASM_CLANG
	sysroot=${DSP_WASM_SYSROOT:?set it beside DSP_WASM_CLANG}
	echo "build-dsp-wasm: building with $clang, which is not the pinned toolchain" >&2
else
	[[ $(uname -sm) == "Linux x86_64" ]] ||
		{ echo "build-dsp-wasm: the pinned wasi-sdk is for Linux on x86-64" >&2; exit 1; }
	fetch "$wasi_sdk_url" "$wasi_sdk_sha256" "$wasi_sdk.tar.gz"
	tar -xzf "$cache/$wasi_sdk.tar.gz" -C "$work"
	clang=$work/$wasi_sdk/bin/clang
	sysroot=$work/$wasi_sdk/share/wasi-sysroot
fi

# -ffile-prefix-map keeps the temporary directory's name out of the output. The defines are what the meson
# build passes, less what concerns shared libraries: WASI's emulated signal and clock headers, which the
# sources include and never call, and libc++'s fast hardening, which turns an out-of-bounds access in the
# C++ library into a trap.
common=(--target=wasm32-wasip1 --sysroot="$sysroot" -O2 -g0 -msimd128 -fno-exceptions
	"-ffile-prefix-map=$work=." "-ffile-prefix-map=$repo=."
	-D_FILE_OFFSET_BITS=64 -DWEBRTC_POSIX -D_WASI_EMULATED_SIGNAL -D_WASI_EMULATED_PROCESS_CLOCKS
	-Wno-everything)
cxx=(-x c++ -std=c++17 -D_LIBCPP_HARDENING_MODE=_LIBCPP_HARDENING_MODE_FAST)
webrtc=(-DNDEBUG -DWEBRTC_LIBRARY_IMPL -DWEBRTC_ENABLE_SYMBOL_EXPORT -D_WINSOCKAPI_ -I"$apm/webrtc" -I"$abseil")
mkdir "$work/obj"
objs=()

# compile <source> <flags>...: one object, in the background, nproc at a time.
compile() {
	local o
	o=$work/obj/$(tr / _ <<<"${1#"$work"/}").o
	"$clang" "${common[@]}" "${@:2}" -c "$1" -o "$o" &
	objs+=("$o")
	while (( $(jobs -r | wc -l) >= $(nproc) )); do wait -n; done
}

# lang <source>: sets $lang to the flags for C or C++, by extension.
lang() {
	case $1 in
	*.cc) lang=("${cxx[@]}") ;;
	*.c) lang=(-std=c11) ;;
	*) echo "build-dsp-wasm: $1 is neither C nor C++" >&2; exit 1 ;;
	esac
}

while read -r f; do
	[[ -f $abseil/$f ]] || { echo "build-dsp-wasm: abseil $abseil_version has no $f" >&2; exit 1; }
	lang "$f"
	compile "$abseil/$f" "${lang[@]}" -I"$abseil"
done <"$lists/abseil-sources.txt"

while read -r f; do
	[[ -f $apm/$f ]] || { echo "build-dsp-wasm: webrtc-audio-processing $apm_version has no $f" >&2; exit 1; }
	# Each meson target adds its own directory to the include path, and the APM's adds the debug dump's
	# switch, which is off.
	case $f in
	webrtc/modules/*) target=(-I"$apm/webrtc/modules/audio_processing" -DWEBRTC_APM_DEBUG_DUMP=0) ;;
	webrtc/third_party/pffft/*) target=(-I"$apm/webrtc/third_party/pffft" -D_GNU_SOURCE) ;;
	webrtc/third_party/rnnoise/*) target=(-I"$apm/webrtc/third_party/rnnoise") ;;
	webrtc/rtc_base/* | webrtc/api/* | webrtc/system_wrappers/* | webrtc/common_audio/*)
		target=(-I"$apm/$(cut -d/ -f1-2 <<<"$f")") ;;
	*) echo "build-dsp-wasm: $f belongs to no target this script knows" >&2; exit 1 ;;
	esac
	lang "$f"
	compile "$apm/$f" "${lang[@]}" "${webrtc[@]}" "${target[@]}"
done <"$lists/apm-sources.txt"

rn=(-std=c11 -I"$rnnoise/include" -I"$rnnoise/src" -I"$csrc/rnnoise")
(( scalar )) && rn+=(-DDISABLE_WASM_SIMD)
for f in celt_lpc.c denoise.c kiss_fft.c nnet.c nnet_default.c parse_lpcnet_weights.c pitch.c rnn.c \
	rnnoise_data.c rnnoise_tables.c; do
	compile "$rnnoise/src/$f" "${rn[@]}"
done

compile "$csrc/shim.cc" "${cxx[@]}" "${webrtc[@]}" -I"$apm/webrtc/modules/audio_processing" -I"$rnnoise/include"
compile "$csrc/wasi_stubs.c" -std=c11
wait

# A reactor: no main, and _initialize runs the constructors. The stack is 1 MiB, as the spike gave AEC3;
# memory starts where the link puts it, and the module declares 8 MiB at most, twice what a chain holds
# once it has run, which the worker caps too (internal/dsp). The objects are linked in sorted order, so
# the bytes do not depend on the order the jobs finished in.
mapfile -t sorted < <(printf '%s\n' "${objs[@]}" | LC_ALL=C sort)
"$clang"++ "${common[@]}" -mexec-model=reactor -Wl,--strip-all -Wl,--gc-sections \
	-Wl,-z,stack-size=1048576 -Wl,--max-memory=8388608 \
	"${sorted[@]}" -o "$work/dsp.wasm"
mv "$work/dsp.wasm" "$out"
echo "build-dsp-wasm: $(sha256sum "$out" | cut -c1-64)  $out" >&2
