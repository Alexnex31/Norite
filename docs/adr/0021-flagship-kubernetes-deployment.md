# ADR 0021: Flagship instance runs on Kubernetes/Helm, self-hosted stays single-binary

## Status
Accepted

## Context
The flagship instance (ADR 0032) is the one deployment that needs real horizontal scale and HA; every
self-hosted instance stays single-process by design (ADR 0020's whole self-hosting simplicity story depends
on this). These are genuinely different operational shapes and need their own architecture rather than
forcing one deployment story to serve both.

## Decision
The flagship runs multiple backend API/gateway replicas as a standard Kubernetes `Deployment` behind a normal
Ingress — only possible because Redis pub/sub fan-out (previously reserved, activated here) lets gateway
events cross replicas instead of living in one process's memory. TURN/SFU pods run separately, with
`hostNetwork: true` (the same pattern real-world Kubernetes WebRTC deployments use), in their own dedicated
"privileged" Pod-Security-Standard namespace, isolated from the "restricted" namespace everything else runs
in. Stateful dependencies run self-managed in-cluster via operators — CloudNativePG (Postgres), a Redis
Helm chart, MinIO (object storage) — not managed cloud services, consistent with the project's self-hosting-
independence ethos applied to its own flagship. `cert-manager` plus Ingress handles TLS here instead of the
backend's built-in `certmagic` (ADR 0020), since every replica racing to manage the same Let's Encrypt cert
would fail. `ulule/limiter` switches to its Redis-backed store here specifically, so replica count can't be
used to multiply an intended rate limit. Database migrations run via a Helm `pre-upgrade` Job hook — same
`golang-migrate` tooling as self-hosted (ADR 0020), different trigger.

**Amended 2026-09-17 — the object-storage choice, not the decision.** This ADR names MinIO because it was
the obvious in-cluster S3-compatible store when this was written. Its community edition has since been
archived: the admin console was removed mid-2025, official binaries and images were discontinued, and the
repository is no longer maintained. The decision this ADR records — self-managed in-cluster operators over
managed cloud services — is unchanged and is what matters here; only the product satisfying one line of it
has to be re-picked, which `docs/roadmap.md` M115 now owns. The body above is left as written, because an
ADR is the record of what was decided and when, not a description of the current world.

Two consequences worth stating so they are not rediscovered. Postgres backups no longer need object storage
at all — M113 moved to CSI volume snapshots, which also sidesteps CloudNativePG deprecating the
`barmanObjectStore` method this ADR's "continuous WAL-archiving" implied. And `minio-go` is unaffected: it
is the S3 *client* SDK, Apache-2.0 and maintained, and speaks to whatever backend M115 lands on.

Graceful rollouts reuse the gateway's existing `Reconnect` op-code via a `preStop` hook, **staggered** across
`terminationGracePeriodSeconds` with client-side randomized exponential backoff — a single replica's
thousands of connections all reconnecting simultaneously would otherwise thundering-herd the remaining
replicas' auth/DB layer. Helm charts package the whole multi-component release as one versioned,
parameterized chart, doubling as a reusable artifact for any self-hoster who wants to run Kubernetes instead
of bare-metal/docker-compose. Deployment is a simple CI-triggered `helm upgrade`, not GitOps — not yet
justified by this deployment's scale.

## Consequences
- Self-hosted instances never activate Redis, the Kubernetes-specific rate-limiter store, or `cert-manager`
  at all — these are flagship-only activations of seams that already exist reserved-but-unused elsewhere.
- This deployment also serves as the reference "how to run this at real scale" pattern for any advanced
  self-hoster who outgrows bare-metal/docker-compose.
- NetworkPolicies restricting pod-to-pod traffic (e.g. TURN/SFU pods have no path to Postgres) are defined
  upfront in the Helm chart — cheap now, meaningfully limits blast radius if the customer-facing, media-
  handling TURN/SFU pods are ever compromised.
- Observability stays minimal for now (the Prometheus endpoint exists, no scraper/dashboard deployed yet) —
  a documented future upgrade, not a v1 requirement for this deployment either.

## Alternatives considered
- **Run the flagship on the same single-binary/docker-compose model as self-hosted instances**: rejected —
  doesn't scale to real public-instance load, and the whole point of activating Redis fan-out/Redis rate
  limiting is that a single process can't serve flagship-scale traffic.
- **GitOps (ArgoCD/FluxCD) from the start**: rejected — its extra guarantees (drift detection, automatic
  reconciliation) aren't yet justified by this deployment's actual scale/team size; a simple CI trigger is
  enough for now.
- **A managed Postgres/Redis/object-storage service (RDS, ElastiCache, S3) instead of in-cluster operators**:
  rejected — contradicts the project's self-hosting-independence framing applied to its own flagship, and
  this deployment doubles as the reference self-hosted-at-scale pattern, which a managed-service dependency
  would undermine.
