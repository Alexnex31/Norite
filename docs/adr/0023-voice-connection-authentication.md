# ADR 0023: Voice connections authenticate through WebRTC itself, not a bearer token

## Status
Accepted. Amends [ADR 0012](0012-voice-in-v1.md) — the SFU choice and DSP chain order stand, but the echo
canceller changes (see below). Fills in the part [ADR 0006](0006-voice-deferred-with-seams.md) deliberately
left open: what `VoiceServerInfo` contains. Depends on [ADR 0010](0010-client-daemon.md) for the voice-worker's
isolation, and is the reasoning [ADR 0022](0022-access-token-signing-and-scope-model.md) rests on.

## Context
A client's voice-worker holds a **direct WebRTC connection to the SFU** — media never passes through the
daemon or the backend, which is what makes both the latency and the fault isolation real. So the SFU has to
decide for itself whether an arriving participant belongs in a room, and it must do so without asking
Postgres: the flagship runs TURN/SFU as separate pods with `hostNetwork: true` in a privileged namespace,
deliberately NetworkPolicy'd away from the database, precisely because they are the most exposed thing in
the deployment (ADR 0021).

The obvious design is a short-lived signed token the backend mints and the SFU verifies. That is what
LiveKit and Janus do. It also forces a verification key onto the exposed pod, which is what made the
access-token signing algorithm look like a security decision.

Two observations changed the shape of the answer:

1. **The access token must never reach the SFU regardless.** A media pod holding live access tokens for
   every participant could simply *replay* them against the API as those users. That is replay of a genuine
   credential, not forgery, and no signature scheme prevents it.
2. **WebRTC already authenticates.** Every connection carries a DTLS certificate fingerprint in its SDP and
   ICE credentials used to HMAC connectivity checks. Both are mandatory and happen anyway. A bearer token
   layered on top is redundant work that also introduces a stealable secret.

## Decision

**No bearer token participates in a voice connection.** Authentication is the WebRTC handshake:

1. The voice-worker generates an **ephemeral DTLS keypair and ICE credentials** per call. The private key
   never leaves the process.
2. Its fingerprint and ICE ufrag travel up through the daemon and over the **gateway** (op 4), which is
   already authenticated and TLS-protected.
3. The backend resolves `PermConnectVoice` against freshly-loaded data, then provisions the chosen SFU over
   an internal **mTLS** channel: expect this fingerprint, this ufrag, in this room, as this user.
4. `VOICE_SERVER_UPDATE` returns the SFU's endpoint, ICE credentials and fingerprint.
5. ICE checks are HMAC-authenticated; the DTLS handshake **mutually verifies both fingerprints**; SRTP keys
   derive from it.

`VoiceServerInfo` therefore carries connection parameters, never a credential.

**Streams are always per-sender and never mixed.** The client receives distinguishable per-user streams and
mixes locally in the voice-worker.

**The client drives subscription.** Over the connection's data channel it can pin a participant, drop one
(local mute becomes an unsubscribe that also saves bandwidth), or cap how many streams it accepts — which is
what makes §4's bandwidth toggle meaningful for voice. Speaker-based selective forwarding is the *default
policy* when the client expresses no preference, not a fixed rule; small channels naturally receive
everything because everything fits.

**Stream-to-participant mapping is signalled on the data channel**, over pre-allocated transceiver slots.
Joins, leaves and speaker changes become a data-channel message rather than an SDP renegotiation, so a busy
channel does not produce a renegotiation storm.

**Active speaker** comes from the RFC 6464 audio-level header extension, which the SFU reads without
touching the payload, and which also drives selective forwarding. Because levels are self-reported, the SFU
cross-checks against observed packet flow: Opus DTX means a silent participant sends almost nothing, so
packet presence is an independent speech signal that needs no access to the payload.

**Revocation is a push**, not an expiry. A participant banned, kicked, or stripped of `PermConnectVoice`
mid-call is dropped by the backend over the same mTLS control channel. The SFU cannot re-check permissions
itself, and waiting for a credential to lapse is not an acceptable answer to a griefer in a public channel.

**Echo cancellation moves to WebRTC's `AEC3`.** ADR 0012 specified `libspeexdsp` for AEC and AGC; that
component dates from the mid-2000s and is the weakest link in matching the call quality users expect. The
WebRTC Audio Processing Module (AEC3, noise suppression, AGC2, VAD) replaces it, is already cgo so it lands
in the voice-worker where cgo is permitted, and collapses three components into one tuned pipeline. The DSP
chain order from ADR 0012 is unchanged.

