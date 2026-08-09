# ADR 0012: Voice (audio) is real v1 functionality, on a self-hosted custom SFU

## Status
Accepted. Supersedes [ADR 0006](0006-voice-deferred-with-seams.md) entirely — the decision to defer real
media is reversed.

## Context
ADR 0006 deferred all real voice/video media to an undetermined later phase. The revised project scope makes
voice calling on every client, including the CLI, real v1 product functionality — not a "coming soon" flag.
Video/screen-share is still genuinely deferred, but architected now so it's additive later.

## Decision
A self-hosted, custom-built SFU on **Pion** (Go) — not LiveKit, not a plain P2P mesh — stays in the all-Go
ecosystem and is scoped exactly to audio-now/video-later. Audio is universal (CLI, GUI, web); video/
screen-share is GUI+web only, never CLI (a terminal can't render video).

A separate **voice-worker subprocess**, spawned on-demand by the daemon (never a persistent idle process),
owns the entire audio session: capture/encode/send, receive/decode/play, noise suppression (RNNoise), echo
cancellation + AGC (`libspeexdsp`), all cgo — the *only* binary in the stack where cgo is allowed. DSP runs
in a strict, non-negotiable order: **Mic Capture → AEC → RNNoise → AGC → Opus Encode** (RNNoise before AEC
would non-linearly distort the signal and break AEC's echo-correlation assumption). The worker holds its own
direct WebRTC connection to the SFU — actual RTP audio never flows through the daemon, only control
signaling does, which is what makes the fault isolation real: a media-pipeline bug can only crash the
worker, never messaging/presence/plugins. `pion/interceptor`'s REMB/TWCC feedback drives adaptive bitrate
into Opus's runtime control, audio-only, no simulcast.

An embedded Go TURN server (`pion/turn`, also answering plain STUN) ships in the backend, so self-hosters
don't need to separately run `coturn`. Voice is a deployment-time opt-out (TURN/SFU need a reachable public
IP and forwarded UDP range) — when disabled, voice UI is hidden entirely in CLI/GUI, never grayed out.

Video/screen-share stays owned directly by the GUI/web client — a second, separate WebRTC connection to the
SFU, never through the daemon. The voice-join payload carries `supports_video: bool` from day one (CLI always
`false`). The SFU's track/participant model is kept codec/track-kind-agnostic so adding video is additive —
but that agnosticism is necessary, not sufficient: simulcast/SVC layer-switching for poor-connection
participants is real, separate engineering work, explicitly scoped into the video milestones rather than
assumed free.

No call recording, ever, anywhere — a permanent non-goal consistent with the project's privacy framing.
Public-matchmaking voice therefore has no recorded evidence for abuse reports; both CLI and GUI voice
surfaces mitigate this with a real-time active-speaker indicator plus separate local-mute and report actions.

## Consequences
- Self-hosting simplicity and voice-in-v1 are in real, unreconciled tension: Postgres-only self-hosting stays
  simple, but the custom SFU/TURN mean self-hosters must handle UDP port ranges and NAT traversal — a
  materially bigger operational burden than text-only, which is exactly why voice is opt-out, not mandatory.
- Mic-permission handoff (which binary actually triggers the OS prompt vs. which one opens the audio device)
  and headless-daemon global-hotkey registration are both unverified per-OS behavior until a dedicated
  throwaway spike milestone determines the real answer on each target OS.
- The daemon respawns the worker and rejoins on crash/restart via a persisted "last active voice channel"
  breadcrumb — the one exception to otherwise-ephemeral daemon state. The client auto-update mechanism must
  defer applying an update while a voice session is active, so it doesn't force a mid-call daemon restart.
- Prometheus voice/SFU metrics (packet loss, bitrate, jitter) feed the adaptive-bitrate loop directly, not
  just a dashboard — voice-call-quality problems are notoriously hard to debug from logs alone.

## Alternatives considered
- **LiveKit** (external SFU process): rejected — stepping outside the all-Go ecosystem for a component this
  central works against the "scope it exactly to what we need" goal Pion gives directly.
- **Plain P2P mesh** (no SFU): rejected — doesn't scale past a handful of participants, and duplicates
  encode/send load per participant on every client.
- **Defer voice further** (keep ADR 0006's original decision): rejected — voice is now core to the project's
  value proposition, not a stretch goal.
