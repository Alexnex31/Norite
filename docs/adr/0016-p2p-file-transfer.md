# ADR 0016: Opt-in P2P (WebRTC) file transfer, client-owned negotiation

## Status
Accepted

## Context
Large-file transfer through server-relayed storage costs bandwidth/storage and hits size limits; a direct
P2P (WebRTC) transfer avoids both, but a direct connection inherently exposes both parties' IP addresses to
each other — a real privacy tradeoff that must be consented to, not silently defaulted into.

## Decision
Explicit opt-in per transfer, never automatic by size threshold — both sender and recipient consent each
time. The default upload path stays server-relayed storage under existing size limits. The initiating attach
client (CLI or GUI) owns the WebRTC negotiation directly — the daemon is not involved, the same rule applied
to video (ADR 0012): "the daemon is only the always-on audio session; every other, one-off/interactive media
use is client-owned."

Consent is enforced as a real three-way handshake, not just a UI convention: the initiator sends a
lightweight, server-relayed "Intent to Transfer" payload first; only after the recipient explicitly accepts
do both clients initialize `RTCPeerConnection` and begin exchanging SDP offers/ICE candidates. Without this
ordering, the initiator's IP — carried in the ICE candidates bundled into the SDP offer — would already have
reached the recipient's machine before any consent was given, defeating the entire point of opt-in.

## Consequences
- A transfer that never gets accepted never exposes anyone's IP — the intent payload itself carries no ICE
  candidates.
- The daemon needs no P2P-specific IPC surface at all, keeping this feature's blast radius confined to
  whichever attach client initiates and receives it.

## Alternatives considered
- **Automatic P2P above a size threshold**: rejected — silently trades away IP privacy based on file size
  alone, with no user awareness that a direct connection (rather than server-relayed) is happening.
- **Negotiate SDP/ICE immediately on transfer initiation, before recipient accepts**: rejected — this is the
  literal IP-leak bug the three-way handshake exists to prevent; standard naive WebRTC signaling would do
  exactly this.
