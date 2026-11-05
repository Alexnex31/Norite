# ADR 0036: Opus runs in WebAssembly, and the voice-worker has no cgo (M29)

## Status
Accepted at M29 (2026-11). **Amends [ADR 0012](0012-voice-in-v1.md)**,
**[ADR 0023](0023-voice-connection-authentication.md)** and **[ADR 0034](0034-mic-permission-and-global-hotkey.md)**:
- **ADR 0012** said the worker's codec and DSP are "all cgo". From M29, Opus is not.
- **ADR 0023** gave AEC3 a home because the worker is "where cgo is permitted". The permission stays, but
  nothing uses it until M30 decides the APM.
- **ADR 0034**'s first consequence said the capture library's cgo stops the worker cross-compiling. The
  capture library chosen here has no cgo.

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
