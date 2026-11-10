# ADR 0036: Opus runs in WebAssembly, and the voice-worker has no cgo (M29)

## Status
Accepted at M29 (2026-11). **Amends [ADR 0012](0012-voice-in-v1.md)**,
**[ADR 0023](0023-voice-connection-authentication.md)** and **[ADR 0034](0034-mic-permission-and-global-hotkey.md)**:
- **ADR 0012** said the worker's codec and DSP are "all cgo". From M29, Opus is not.
- **ADR 0023** gave AEC3 a home because the worker is "where cgo is permitted". The permission stays, but
  nothing uses it until M30 decides the APM.
- **ADR 0034**'s first consequence said the capture library's cgo stops the worker cross-compiling. The
  capture library chosen here has no cgo.

**Amended at M30 (2026-11)**: the audio DSP, RNNoise with WebRTC's APM, runs in WebAssembly too, in one
module, for the same reasons; [The DSP (M30)](#the-dsp-m30) below records it. The worker still has no cgo.

`architecture.md` §6 and §16 point here.

## Context
M29's entry named `hraban/opus`, a cgo binding, and left open two questions that cgo makes expensive.

**The release.**
- Every binary is cross-compiled on one Linux runner, with `CGO_ENABLED=0`, by goreleaser OSS.
- cgo for darwin does not cross-compile there without Apple's SDK, whose licence is unclear on a Linux
  runner.
- goreleaser OSS has no way to take a binary built elsewhere: the `prebuilt` builder and split/merge are
  Pro features.
- A cgo worker therefore means rebuilding the release around a macOS runner, or a wrapper that copies a
  binary in instead of building it.

**What the decoder reads** (M28's `/security-sweep`, the M29 entry):
- The SFU never reads a payload, so the decoder parses bytes another participant chose.
- From M33 that participant may be on a stranger's instance, and in public matchmaking anybody.
- The worker runs as the account. A decoder bug that runs code reaches what the account reaches, the
  daemon's attach socket and the credential store included.
- Confining a native worker takes one mechanism per OS:
  - Landlock and seccomp on Linux;
  - a sandbox profile on macOS that leaves TCC's attribution to the daemon intact;
  - a restricted token on Windows that still reaches the audio service.

  That is three mechanisms, two of them hard to test from one machine.

WebAssembly answers both questions at once, provided it is fast enough. That was measured before deciding.

## Decision
**libopus 1.6.1 is compiled to `wasm32-wasip1` and runs under wazero, in the worker. The worker is built
with `CGO_ENABLED=0`, like every other binary.**

- **The module imports nothing.**
  - Built with `--gc-sections`, libopus with a non-variadic shim reaches no WASI call.
  - It is instantiated with no host module at all, not even `wasi_snapshot_preview1`.
  - Its only window on the process is its own linear memory, capped with `WithMemoryLimitPages`.
- **One instance per encoder and per decoder.** A decoder that traps is discarded, and that source's next
  packet starts a fresh one. A trap never ends the call, and one stream's bytes never reach another
  stream's decoder state. That holds in time too: when a slot changes sender, its decoder is a new
  instance, not the old one asked to reset. An instance the previous sender took over could ignore the
  request and speak for the next sender (M29's `/security-sweep`).
- **The module is compiled once per worker start, while ICE runs, and never cached on disk.** A cache
  would be compiled native code that the account can write, and the worker would load it.
- **A call into an instance cannot be interrupted.**
  - wazero's `WithCloseOnContextDone` checks for cancellation in every loop and call, and it measured five
    times the cost of a frame: 2.9 ms against 0.59.
  - A decoder that never returns is bounded by the session instead. A watchdog ends the call, and the
    worker with it, when a decode outlasts its bound.
  - libopus's work per packet is bounded by the format, so only a bug reaches the watchdog. What a
    sender gains by finding one is ending the call it is in.
- **The `.wasm` is built reproducibly, committed, and embedded.**
  - It is built by wasi-sdk, pinned by version and hash, from the xiph tarball checked against its
    `SHA256SUMS`.
  - CI rebuilds it and fails on a different hash, as it does for the notices.
  - libopus (BSD-3-Clause) is added to the notices by hand, since `go-licenses` cannot see it.
- **Capture and playback are pure Go**, one backend per OS:
  - **Linux:** the PulseAudio protocol, which PipeWire serves through `pipewire-pulse` on every
    mainstream desktop. An ALSA-only system is not supported at M29.
  - **Windows:** WASAPI through `syscall` and COM.
  - **macOS:** AudioQueue through `ebitengine/purego`. Apple's own framework is loaded into the worker,
    and it reads the local device, never the network.
- **A cgo implementation of the same interface is kept**, built from the same shim against a native
  libopus, behind a build tag no release passes. It is the native side of the benchmark below, and the
  golden and fuzz tests run against both. Switching to native means changing the default.

### The measurement
Intel Core Ultra 7 258V, pinned to one performance core.
- Workload, per 20 ms mono frame at 48 kHz:
  - 1 encode at 64 kbps, complexity 10, with FEC and DTX;
  - 3 decodes;
  - a 3-way mix.
- Both sides use the same libopus source, defines and compiler. Each WASM call includes copying the
  samples and the packet across the linear-memory boundary.

| µs per frame, p50 / p99 | encode | decode ×1 | whole frame | of one core |
|---|---|---|---|---|
| native, SSE–AVX2 chosen at run time | 166 / 199 | 22.7 / 29.9 | 229 / 270 | 1.15% |
| WASM, `-msimd128` | 470 / 534 | 46.4 / 58.0 | 599 / 678 | 3.00% |

- **Throughput:** WASM is 2.6× native, 370 µs more per frame.
- **Latency:** at a call's own cadence, one frame every 20 ms, the core slows between frames. The
  difference is then 0.9 ms at p50 and 1.4 ms at p99 of end-to-end latency, against a budget of 60–120 ms.
- **Start-up:** compiling the module takes 97 ms cold, once per call.
- **Memory:** the process holds 9.5 MB more resident memory.
- **Threshold:** the one set before measuring was 5% of one core, so B passes.

**Measured again on the finished pipeline (M29).** Two callers speaking through M26's SFU, measured by the
process's CPU time, codec, Pion and SRTP together, at a call's cadence:

| | WebAssembly | native |
|---|---|---|
| per caller, share of one core | 8.6–8.9% | 4.1–4.3% |

- This is twice the codec's own 3%, because between frames the core drops its clock. A profile puts 85%
  of the WebAssembly call in wazero's compiled code.
- Complexity 8 encodes exactly as 10 does, and 6 brings the call to 7.3%.
- In the module, wazero 1.12 measured 584 µs per frame against the spike's 599 µs.

### When to switch to native
On the least capable machine supported, the worker switches to native if either measurement passes its
bound:
- the whole frame passes 10% of one core, measured as above for the codec alone, unpaced (`BenchmarkFrame`);
- the latency added at call cadence passes 5 ms at p99.

A whole call's share of a core is not that measurement. It includes Pion, and the clock the core runs at
between frames. It is reported beside the threshold (`TestWhatACallCosts`) and does not trigger the switch
on its own.

Before switching, the cheaper lever is the encoder's complexity: the encoder is 80% of the frame.
Switching brings back what this record avoids:
- the release problem above;
- the confinement question, since the ledger entry M29 writes reopens on native code reading network
  bytes.

## Consequences
- **The release does not change.** `norite-voice` cross-compiles with the rest, and `just release-dry-run`
  builds it for all six targets as before.
- **The worker is not confined by the OS at M29**, and the ledger says so with its reopening conditions.
  What reads a stranger's bytes is either:
  - Go: the RTP and the jitter buffer;
  - or the decoder inside wazero, which can reach its own memory and nothing else.

  What remains is a bug in wazero's compiler, a smaller and differently-owned surface than libopus.
- **M30 inherits the question for the DSP.**
  - RNNoise is C and compiles to WASM the same way.
  - The APM is C++ with abseil and will be harder. M30 spikes it before choosing.
  - A native APM reads only the local microphone and the decoded far end, not the network's bytes. It
    would still bring back the release problem, and the ledger entry names it.
- **M31 drives the encoder's bitrate through the shim**, not through `hraban/opus`.
- **The cgo permission in CLAUDE.md stays** for M30. It is unused while the worker has none.
- **The codec is only as correct as wazero's compiler for each architecture**, and M29's macOS pass found
  one wrong.
  - On arm64, wazero v1.12.0 mistranslated libopus's `silk_noise_shape_quantizer_del_dec` built at
    `-O2`. Every encode at complexity 5 or more trapped from the second frame on, and the call carried no
    audio. It was the same on macOS and on Linux, the latter under `qemu-aarch64`.
  - Its interpreter ran the module correctly, as did its amd64 compiler. No Go version from 1.25 to 1.27,
    no SIMD and no bulk memory changed the outcome; the file's optimization level did.
  - **`silk/NSQ_del_dec.c` is built at `-Os`**, which avoids it (`scripts/build-opus-wasm.sh`).
  - Because a mistranslation need not trap, **`TestTheCompilerAgreesWithTheInterpreter`** runs every codec
    path through both engines and requires the same bytes. With the old module it failed on arm64; with
    the new one it passes.
  - **CI runs the voice-worker's tests on an arm64 runner** as well, since every OS's release has an
    arm64 build and the amd64 suite could not have seen this.
  - The interpreter is no fallback: it takes about 30 ms to encode a 20 ms frame.
  - **The cause is wazero's, and it is fixed there but not released.** wazero
    [#2508](https://github.com/tetratelabs/wazero/pull/2508), merged on 2026-06-22 as `89079b9`, keeps an
    `i32.rem` result from being overwritten under register pressure on arm64. The function computes its
    ring-buffer indices with `%` and takes 26 parameters. Run under `qemu-aarch64`, the `-O2` module traps
    at `89079b9`'s parent and passes at `89079b9` and at `main` (2026-10-19). v1.12.0 is still the latest
    release.
  - **The workaround goes when a release carries the fix**: `voiceworker/go.mod` moves to it, the `-Os`
    leaves the build script, and the differential test and the arm64 job say whether that was right. The
    worker does not take a pseudo-version of `main` meanwhile, which would ship a compiler no release has
    fixed in place.

## Alternatives rejected
- **cgo throughout, as the entry said** (`hraban/opus`, miniaudio through `malgo`). It is faster, and
  Discord's native clients do the same with a C++ engine on libwebrtc. Its costs are the release problem
  and three confinement mechanisms. It is kept within reach rather than chosen.
- **WASM Opus with cgo capture.** It keeps the release problem in full, for half the benefit.
- **A published `.wasm`** (`jj11hh/opus`, MIT). Nobody can say what built it. Used as a reference only.
- **A pure-Go Opus.**
  - The ports found at M29 decode only part of the format, and none encodes.
  - A libopus transpiled to Go (by ccgo, as `modernc.org/sqlite` is) keeps C's memory model behind
    `unsafe`, so it would confine nothing.

## The DSP (M30)

### Context
- **M30's entry** wires RNNoise and WebRTC's Audio Processing Module, AEC3 and AGC2, in a fixed order:
  capture → AEC → RNNoise → AGC → encode. ADR 0036 left their place to M30.
- **A native APM would bring back what this record avoids.** It would bring back the release problem.
  It would also reopen the ledger's confinement entry, since the APM reads the decoded far end, which is
  a stranger's audio.
- **Decided by the author before anything was measured** (2026-11-09):
  - the threshold: 10% of one core per caller, unpaced, for RNNoise and the APM together, measured as
    `BenchmarkFrame` measures Opus;
  - if the APM would not build for WebAssembly, a reduced AEC in WebAssembly, and not a native APM.

### Decision
**One `dsp.wasm` holds RNNoise 0.2, webrtc-audio-processing 2.1 with abseil 20240722.0, a C++ shim and
`wasi_stubs.c`. It runs under wazero in the worker, one instance per call.**

- **The build** is `scripts/build-dsp-wasm.sh`:
  - wasi-sdk 34 and the three tarballs, each pinned by hash;
  - no meson;
  - the 203 source files the shim reaches, computed as a closure and committed beside the script;
  - three one-line patches, which treat wasi as the code already treats Emscripten without threads.

  The module is committed and embedded. CI rebuilds it and fails on a different byte (`just dsp-wasm`,
  `just dsp-wasm-check`).
- **It imports nothing**, like libopus's module.
  - wasi-libc keeps all its `__wasi_*` wrappers in one object, so one reference pulls in every import.
    `wasi_stubs.c` therefore defines all 22 that libc references before `--gc-sections`:
    - a clock that never moves;
    - writes that go nowhere;
    - exit as a trap.
  - The spike's output with the stubs was byte-identical to the one reading the real clock over 20 s.
  - The memory cap is 8 MiB, the module's own declared limit and the worker's.
- **RNNoise has a WebAssembly SIMD port**, `vec_wasm.h`. It is bit-exact against the plain-C path by
  construction:
  - per lane, the same operations in the same order;
  - no FMA;
  - standard SIMD128 only, never relaxed-simd.

  CI's codegen job builds the plain-C module and holds the two byte for byte (`just dsp-simd-check`).
  Offering the file to xiph is the author's step.
- **The components are exported one by one, and the order is the worker's, in Go**, held by a test that
  fails when two of them are swapped. The APM's own chain has no place for RNNoise between its echo
  canceller and its gain, so the instance holds two `AudioProcessing` objects:
  - one for AEC3 and its high-pass filter;
  - one for AGC2's adaptive digital gain, never the OS microphone's volume.

  AEC3's stream delay is never set, so it starts from its default and estimates.
- **The DSP runs in `send`**, the one goroutine that enters the instance.
  - **The reference:** the mix writes what the speakers played into a ring. Each captured frame is
    stamped with the ring's count, and `send` renders up to the stamp before the frame.
  - **The stamp is what keeps a sender running behind from rendering ahead.** AEC3 resets its delay
    estimate when unread render stays above 32 ms for a second (`DetectExcessRenderBlocks`).
  - **Nothing is dropped before the DSP**, since AEC3 pairs the two directions by order and count. A
    sender behind skips the encode instead, never twice in a row. The RTP timestamp is the frame's
    capture number, so a skip leaves a gap the far end reads as DTX.
- **What fails, how:**
  - **A call into the chain that fails or traps drops it for the rest of the call**, and the frame goes
    as captured. The reference is a stranger's audio, so ending the call on its account would let anybody
    end everybody's.
  - **A call that never returns, the chain's construction included, ends the call**, through the session's
    watchdog.
  - **A DSP that keeps the sender four frames behind for 25 frames in a row is dropped.** Skipping cannot
    drain a queue the DSP alone keeps full, and a far end sent half the frames hears nothing.
- **It compiles at every call start, beside the codec, and is not awaited.** It takes about 200 ms, twice
  the codec, and `send` takes it up when it is ready. It is never cached on disk, for the codec's reason.
- **The settings:**
  - three keys, `voice.echo_cancellation`, `voice.noise_suppression` and `voice.auto_gain`, all on by
    default, read by the client;
  - carried in the join's body and in the worker's `Join`.

  With noise suppression off, RNNoise still runs for its voice probability, and its output is discarded.
- **The audio level is computed after AGC2**, on the frame as sent. Its V flag is set when RNNoise's
  probability reaches 0.5.
- **No native twin** (unlike Opus). The spike measured native and that measurement is below. A committed
  twin would bring cgo, a C++ toolchain and abseil into the worker's test builds, and it is the very thing
  whose arrival reopens the confinement entry. A later milestone that wants one adds a tag and a
  `native.go`.
- **wazero v1.12.0 caches its version in an unlocked variable** (`GetWazeroVersion`), which the race
  detector caught when the codec's and the DSP's runtimes were made on two goroutines.
  `internal/wasmrt.Warm` makes one runtime first, in both packages. Reporting it upstream is the
  author's call.

### The measurement
Same machine as above, pinned to one core.

| Per 10 ms, µs, p50 | native, SIMD | native, no SIMD | WebAssembly | share of one core, WebAssembly |
|---|---|---|---|---|
| AEC3 + AGC2 (the APM) | 44 | 120 | 344–349 | 3.5% |
| RNNoise, plain C | — | 254 | 555–558 | 5.6% |
| RNNoise, `vec_wasm.h` | 87 | — | 150 | 1.5% |

- **The chain as built: 5.05% of one core per caller, unpaced**, with p50 1,011 µs and p99 1,160 µs per
  20 ms frame (`BenchmarkChain`). That is half the threshold. Without RNNoise's port it would have been
  9.1%.
- **Start: 176 ms** to compile the module and make one chain (`BenchmarkStart`). Making a chain alone
  takes 1 to 3 ms, and up to 8 under `-race`.
- **Memory: 3.5 MiB** after 10 s.
- **Delay: 1,385 samples, 28.9 ms**, which `dsp.Delay` states and a test holds:
  - RNNoise accounts for 20 ms: one chunk of overlap-add, and one because 0.2 applies each chunk's gains
    to the chunk before it;
  - AEC3 accounts for about 9 ms.

  Mouth to ear in process goes from M29's 38–44 ms to 69–73 ms. The latency budget went from 90 to
  120 ms, and 160 ms with loss, and its test runs without `-race`.
- **A whole call, paced, costs 19.9% of one core per caller** (`TestWhatACallCosts`), against M29's 8.6%.
  It is reported, not a trigger, as above.
- **What it does,** on recorded speech (CC0 and public domain, stored as Opus) and a synthetic room:
  - far end alone: AEC before RNNoise removes 42–45 dB, the other order 21–29 dB;
  - two callers through M26's SFU: the first hears themselves about 60 dB below what the second played
    with AEC alone, and 74–75 dB below, or nothing, with the whole chain;
  - a starved sender: about 60 dB, where M29's drop-before-the-encoder queue left 13;
  - rain under a reader: the floor drops 51.5 dB in a pause, and speech stays within 1.6 dB;
  - AGC2 lifts a quiet speaker from 52.8 to 30.6 −dBov;
  - V is set on 83% of speech packets and on none of rain's.
- **The compiler agrees with the interpreter byte for byte**, on amd64 and on `test-arm64`
  (`TestTheCompilerAgreesWithTheInterpreter`). The interpreter takes 21 ms for the APM's 10 ms and 14 ms
  for RNNoise's. The test took 345 s under `-race`, which is why both differential tests run in a step
  without it.
- **Not measured:** the Mac, which ran M29's call 1.7 times dearer, and arm64's cost.

### When to change
On the least capable machine supported, if the chain passes the 10% threshold, measured as above:
1. **First, the APM's own SIMD gap**, 2.7×: `simd128` kernels for AEC3's matched filter and adaptive FIR,
   and for pffft.
2. **Then a narrower chain**, by default or by setting.
3. **Native last.** It brings back the release problem and reopens the confinement entry, as above.

16 and 32 kHz are not a lever: AEC3 filters in its 16 kHz band whatever the rate, and they saved 1–6%.

### Consequences
- **The worker still has no cgo, the release does not change, and the confinement entry stays closed.**
  The cgo permission in CLAUDE.md is unused.
- **The notices gained RNNoise, the APM and its PATENTS file, PFFFT, Ooura's FFT, `spl_sqrt_floor`, the
  APM's copy of RNNoise's weights, and abseil.** They also gained wasi-libc, which both modules link and
  which was missing since M29. `scripts/wasi-libc-notice.sh` fetches its licences at the commit the
  pinned wasi-sdk names.
- **M34 inherits mute and deafen against the DSP:**
  - deafen makes the mix play silence, so nothing stale is cancelled;
  - mute must keep the chain running and stop only the sending, or AEC3 loses its convergence.
- **M34 also owns the settings' verbs, and a DSP state the worker reports.** Today the status says what
  the join asked, and a DSP dropped or never started is in the worker's log alone.
- **M35's voice detection reads RNNoise's probability**, which runs whatever the settings, and adds the
  hysteresis the V flag lacks.

### Alternatives rejected
- **A native APM, by cgo.** The APM costs 7.9 times its native self in WebAssembly, and buys the release
  problem and the confinement question back.
- **A reduced AEC built from AEC3's components.** It was the fallback, and is not needed: the whole APM
  builds, abseil's synchronization included, against wasi-libc's single-threaded stubs.
- **The APM's own noise suppressor instead of RNNoise.** The entry names RNNoise, and the APM's chain has
  no place for it between AEC and AGC, which is why the order lives in Go.
- **Rendering everything played before each frame, without a stamp.** It was built first, and no test
  saw a difference until one held a sender behind for a second; AEC3 then resets.
- **meson in the build.** Its build files cannot be read as text the way libopus's lists can.