**Region selection is a seam, not a v1 feature.** `MediaCoordinator.AllocateSession` chooses which SFU serves
a session, so proximity routing is an implementation change behind an interface that already exists. v1 runs
a single region; the flagship's early users are European and a single European deployment serves them at
parity with commercial platforms.

## Consequences

- **There is no credential to steal.** A compromised SFU pod holds ephemeral ICE and DTLS state for calls it
  is already carrying — it already has the media. It cannot impersonate anyone to the API, mint anything, or
  admit a participant the backend did not provision.
- **Authentication is mutual.** The client verifies the SFU's fingerprint too, so a spoofed or hijacked media
  endpoint cannot man-in-the-middle a call. A bearer token gives the opposite property: the client hands its
  credential to whatever answers.
- **The voice-worker holds no account credential**, which preserves ADR 0010's invariant on the client side.
  That process is the cgo one, parsing hostile RTP from strangers in public matchmaking — the least
  trustworthy thing shipped, and now the one with nothing worth stealing.
- **Integrity of the SDP relay is the property to protect**: the gateway leg (TLS) and the backend→SFU leg
  (mTLS). Substituting a fingerprint there is the only way to subvert the binding.
- A backend→SFU control channel is required regardless, for eviction. That it must exist anyway is what made
  token-based admission redundant.
- **TURN still needs a shared secret** for its long-term credential mechanism, and that secret does live on
  the exposed pod. The prize is bounded: free relay bandwidth, not call access or impersonation. Rotate and
  rate-limit it.
- Per-sender streams cost downstream bandwidth that scales with participants — at Opus 64kbps, twenty
  participants is ~1.2 Mbps unmanaged — which is exactly why subscription is client-driven and capped.
- Per-user streams make **spatial audio and per-user jitter buffers** available: one participant on poor wifi
  degrades only their own stream, rather than forcing a shared buffer that penalises everyone. Neither is
  possible with a mixed stream.
- **Global call quality is bounded by deployment topology, not by this design.** Users far from the single
  region will see higher mouth-to-ear latency that no codec or DSP work compensates for.

## Open question, deliberately not decided here

**End-to-end encrypted media.** Because the SFU only forwards, participants could encrypt payloads with a
group key and leave RTP headers readable for routing — the SFU would never hear plaintext, making "no call
recording, ever" cryptographically enforced rather than policy, and would do *less* work (no decrypt and
re-encrypt per recipient). It contradicts [ADR 0014](0014-e2e-encryption.md)'s "E2E is `DM` only, full stop",
so it must be amended deliberately rather than by drift, and it carries real cost in group key management and
rekeying on join and leave.

If it is taken, one constraint from this ADR carries over: **the RFC 6464 audio-level header extension must
remain SFU-readable.** It drives both the active-speaker indicator and speaker-based selective forwarding,
and encrypting it would cap practical channel size and remove the indicator entirely. The honest cost of
leaving it readable is that the SFU learns who speaks and when — activity metadata, not content.

## Alternatives considered

- **Backend-signed voice token the SFU verifies** (LiveKit, Janus): rejected. It duplicates authentication
  the DTLS handshake performs anyway, puts a verification key on the most exposed pod in the deployment, and
  introduces a stealable secret where none needed to exist. It would also have forced the access-token
  signing decision (ADR 0022) for no gain.
- **Backend provisions an opaque secret the client presents**: viable, and close to what is chosen — the
  difference is that the fingerprint *is* the secret, is never transmitted, and is verified by the protocol
  rather than by comparison. Strictly better for the same control-channel cost.
- **SFU calls back to the backend to validate a token**: rejected — an extra round trip on every join, for a
  weaker property than the handshake already provides.
- **Shared Redis between backend and SFU**: rejected outright, and the worst option considered. It would give
  an internet-exposed privileged pod access to the gateway event bus and rate-limit state — a far larger
  prize than anything else on the table.
- **An MCU that mixes server-side**: rejected. It would save client bandwidth and CPU while destroying
  per-user volume, local mute, spatial audio, and per-user jitter buffers — and mixing is the one thing a
  chat platform's users notice the absence of.
- **A track per participant with SDP renegotiation on join/leave**: rejected in favour of pre-allocated slots,
  which avoid a round trip per membership change and a renegotiation storm in a busy channel.
